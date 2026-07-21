package id

import (
	"math"
	"strings"
	"testing"
)

func TestNew(t *testing.T) {
	id, err := New("ws-")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !strings.HasPrefix(id, "ws-") {
		t.Errorf("prefix: %s", id)
	}
	if len(id) != 3+8 {
		t.Errorf("len: %d", len(id))
	}
}

func TestNewSession(t *testing.T) {
	id, err := NewSession()
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if !strings.Contains(id, "-") {
		t.Errorf("format: %s", id)
	}
	parts := strings.Split(id, "-")
	if len(parts) != 3 || len(parts[2]) != 8 {
		t.Errorf("format: %s", id)
	}
}

func TestNewBytes(t *testing.T) {
	id, err := NewBytes(4)
	if err != nil {
		t.Fatalf("NewBytes(4): %v", err)
	}
	if len(id) != 8 {
		t.Errorf("len: %d", len(id))
	}

	_, err = NewBytes(0)
	if err == nil || !strings.Contains(err.Error(), "n>0") {
		t.Errorf("NewBytes(0) err: %v", err)
	}
	_, err = NewBytes(-1)
	if err == nil || !strings.Contains(err.Error(), "n>0") {
		t.Errorf("NewBytes(-1) err: %v", err)
	}
}

func TestNewPairingCode(t *testing.T) {
	code, err := NewPairingCode()
	if err != nil {
		t.Fatalf("NewPairingCode: %v", err)
	}
	if len(code) != 6 {
		t.Errorf("len: %d", len(code))
	}
	for _, c := range code {
		if !strings.ContainsRune(pairingAlphabet, c) {
			t.Errorf("char %q not in pairing alphabet", c)
		}
		if strings.ContainsRune("0O1I", c) {
			t.Errorf("ambiguous char: %c", c)
		}
	}
}

// TestNewPairingCodeUniformity checks that pairing-code characters are drawn
// without detectable modulo bias. N=100_000 codes → 600_000 character samples.
// Critical value is χ²_{31, 0.001} ≈ 61.098 (alphabet length 32 → df=31).
// True uniform samples almost always land far below this threshold.
func TestNewPairingCodeUniformity(t *testing.T) {
	const (
		nCodes   = 100_000
		codeLen  = 6
		alphabet = pairingAlphabet
		alphaLen = len(alphabet)
		samples  = nCodes * codeLen
		// χ² critical value for 31 degrees of freedom at p=0.001.
		// Source: standard chi-squared table (df=31, upper tail 0.001).
		chi2Critical = 61.098
	)
	if alphaLen != 32 {
		t.Fatalf("pairing alphabet length = %d, want 32 (unchanged alphabet)", alphaLen)
	}
	// Rejection bound must be the largest multiple of the alphabet size ≤ 256.
	wantMax := 256 - (256 % alphaLen)
	if maxUnbiasedPairingByte != wantMax {
		t.Fatalf("maxUnbiasedPairingByte = %d, want %d", maxUnbiasedPairingByte, wantMax)
	}

	freq := make(map[byte]int, alphaLen)
	for i := 0; i < nCodes; i++ {
		code, err := NewPairingCode()
		if err != nil {
			t.Fatalf("NewPairingCode: %v", err)
		}
		if len(code) != codeLen {
			t.Fatalf("len=%d, want %d (code %q)", len(code), codeLen, code)
		}
		for j := 0; j < len(code); j++ {
			c := code[j]
			if !strings.ContainsRune(alphabet, rune(c)) {
				t.Fatalf("char %q not in alphabet (code %q)", c, code)
			}
			freq[c]++
		}
	}

	expected := float64(samples) / float64(alphaLen)
	var chi2 float64
	var maxRelDev float64
	for i := 0; i < alphaLen; i++ {
		c := alphabet[i]
		obs := float64(freq[c])
		diff := obs - expected
		chi2 += diff * diff / expected
		rel := math.Abs(diff) / expected
		if rel > maxRelDev {
			maxRelDev = rel
		}
	}
	if chi2 >= chi2Critical {
		t.Fatalf("chi-squared=%.2f >= critical %.3f (df=%d, p=0.001); max relative deviation=%.4f; distribution not uniform",
			chi2, chi2Critical, alphaLen-1, maxRelDev)
	}
	t.Logf("chi-squared=%.2f (critical %.3f), max relative deviation=%.4f over %d samples",
		chi2, chi2Critical, maxRelDev, samples)
}
