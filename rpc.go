package mpp

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"
)

// LamportsPerSol is the number of lamports in one SOL.
const LamportsPerSol = 1_000_000_000

// base58Alphabet is the Bitcoin/Solana base58 character set.
const base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// base58Encode encodes a byte slice to a base58 string.
func base58Encode(b []byte) string {
	if len(b) == 0 {
		return ""
	}

	// Count leading zero bytes.
	leadingZeros := 0
	for _, byt := range b {
		if byt != 0 {
			break
		}
		leadingZeros++
	}

	// Convert bytes to big integer.
	num := new(big.Int).SetBytes(b)
	base := big.NewInt(58)
	mod := new(big.Int)
	zero := big.NewInt(0)

	var result []byte
	for num.Cmp(zero) > 0 {
		num.DivMod(num, base, mod)
		result = append(result, base58Alphabet[mod.Int64()])
	}

	// Add '1' for each leading zero byte.
	for i := 0; i < leadingZeros; i++ {
		result = append(result, base58Alphabet[0])
	}

	// Reverse the result.
	for i, j := 0, len(result)-1; i < j; i, j = i+1, j-1 {
		result[i], result[j] = result[j], result[i]
	}

	return string(result)
}

// base58Decode decodes a base58 string to a byte slice.
func base58Decode(s string) ([]byte, error) {
	if s == "" {
		return []byte{}, nil
	}

	// Count leading '1's.
	leadingZeros := 0
	for _, c := range s {
		if c != '1' {
			break
		}
		leadingZeros++
	}

	num := new(big.Int)
	base := big.NewInt(58)

	for _, c := range s {
		idx := strings.IndexRune(base58Alphabet, c)
		if idx < 0 {
			return nil, fmt.Errorf("base58Decode: invalid character %q", c)
		}
		num.Mul(num, base)
		num.Add(num, big.NewInt(int64(idx)))
	}

	decoded := num.Bytes()

	// Prepend leading zero bytes.
	result := make([]byte, leadingZeros+len(decoded))
	copy(result[leadingZeros:], decoded)
	return result, nil
}

// encodeCompactU16 encodes an integer as Solana compact-u16.
// Each byte uses the low 7 bits for data and bit 7 as a continuation flag.
func encodeCompactU16(n int) []byte {
	if n < 0x80 {
		return []byte{byte(n)}
	}
	if n < 0x4000 {
		lo := byte(n&0x7F) | 0x80
		hi := byte(n >> 7)
		return []byte{lo, hi}
	}
	// Values >= 0x4000 require 3 bytes.
	b0 := byte(n&0x7F) | 0x80
	b1 := byte((n>>7)&0x7F) | 0x80
	b2 := byte(n >> 14)
	return []byte{b0, b1, b2}
}

// systemProgramID is the Solana System Program public key (32 zero bytes = "111...1" in base58).
var systemProgramID = [32]byte{}

// buildTransferTransaction constructs a serialized legacy Solana SOL transfer
// transaction and returns it base64-encoded, ready to submit via sendTransaction.
//
// Solana legacy transaction wire format:
//
//	compact-u16(1)          -number of signatures (always 1 for this tx)
//	[64]byte                -ed25519 signature over the message
//	Message:
//	  [3]byte               -header: (num_required_sigs=1, num_ro_signed=0, num_ro_unsigned=1)
//	  compact-u16(3)        -number of account keys
//	  [32*3]byte            -account keys: [from, to, system_program]
//	  [32]byte              -recent blockhash
//	  compact-u16(1)        -number of instructions
//	  Instruction:
//	    u8(2)               -program_id_index (system program is index 2)
//	    compact-u16(2)      -num account indices
//	    [u8, u8]            -account indices [0=from, 1=to]
//	    compact-u16(12)     -instruction data length
//	    [4]byte LE          -SystemInstruction::Transfer = 2
//	    [8]byte LE          -lamports (u64 little-endian)
func buildTransferTransaction(
	fromPrivKey ed25519.PrivateKey,
	fromPubKey []byte,
	toPubKey []byte,
	lamports uint64,
	blockhash []byte,
) (string, error) {
	if len(fromPubKey) != 32 {
		return "", fmt.Errorf("buildTransferTransaction: fromPubKey must be 32 bytes, got %d", len(fromPubKey))
	}
	if len(toPubKey) != 32 {
		return "", fmt.Errorf("buildTransferTransaction: toPubKey must be 32 bytes, got %d", len(toPubKey))
	}
	if len(blockhash) != 32 {
		return "", fmt.Errorf("buildTransferTransaction: blockhash must be 32 bytes, got %d", len(blockhash))
	}

	// ── Build message ────────────────────────────────────────────

	var msg bytes.Buffer

	// Header bytes.
	msg.Write([]byte{1, 0, 1})

	// Account keys section.
	msg.Write(encodeCompactU16(3))
	msg.Write(fromPubKey)
	msg.Write(toPubKey)
	msg.Write(systemProgramID[:])

	// Recent blockhash.
	msg.Write(blockhash)

	// Instructions section: 1 instruction.
	msg.Write(encodeCompactU16(1))

	// Instruction: program_id_index = 2 (system program).
	msg.WriteByte(2)

	// Account indices for this instruction.
	msg.Write(encodeCompactU16(2))
	msg.Write([]byte{0, 1})

	// Instruction data: Transfer discriminator (4 bytes) + lamports (8 bytes).
	msg.Write(encodeCompactU16(12))
	instrType := make([]byte, 4)
	binary.LittleEndian.PutUint32(instrType, 2)
	msg.Write(instrType)
	lamportBytes := make([]byte, 8)
	binary.LittleEndian.PutUint64(lamportBytes, lamports)
	msg.Write(lamportBytes)

	msgBytes := msg.Bytes()

	// ── Sign message ─────────────────────────────────────────────

	sig := ed25519.Sign(fromPrivKey, msgBytes)

	// ── Assemble transaction: sig_count + sig + message ─────────

	var tx bytes.Buffer
	tx.Write(encodeCompactU16(1))
	tx.Write(sig)
	tx.Write(msgBytes)

	return base64.StdEncoding.EncodeToString(tx.Bytes()), nil
}

// ── JSON-RPC client ──────────────────────────────────────────────────────────

// rpcRequest is the JSON-RPC 2.0 request envelope.
type rpcRequest struct {
	JSONRPC string        `json:"jsonrpc"`
	ID      int           `json:"id"`
	Method  string        `json:"method"`
	Params  []interface{} `json:"params"`
}

// rpcResponse is the JSON-RPC 2.0 response envelope.
type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

// rpcError represents an error returned by the JSON-RPC server.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// rpcClient sends JSON-RPC requests to a Solana RPC endpoint.
type rpcClient struct {
	endpoint   string
	httpClient *http.Client
}

// newRPCClient creates a new rpcClient targeting the given endpoint URL.
func newRPCClient(endpoint string) *rpcClient {
	return &rpcClient{
		endpoint: endpoint,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// call sends a JSON-RPC request and returns the raw result bytes.
func (c *rpcClient) call(ctx context.Context, method string, params []interface{}) (json.RawMessage, error) {
	reqBody, err := json.Marshal(rpcRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  method,
		Params:  params,
	})
	if err != nil {
		return nil, fmt.Errorf("rpc marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("rpc new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("rpc http: %w", err)
	}
	defer resp.Body.Close()

	var rpcResp rpcResponse
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		return nil, fmt.Errorf("rpc decode: %w", err)
	}
	if rpcResp.Error != nil {
		return nil, fmt.Errorf("rpc error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}
	return rpcResp.Result, nil
}

// getLatestBlockhash returns the latest blockhash and last valid block height.
func (c *rpcClient) getLatestBlockhash(ctx context.Context) (string, uint64, error) {
	result, err := c.call(ctx, "getLatestBlockhash", []interface{}{
		map[string]string{"commitment": "confirmed"},
	})
	if err != nil {
		return "", 0, err
	}

	var outer struct {
		Value struct {
			Blockhash            string `json:"blockhash"`
			LastValidBlockHeight uint64 `json:"lastValidBlockHeight"`
		} `json:"value"`
	}
	if err := json.Unmarshal(result, &outer); err != nil {
		return "", 0, fmt.Errorf("getLatestBlockhash unmarshal: %w", err)
	}
	return outer.Value.Blockhash, outer.Value.LastValidBlockHeight, nil
}

// requestAirdrop requests an airdrop of lamports to the given address.
// Returns the transaction signature.
func (c *rpcClient) requestAirdrop(ctx context.Context, pubkeyBase58 string, lamports uint64) (string, error) {
	result, err := c.call(ctx, "requestAirdrop", []interface{}{pubkeyBase58, lamports})
	if err != nil {
		return "", err
	}

	var sig string
	if err := json.Unmarshal(result, &sig); err != nil {
		return "", fmt.Errorf("requestAirdrop unmarshal: %w", err)
	}
	return sig, nil
}

// confirmTransaction polls until the given signature is confirmed (up to 60 s).
func (c *rpcClient) confirmTransaction(ctx context.Context, sig string) error {
	deadline := time.Now().Add(60 * time.Second)
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("confirmTransaction: timed out waiting for %s", sig)
		}

		result, err := c.call(ctx, "getSignatureStatuses", []interface{}{
			[]string{sig},
			map[string]bool{"searchTransactionHistory": true},
		})
		if err != nil {
			return err
		}

		var statusResp struct {
			Value []struct {
				ConfirmationStatus string      `json:"confirmationStatus"`
				Err                interface{} `json:"err"`
			} `json:"value"`
		}
		if err := json.Unmarshal(result, &statusResp); err != nil {
			return fmt.Errorf("confirmTransaction unmarshal: %w", err)
		}

		if len(statusResp.Value) > 0 && statusResp.Value[0].ConfirmationStatus != "" {
			status := statusResp.Value[0]
			if status.Err != nil {
				return fmt.Errorf("confirmTransaction: transaction %s failed: %v", sig, status.Err)
			}
			if status.ConfirmationStatus == "confirmed" || status.ConfirmationStatus == "finalized" {
				return nil
			}
		}

		// Wait before polling again, respecting context cancellation.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// sendTransaction submits a base64-encoded transaction and returns the signature.
func (c *rpcClient) sendTransaction(ctx context.Context, txBase64 string) (string, error) {
	result, err := c.call(ctx, "sendTransaction", []interface{}{
		txBase64,
		map[string]string{"encoding": "base64"},
	})
	if err != nil {
		return "", err
	}

	var sig string
	if err := json.Unmarshal(result, &sig); err != nil {
		return "", fmt.Errorf("sendTransaction unmarshal: %w", err)
	}
	return sig, nil
}

// getTransaction fetches a confirmed transaction by signature.
// Returns nil (no error) when the transaction is not found.
func (c *rpcClient) getTransaction(ctx context.Context, sig string) (map[string]interface{}, error) {
	result, err := c.call(ctx, "getTransaction", []interface{}{
		sig,
		map[string]interface{}{
			"encoding":                       "jsonParsed",
			"commitment":                     "confirmed",
			"maxSupportedTransactionVersion": 0,
		},
	})
	if err != nil {
		return nil, err
	}

	if string(result) == "null" {
		return nil, nil
	}

	var tx map[string]interface{}
	if err := json.Unmarshal(result, &tx); err != nil {
		return nil, fmt.Errorf("getTransaction unmarshal: %w", err)
	}
	return tx, nil
}
