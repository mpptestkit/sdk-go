# mpp-test-sdk-go

A production-ready Go SDK for the **Machine Payments Protocol (MPP)** on Solana.

Implements the full HTTP 402 payment flow in pure Go -no external Solana libraries required. Uses only the standard library (`crypto/ed25519`, `net/http`, `encoding/json`, etc.) and talks to Solana via raw JSON-RPC.

## Installation

```bash
go get github.com/mpptestkit/mpp-test-sdk-go
```

## Quick start

### Client

```go
import mpp "github.com/mpptestkit/mpp-test-sdk-go"

// Zero config -auto-generates a Solana wallet and airdrops 2 SOL on devnet.
client, err := mpp.CreateTestClient(ctx, nil)
if err != nil {
    log.Fatal(err)
}

// Fetch automatically handles the 402 → pay → retry flow.
resp, err := client.Fetch(ctx, "http://localhost:3001/api/data", nil)
```

### Shared client (drop-in)

```go
// MppFetch lazily creates a shared client on first call.
resp, err := mpp.MppFetch(ctx, "http://localhost:3001/api/data", nil)

// Reset to force a new wallet on the next call.
mpp.ResetMppFetch()
```

### Server middleware

```go
import mpp "github.com/mpptestkit/mpp-test-sdk-go"

server, err := mpp.CreateTestServer(&mpp.TestServerConfig{
    Network: mpp.NetworkDevnet,
})
if err != nil {
    log.Fatal(err)
}

mux := http.NewServeMux()
mux.Handle("/api/data",
    server.Charge(mpp.ChargeOptions{Amount: "0.001"})(
        http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            w.Header().Set("Content-Type", "application/json")
            _, _ = w.Write([]byte(`{"data":"premium content"}`))
        }),
    ),
)
http.ListenAndServe(":3001", mux)
```

## Protocol

**402 flow:**

1. Client sends a request.
2. Server returns `402 Payment Required` with a `Payment-Request` header:
   ```
   Payment-Request: solana; amount="0.001"; recipient="9WzD..."; network="devnet"
   ```
3. Client submits a SOL transfer on-chain and gets a transaction signature.
4. Client retries the original request with a `Payment-Receipt` header:
   ```
   Payment-Receipt: solana; signature="3xKm7..."; network="devnet"; amount="0.001"
   ```
5. Server verifies the transaction on-chain and calls the next handler on success.

## Configuration

### `TestClientConfig`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `Network` | `SolanaNetwork` | `"devnet"` | Solana cluster |
| `SecretKey` | `[]byte` | -| 32-byte seed or 64-byte private key. Auto-generated on devnet/testnet. **Required on mainnet.** |
| `OnStep` | `func(PaymentStep)` | -| Lifecycle event callback |
| `Timeout` | `time.Duration` | 30s | Full flow timeout |
| `RPCURL` | `string` | cluster default | Override Solana RPC endpoint |

### `TestServerConfig`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `Network` | `SolanaNetwork` | `"devnet"` | Solana cluster |
| `SecretKey` | `[]byte` | -| Server keypair (auto-generated if omitted) |
| `RecipientAddress` | `string` | server keypair pubkey | Override payment recipient |
| `RPCURL` | `string` | cluster default | Override Solana RPC endpoint |

## Networks

| Constant | Value | Airdrop |
|----------|-------|---------|
| `mpp.NetworkDevnet` | `"devnet"` | Yes (2 SOL) |
| `mpp.NetworkTestnet` | `"testnet"` | Yes (2 SOL) |
| `mpp.NetworkMainnet` | `"mainnet"` | No -pre-funded `SecretKey` required |

## Error types

| Type | When |
|------|------|
| `*MppNetworkError` | Mainnet used without `SecretKey` |
| `*MppFaucetError` | Airdrop failed after 3 retries |
| `*MppPaymentError` | Unexpected HTTP status or malformed `Payment-Request` |
| `*MppTimeoutError` | Full flow exceeded `Timeout` |

```go
resp, err := client.Fetch(ctx, url, nil)
var timeoutErr *mpp.MppTimeoutError
if errors.As(err, &timeoutErr) {
    fmt.Printf("timed out after %dms\n", timeoutErr.TimeoutMs)
}
```

## Lifecycle events

```go
client, _ := mpp.CreateTestClient(ctx, &mpp.TestClientConfig{
    OnStep: func(step mpp.PaymentStep) {
        fmt.Printf("[%s] %s\n", step.Type, step.Message)
    },
})
```

Events: `wallet-created`, `funded`, `request`, `payment`, `retry`, `success`, `error`.

## Implementation notes

- **No external dependencies** -pure Go standard library only.
- **Solana transactions** built from scratch: compact-u16 encoding, ed25519 signing, legacy transaction wire format.
- **Base58** implemented without `math/big` dependencies (uses `math/big` from stdlib).
- **Airdrop retry** -3 attempts with exponential back-off (1s, 2s, 4s).
- **Confirmation polling** -polls `getSignatureStatuses` up to 60 seconds.

## Running tests

```bash
go test ./... -v
```

All tests use `httptest.NewServer` — no real Solana network is contacted.
