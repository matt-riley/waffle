package redact

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestResidualScrubsKnownSecretFormats(t *testing.T) {
	value := "key AGE-SECRET-KEY-abc123 sk-proj-xyz /var/lib/waffle/workspace/main WAFFLE_AGE_IDENTITY plain"
	clean := Residual(value)
	for _, want := range []string{"AGE-SECRET-KEY-abc123", "sk-proj-xyz", "/var/lib/waffle/workspace/main", "WAFFLE_AGE_IDENTITY"} {
		if strings.Contains(clean, want) {
			t.Fatalf("Residual(%q) = %q, still contains %q", value, clean, want)
		}
	}
	if !strings.Contains(clean, "plain") {
		t.Fatalf("Residual dropped non-secret text: %q", clean)
	}
}

func TestExactReplacesPrivateValues(t *testing.T) {
	clean := Exact("api key sk-live-1 and scope 42", "sk-live-1", "42")
	if strings.Contains(clean, "sk-live-1") || strings.Contains(clean, " 42") {
		t.Fatalf("Exact = %q", clean)
	}
	if !strings.Contains(clean, "[REDACTED]") {
		t.Fatalf("Exact = %q", clean)
	}
	if got := Exact("unchanged"); got != "unchanged" {
		t.Fatalf("Exact without private = %q", got)
	}
}

func TestRedactErrorPreservesCancelClassificationWithoutLeaking(t *testing.T) {
	// The cause genuinely wraps context.Canceled; redaction must not lose
	// that classification even though the message is scrubbed.
	plain := fmt.Errorf("provider call canceled: sk-secret-key: %w", context.Canceled)
	err := RedactError(plain, func(value string) string {
		return strings.ReplaceAll(value, "sk-secret-key", "[REDACTED]")
	})
	if err == nil {
		t.Fatal("RedactError returned nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("errors.Is(err, context.Canceled) = false, err = %v", err)
	}
	if strings.Contains(err.Error(), "sk-secret-key") {
		t.Fatalf("message leaked secret: %v", err)
	}

	// The raw cause must not be reachable via Unwrap (tree walks must not
	// observe the credential).
	if _, ok := err.(interface{ Unwrap() error }); ok {
		t.Fatalf("redacted error exposes Unwrap")
	}

	// DeadlineExceeded survives the same way.
	expired := fmt.Errorf("timeout: sk-secret-key: %w", context.DeadlineExceeded)
	redacted := RedactError(expired, func(value string) string {
		return strings.ReplaceAll(value, "sk-secret-key", "[REDACTED]")
	})
	if !errors.Is(redacted, context.DeadlineExceeded) {
		t.Fatalf("deadline classification lost: %v", redacted)
	}
}

// cleanupMarker is a marker interface that must survive redaction so chat
// cleanup-completion detection keeps working (errors.As path).
type cleanupMarker interface{ CleanupCompleted() bool }

type markedError struct{ error }

func (m markedError) CleanupCompleted() bool { return true }

func TestRedactErrorPreservesAsThroughRedaction(t *testing.T) {
	err := RedactError(fmt.Errorf("cleanup: sk-secret-key: %w", markedError{errors.New("cause")}), func(value string) string {
		return strings.ReplaceAll(value, "sk-secret-key", "[REDACTED]")
	})
	var marker cleanupMarker
	if !errors.As(err, &marker) || !marker.CleanupCompleted() {
		t.Fatalf("errors.As lost the wrapped marker through redaction: %v", err)
	}
	if strings.Contains(err.Error(), "sk-secret-key") {
		t.Fatalf("message leaked secret: %v", err)
	}
	// The raw cause is still not reachable by tree walking.
	if _, ok := err.(interface{ Unwrap() error }); ok {
		t.Fatal("redacted error exposes Unwrap")
	}
}

func TestRedactErrorReturnsOriginalWhenUnchanged(t *testing.T) {
	original := errors.New("nothing to scrub")
	got := RedactError(original, func(value string) string { return value })
	if got != original {
		t.Fatalf("RedactError changed an already-safe error: %v", got)
	}
	if RedactError(nil, func(value string) string { return value }) != nil {
		t.Fatal("RedactError(nil) != nil")
	}
}
