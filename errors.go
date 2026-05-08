// Package mpp provides a Go SDK for the Machine Payments Protocol (MPP) on Solana.
// It handles the HTTP 402 payment flow, Solana wallet management, and on-chain
// payment verification without any external dependencies beyond the Go standard library.
package mpp

import "fmt"

// MppError is the base error type for all MPP errors.
type MppError struct {
	// Message is the human-readable error description.
	Message string
}

// Error implements the error interface.
func (e *MppError) Error() string { return e.Message }

// MppFaucetError is returned when an airdrop from the devnet/testnet faucet fails
// after all retry attempts have been exhausted.
type MppFaucetError struct {
	MppError
	// Address is the base58-encoded Solana public key that could not be funded.
	Address string
}

// MppPaymentError is returned when an HTTP request fails with an unexpected
// status code or when the 402 payment flow cannot be completed.
type MppPaymentError struct {
	MppError
	// URL is the endpoint that triggered the error.
	URL string
	// Status is the HTTP status code received.
	Status int
}

// MppTimeoutError is returned when the full MPP payment flow (wallet creation,
// funding, payment, and retry) exceeds the configured timeout duration.
type MppTimeoutError struct {
	MppError
	// URL is the endpoint that timed out.
	URL string
	// TimeoutMs is the timeout duration in milliseconds.
	TimeoutMs int
}

// MppNetworkError is returned when there is a network configuration error,
// for example when mainnet is used without providing a pre-funded secret key.
type MppNetworkError struct {
	MppError
	// Network is the Solana network name that caused the error.
	Network string
}

// newMppFaucetError creates a new MppFaucetError for the given address.
func newMppFaucetError(address string) *MppFaucetError {
	return &MppFaucetError{
		MppError: MppError{
			Message: fmt.Sprintf(
				"Failed to airdrop SOL to wallet %s. "+
					"The devnet/testnet faucet may be rate-limited. "+
					"Wait 30s and retry, or pass a pre-funded SecretKey to skip airdrop.",
				address,
			),
		},
		Address: address,
	}
}

// newMppPaymentError creates a new MppPaymentError for the given URL and status.
func newMppPaymentError(url string, status int, detail string) *MppPaymentError {
	msg := fmt.Sprintf("Payment failed for %s (status: %d)", url, status)
	if detail != "" {
		msg = fmt.Sprintf("%s: %s", msg, detail)
	}
	return &MppPaymentError{
		MppError: MppError{Message: msg},
		URL:      url,
		Status:   status,
	}
}

// newMppTimeoutError creates a new MppTimeoutError for the given URL and timeout.
func newMppTimeoutError(url string, timeoutMs int) *MppTimeoutError {
	return &MppTimeoutError{
		MppError: MppError{
			Message: fmt.Sprintf(
				"Request to %s timed out after %dms. "+
					"Increase the Timeout option or check your Solana RPC connection.",
				url, timeoutMs,
			),
		},
		URL:       url,
		TimeoutMs: timeoutMs,
	}
}

// newMppNetworkError creates a new MppNetworkError for the given network.
func newMppNetworkError(network, detail string) *MppNetworkError {
	msg := detail
	if msg == "" {
		msg = fmt.Sprintf(
			`Network configuration error for %q. `+
				`Mainnet requires a pre-funded SecretKey (no airdrop available).`,
			network,
		)
	}
	return &MppNetworkError{
		MppError: MppError{Message: msg},
		Network:  network,
	}
}
