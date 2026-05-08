// Package mpp integration tests.
//
// These tests exercise the full MPP 402 payment flow using real HTTP servers
// (httptest.NewServer) for both the application layer and the Solana JSON-RPC
// layer. No real Solana network is contacted.
package mpp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// ── Mock Solana RPC server ────────────────────────────────────────────────────

// solanaRPCMock is a lightweight JSON-RPC server that mimics the subset of the
// Solana RPC API used by the Go SDK.
type solanaRPCMock struct {
	// airdropSig is returned for requestAirdrop calls.
	airdropSig string
	// sendTxSig is returned for sendTransaction calls.
	sendTxSig string
	// blockhash is returned for getLatestBlockhash calls.
	blockhash string
	// txResult is returned for getTransaction calls (nil → "null").
	txResult interface{}
	// airdropCalls counts how many requestAirdrop calls were made.
	airdropCalls int32
	// sendTxCalls counts how many sendTransaction calls were made.
	sendTxCalls int32
}

func newSolanaRPCMock() *solanaRPCMock {
	return &solanaRPCMock{
		airdropSig: "mock_airdrop_sig",
		sendTxSig:  "mock_payment_sig_integration",
		// Valid base58 string that decodes to 32 bytes (32 zero bytes = all '1's in base58).
		blockhash: "11111111111111111111111111111111",
	}
}

func (m *solanaRPCMock) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)

		w.Header().Set("Content-Type", "application/json")

		respond := func(result interface{}) {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  result,
			})
		}

		switch req.Method {
		case "requestAirdrop":
			atomic.AddInt32(&m.airdropCalls, 1)
			respond(m.airdropSig)

		case "getSignatureStatuses":
			respond(map[string]interface{}{
				"value": []interface{}{
					map[string]interface{}{
						"confirmationStatus": "confirmed",
						"err":                nil,
					},
				},
			})

		case "getLatestBlockhash":
			respond(map[string]interface{}{
				"value": map[string]interface{}{
					"blockhash":            m.blockhash,
					"lastValidBlockHeight": 9999,
				},
			})

		case "sendTransaction":
			atomic.AddInt32(&m.sendTxCalls, 1)
			respond(m.sendTxSig)

		case "getTransaction":
			if m.txResult == nil {
				// Encode JSON null explicitly.
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":null}`))
				return
			}
			respond(m.txResult)

		default:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error":   map[string]interface{}{"code": -32601, "message": "method not found"},
			})
		}
	}
}

// ── Mock application server ───────────────────────────────────────────────────

// appServerState drives behaviour of the mock application server across calls.
type appServerState struct {
	callCount      int32
	paymentReceipt string // captured from the second (retry) request
}

// buildAppServer returns an httptest.Server + appServerState.
// On the first call it returns 402 with a Payment-Request header referencing
// recipient. On subsequent calls it returns 200.
func buildAppServer(recipient string, amount string) (*httptest.Server, *appServerState) {
	state := &appServerState{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&state.callCount, 1)
		if n == 1 {
			// First request: demand payment.
			w.Header().Set("Payment-Request", `solana; amount="`+amount+`"; recipient="`+recipient+`"; network="devnet"`)
			w.WriteHeader(http.StatusPaymentRequired)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "Payment Required"})
			return
		}
		// Subsequent requests: accepted.
		state.paymentReceipt = r.Header.Get("payment-receipt")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]bool{"paid": true})
	}))
	return srv, state
}

// buildValidTxFixture returns a transaction fixture map that passes all server
// verification checks for the given recipient and lamports.
func buildValidTxFixture(recipient string, lamports float64) map[string]interface{} {
	return map[string]interface{}{
		"meta": map[string]interface{}{
			"err":          nil,
			"preBalances":  []interface{}{float64(2_000_000_000), float64(0)},
			"postBalances": []interface{}{2_000_000_000 - lamports - 5000, lamports},
		},
		"transaction": map[string]interface{}{
			"message": map[string]interface{}{
				"accountKeys": []interface{}{
					map[string]interface{}{"pubkey": "ClientAddr111111111111111111111111111111"},
					map[string]interface{}{"pubkey": recipient},
				},
			},
		},
	}
}

// ── Integration tests ─────────────────────────────────────────────────────────

// integrationRecipient is a valid base58-encoded 32-byte Solana public key used
// in integration tests. All characters are in the base58 alphabet (no 0/O/I/l).
const integrationRecipient = "9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM"

func TestIntegration_FreeEndpointNoPayment(t *testing.T) {
	rpcMock := newSolanaRPCMock()
	rpcSrv := httptest.NewServer(rpcMock.handler())
	defer rpcSrv.Close()

	appSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]bool{"free": true})
	}))
	defer appSrv.Close()

	ctx := context.Background()
	client, err := CreateTestClient(ctx, &TestClientConfig{
		Network: NetworkDevnet,
		RPCURL:  rpcSrv.URL,
	})
	if err != nil {
		t.Fatalf("CreateTestClient: %v", err)
	}

	resp, err := client.Fetch(ctx, appSrv.URL+"/free", nil)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want 200", resp.StatusCode)
	}
	if atomic.LoadInt32(&rpcMock.sendTxCalls) != 0 {
		t.Error("sendTransaction should not have been called for a free endpoint")
	}
}

func TestIntegration_First402ThenPaymentThen200(t *testing.T) {
	rpcMock := newSolanaRPCMock()
	rpcSrv := httptest.NewServer(rpcMock.handler())
	defer rpcSrv.Close()

	appSrv, state := buildAppServer(integrationRecipient, "0.001")
	defer appSrv.Close()

	ctx := context.Background()
	client, err := CreateTestClient(ctx, &TestClientConfig{
		Network: NetworkDevnet,
		RPCURL:  rpcSrv.URL,
	})
	if err != nil {
		t.Fatalf("CreateTestClient: %v", err)
	}

	resp, err := client.Fetch(ctx, appSrv.URL+"/paid", nil)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want 200", resp.StatusCode)
	}
	if atomic.LoadInt32(&state.callCount) != 2 {
		t.Errorf("app server call count = %d, want 2", state.callCount)
	}
	if atomic.LoadInt32(&rpcMock.sendTxCalls) != 1 {
		t.Errorf("sendTransaction call count = %d, want 1", rpcMock.sendTxCalls)
	}
}

func TestIntegration_RetryRequestContainsPaymentReceiptHeader(t *testing.T) {
	rpcMock := newSolanaRPCMock()
	rpcSrv := httptest.NewServer(rpcMock.handler())
	defer rpcSrv.Close()

	appSrv, state := buildAppServer(integrationRecipient, "0.001")
	defer appSrv.Close()

	ctx := context.Background()
	client, err := CreateTestClient(ctx, &TestClientConfig{
		Network: NetworkDevnet,
		RPCURL:  rpcSrv.URL,
	})
	if err != nil {
		t.Fatalf("CreateTestClient: %v", err)
	}

	_, err = client.Fetch(ctx, appSrv.URL+"/paid", nil)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	if state.paymentReceipt == "" {
		t.Fatal("retry request did not include payment-receipt header")
	}
	if !strings.Contains(state.paymentReceipt, "solana") {
		t.Errorf("payment-receipt %q missing 'solana'", state.paymentReceipt)
	}
	if !strings.Contains(state.paymentReceipt, rpcMock.sendTxSig) {
		t.Errorf("payment-receipt %q missing signature %q", state.paymentReceipt, rpcMock.sendTxSig)
	}
	if !strings.Contains(state.paymentReceipt, `signature="`+rpcMock.sendTxSig+`"`) {
		t.Errorf("payment-receipt header format wrong: %q", state.paymentReceipt)
	}
}

func TestIntegration_ServerVerifiesPayment(t *testing.T) {
	// Set up a mock RPC that will be used by both client AND server (same mock).
	rpcMock := newSolanaRPCMock()
	rpcMock.txResult = buildValidTxFixture(integrationRecipient, 1_000_000)
	rpcSrv := httptest.NewServer(rpcMock.handler())
	defer rpcSrv.Close()

	// Set up an MPP server wired to the same mock RPC.
	mppSrv, err := CreateTestServer(&TestServerConfig{
		Network:          NetworkDevnet,
		RecipientAddress: integrationRecipient,
		RPCURL:           rpcSrv.URL,
	})
	if err != nil {
		t.Fatalf("CreateTestServer: %v", err)
	}

	// Application handler behind the charge middleware.
	nextHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]bool{"paid": true})
	})
	wrapped := mppSrv.Charge(ChargeOptions{Amount: "0.001"})(nextHandler)

	// Manually simulate what the client would do after payment:
	// send the retry request with a Payment-Receipt header.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/paid", nil)
	req.Header.Set("payment-receipt", `solana; signature="mock_payment_sig_integration"; network="devnet"; amount="0.001"`)
	wrapped.ServeHTTP(rec, req)

	result := rec.Result()
	if result.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(result.Body)
		t.Errorf("StatusCode = %d, want 200\nbody: %s", result.StatusCode, body)
	}
}

func TestIntegration_ServerReturns403ForBadTransaction(t *testing.T) {
	rpcMock := newSolanaRPCMock()
	rpcMock.txResult = nil // transaction not found
	rpcSrv := httptest.NewServer(rpcMock.handler())
	defer rpcSrv.Close()

	mppSrv, _ := CreateTestServer(&TestServerConfig{
		Network:          NetworkDevnet,
		RecipientAddress: integrationRecipient,
		RPCURL:           rpcSrv.URL,
	})

	nextHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	wrapped := mppSrv.Charge(ChargeOptions{Amount: "0.001"})(nextHandler)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/paid", nil)
	req.Header.Set("payment-receipt", `solana; signature="fake_sig"; network="devnet"; amount="0.001"`)
	wrapped.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("StatusCode = %d, want 403", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "not found") {
		t.Errorf("body should mention 'not found', got: %s", body)
	}
}

func TestIntegration_ServerReturns403ForWrongRecipient(t *testing.T) {
	rpcMock := newSolanaRPCMock()
	rpcMock.txResult = buildValidTxFixture("WrongRecipient11111111111111111111111111", 1_000_000)
	rpcSrv := httptest.NewServer(rpcMock.handler())
	defer rpcSrv.Close()

	mppSrv, _ := CreateTestServer(&TestServerConfig{
		Network:          NetworkDevnet,
		RecipientAddress: integrationRecipient, // different from tx recipient
		RPCURL:           rpcSrv.URL,
	})

	nextHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	wrapped := mppSrv.Charge(ChargeOptions{Amount: "0.001"})(nextHandler)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/paid", nil)
	req.Header.Set("payment-receipt", `solana; signature="sig"; network="devnet"; amount="0.001"`)
	wrapped.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("StatusCode = %d, want 403", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "not found in transaction") {
		t.Errorf("body should mention 'not found in transaction', got: %s", body)
	}
}

func TestIntegration_AirdropCalledOnceOnClientCreation(t *testing.T) {
	rpcMock := newSolanaRPCMock()
	rpcSrv := httptest.NewServer(rpcMock.handler())
	defer rpcSrv.Close()

	ctx := context.Background()
	_, err := CreateTestClient(ctx, &TestClientConfig{
		Network: NetworkDevnet,
		RPCURL:  rpcSrv.URL,
	})
	if err != nil {
		t.Fatalf("CreateTestClient: %v", err)
	}

	if n := atomic.LoadInt32(&rpcMock.airdropCalls); n != 1 {
		t.Errorf("airdropCalls = %d, want 1", n)
	}
}

func TestIntegration_MainnetNoAirdrop(t *testing.T) {
	rpcMock := newSolanaRPCMock()
	rpcSrv := httptest.NewServer(rpcMock.handler())
	defer rpcSrv.Close()

	seed := make([]byte, 32)
	seed[0] = 42

	ctx := context.Background()
	_, err := CreateTestClient(ctx, &TestClientConfig{
		Network:   NetworkMainnet,
		SecretKey: seed,
		RPCURL:    rpcSrv.URL,
	})
	if err != nil {
		t.Fatalf("CreateTestClient: %v", err)
	}

	if n := atomic.LoadInt32(&rpcMock.airdropCalls); n != 0 {
		t.Errorf("airdropCalls = %d on mainnet, want 0", n)
	}
}

func TestIntegration_FullE2EWithLifecycleSteps(t *testing.T) {
	rpcMock := newSolanaRPCMock()
	rpcSrv := httptest.NewServer(rpcMock.handler())
	defer rpcSrv.Close()

	appSrv, _ := buildAppServer(integrationRecipient, "0.001")
	defer appSrv.Close()

	var steps []PaymentStepType
	ctx := context.Background()
	client, err := CreateTestClient(ctx, &TestClientConfig{
		Network: NetworkDevnet,
		RPCURL:  rpcSrv.URL,
		OnStep:  func(s PaymentStep) { steps = append(steps, s.Type) },
	})
	if err != nil {
		t.Fatalf("CreateTestClient: %v", err)
	}

	steps = nil // clear setup steps (wallet-created, funded)
	resp, err := client.Fetch(ctx, appSrv.URL+"/paid", nil)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want 200", resp.StatusCode)
	}

	stepSet := make(map[PaymentStepType]bool)
	for _, s := range steps {
		stepSet[s] = true
	}
	for _, expected := range []PaymentStepType{StepRequest, StepPayment, StepRetry, StepSuccess} {
		if !stepSet[expected] {
			t.Errorf("expected step %q was not emitted; steps = %v", expected, steps)
		}
	}
}

func TestIntegration_PremiumEndpointHigherAmount(t *testing.T) {
	rpcMock := newSolanaRPCMock()
	rpcSrv := httptest.NewServer(rpcMock.handler())
	defer rpcSrv.Close()

	appSrv, state := buildAppServer(integrationRecipient, "0.005")
	defer appSrv.Close()

	ctx := context.Background()
	client, err := CreateTestClient(ctx, &TestClientConfig{
		Network: NetworkDevnet,
		RPCURL:  rpcSrv.URL,
	})
	if err != nil {
		t.Fatalf("CreateTestClient: %v", err)
	}

	resp, err := client.Fetch(ctx, appSrv.URL+"/premium", nil)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want 200", resp.StatusCode)
	}

	// The Payment-Receipt should encode 0.005 SOL.
	if !strings.Contains(state.paymentReceipt, "0.005") {
		t.Errorf("Payment-Receipt %q should contain '0.005'", state.paymentReceipt)
	}
}

func TestIntegration_FetchOptions_CustomHeadersPreserved(t *testing.T) {
	rpcMock := newSolanaRPCMock()
	rpcSrv := httptest.NewServer(rpcMock.handler())
	defer rpcSrv.Close()

	var capturedHeader string
	appSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedHeader = r.Header.Get("x-custom")
		w.WriteHeader(http.StatusOK)
	}))
	defer appSrv.Close()

	ctx := context.Background()
	client, _ := CreateTestClient(ctx, &TestClientConfig{
		Network: NetworkDevnet,
		RPCURL:  rpcSrv.URL,
	})

	_, err := client.Fetch(ctx, appSrv.URL+"/", &FetchOptions{
		Headers: map[string]string{"x-custom": "my-value"},
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if capturedHeader != "my-value" {
		t.Errorf("custom header = %q, want %q", capturedHeader, "my-value")
	}
}
