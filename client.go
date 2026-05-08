package mpp

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SolanaNetwork identifies a Solana cluster.
type SolanaNetwork string

const (
	// NetworkDevnet is the Solana devnet cluster. Supports free SOL airdrops.
	NetworkDevnet SolanaNetwork = "devnet"
	// NetworkTestnet is the Solana testnet cluster. Supports free SOL airdrops.
	NetworkTestnet SolanaNetwork = "testnet"
	// NetworkMainnet is the Solana mainnet-beta cluster. Requires a pre-funded wallet.
	NetworkMainnet SolanaNetwork = "mainnet"
)

// networkRPC maps each network to its default public RPC endpoint.
var networkRPC = map[SolanaNetwork]string{
	NetworkDevnet:  "https://api.devnet.solana.com",
	NetworkTestnet: "https://api.testnet.solana.com",
	NetworkMainnet: "https://api.mainnet-beta.solana.com",
}

// airdropNetworks lists the networks that support free SOL airdrops.
var airdropNetworks = map[SolanaNetwork]bool{
	NetworkDevnet:  true,
	NetworkTestnet: true,
}

// PaymentStepType describes the lifecycle stage of a payment flow event.
type PaymentStepType string

const (
	// StepWalletCreated fires after a new Solana keypair is generated.
	StepWalletCreated PaymentStepType = "wallet-created"
	// StepFunded fires after the wallet has been funded (airdrop or pre-funded).
	StepFunded PaymentStepType = "funded"
	// StepRequest fires when the initial HTTP request is sent.
	StepRequest PaymentStepType = "request"
	// StepPayment fires when a SOL payment is being submitted on-chain.
	StepPayment PaymentStepType = "payment"
	// StepRetry fires when the original request is retried with a Payment-Receipt.
	StepRetry PaymentStepType = "retry"
	// StepSuccess fires when the server accepts the payment and returns 2xx.
	StepSuccess PaymentStepType = "success"
	// StepError fires when the flow encounters an unrecoverable error.
	StepError PaymentStepType = "error"
)

// PaymentStep carries structured information about a single stage of the
// MPP payment lifecycle. Delivered via TestClientConfig.OnStep.
type PaymentStep struct {
	// Type identifies which lifecycle stage fired.
	Type PaymentStepType
	// Message is a human-readable description of the event.
	Message string
	// Data contains optional key/value pairs with additional context.
	Data map[string]interface{}
}

// TestClientConfig holds the configuration for CreateTestClient.
type TestClientConfig struct {
	// Network selects the Solana cluster. Defaults to NetworkDevnet.
	Network SolanaNetwork
	// SecretKey is a 64-byte ed25519 private key or a 32-byte seed.
	// On devnet/testnet it is optional — the wallet is auto-funded via airdrop.
	// On mainnet it is required.
	SecretKey []byte
	// OnStep is an optional callback that receives lifecycle events.
	OnStep func(PaymentStep)
	// Timeout is the maximum duration for the full payment flow.
	// Defaults to 30 seconds.
	Timeout time.Duration
	// RPCURL overrides the default Solana RPC endpoint for the selected network.
	RPCURL string
}

// FetchOptions carries optional parameters for TestClient.Fetch.
type FetchOptions struct {
	// Method is the HTTP method. Defaults to "GET".
	Method string
	// Headers are additional HTTP headers to include in the request.
	Headers map[string]string
	// Body is the request body bytes.
	Body []byte
}

// TestClient is an MPP-aware HTTP client backed by a Solana wallet.
// Use CreateTestClient to construct one.
type TestClient struct {
	// Address is the base58-encoded Solana public key of this client's wallet.
	Address string
	// Network is the Solana cluster this client is connected to.
	Network SolanaNetwork
	// Method is always "solana" and identifies the payment method.
	Method string

	rpc         *rpcClient
	privKey     ed25519.PrivateKey
	pubKeyBytes []byte
	emit        func(PaymentStep)
	timeoutMs   int
	httpClient  *http.Client
}

// Fetch performs an HTTP request with automatic MPP 402 payment handling.
//
// Flow:
//  1. Send the initial request.
//  2. If the server returns 402, parse the Payment-Request header.
//  3. Build and submit a SOL transfer transaction to the Solana network.
//  4. Retry the original request with a Payment-Receipt header containing the
//     transaction signature.
//  5. Return the final response.
//
// If opts is nil a plain GET with no body or extra headers is sent.
func (c *TestClient) Fetch(ctx context.Context, url string, opts *FetchOptions) (*http.Response, error) {
	if opts == nil {
		opts = &FetchOptions{}
	}

	method := opts.Method
	if method == "" {
		method = http.MethodGet
	}

	c.emit(PaymentStep{
		Type:    StepRequest,
		Message: fmt.Sprintf("→ %s", url),
		Data:    map[string]interface{}{"url": url},
	})

	// Apply the per-client timeout via a derived context.
	timeoutCtx, cancel := context.WithTimeout(ctx, time.Duration(c.timeoutMs)*time.Millisecond)
	defer cancel()

	// ── Step 1: Initial request ──────────────────────────────────

	resp, err := c.doRequest(timeoutCtx, method, url, opts.Headers, opts.Body)
	if err != nil {
		if isContextTimeout(timeoutCtx, err) {
			return nil, newMppTimeoutError(url, c.timeoutMs)
		}
		return nil, err
	}

	// ── Non-402 path ─────────────────────────────────────────────

	if resp.StatusCode != http.StatusPaymentRequired {
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			c.emit(PaymentStep{
				Type:    StepError,
				Message: fmt.Sprintf("← %d", resp.StatusCode),
				Data:    map[string]interface{}{"status": resp.StatusCode},
			})
			return nil, newMppPaymentError(url, resp.StatusCode, "")
		}
		c.emit(PaymentStep{
			Type:    StepSuccess,
			Message: fmt.Sprintf("← %d OK", resp.StatusCode),
			Data:    map[string]interface{}{"status": resp.StatusCode},
		})
		return resp, nil
	}
	resp.Body.Close()

	// ── Step 2: Parse Payment-Request ────────────────────────────

	prHeader := resp.Header.Get("Payment-Request")
	if prHeader == "" {
		return nil, newMppPaymentError(url, 402, "server returned 402 without Payment-Request header")
	}

	params := parseHeaderParams(prHeader)

	recipient, ok := params["recipient"]
	if !ok || recipient == "" {
		return nil, newMppPaymentError(url, 402, "Payment-Request header missing recipient field")
	}

	amountStr, ok := params["amount"]
	if !ok || amountStr == "" {
		return nil, newMppPaymentError(url, 402, "Payment-Request header missing amount field")
	}

	amountSOL, err := strconv.ParseFloat(amountStr, 64)
	if err != nil || amountSOL <= 0 {
		return nil, newMppPaymentError(url, 402, fmt.Sprintf("invalid payment amount: %s", amountStr))
	}

	lamports := uint64(math.Round(amountSOL * LamportsPerSol))

	c.emit(PaymentStep{
		Type:    StepPayment,
		Message: fmt.Sprintf("Paying %g SOL → %s...", amountSOL, truncate(recipient, 8)),
		Data:    map[string]interface{}{"amount": amountSOL, "recipient": recipient},
	})

	// ── Step 3: Submit SOL transfer ──────────────────────────────

	signature, err := c.sendPayment(timeoutCtx, recipient, lamports)
	if err != nil {
		if isContextTimeout(timeoutCtx, err) {
			return nil, newMppTimeoutError(url, c.timeoutMs)
		}
		return nil, fmt.Errorf("mpp: payment failed: %w", err)
	}

	c.emit(PaymentStep{
		Type:    StepPayment,
		Message: fmt.Sprintf("Confirmed: %s...", truncate(signature, 16)),
		Data:    map[string]interface{}{"signature": signature, "amount": amountSOL},
	})

	// ── Step 4: Retry with Payment-Receipt ───────────────────────

	c.emit(PaymentStep{
		Type:    StepRetry,
		Message: "↑ Retrying with payment proof",
		Data:    map[string]interface{}{"signature": signature},
	})

	receiptHeader := fmt.Sprintf(
		`solana; signature="%s"; network="%s"; amount="%g"`,
		signature, string(c.Network), amountSOL,
	)

	retryHeaders := make(map[string]string, len(opts.Headers)+1)
	for k, v := range opts.Headers {
		retryHeaders[k] = v
	}
	retryHeaders["payment-receipt"] = receiptHeader

	retryResp, err := c.doRequest(timeoutCtx, method, url, retryHeaders, opts.Body)
	if err != nil {
		if isContextTimeout(timeoutCtx, err) {
			return nil, newMppTimeoutError(url, c.timeoutMs)
		}
		return nil, err
	}

	stepType := StepSuccess
	if retryResp.StatusCode < 200 || retryResp.StatusCode >= 300 {
		stepType = StepError
	}
	c.emit(PaymentStep{
		Type:    stepType,
		Message: fmt.Sprintf("← %d", retryResp.StatusCode),
		Data:    map[string]interface{}{"status": retryResp.StatusCode, "signature": signature},
	})

	return retryResp, nil
}

// sendPayment builds, signs, submits, and confirms a SOL transfer transaction.
func (c *TestClient) sendPayment(ctx context.Context, recipientBase58 string, lamports uint64) (string, error) {
	// Resolve recipient public key.
	toPubKeyBytes, err := base58Decode(recipientBase58)
	if err != nil {
		return "", fmt.Errorf("sendPayment: invalid recipient address: %w", err)
	}
	if len(toPubKeyBytes) != 32 {
		return "", fmt.Errorf("sendPayment: recipient must be a 32-byte public key, got %d bytes", len(toPubKeyBytes))
	}

	// Fetch latest blockhash.
	blockhashB58, _, err := c.rpc.getLatestBlockhash(ctx)
	if err != nil {
		return "", fmt.Errorf("sendPayment: %w", err)
	}

	blockhashBytes, err := base58Decode(blockhashB58)
	if err != nil {
		return "", fmt.Errorf("sendPayment: invalid blockhash: %w", err)
	}
	if len(blockhashBytes) != 32 {
		return "", fmt.Errorf("sendPayment: blockhash must be 32 bytes, got %d", len(blockhashBytes))
	}

	// Build and sign the transaction.
	txBase64, err := buildTransferTransaction(c.privKey, c.pubKeyBytes, toPubKeyBytes, lamports, blockhashBytes)
	if err != nil {
		return "", fmt.Errorf("sendPayment: %w", err)
	}

	// Submit.
	sig, err := c.rpc.sendTransaction(ctx, txBase64)
	if err != nil {
		return "", fmt.Errorf("sendPayment: sendTransaction: %w", err)
	}

	// Wait for confirmation.
	if err := c.rpc.confirmTransaction(ctx, sig); err != nil {
		return "", fmt.Errorf("sendPayment: confirm: %w", err)
	}

	return sig, nil
}

// doRequest performs a single HTTP request and returns the response.
func (c *TestClient) doRequest(ctx context.Context, method, url string, headers map[string]string, body []byte) (*http.Response, error) {
	var bodyReader *bytes.Reader
	if len(body) > 0 {
		bodyReader = bytes.NewReader(body)
	}

	var req *http.Request
	var err error
	if bodyReader != nil {
		req, err = http.NewRequestWithContext(ctx, method, url, bodyReader)
	} else {
		req, err = http.NewRequestWithContext(ctx, method, url, nil)
	}
	if err != nil {
		return nil, fmt.Errorf("doRequest: %w", err)
	}

	for k, v := range headers {
		req.Header.Set(k, v)
	}

	return c.httpClient.Do(req)
}

// isContextTimeout reports whether the error is due to a context deadline/timeout.
func isContextTimeout(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return true
	}
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "context deadline exceeded") ||
		strings.Contains(msg, "context canceled") ||
		strings.Contains(msg, "deadline exceeded")
}

// truncate returns the first n bytes of s, or s itself if shorter.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// parseHeaderParams splits a structured header value on ";" and parses
// key="value" pairs. The first token (e.g. "solana") is ignored.
//
// Example input: `solana; amount="0.001"; recipient="9WzD..."; network="devnet"`
func parseHeaderParams(header string) map[string]string {
	params := make(map[string]string)
	parts := strings.Split(header, ";")
	for _, part := range parts[1:] {
		part = strings.TrimSpace(part)
		eqIdx := strings.IndexByte(part, '=')
		if eqIdx <= 0 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(part[:eqIdx]))
		val := strings.TrimSpace(part[eqIdx+1:])
		val = strings.Trim(val, `"`)
		params[key] = val
	}
	return params
}

// airdropWithRetry requests 2 SOL with up to 3 attempts and exponential backoff.
// Returns MppFaucetError if all attempts fail.
func airdropWithRetry(ctx context.Context, rpc *rpcClient, pubkeyBase58 string) error {
	const maxAttempts = 3
	const airdropLamports = 2 * LamportsPerSol

	for attempt := 0; attempt < maxAttempts; attempt++ {
		sig, err := rpc.requestAirdrop(ctx, pubkeyBase58, airdropLamports)
		if err == nil {
			// Confirm the airdrop transaction.
			if confirmErr := rpc.confirmTransaction(ctx, sig); confirmErr == nil {
				return nil
			}
		}

		if attempt == maxAttempts-1 {
			// Last attempt exhausted.
			break
		}

		// Exponential backoff: 1s, 2s, 4s.
		backoff := time.Duration(1<<uint(attempt)) * time.Second
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
	}

	return newMppFaucetError(pubkeyBase58)
}

// CreateTestClient creates a new MPP test client backed by a Solana wallet.
//
// On devnet and testnet the wallet is automatically funded with 2 SOL via airdrop.
// On mainnet a pre-funded SecretKey must be provided in the config.
//
// Example (devnet, zero config):
//
//	client, err := mpp.CreateTestClient(ctx, nil)
//	res, err := client.Fetch(ctx, "http://localhost:3001/api/data", nil)
//
// Example (mainnet):
//
//	client, err := mpp.CreateTestClient(ctx, &mpp.TestClientConfig{
//	    Network:   mpp.NetworkMainnet,
//	    SecretKey: myKeypairBytes,
//	})
func CreateTestClient(ctx context.Context, config *TestClientConfig) (*TestClient, error) {
	if config == nil {
		config = &TestClientConfig{}
	}

	emit := config.OnStep
	if emit == nil {
		emit = func(PaymentStep) {}
	}

	network := config.Network
	if network == "" {
		network = NetworkDevnet
	}

	timeout := config.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	rpcURL := config.RPCURL
	if rpcURL == "" {
		rpcURL = networkRPC[network]
	}

	// Mainnet requires a pre-funded wallet — no airdrop available.
	if network == NetworkMainnet && len(config.SecretKey) == 0 {
		return nil, newMppNetworkError(
			string(network),
			"CreateTestClient: mainnet requires a pre-funded SecretKey. "+
				"Airdrop is not available on mainnet. "+
				"Pass your keypair's SecretKey in the config.",
		)
	}

	// Generate or restore keypair.
	var privKey ed25519.PrivateKey
	var pubKeyBytes []byte

	if len(config.SecretKey) > 0 {
		switch len(config.SecretKey) {
		case ed25519.SeedSize: // 32-byte seed
			privKey = ed25519.NewKeyFromSeed(config.SecretKey)
		case ed25519.PrivateKeySize: // 64-byte full private key
			privKey = ed25519.PrivateKey(config.SecretKey)
		default:
			return nil, fmt.Errorf("CreateTestClient: SecretKey must be 32 (seed) or 64 (full) bytes, got %d", len(config.SecretKey))
		}
		pubKeyBytes = []byte(privKey.Public().(ed25519.PublicKey))
	} else {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("CreateTestClient: key generation failed: %w", err)
		}
		privKey = priv
		pubKeyBytes = []byte(pub)
	}

	address := base58Encode(pubKeyBytes)
	rpc := newRPCClient(rpcURL)

	emit(PaymentStep{
		Type:    StepWalletCreated,
		Message: fmt.Sprintf("Wallet %s", address),
		Data:    map[string]interface{}{"address": address, "network": string(network)},
	})

	// Fund via airdrop on devnet/testnet; skip on mainnet.
	if airdropNetworks[network] {
		if err := airdropWithRetry(ctx, rpc, address); err != nil {
			return nil, err
		}
		emit(PaymentStep{
			Type:    StepFunded,
			Message: fmt.Sprintf("Wallet funded via %s airdrop (2 SOL)", network),
			Data:    map[string]interface{}{"network": string(network), "amount": 2},
		})
	} else {
		emit(PaymentStep{
			Type:    StepFunded,
			Message: "Using pre-funded mainnet wallet",
			Data:    map[string]interface{}{"network": string(network)},
		})
	}

	return &TestClient{
		Address:     address,
		Network:     network,
		Method:      "solana",
		rpc:         rpc,
		privKey:     privKey,
		pubKeyBytes: pubKeyBytes,
		emit:        emit,
		timeoutMs:   int(timeout.Milliseconds()),
		httpClient:  &http.Client{},
	}, nil
}

// ── Shared client (MppFetch) ─────────────────────────────────────────────────

var (
	sharedClientMu sync.Mutex
	sharedClient   *TestClient
)

// MppFetch is a drop-in replacement for http.Get that automatically handles the
// Solana MPP 402 payment flow. It uses a lazily created shared client (devnet
// by default). Call ResetMppFetch to discard the shared client and force a new
// wallet to be created on the next call.
//
// Example:
//
//	res, err := mpp.MppFetch(ctx, "http://localhost:3001/api/data", nil)
func MppFetch(ctx context.Context, url string, opts *FetchOptions) (*http.Response, error) {
	sharedClientMu.Lock()
	client := sharedClient
	sharedClientMu.Unlock()

	if client == nil {
		var err error
		client, err = CreateTestClient(ctx, nil)
		if err != nil {
			return nil, err
		}
		sharedClientMu.Lock()
		if sharedClient == nil {
			sharedClient = client
		} else {
			client = sharedClient
		}
		sharedClientMu.Unlock()
	}

	return client.Fetch(ctx, url, opts)
}

// ResetMppFetch discards the shared client used by MppFetch. The next call to
// MppFetch will create a fresh Solana wallet and airdrop funds.
func ResetMppFetch() {
	sharedClientMu.Lock()
	sharedClient = nil
	sharedClientMu.Unlock()
}
