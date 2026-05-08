package mpp

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
)

// TestServerConfig holds the configuration for CreateTestServer.
type TestServerConfig struct {
	// Network selects the Solana cluster. Defaults to NetworkDevnet.
	Network SolanaNetwork
	// SecretKey is a 64-byte ed25519 private key or 32-byte seed for the server wallet.
	// A keypair is auto-generated when omitted.
	SecretKey []byte
	// RecipientAddress overrides the default recipient (the server keypair's public key).
	// Must be a valid base58-encoded Solana public key when set.
	RecipientAddress string
	// RPCURL overrides the default Solana RPC endpoint for the selected network.
	RPCURL string
}

// MppServer is an MPP-enabled HTTP middleware factory.
// Use CreateTestServer to construct one.
type MppServer struct {
	// RecipientAddress is the base58-encoded Solana address that receives payments.
	RecipientAddress string
	// Network is the Solana cluster this server is configured for.
	Network SolanaNetwork

	rpc    *rpcClient
	rpcURL string
}

// ChargeOptions configures the per-route payment requirement.
type ChargeOptions struct {
	// Amount is the required SOL payment expressed as a decimal string, e.g. "0.001".
	Amount string
}

// paymentRequestBody is the JSON body returned with a 402 response.
type paymentRequestBody struct {
	Error   string          `json:"error"`
	Payment paymentDetails  `json:"payment"`
}

type paymentDetails struct {
	Amount    string `json:"amount"`
	Currency  string `json:"currency"`
	Recipient string `json:"recipient"`
	Network   string `json:"network"`
}

// Charge returns an http.Handler middleware that enforces an SOL payment before
// delegating to the wrapped handler.
//
// Behaviour:
//   - No Payment-Receipt header → 402 with Payment-Request header and JSON body.
//   - Payment-Receipt present but invalid → 403 with JSON error body.
//   - Payment-Receipt valid and on-chain confirmed → next.ServeHTTP called.
//
// Usage:
//
//	mux.Handle("/api/data", server.Charge(mpp.ChargeOptions{Amount: "0.001"})(myHandler))
func (s *MppServer) Charge(opts ChargeOptions) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			receiptHeader := r.Header.Get("payment-receipt")

			// ── No receipt: issue Payment-Request ────────────────

			if receiptHeader == "" {
				w.Header().Set("Payment-Request", fmt.Sprintf(
					`solana; amount="%s"; recipient="%s"; network="%s"`,
					opts.Amount, s.RecipientAddress, string(s.Network),
				))
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusPaymentRequired)
				_ = json.NewEncoder(w).Encode(paymentRequestBody{
					Error: "Payment Required",
					Payment: paymentDetails{
						Amount:    opts.Amount,
						Currency:  "SOL",
						Recipient: s.RecipientAddress,
						Network:   string(s.Network),
					},
				})
				return
			}

			// ── Receipt present: verify ───────────────────────────

			requiredSOL, err := strconv.ParseFloat(opts.Amount, 64)
			if err != nil || requiredSOL <= 0 {
				writeJSON(w, http.StatusInternalServerError, map[string]string{
					"error": fmt.Sprintf("server configuration error: invalid amount %q", opts.Amount),
				})
				return
			}

			ok, reason := verifyPayment(r.Context(), s.rpcURL, receiptHeader, s.RecipientAddress, requiredSOL)
			if !ok {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": reason})
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// writeJSON writes a JSON response with the given status code.
func writeJSON(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// verifyPayment checks that the receipt header references a valid, confirmed
// on-chain transaction that transferred at least requiredSOL to recipientAddress.
//
// Returns (true, "") on success or (false, reason) on failure.
func verifyPayment(
	ctx context.Context,
	rpcURL string,
	receiptHeader string,
	recipientAddress string,
	requiredSOL float64,
) (bool, string) {
	params := parseHeaderParams(receiptHeader)

	signature := params["signature"]
	if signature == "" {
		return false, "Payment-Receipt missing signature field"
	}

	// Validate claimed amount (early rejection before RPC call).
	paidAmount, err := strconv.ParseFloat(params["amount"], 64)
	if err != nil || paidAmount < requiredSOL {
		claimedStr := params["amount"]
		if claimedStr == "" {
			claimedStr = "0"
		}
		return false, fmt.Sprintf(
			"Insufficient payment: claimed %s SOL, required %g SOL",
			claimedStr, requiredSOL,
		)
	}

	// Fetch the on-chain transaction.
	rpc := newRPCClient(rpcURL)
	tx, err := rpc.getTransaction(ctx, signature)
	if err != nil {
		return false, fmt.Sprintf("Payment verification failed: %s", err.Error())
	}
	if tx == nil {
		return false, "Transaction not found on chain"
	}

	// Check that the transaction did not fail.
	if meta, ok := tx["meta"].(map[string]interface{}); ok {
		if txErr := meta["err"]; txErr != nil {
			return false, "Transaction failed on chain"
		}

		// Locate the recipient in accountKeys and verify the balance delta.
		transaction, ok2 := tx["transaction"].(map[string]interface{})
		if !ok2 {
			return false, "Payment verification failed: malformed transaction structure"
		}
		message, ok3 := transaction["message"].(map[string]interface{})
		if !ok3 {
			return false, "Payment verification failed: malformed transaction message"
		}
		accountKeys, ok4 := message["accountKeys"].([]interface{})
		if !ok4 {
			return false, "Payment verification failed: missing accountKeys"
		}

		preBalances := toFloat64Slice(meta["preBalances"])
		postBalances := toFloat64Slice(meta["postBalances"])

		recipientIdx := -1
		for i, k := range accountKeys {
			addr := extractAccountAddress(k)
			if addr == recipientAddress {
				recipientIdx = i
				break
			}
		}

		if recipientIdx < 0 {
			return false, fmt.Sprintf(
				"Recipient %s... not found in transaction",
				truncate(recipientAddress, 8),
			)
		}

		if recipientIdx >= len(preBalances) || recipientIdx >= len(postBalances) {
			return false, "Payment verification failed: balance arrays too short"
		}

		received := (postBalances[recipientIdx] - preBalances[recipientIdx]) / LamportsPerSol
		if received < requiredSOL {
			return false, fmt.Sprintf(
				"Payment too small: received %g SOL, required %g SOL",
				received, requiredSOL,
			)
		}

		return true, ""
	}

	return false, "Payment verification failed: could not read transaction metadata"
}

// toFloat64Slice converts an interface{} that is a []interface{} of numbers
// into a []float64. Returns nil on failure.
func toFloat64Slice(v interface{}) []float64 {
	slice, ok := v.([]interface{})
	if !ok {
		return nil
	}
	result := make([]float64, len(slice))
	for i, elem := range slice {
		switch n := elem.(type) {
		case float64:
			result[i] = n
		case json.Number:
			f, err := n.Float64()
			if err == nil {
				result[i] = f
			}
		}
	}
	return result
}

// extractAccountAddress tries to read the Solana address from an accountKey
// element. Handles both string values and the jsonParsed object format
// {"pubkey": "...", "signer": ..., "writable": ...}.
func extractAccountAddress(key interface{}) string {
	switch k := key.(type) {
	case string:
		return k
	case map[string]interface{}:
		if pubkey, ok := k["pubkey"].(string); ok {
			return pubkey
		}
	}
	return ""
}

// CreateTestServer creates an MPP-enabled HTTP middleware server.
//
// A Solana keypair is auto-generated when SecretKey is omitted.
// The recipient address defaults to the keypair's public key but can be
// overridden via TestServerConfig.RecipientAddress.
//
// Example:
//
//	server, err := mpp.CreateTestServer(&mpp.TestServerConfig{Network: mpp.NetworkDevnet})
//	mux.Handle("/api/data", server.Charge(mpp.ChargeOptions{Amount: "0.001"})(dataHandler))
func CreateTestServer(config *TestServerConfig) (*MppServer, error) {
	if config == nil {
		config = &TestServerConfig{}
	}

	network := config.Network
	if network == "" {
		network = NetworkDevnet
	}

	rpcURL := config.RPCURL
	if rpcURL == "" {
		rpcURL = networkRPC[network]
	}

	// Resolve recipient address.
	recipientAddress := config.RecipientAddress
	if recipientAddress == "" {
		// Derive from secret key or generate a new keypair.
		var pubKeyBytes []byte
		if len(config.SecretKey) > 0 {
			var privKey ed25519.PrivateKey
			switch len(config.SecretKey) {
			case ed25519.SeedSize:
				privKey = ed25519.NewKeyFromSeed(config.SecretKey)
			case ed25519.PrivateKeySize:
				privKey = ed25519.PrivateKey(config.SecretKey)
			default:
				return nil, fmt.Errorf("CreateTestServer: SecretKey must be 32 or 64 bytes, got %d", len(config.SecretKey))
			}
			pubKeyBytes = []byte(privKey.Public().(ed25519.PublicKey))
		} else {
			pub, _, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				return nil, fmt.Errorf("CreateTestServer: key generation failed: %w", err)
			}
			pubKeyBytes = []byte(pub)
		}
		recipientAddress = base58Encode(pubKeyBytes)
	}

	return &MppServer{
		RecipientAddress: recipientAddress,
		Network:          network,
		rpc:              newRPCClient(rpcURL),
		rpcURL:           rpcURL,
	}, nil
}

// parseHeaderParams is defined in client.go and shared across the package.
