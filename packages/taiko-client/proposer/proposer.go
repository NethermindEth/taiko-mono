package proposer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"sync"
	"time"

	gorilla_rcp "github.com/gorilla/rpc/v2"
	"github.com/gorilla/rpc/v2/json2"

	"github.com/ethereum-optimism/optimism/op-service/txmgr"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/urfave/cli/v2"
	"golang.org/x/sync/errgroup"

	"github.com/taikoxyz/taiko-mono/packages/taiko-client/bindings"
	"github.com/taikoxyz/taiko-mono/packages/taiko-client/bindings/encoding"
	"github.com/taikoxyz/taiko-mono/packages/taiko-client/internal/metrics"
	"github.com/taikoxyz/taiko-mono/packages/taiko-client/internal/utils"
	"github.com/taikoxyz/taiko-mono/packages/taiko-client/pkg/rpc"
	selector "github.com/taikoxyz/taiko-mono/packages/taiko-client/proposer/prover_selector"
	builder "github.com/taikoxyz/taiko-mono/packages/taiko-client/proposer/transaction_builder"
)

// Proposer keep proposing new transactions from L2 execution engine's tx pool at a fixed interval.
type Proposer struct {
	// configurations
	*Config

	// RPC clients
	rpc *rpc.Client

	// Private keys and account addresses
	proposerAddress common.Address

	// proposingTimer *time.Timer

	tiers    []*rpc.TierProviderTierWithID
	tierFees []encoding.TierFee

	// Prover selector
	proverSelector selector.ProverSelector

	// Transaction builder
	txBuilder builder.ProposeBlockTransactionBuilder

	// Protocol configurations
	protocolConfigs *bindings.TaikoDataConfig

	lastProposedAt time.Time

	txmgr *txmgr.SimpleTxManager

	ctx context.Context
	wg  sync.WaitGroup
}

// InitFromCli New initializes the given proposer instance based on the command line flags.
func (p *Proposer) InitFromCli(ctx context.Context, c *cli.Context) error {
	cfg, err := NewConfigFromCliContext(c)
	if err != nil {
		return err
	}

	return p.InitFromConfig(ctx, cfg, nil)
}

// InitFromConfig initializes the proposer instance based on the given configurations.
func (p *Proposer) InitFromConfig(ctx context.Context, cfg *Config, txMgr *txmgr.SimpleTxManager) (err error) {
	p.proposerAddress = crypto.PubkeyToAddress(cfg.L1ProposerPrivKey.PublicKey)
	p.ctx = ctx
	p.Config = cfg
	p.lastProposedAt = time.Now()

	// RPC clients
	if p.rpc, err = rpc.NewClient(p.ctx, cfg.ClientConfig); err != nil {
		return fmt.Errorf("initialize rpc clients error: %w", err)
	}

	// Protocol configs
	protocolConfigs, err := p.rpc.TaikoL1.GetConfig(&bind.CallOpts{Context: ctx})
	if err != nil {
		return fmt.Errorf("failed to get protocol configs: %w", err)
	}
	p.protocolConfigs = &protocolConfigs

	log.Info("Protocol configs", "configs", p.protocolConfigs)

	if p.tiers, err = p.rpc.GetTiers(ctx); err != nil {
		return err
	}
	if err := p.initTierFees(); err != nil {
		return err
	}

	if txMgr != nil {
		p.txmgr = txMgr
	} else {
		if p.txmgr, err = txmgr.NewSimpleTxManager(
			"proposer",
			log.Root(),
			&metrics.TxMgrMetrics,
			*cfg.TxmgrConfigs,
		); err != nil {
			return err
		}
	}

	if p.proverSelector, err = selector.NewETHFeeEOASelector(
		&protocolConfigs,
		p.rpc,
		p.proposerAddress,
		cfg.TaikoL1Address,
		cfg.ProverSetAddress,
		p.tierFees,
		cfg.TierFeePriceBump,
		cfg.ProverEndpoints,
		cfg.MaxTierFeePriceBumps,
	); err != nil {
		return err
	}

	if cfg.BlobAllowed {
		p.txBuilder = builder.NewBlobTransactionBuilder(
			p.rpc,
			p.L1ProposerPrivKey,
			p.proverSelector,
			p.Config.L1BlockBuilderTip,
			cfg.TaikoL1Address,
			cfg.ProverSetAddress,
			cfg.L2SuggestedFeeRecipient,
			cfg.ProposeBlockTxGasLimit,
			cfg.ExtraData,
			len(cfg.PreconfirmationRPC) > 0,
		)
	} else {
		p.txBuilder = builder.NewCalldataTransactionBuilder(
			p.rpc,
			p.L1ProposerPrivKey,
			p.proverSelector,
			p.Config.L1BlockBuilderTip,
			cfg.L2SuggestedFeeRecipient,
			cfg.TaikoL1Address,
			cfg.ProverSetAddress,
			cfg.ProposeBlockTxGasLimit,
			cfg.ExtraData,
			len(cfg.PreconfirmationRPC) > 0,
		)
	}

	return nil
}

// Start starts the proposer's main loop.
func (p *Proposer) Start() error {
	startRPCServer(p)

	// p.wg.Add(1)
	// go p.eventLoop()
	return nil
}

// Args represents the arguments to be passed to the RPC method.
type Args struct {
}

type RPCReplyL2TxLists struct {
	TxLists        []types.Transactions
	TxListBytes    [][]byte
	ParentMetaHash common.Hash
}

type CustomResponse struct {
	Result *RPCReplyL2TxLists `json:"result,omitempty"`
	Error  interface{}        `json:"error,omitempty"`
}

// RPC is the receiver type for the RPC methods.
type RPC struct {
	proposer *Proposer
}

func (p *RPC) GetL2TxLists(_ *http.Request, _ *Args, reply *RPCReplyL2TxLists) error {
	txLists, compressedTxLists, err := p.proposer.ProposeOpForTakingL2Blocks(context.Background())
	if err != nil {
		return err
	}
	log.Info("Received L2 txLists ", "txListsLength", len(txLists))
	if len(txLists) == 1 {
		log.Info("Single L2 txList", "txList", txLists[0])
	}

	parentMetaHash, err := builder.GetParentMetaHash(p.proposer.ctx, p.proposer.rpc)
	if err != nil {
		return err
	}

	*reply = RPCReplyL2TxLists{TxLists: txLists, TxListBytes: compressedTxLists, ParentMetaHash: parentMetaHash}
	return nil
}

const rpcPort = 1234

func startRPCServer(proposer *Proposer) {
	s := gorilla_rcp.NewServer()
	s.RegisterCodec(NewCustomCodec(), "application/json")
	proposerRPC := &RPC{proposer: proposer}
	err := s.RegisterService(proposerRPC, "")
	if err != nil {
		log.Error("Failed to register proposer RPC service", "error", err)
	}

	http.Handle("/rpc", s)
	log.Info("Starting JSON-RPC server", "port", rpcPort)

	// Create a custom HTTP server with timeouts
	server := &http.Server{
		Addr:         fmt.Sprintf(":%d", rpcPort),
		Handler:      s,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  15 * time.Second,
	}

	go func() {
		if err := server.ListenAndServe(); err != nil {
			log.Error("Failed to start HTTP server", "error", err)
		}
	}()
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
		response.Result = reply.(*RPCReplyL2TxLists)
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	encoder := json.NewEncoder(w)
	return encoder.Encode(response)
}

// eventLoop starts the main loop of Taiko proposer.
// func (p *Proposer) eventLoop() {
// 	defer func() {
// 		p.proposingTimer.Stop()
// 		p.wg.Done()
// 	}()

// 	for {
// 		p.updateProposingTicker()

// 		select {
// 		case <-p.ctx.Done():
// 			return
// 		// proposing interval timer has been reached
// 		case <-p.proposingTimer.C:
// 			metrics.ProposerProposeEpochCounter.Add(1)

// 			// Attempt a proposing operation
// 			if err := p.ProposeOp(p.ctx); err != nil {
// 				log.Error("Proposing operation error", "error", err)
// 				continue
// 			}
// 		}
// 	}
// }

// Close closes the proposer instance.
func (p *Proposer) Close(_ context.Context) {
	p.wg.Wait()
}

// fetchPoolContent fetches the transaction pool content from L2 execution engine.
func (p *Proposer) fetchPoolContent(filterPoolContent bool) ([]types.Transactions, error) {
	// Fetch the pool content.
	preBuiltTxList, err := p.rpc.GetPoolContent(
		p.ctx,
		p.proposerAddress,
		p.protocolConfigs.BlockMaxGasLimit,
		rpc.BlockMaxTxListBytes,
		p.LocalAddresses,
		p.MaxProposedTxListsPerEpoch,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch transaction pool content: %w", err)
	}

	txLists := []types.Transactions{}
	for i, txs := range preBuiltTxList {
		// Filter the pool content if the filterPoolContent flag is set.
		if txs.EstimatedGasUsed < p.MinGasUsed && txs.BytesLength < p.MinTxListBytes && filterPoolContent {
			log.Info(
				"Pool content skipped",
				"index", i,
				"estimatedGasUsed", txs.EstimatedGasUsed,
				"minGasUsed", p.MinGasUsed,
				"bytesLength", txs.BytesLength,
				"minBytesLength", p.MinTxListBytes,
			)
			break
		}
		txLists = append(txLists, txs.TxList)
	}
	// If the pool content is empty and the checkPoolContent flag is not set, return an empty list.
	if !filterPoolContent && len(txLists) == 0 {
		log.Info(
			"Pool content is empty, proposing an empty block",
			"lastProposedAt", p.lastProposedAt,
			"minProposingInternal", p.MinProposingInternal,
		)
		txLists = append(txLists, types.Transactions{})
	}

	// If LocalAddressesOnly is set, filter the transactions by the local addresses.
	if p.LocalAddressesOnly {
		var (
			localTxsLists []types.Transactions
			signer        = types.LatestSignerForChainID(p.rpc.L2.ChainID)
		)
		for _, txs := range txLists {
			var filtered types.Transactions
			for _, tx := range txs {
				sender, err := types.Sender(signer, tx)
				if err != nil {
					return nil, err
				}

				for _, localAddress := range p.LocalAddresses {
					if sender == localAddress {
						filtered = append(filtered, tx)
					}
				}
			}

			if filtered.Len() != 0 {
				localTxsLists = append(localTxsLists, filtered)
			}
		}
		txLists = localTxsLists
	}

	log.Info("Transactions lists count", "count", len(txLists))

	return txLists, nil
}

// ProposeOp performs a proposing operation, fetching transactions
// from L2 execution engine's tx pool, splitting them by proposing constraints,
// and then proposing them to TaikoL1 contract.
func (p *Proposer) ProposeOp(ctx context.Context) error {
	// Check if it's time to propose unfiltered pool content.
	filterPoolContent := time.Now().Before(p.lastProposedAt.Add(p.MinProposingInternal))

	// Wait until L2 execution engine is synced at first.
	if err := p.rpc.WaitTillL2ExecutionEngineSynced(ctx); err != nil {
		return fmt.Errorf("failed to wait until L2 execution engine synced: %w", err)
	}

	log.Info(
		"Start fetching L2 execution engine's transaction pool content",
		"filterPoolContent", filterPoolContent,
		"lastProposedAt", p.lastProposedAt,
	)

	txLists, err := p.fetchPoolContent(filterPoolContent)
	if err != nil {
		return err
	}

	// If the pool content is empty, return.
	if len(txLists) == 0 {
		return nil
	}

	g, gCtx := errgroup.WithContext(ctx)
	// Propose all L2 transactions lists.
	for _, txs := range txLists[:utils.Min(p.MaxProposedTxListsPerEpoch, uint64(len(txLists)))] {
		nonce, err := p.rpc.L1.PendingNonceAt(ctx, p.proposerAddress)
		if err != nil {
			log.Error("Failed to get proposer nonce", "error", err)
			break
		}

		log.Info("Proposer current pending nonce", "nonce", nonce)

		g.Go(func() error {
			txListBytes, err := rlp.EncodeToBytes(txs)
			if err != nil {
				return fmt.Errorf("failed to encode transactions: %w", err)
			}
			if err := p.ProposeTxList(gCtx, txListBytes, uint(txs.Len())); err != nil {
				return err
			}
			p.lastProposedAt = time.Now()
			return nil
		})

		if err := p.rpc.WaitL1NewPendingTransaction(ctx, p.proposerAddress, nonce); err != nil {
			log.Error("Failed to wait for new pending transaction", "error", err)
		}
	}
	if err := g.Wait(); err != nil {
		return err
	}

	return nil
}

func (p *Proposer) ProposeOpForTakingL2Blocks(ctx context.Context) ([]types.Transactions, [][]byte, error) {
	log.Info("ProposeOpForTakingL2Blocks")
	// Check if it's time to propose unfiltered pool content.
	filterPoolContent := time.Now().Before(p.lastProposedAt.Add(p.MinProposingInternal))

	// Wait until L2 execution engine is synced at first.
	if err := p.rpc.WaitTillL2ExecutionEngineSynced(ctx); err != nil {
		return nil, nil, fmt.Errorf("failed to wait until L2 execution engine synced: %w", err)
	}

	log.Info(
		"Start fetching L2 execution engine's transaction pool content",
		"filterPoolContent", filterPoolContent,
		"lastProposedAt", p.lastProposedAt,
	)

	txLists, err := p.fetchPoolContent(filterPoolContent)
	if err != nil {
		return nil, nil, err
	}

	// If the pool content is empty, return.
	if len(txLists) == 0 {
		return nil, nil, nil
	}

	if len(txLists) == 1 && len(txLists[0]) == 0 {
		return []types.Transactions{}, [][]byte{}, nil
	}

	compressedTxLists := [][]byte{}
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get parent meta hash: %w", err)
	}

	//TODO adjust the Max value
	for _, txs := range txLists[:utils.Min(p.MaxProposedTxListsPerEpoch, uint64(len(txLists)))] {
		txListBytes, err := rlp.EncodeToBytes(txs)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to encode transactions: %w", err)
		}
		compressedTxListBytes, err := utils.Compress(txListBytes)
		if err != nil {
			return nil, nil, err
		}
		compressedTxLists = append(compressedTxLists, compressedTxListBytes)
		p.lastProposedAt = time.Now() //TODO check if it's correct
	}

	return txLists, compressedTxLists, nil
}

// ProposeTxList proposes the given transactions list to TaikoL1 smart contract.
func (p *Proposer) ProposeTxList(
	ctx context.Context,
	txListBytes []byte,
	txNum uint,
) error {
	compressedTxListBytes, err := utils.Compress(txListBytes)
	if err != nil {
		return err
	}

	var (
		l1StateBlockNumber = uint32(0)
		timestamp          = uint64(0)
		parentMetaHash     = [32]byte{}
	)

	parent, err := p.getParentOfLatestProposedBlock(ctx, p.rpc)
	if err != nil {
		return err
	}

	if p.IncludeParentMetaHash {
		parentMetaHash = parent.MetaHash
	}

	txCandidate, err := p.txBuilder.Build(
		ctx,
		p.tierFees,
		compressedTxListBytes,
		l1StateBlockNumber,
		timestamp,
		parentMetaHash,
	)
	if err != nil {
		log.Warn("Failed to build TaikoL1.proposeBlock transaction", "error", encoding.TryParsingCustomError(err))
		return err
	}

	receipt, err := p.txmgr.Send(ctx, *txCandidate)
	if err != nil {
		log.Warn("Failed to send TaikoL1.proposeBlock transaction", "error", encoding.TryParsingCustomError(err))
		return err
	}

	if receipt.Status != types.ReceiptStatusSuccessful {
		return fmt.Errorf("failed to propose block: %s", receipt.TxHash.Hex())
	}

	log.Info("📝 Propose transactions succeeded", "txs", txNum)

	metrics.ProposerProposedTxListsCounter.Add(1)
	metrics.ProposerProposedTxsCounter.Add(float64(txNum))

	return nil
}

// updateProposingTicker updates the internal proposing timer.
// func (p *Proposer) updateProposingTicker() {
// 	if p.proposingTimer != nil {
// 		p.proposingTimer.Stop()
// 	}

// 	var duration time.Duration
// 	if p.ProposeInterval != 0 {
// 		duration = p.ProposeInterval
// 	} else {
// 		// Random number between 12 - 120
// 		randomSeconds := rand.Intn(120-11) + 12 // nolint: gosec
// 		duration = time.Duration(randomSeconds) * time.Second
// 	}

// 	p.proposingTimer = time.NewTimer(duration)
// }

// Name returns the application name.
func (p *Proposer) Name() string {
	return "proposer"
}

// initTierFees initializes the proving fees for every proof tier configured in the protocol for the proposer.
func (p *Proposer) initTierFees() error {
	for _, tier := range p.tiers {
		log.Info(
			"Protocol tier",
			"id", tier.ID,
			"name", string(bytes.TrimRight(tier.VerifierName[:], "\x00")),
			"validityBond", utils.WeiToEther(tier.ValidityBond),
			"contestBond", utils.WeiToEther(tier.ContestBond),
			"provingWindow", tier.ProvingWindow,
			"cooldownWindow", tier.CooldownWindow,
		)

		switch tier.ID {
		case encoding.TierOptimisticID:
			p.tierFees = append(p.tierFees, encoding.TierFee{Tier: tier.ID, Fee: p.OptimisticTierFee})
		case encoding.TierSgxID:
			p.tierFees = append(p.tierFees, encoding.TierFee{Tier: tier.ID, Fee: p.SgxTierFee})
		case encoding.TierGuardianMinorityID:
			p.tierFees = append(p.tierFees, encoding.TierFee{Tier: tier.ID, Fee: common.Big0})
		case encoding.TierGuardianMajorityID:
			// Guardian prover should not charge any fee.
			p.tierFees = append(p.tierFees, encoding.TierFee{Tier: tier.ID, Fee: common.Big0})
		default:
			return fmt.Errorf("unknown tier: %d", tier.ID)
		}
	}

	return nil
}

// getParentOfLatestProposedBlock returns the parent block of the latest proposed block in protocol
func (p *Proposer) getParentOfLatestProposedBlock(
	ctx context.Context,
	rpc *rpc.Client,
) (*bindings.TaikoDataBlock, error) {
	state, err := rpc.TaikoL1.State(&bind.CallOpts{Context: ctx})
	if err != nil {
		return nil, err
	}

	parent, err := rpc.GetL2BlockInfo(ctx, new(big.Int).SetUint64(state.SlotB.NumBlocks-1))
	if err != nil {
		return nil, err
	}

	return &parent, nil
}
