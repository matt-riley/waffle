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
		return "", fmt.Errorf("crypto/rand: %w", err)
	}
	return prefix + hex.EncodeToString(b[:]), nil
}

// NewSession returns a timestamped session ID of the form
// 20060102-150405-XXXXXXXX (UTC time + 4 random bytes as hex).
func NewSession() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("crypto/rand: %w", err)
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
		return "", fmt.Errorf("crypto/rand: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// pairingAlphabet avoids ambiguous characters (0/O, 1/I). Length is 32.
const pairingAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// maxUnbiasedPairingByte is the largest multiple of len(pairingAlphabet) that
// fits in a byte. Bytes in [0, maxUnbiasedPairingByte) map uniformly onto the
// alphabet via % len; larger values are discarded and re-sampled so that
// NewPairingCode has no modulo bias even if the alphabet length changes to a
// value that does not divide 256.
const maxUnbiasedPairingByte = 256 - (256 % len(pairingAlphabet))

// NewPairingCode returns a 6-character pairing code using an alphabet that
// avoids ambiguous characters (0/O, 1/I). Characters are drawn uniformly via
// rejection sampling over crypto/rand bytes (no modulo bias).
func NewPairingCode() (string, error) {
	const n = 6
	var out [n]byte
	// Batch-read a small buffer and refill as needed. When the alphabet length
	// divides 256, every byte is accepted; otherwise the rejection rate is
	// (256-maxUnbiased)/256 and we may need more than one draw.
	var buf [n]byte
	filled := 0
	for filled < n {
		if _, err := rand.Read(buf[:]); err != nil {
			return "", fmt.Errorf("crypto/rand: %w", err)
		}
		for _, b := range buf {
			if int(b) >= maxUnbiasedPairingByte {
				continue
			}
			out[filled] = pairingAlphabet[int(b)%len(pairingAlphabet)]
			filled++
			if filled == n {
				break
			}
		}
	}
	return string(out[:]), nil
}
