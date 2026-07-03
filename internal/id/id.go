// Package id centralizes generation of opaque random identifiers using
// crypto/rand. Generators return error on failure (instead of panicking) so
// that callers in hot paths (docker sandbox creation in chat, workspace open,
// broker token mint, session/entity/schedule creation, etc.) can fail the
// specific operation gracefully rather than crashing the process.
package id

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// New returns a random ID consisting of the given prefix followed by 8 hex
// characters (4 random bytes).
func New(prefix string) (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err // crypto/rand failing is not a recoverable state
	}
	return prefix + hex.EncodeToString(b[:]), nil
}

// NewSession returns a timestamped session ID of the form
// 20060102-150405-XXXXXXXX (UTC time + 4 random bytes as hex).
func NewSession() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err // crypto/rand failing is not a recoverable state
	}
	return time.Now().UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b[:]), nil
}

// NewBytes returns n random bytes encoded as lowercase hex (no prefix).
// Suitable for sandbox dir suffixes (n=4) and broker wk_ tokens (n=16).
// n must be > 0; otherwise an error is returned (instead of panicking on
// make or returning empty).
func NewBytes(n int) (string, error) {
	if n <= 0 {
		return "", fmt.Errorf("id: NewBytes requires n>0, got %d", n)
	}
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err // crypto/rand failing is not a recoverable state
	}
	return hex.EncodeToString(b), nil
}

// NewPairingCode returns a 6-character pairing code using an alphabet that
// avoids ambiguous characters (0/O, 1/I).
func NewPairingCode() (string, error) {
	const pairingAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err // crypto/rand failing is not a recoverable state
	}
	for i := range b {
		b[i] = pairingAlphabet[int(b[i])%len(pairingAlphabet)]
	}
	return string(b[:]), nil
}
