package mpp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ── RPC mock helpers ─────────────────────────────────────────────────────────

// rpcHandler returns an http.HandlerFunc that records calls and serves canned
// JSON-RPC responses.
type rpcHandlerConfig struct {
	airdropSig        string
	airdropErr        string
	blockhash         string
	sendTxSig         string
	confirmStatus     string
	confirmErrPayload interface{}
	airdropCallCount  *int32 // optional; incremented each call
}

func defaultRPCConfig() rpcHandlerConfig {
	return rpcHandlerConfig{
		airdropSig: "airdrop_sig_123",
		// Must be a valid base58 string (no 0/O/I/l characters) that decodes to 32 bytes.
		// This is the base58-encoding of 32 zero bytes.
		blockhash:     "11111111111111111111111111111111",
		sendTxSig:     "tx_sig_abc123456789",
		confirmStatus: "confirmed",
	}
}

func buildRPCHandler(cfg rpcHandlerConfig) http.HandlerFunc {
	var mu sync.Mutex
	return func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		_ = json.NewDecoder(r.Body).Decode(&req)

		w.Header().Set("Content-Type", "application/json")

		respond := func(result interface{}) {
			resp := map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  result,
			}
			_ = json.NewEncoder(w).Encode(resp)
		}

		respondErr := func(code int, msg string) {
			resp := map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error":   map[string]interface{}{"code": code, "message": msg},
			}
			_ = json.NewEncoder(w).Encode(resp)
		}

		mu.Lock()
		defer mu.Unlock()

		switch req.Method {
		case "requestAirdrop":
			if cfg.airdropCallCount != nil {
				atomic.AddInt32(cfg.airdropCallCount, 1)
			}
			if cfg.airdropErr != "" {
				respondErr(-32000, cfg.airdropErr)
				return
			}
			respond(cfg.airdropSig)

		case "getSignatureStatuses":
			status := map[string]interface{}{
				"confirmationStatus": cfg.confirmStatus,
				"err":                cfg.confirmErrPayload,
			}
			respond(map[string]interface{}{
				"value": []interface{}{status},
			})

		case "getLatestBlockhash":
			respond(map[string]interface{}{
				"value": map[string]interface{}{
					"blockhash":            cfg.blockhash,
					"lastValidBlockHeight": 100,
				},
			})

		case "sendTransaction":
			respond(cfg.sendTxSig)

		default:
			respondErr(-32601, "method not found")
		}
	}
}

// newTestRPCServer starts an httptest.Server with the given RPC handler config
// and returns both the server and a patched rpcClient pointed at it.
func newTestRPCServer(cfg rpcHandlerConfig) (*httptest.Server, *rpcClient) {
	srv := httptest.NewServer(buildRPCHandler(cfg))
	rpc := newRPCClient(srv.URL)
	// Override the http client timeout for tests.
	rpc.httpClient.Timeout = 5 * time.Second
	return srv, rpc
}

// ── CreateTestClient ─────────────────────────────────────────────────────────

func TestCreateTestClient_DevnetDefaultKeypair(t *testing.T) {
	rpcSrv, _ := newTestRPCServer(defaultRPCConfig())
	defer rpcSrv.Close()

	ctx := context.Background()
	client, err := CreateTestClient(ctx, &TestClientConfig{
		Network: NetworkDevnet,
		RPCURL:  rpcSrv.URL,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if client.Address == "" {
		t.Error("expected non-empty Address")
	}
	if client.Method != "solana" {
		t.Errorf("Method = %q, want %q", client.Method, "solana")
	}
	if client.Network != NetworkDevnet {
		t.Errorf("Network = %q, want %q", client.Network, NetworkDevnet)
	}
}

func TestCreateTestClient_WithSecretKeyNoAirdropOnMainnet(t *testing.T) {
	// Use a 32-byte seed (ed25519 seed size).
	seed := make([]byte, 32)
	seed[0] = 1

	// No real RPC needed since mainnet skips airdrop.
	ctx := context.Background()
	client, err := CreateTestClient(ctx, &TestClientConfig{
		Network:   NetworkMainnet,
		SecretKey: seed,
		RPCURL:    "http://127.0.0.1:1", // unreachable -should not be called
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if client.Address == "" {
		t.Error("expected non-empty Address")
	}
	if client.Network != NetworkMainnet {
		t.Errorf("Network = %q, want %q", client.Network, NetworkMainnet)
	}
}

func TestCreateTestClient_MainnetWithoutSecretKeyErrors(t *testing.T) {
	ctx := context.Background()
	_, err := CreateTestClient(ctx, &TestClientConfig{
		Network: NetworkMainnet,
	})
	if err == nil {
		t.Fatal("expected error for mainnet without SecretKey")
	}
	netErr, ok := err.(*MppNetworkError)
	if !ok {
		t.Fatalf("expected *MppNetworkError, got %T", err)
	}
	if netErr.Network != "mainnet" {
		t.Errorf("Network = %q, want %q", netErr.Network, "mainnet")
	}
	if !strings.Contains(netErr.Message, "mainnet") {
		t.Error("error message should mention 'mainnet'")
	}
	if !strings.Contains(netErr.Message, "SecretKey") {
		t.Error("error message should mention 'SecretKey'")
	}
}

func TestCreateTestClient_OnStepCallbacks(t *testing.T) {
	rpcSrv, _ := newTestRPCServer(defaultRPCConfig())
	defer rpcSrv.Close()

	var steps []PaymentStep
	ctx := context.Background()
	_, err := CreateTestClient(ctx, &TestClientConfig{
		Network: NetworkDevnet,
		RPCURL:  rpcSrv.URL,
		OnStep:  func(s PaymentStep) { steps = append(steps, s) },
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(steps) < 2 {
		t.Fatalf("expected at least 2 steps, got %d", len(steps))
	}
	if steps[0].Type != StepWalletCreated {
		t.Errorf("step[0].Type = %q, want %q", steps[0].Type, StepWalletCreated)
	}
	if steps[1].Type != StepFunded {
		t.Errorf("step[1].Type = %q, want %q", steps[1].Type, StepFunded)
	}
	// wallet-created data must carry address and network.
	if steps[0].Data["address"] == "" {
		t.Error("wallet-created data.address is empty")
	}
	if steps[0].Data["network"] != "devnet" {
		t.Errorf("wallet-created data.network = %v, want devnet", steps[0].Data["network"])
	}
}

func TestCreateTestClient_TestnetAirdrops(t *testing.T) {
	var count int32
	cfg := defaultRPCConfig()
	cfg.airdropCallCount = &count
	rpcSrv, _ := newTestRPCServer(cfg)
	defer rpcSrv.Close()

	ctx := context.Background()
	_, err := CreateTestClient(ctx, &TestClientConfig{
		Network: NetworkTestnet,
		RPCURL:  rpcSrv.URL,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if atomic.LoadInt32(&count) == 0 {
		t.Error("expected airdrop to be called on testnet")
	}
}

// ── client.Fetch -free endpoint ─────────────────────────────────────────────

func TestFetch_FreeEndpoint200(t *testing.T) {
	rpcSrv, _ := newTestRPCServer(defaultRPCConfig())
	defer rpcSrv.Close()

	appSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":"free"}`))
	}))
	defer appSrv.Close()

	ctx := context.Background()
	client, _ := CreateTestClient(ctx, &TestClientConfig{
		Network: NetworkDevnet,
		RPCURL:  rpcSrv.URL,
	})

	resp, err := client.Fetch(ctx, appSrv.URL+"/free", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestFetch_NonOKStatusReturnsMppPaymentError(t *testing.T) {
	rpcSrv, _ := newTestRPCServer(defaultRPCConfig())
	defer rpcSrv.Close()

	appSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer appSrv.Close()

	ctx := context.Background()
	client, _ := CreateTestClient(ctx, &TestClientConfig{
		Network: NetworkDevnet,
		RPCURL:  rpcSrv.URL,
	})

	_, err := client.Fetch(ctx, appSrv.URL+"/private", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	payErr, ok := err.(*MppPaymentError)
	if !ok {
		t.Fatalf("expected *MppPaymentError, got %T", err)
	}
	if payErr.Status != http.StatusForbidden {
		t.Errorf("Status = %d, want 403", payErr.Status)
	}
	if !strings.Contains(payErr.URL, appSrv.URL) {
		t.Errorf("URL %q should contain server URL", payErr.URL)
	}
}

// ── client.Fetch -402 payment flow ──────────────────────────────────────────

const testPaymentRequestHeader = `solana; amount="0.001"; recipient="9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM"; network="devnet"`

func TestFetch_402FlowSucceeds(t *testing.T) {
	rpcSrv, _ := newTestRPCServer(defaultRPCConfig())
	defer rpcSrv.Close()

	callCount := 0
	appSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			w.Header().Set("Payment-Request", testPaymentRequestHeader)
			w.WriteHeader(http.StatusPaymentRequired)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":"paid"}`))
	}))
	defer appSrv.Close()

	ctx := context.Background()
	client, _ := CreateTestClient(ctx, &TestClientConfig{
		Network: NetworkDevnet,
		RPCURL:  rpcSrv.URL,
	})

	resp, err := client.Fetch(ctx, appSrv.URL+"/paid", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want 200", resp.StatusCode)
	}
	if callCount != 2 {
		t.Errorf("server call count = %d, want 2", callCount)
	}
	resp.Body.Close()
}

func TestFetch_402RetryHasPaymentReceiptHeader(t *testing.T) {
	rpcSrv, _ := newTestRPCServer(defaultRPCConfig())
	defer rpcSrv.Close()

	var retryReceiptHeader string
	callCount := 0
	appSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			w.Header().Set("Payment-Request", testPaymentRequestHeader)
			w.WriteHeader(http.StatusPaymentRequired)
			return
		}
		retryReceiptHeader = r.Header.Get("payment-receipt")
		w.WriteHeader(http.StatusOK)
	}))
	defer appSrv.Close()

	ctx := context.Background()
	client, _ := CreateTestClient(ctx, &TestClientConfig{
		Network: NetworkDevnet,
		RPCURL:  rpcSrv.URL,
	})

	_, err := client.Fetch(ctx, appSrv.URL+"/paid", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if retryReceiptHeader == "" {
		t.Fatal("retry request missing payment-receipt header")
	}
	if !strings.Contains(retryReceiptHeader, "solana") {
		t.Error("payment-receipt should contain 'solana'")
	}
	if !strings.Contains(retryReceiptHeader, "tx_sig_abc123456789") {
		t.Errorf("payment-receipt %q should contain the transaction signature", retryReceiptHeader)
	}
	if !strings.HasPrefix(retryReceiptHeader, "solana;") || !strings.Contains(retryReceiptHeader, `signature="tx_sig_abc123456789"`) {
		t.Errorf("payment-receipt header format unexpected: %q", retryReceiptHeader)
	}
}

func TestFetch_402WithoutPaymentRequestHeaderErrors(t *testing.T) {
	rpcSrv, _ := newTestRPCServer(defaultRPCConfig())
	defer rpcSrv.Close()

	appSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired) // no Payment-Request header
	}))
	defer appSrv.Close()

	ctx := context.Background()
	client, _ := CreateTestClient(ctx, &TestClientConfig{
		Network: NetworkDevnet,
		RPCURL:  rpcSrv.URL,
	})

	_, err := client.Fetch(ctx, appSrv.URL+"/paid", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if _, ok := err.(*MppPaymentError); !ok {
		t.Fatalf("expected *MppPaymentError, got %T: %v", err, err)
	}
}

func TestFetch_402MissingRecipientErrors(t *testing.T) {
	rpcSrv, _ := newTestRPCServer(defaultRPCConfig())
	defer rpcSrv.Close()

	appSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Payment-Request", `solana; amount="0.001"; network="devnet"`)
		w.WriteHeader(http.StatusPaymentRequired)
	}))
	defer appSrv.Close()

	ctx := context.Background()
	client, _ := CreateTestClient(ctx, &TestClientConfig{
		Network: NetworkDevnet,
		RPCURL:  rpcSrv.URL,
	})

	_, err := client.Fetch(ctx, appSrv.URL+"/paid", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	payErr, ok := err.(*MppPaymentError)
	if !ok {
		t.Fatalf("expected *MppPaymentError, got %T", err)
	}
	if !strings.Contains(payErr.Message, "recipient") {
		t.Errorf("error message %q should mention 'recipient'", payErr.Message)
	}
}

func TestFetch_402InvalidAmountErrors(t *testing.T) {
	rpcSrv, _ := newTestRPCServer(defaultRPCConfig())
	defer rpcSrv.Close()

	appSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Payment-Request", `solana; amount="not-a-number"; recipient="9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM"; network="devnet"`)
		w.WriteHeader(http.StatusPaymentRequired)
	}))
	defer appSrv.Close()

	ctx := context.Background()
	client, _ := CreateTestClient(ctx, &TestClientConfig{
		Network: NetworkDevnet,
		RPCURL:  rpcSrv.URL,
	})

	_, err := client.Fetch(ctx, appSrv.URL+"/paid", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if _, ok := err.(*MppPaymentError); !ok {
		t.Fatalf("expected *MppPaymentError, got %T", err)
	}
}

func TestFetch_402FlowEmitsAllLifecycleSteps(t *testing.T) {
	rpcSrv, _ := newTestRPCServer(defaultRPCConfig())
	defer rpcSrv.Close()

	callCount := 0
	appSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			w.Header().Set("Payment-Request", testPaymentRequestHeader)
			w.WriteHeader(http.StatusPaymentRequired)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer appSrv.Close()

	var steps []PaymentStep
	ctx := context.Background()
	client, _ := CreateTestClient(ctx, &TestClientConfig{
		Network: NetworkDevnet,
		RPCURL:  rpcSrv.URL,
		OnStep:  func(s PaymentStep) { steps = append(steps, s) },
	})

	steps = nil // clear setup steps
	_, err := client.Fetch(ctx, appSrv.URL+"/paid", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	stepTypes := make(map[PaymentStepType]bool)
	for _, s := range steps {
		stepTypes[s.Type] = true
	}

	for _, expected := range []PaymentStepType{StepRequest, StepPayment, StepRetry, StepSuccess} {
		if !stepTypes[expected] {
			t.Errorf("missing step type %q in emitted steps", expected)
		}
	}
}

func TestFetch_PaymentEventDataHasAmountAndRecipient(t *testing.T) {
	rpcSrv, _ := newTestRPCServer(defaultRPCConfig())
	defer rpcSrv.Close()

	callCount := 0
	appSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			w.Header().Set("Payment-Request", testPaymentRequestHeader)
			w.WriteHeader(http.StatusPaymentRequired)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer appSrv.Close()

	var steps []PaymentStep
	ctx := context.Background()
	client, _ := CreateTestClient(ctx, &TestClientConfig{
		Network: NetworkDevnet,
		RPCURL:  rpcSrv.URL,
		OnStep:  func(s PaymentStep) { steps = append(steps, s) },
	})

	steps = nil
	_, _ = client.Fetch(ctx, appSrv.URL+"/paid", nil)

	for _, s := range steps {
		if s.Type == StepPayment && s.Data["amount"] != nil {
			if s.Data["amount"] != 0.001 {
				t.Errorf("payment event amount = %v, want 0.001", s.Data["amount"])
			}
			if s.Data["recipient"] != "9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM" {
				t.Errorf("payment event recipient = %v", s.Data["recipient"])
			}
			return
		}
	}
	t.Error("no payment step with amount/recipient data found")
}

// ── Airdrop retry ─────────────────────────────────────────────────────────────

func TestCreateTestClient_AirdropRetrySucceedsOnThirdAttempt(t *testing.T) {
	var callCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")

		if req.Method == "requestAirdrop" {
			n := atomic.AddInt32(&callCount, 1)
			if n < 3 {
				// Fail first two attempts.
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      1,
					"error":   map[string]interface{}{"code": -32000, "message": "rate limited"},
				})
				return
			}
			// Succeed on third.
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "airdrop_sig_success",
			})
			return
		}

		if req.Method == "getSignatureStatuses" {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result": map[string]interface{}{
					"value": []interface{}{
						map[string]interface{}{"confirmationStatus": "confirmed", "err": nil},
					},
				},
			})
			return
		}

		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"error":   map[string]interface{}{"code": -32601, "message": "method not found"},
		})
	}))
	defer srv.Close()

	ctx := context.Background()
	client, err := CreateTestClient(ctx, &TestClientConfig{
		Network: NetworkDevnet,
		RPCURL:  srv.URL,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if client == nil {
		t.Fatal("expected non-nil client")
	}
	if atomic.LoadInt32(&callCount) != 3 {
		t.Errorf("airdrop call count = %d, want 3", atomic.LoadInt32(&callCount))
	}
}

func TestCreateTestClient_AirdropAllFailReturnsMppFaucetError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"error":   map[string]interface{}{"code": -32000, "message": "rate limited"},
		})
	}))
	defer srv.Close()

	ctx := context.Background()
	_, err := CreateTestClient(ctx, &TestClientConfig{
		Network: NetworkDevnet,
		RPCURL:  srv.URL,
	})
	if err == nil {
		t.Fatal("expected error")
	}
	faucetErr, ok := err.(*MppFaucetError)
	if !ok {
		t.Fatalf("expected *MppFaucetError, got %T: %v", err, err)
	}
	if faucetErr.Address == "" {
		t.Error("MppFaucetError.Address should not be empty")
	}
	if !strings.Contains(faucetErr.Message, "rate-limited") {
		t.Errorf("error message should mention rate-limiting, got: %s", faucetErr.Message)
	}
}

// ── Timeout ───────────────────────────────────────────────────────────────────

func TestFetch_TimeoutReturnsMppTimeoutError(t *testing.T) {
	rpcSrv, _ := newTestRPCServer(defaultRPCConfig())
	defer rpcSrv.Close()

	// Server that hangs indefinitely.
	appSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(10 * time.Second)
	}))
	defer appSrv.Close()

	ctx := context.Background()
	client, _ := CreateTestClient(ctx, &TestClientConfig{
		Network: NetworkDevnet,
		RPCURL:  rpcSrv.URL,
		Timeout: 50 * time.Millisecond,
	})

	_, err := client.Fetch(ctx, appSrv.URL+"/slow", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	timeoutErr, ok := err.(*MppTimeoutError)
	if !ok {
		t.Fatalf("expected *MppTimeoutError, got %T: %v", err, err)
	}
	if !strings.Contains(timeoutErr.URL, appSrv.URL) {
		t.Errorf("TimeoutError.URL = %q, should contain server URL", timeoutErr.URL)
	}
	if timeoutErr.TimeoutMs != 50 {
		t.Errorf("TimeoutMs = %d, want 50", timeoutErr.TimeoutMs)
	}
}

// ── MppFetch shared client ────────────────────────────────────────────────────

func TestMppFetch_SharedClientCreatedOnce(t *testing.T) {
	ResetMppFetch()
	t.Cleanup(ResetMppFetch)

	var airdropCount int32
	rpcSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")

		switch req.Method {
		case "requestAirdrop":
			atomic.AddInt32(&airdropCount, 1)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"jsonrpc": "2.0", "id": 1, "result": "airdrop_sig",
			})
		case "getSignatureStatuses":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"jsonrpc": "2.0", "id": 1,
				"result": map[string]interface{}{
					"value": []interface{}{
						map[string]interface{}{"confirmationStatus": "confirmed", "err": nil},
					},
				},
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"jsonrpc": "2.0", "id": 1,
				"error": map[string]interface{}{"code": -32601, "message": "not found"},
			})
		}
	}))
	defer rpcSrv.Close()

	appSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer appSrv.Close()

	// Temporarily override the devnet RPC for the shared client by injecting
	// it via the environment-level networkRPC map (restored after).
	origDevnet := networkRPC[NetworkDevnet]
	networkRPC[NetworkDevnet] = rpcSrv.URL
	defer func() { networkRPC[NetworkDevnet] = origDevnet }()

	ctx := context.Background()
	_, err := MppFetch(ctx, appSrv.URL, nil)
	if err != nil {
		t.Fatalf("first MppFetch: %v", err)
	}
	_, err = MppFetch(ctx, appSrv.URL, nil)
	if err != nil {
		t.Fatalf("second MppFetch: %v", err)
	}

	if n := atomic.LoadInt32(&airdropCount); n != 1 {
		t.Errorf("airdrop called %d times, want 1", n)
	}
}

func TestMppFetch_ResetCreatesNewClient(t *testing.T) {
	ResetMppFetch()
	t.Cleanup(ResetMppFetch)

	var airdropCount int32
	rpcSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")

		switch req.Method {
		case "requestAirdrop":
			atomic.AddInt32(&airdropCount, 1)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"jsonrpc": "2.0", "id": 1, "result": "airdrop_sig",
			})
		case "getSignatureStatuses":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"jsonrpc": "2.0", "id": 1,
				"result": map[string]interface{}{
					"value": []interface{}{
						map[string]interface{}{"confirmationStatus": "confirmed", "err": nil},
					},
				},
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"jsonrpc": "2.0", "id": 1,
				"error": map[string]interface{}{"code": -32601, "message": "not found"},
			})
		}
	}))
	defer rpcSrv.Close()

	appSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer appSrv.Close()

	origDevnet := networkRPC[NetworkDevnet]
	networkRPC[NetworkDevnet] = rpcSrv.URL
	defer func() { networkRPC[NetworkDevnet] = origDevnet }()

	ctx := context.Background()
	_, _ = MppFetch(ctx, appSrv.URL, nil)
	ResetMppFetch()
	_, _ = MppFetch(ctx, appSrv.URL, nil)

	if n := atomic.LoadInt32(&airdropCount); n != 2 {
		t.Errorf("airdrop called %d times after reset, want 2", n)
	}
}

// ── parseHeaderParams ─────────────────────────────────────────────────────────

func TestParseHeaderParams(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   map[string]string
	}{
		{
			name:   "full payment-request header",
			header: `solana; amount="0.001"; recipient="9WzD..."; network="devnet"`,
			want:   map[string]string{"amount": "0.001", "recipient": "9WzD...", "network": "devnet"},
		},
		{
			name:   "extra whitespace",
			header: `solana ;  amount = "1.5" ; network = "mainnet"`,
			want:   map[string]string{"amount": "1.5", "network": "mainnet"},
		},
		{
			name:   "no params",
			header: "solana",
			want:   map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseHeaderParams(tt.header)
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("key %q: got %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

// ── base58 round-trip ─────────────────────────────────────────────────────────

func TestBase58RoundTrip(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{"empty", []byte{}},
		{"single byte", []byte{0x01}},
		{"leading zeros", []byte{0, 0, 1, 2, 3}},
		{"32 bytes", make([]byte, 32)},
		{"all 0xff", []byte{0xff, 0xff, 0xff}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoded := base58Encode(tt.data)
			decoded, err := base58Decode(encoded)
			if err != nil {
				t.Fatalf("decode error: %v", err)
			}
			if len(tt.data) == 0 {
				return
			}
			if string(decoded) != string(tt.data) {
				t.Errorf("round-trip failed: got %v, want %v", decoded, tt.data)
			}
		})
	}
}

// ── encodeCompactU16 ──────────────────────────────────────────────────────────

func TestEncodeCompactU16(t *testing.T) {
	tests := []struct {
		n    int
		want []byte
	}{
		{0, []byte{0}},
		{1, []byte{1}},
		{127, []byte{0x7f}},
		{128, []byte{0x80, 0x01}},
		{256, []byte{0x80 | 0, 0x02}},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("n=%d", tt.n), func(t *testing.T) {
			got := encodeCompactU16(tt.n)
			if string(got) != string(tt.want) {
				t.Errorf("encodeCompactU16(%d) = %v, want %v", tt.n, got, tt.want)
			}
		})
	}
}
