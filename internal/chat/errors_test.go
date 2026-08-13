package chat

import (
	"errors"
	"fmt"
	"testing"
)

// StableError must satisfy the exact contract chatwire checks for
// (writeBackendError's stableBackendError interface): ErrorCode() and
// SafeMessage() both non-empty, with the raw cause reachable only through
// Error()/Unwrap so the host log keeps the detail the wire never sees.
func TestStableErrorContract(t *testing.T) {
	cause := errors.New("git clone -- https://secret.example/o/r.git /work/repo: error: exit status 128")
	e := &StableError{Code: "repo_clone_failed", Message: "the repository clone failed", Cause: cause}

	if got := e.ErrorCode(); got != "repo_clone_failed" {
		t.Fatalf("ErrorCode = %q", got)
	}
	if got := e.SafeMessage(); got != "the repository clone failed" {
		t.Fatalf("SafeMessage = %q", got)
	}
	if !errors.Is(e, cause) {
		// errors.Is walks Unwrap, so this only passes if StableError.Unwrap
		// really exposes Cause.
		t.Fatal("Unwrap must expose the cause")
	}
	if msg := e.Error(); msg != "the repository clone failed: "+cause.Error() {
		t.Fatalf("Error() = %q, want safe message plus cause", msg)
	}

	// errors.As through a wrapping %w chain must still find the StableError,
	// exactly as chatwire's writeBackendError does.
	wrapped := fmt.Errorf("workspace setup: %w", e)
	var stable *StableError
	if !errors.As(wrapped, &stable) || stable != e {
		t.Fatalf("errors.As through %%w chain = %v, want the StableError", stable)
	}
}

func TestStableErrorZeroValueFailsChatwireContract(t *testing.T) {
	// chatwire only substitutes when both Code and SafeMessage are non-empty;
	// a zero-value StableError must not match that gate.
	e := &StableError{}
	if e.ErrorCode() != "" || e.SafeMessage() != "" {
		t.Fatal("zero StableError must have empty Code and SafeMessage")
	}
}
