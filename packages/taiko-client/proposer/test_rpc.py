import requests
import json

# Define the URL of the JSON-RPC server
url = "http://localhost:1234/rpc"

# Define the headers
headers = {
    "Content-Type": "application/json",
}

# Define the payload for the `GetL2Block` method
payload = {
    "jsonrpc": "2.0",
    "method": "ProposerRPC.GetL2Block",
    "params": [
        {"BlockNumber": 2342}
    ],
    "id": 1,
}

# Print the payload for verification
print("Payload:", json.dumps(payload, indent=4))


# Send the request
response = requests.post(url, headers=headers, data=json.dumps(payload))
print(f"Response: {response.text}")

if response.status_code != 200:
    print(f"Error: {response.status_code}")
    # print(f"Response: {response.text}")
else:
    print("Request was successful.")

print(str(response))
