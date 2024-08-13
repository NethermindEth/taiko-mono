package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	gorilla_rcp "github.com/gorilla/rpc/v2"
	"github.com/gorilla/rpc/v2/json2"

	"github.com/cenkalti/backoff/v4"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/beacon/engine"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
	"github.com/urfave/cli/v2"

	"github.com/taikoxyz/taiko-mono/packages/taiko-client/bindings"
	chainSyncer "github.com/taikoxyz/taiko-mono/packages/taiko-client/driver/chain_syncer"
	"github.com/taikoxyz/taiko-mono/packages/taiko-client/driver/state"
	"github.com/taikoxyz/taiko-mono/packages/taiko-client/pkg/rpc"
)

const (
	protocolStatusReportInterval     = 30 * time.Second
	exchangeTransitionConfigInterval = 1 * time.Minute
)

// Driver keeps the L2 execution engine's local block chain in sync with the TaikoL1
// contract.
type Driver struct {
	*Config
	rpc           *rpc.Client
	l2ChainSyncer *chainSyncer.L2ChainSyncer
	state         *state.State

	l1HeadCh  chan *types.Header
	l1HeadSub event.Subscription

	ctx context.Context
	wg  sync.WaitGroup

	blockProposedEventChan chan *bindings.TaikoL1ClientBlockProposed
}

// InitFromCli initializes the given driver instance based on the command line flags.
func (d *Driver) InitFromCli(ctx context.Context, c *cli.Context) error {
	cfg, err := NewConfigFromCliContext(c)
	if err != nil {
		return err
	}

	return d.InitFromConfig(ctx, cfg)
}

// InitFromConfig initializes the driver instance based on the given configurations.
func (d *Driver) InitFromConfig(ctx context.Context, cfg *Config) (err error) {
	d.l1HeadCh = make(chan *types.Header, 1024)
	d.ctx = ctx
	d.Config = cfg

	if d.rpc, err = rpc.NewClient(d.ctx, cfg.ClientConfig); err != nil {
		log.Error("error initializing rpc.NewClient", "error", err)
		return err
	}

	if d.state, err = state.New(d.ctx, d.rpc); err != nil {
		log.Error("error initializing state.New", "error", err)
		return err
	}

	peers, err := d.rpc.L2.PeerCount(d.ctx)
	if err != nil {
		return err
	}

	if cfg.P2PSync && peers == 0 {
		log.Warn("P2P syncing verified blocks enabled, but no connected peer found in L2 execution engine")
	}

	eventChan := make(chan *bindings.TaikoL1ClientBlockProposed)
	d.blockProposedEventChan = eventChan

	if d.l2ChainSyncer, err = chainSyncer.New(
		d.ctx,
		d.rpc,
		d.state,
		cfg.P2PSync,
		cfg.P2PSyncTimeout,
		cfg.MaxExponent,
		cfg.BlobServerEndpoint,
		cfg.SocialScanEndpoint,
		eventChan,
	); err != nil {
		return err
	}

	d.l1HeadSub = d.state.SubL1HeadsFeed(d.l1HeadCh)

	return nil
}

// Start starts the driver instance.
func (d *Driver) Start() error {
	go d.startRPCServer()
	go d.eventLoop()
	go d.reportProtocolStatus()
	go d.exchangeTransitionConfigLoop()

	return nil
}

// Close closes the driver instance.
func (d *Driver) Close(_ context.Context) {
	d.l1HeadSub.Unsubscribe()
	d.state.Close()
	d.wg.Wait()
}

// eventLoop starts the main loop of a L2 execution engine's driver.
func (d *Driver) eventLoop() {
	d.wg.Add(1)
	defer d.wg.Done()

	syncNotify := make(chan struct{}, 1)
	// reqSync requests performing a synchronising operation, won't block
	// if we are already synchronising.
	reqSync := func() {
		select {
		case syncNotify <- struct{}{}:
		default:
		}
	}

	// doSyncWithBackoff performs a synchronising operation with a backoff strategy.
	doSyncWithBackoff := func() {
		if err := backoff.Retry(
			d.doSync,
			backoff.WithContext(backoff.NewConstantBackOff(d.RetryInterval), d.ctx),
		); err != nil {
			log.Error("Sync L2 execution engine's block chain error", "error", err)
		}
	}

	// Call doSync() right away to catch up with the latest known L1 head.
	doSyncWithBackoff()

	for {
		select {
		case <-d.ctx.Done():
			return
		case <-syncNotify:
			doSyncWithBackoff()
		case <-d.l1HeadCh:
			reqSync()
		}
	}
}

// doSync fetches all `BlockProposed` events emitted from local
// L1 sync cursor to the L1 head, and then applies all corresponding
// L2 blocks into node's local blockchain.
func (d *Driver) doSync() error {
	// Check whether the application is closing.
	if d.ctx.Err() != nil {
		log.Warn("Driver context error", "error", d.ctx.Err())
		return nil
	}

	if err := d.l2ChainSyncer.Sync(); err != nil {
		log.Error("Process new L1 blocks error", "error", err)
		return err
	}

	return nil
}

// ChainSyncer returns the driver's chain syncer, this method
// should only be used for testing.
func (d *Driver) ChainSyncer() *chainSyncer.L2ChainSyncer {
	return d.l2ChainSyncer
}

// reportProtocolStatus reports some protocol status intervally.
func (d *Driver) reportProtocolStatus() {
	var (
		ticker       = time.NewTicker(protocolStatusReportInterval)
		maxNumBlocks uint64
	)
	d.wg.Add(1)

	defer func() {
		ticker.Stop()
		d.wg.Done()
	}()

	if err := backoff.Retry(
		func() error {
			if d.ctx.Err() != nil {
				return nil
			}
			configs, err := d.rpc.TaikoL1.GetConfig(&bind.CallOpts{Context: d.ctx})
			if err != nil {
				return err
			}

			maxNumBlocks = configs.BlockMaxProposals
			return nil
		},
		backoff.WithContext(backoff.NewConstantBackOff(d.RetryInterval), d.ctx),
	); err != nil {
		log.Error("Failed to get protocol state variables", "error", err)
		return
	}

	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			vars, err := d.rpc.GetProtocolStateVariables(&bind.CallOpts{Context: d.ctx})
			if err != nil {
				log.Error("Failed to get protocol state variables", "error", err)
				continue
			}

			log.Info(
				"📖 Protocol status",
				"lastVerifiedBlockId", vars.B.LastVerifiedBlockId,
				"pendingBlocks", vars.B.NumBlocks-vars.B.LastVerifiedBlockId-1,
				"availableSlots", vars.B.LastVerifiedBlockId+maxNumBlocks-vars.B.NumBlocks,
			)
		}
	}
}

// exchangeTransitionConfigLoop keeps exchanging transition configs with the
// L2 execution engine.
func (d *Driver) exchangeTransitionConfigLoop() {
	ticker := time.NewTicker(exchangeTransitionConfigInterval)
	d.wg.Add(1)

	defer func() {
		ticker.Stop()
		d.wg.Done()
	}()

	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			tc, err := d.rpc.L2Engine.ExchangeTransitionConfiguration(d.ctx, &engine.TransitionConfigurationV1{
				TerminalTotalDifficulty: (*hexutil.Big)(common.Big0),
				TerminalBlockHash:       common.Hash{},
				TerminalBlockNumber:     0,
			})
			if err != nil {
				log.Error("Failed to exchange Transition Configuration", "error", err)
			} else {
				log.Debug("Exchanged transition config", "transitionConfig", tc)
			}
		}
	}
}

// Name returns the application name.
func (d *Driver) Name() string {
	return "driver"
}

// Args represents the arguments to be passed to the RPC method.
type Args struct {
	TxLists []types.Transactions
	GasUsed uint64
}

// RPC is the receiver type for the RPC methods.
type RPC struct {
	driver *Driver
}

func (p *RPC) AdvanceL2ChainHeadWithNewBlocks(_ *http.Request, args *Args, reply *string) error {
	log.Info("AdvanceL2ChainHeadWithNewBlocks", "args", args)
	syncer := p.driver.l2ChainSyncer.BlobSyncer()

	for _, txList := range args.TxLists {
		err := syncer.MoveTheHead(p.driver.ctx, txList)
		if err != nil {
			log.Error("Failed to move the head with new block", "error", err)
			return err
		}
	}

	*reply = "Request received and processed successfully"
	return nil
}

type RPCReplyBlockProposed struct {
	BlockID    uint64
	TxListHash [32]byte
	Proposer   common.Address
}

func (p *RPC) WaitForBlockProposed(_ *http.Request, _ *Args, reply *RPCReplyBlockProposed) error {
	log.Info("Waiting for BlockProposed event")
	blockProposedEvent := <-p.driver.blockProposedEventChan
	*reply = RPCReplyBlockProposed{
		BlockID:    blockProposedEvent.BlockId.Uint64(),
		TxListHash: blockProposedEvent.Meta.BlobHash,
		Proposer:   blockProposedEvent.Meta.Sender,
	}
	log.Info("BlockProposed event received", "reply", *reply)
	return nil
}

const rpcPort = 1235

func (d *Driver) startRPCServer() {
	s := gorilla_rcp.NewServer()
	s.RegisterCodec(NewCustomCodec(), "application/json")
	driverRPC := &RPC{driver: d}
	if err := s.RegisterService(driverRPC, ""); err != nil {
		log.Error("Failed to register driver RPC service", "error", err)
	}

	http.Handle("/rpc", s)
	log.Info("Starting JSON-RPC server", "port", rpcPort, "writeTimeout", d.RpcWriteTimeout)
	// Create a custom HTTP server with timeouts
	server := &http.Server{
		Addr:         fmt.Sprintf(":%d", rpcPort),
		Handler:      s,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: d.RpcWriteTimeout,
		IdleTimeout:  15 * time.Second,
	}

	go func() {
		if err := server.ListenAndServe(); err != nil {
			log.Error("Failed to start HTTP server", "error", err)
		}
	}()
}

type CustomResponse struct {
	Result *string     `json:"result,omitempty"`
	Error  interface{} `json:"error,omitempty"`
}

type CustomCodec struct {
	*json2.Codec
}

func NewCustomCodec() *CustomCodec {
	return &CustomCodec{json2.NewCodec()}
}

func (c *CustomCodec) WriteResponse(w http.ResponseWriter, reply interface{}, methodErr error) error {
	response := CustomResponse{}

	if methodErr != nil {
		response.Error = methodErr.Error()
	} else if reply != nil {
		response.Result = reply.(*string)
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	encoder := json.NewEncoder(w)
	return encoder.Encode(response)
}
