package skill

import "testing"

func TestFingerprintError(t *testing.T) {
	sig, sample := fingerprintError("error: open /tmp/foo123: no such file or directory")
	if sig == "" || sample == "" {
		t.Fatalf("sig=%q sample=%q", sig, sample)
	}
	sig2, _ := fingerprintError("error: open /var/bar999: no such file or directory")
	if sig != sig2 {
		t.Fatalf("expected same fingerprint: %q vs %q", sig, sig2)
	}
}
