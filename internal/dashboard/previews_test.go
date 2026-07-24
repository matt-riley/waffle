package dashboard

import (
	"bytes"
	"encoding/base64"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestPreviewTokenIsSingleUseAndResourceBound(t *testing.T) {
	store := NewPreviewStore(fixedPreviewClock(time.Unix(1_000, 0)), previewEntropy(2))

	token := store.Issue("workspace-close", "ws-1", time.Minute)
	if err := store.Consume(token, "workspace-close", "ws-2"); !errors.Is(err, ErrPreviewMismatch) {
		t.Fatalf("wrong resource error = %v", err)
	}
	if err := store.Consume(token, "workspace-close", "ws-1"); !errors.Is(err, ErrPreviewUsed) {
		t.Fatalf("mismatched token replay error = %v", err)
	}

	token = store.Issue("workspace-close", "ws-1", time.Minute)
	if err := store.Consume(token, "workspace-close", "ws-1"); err != nil {
		t.Fatal(err)
	}
	if err := store.Consume(token, "workspace-close", "ws-1"); !errors.Is(err, ErrPreviewUsed) {
		t.Fatalf("replay error = %v", err)
	}
}

func TestPreviewTokenBindingsAreExact(t *testing.T) {
	store := NewPreviewStore(fixedPreviewClock(time.Unix(1_500, 0)), previewEntropy(2))

	token := store.Issue(" workspace-close", "ws-1", time.Minute)
	if err := store.Consume(token, "workspace-close", "ws-1"); !errors.Is(err, ErrPreviewMismatch) {
		t.Fatalf("operation whitespace error = %v", err)
	}

	token = store.Issue("workspace-close", " ws-1", time.Minute)
	if err := store.Consume(token, "workspace-close", "ws-1"); !errors.Is(err, ErrPreviewMismatch) {
		t.Fatalf("resource whitespace error = %v", err)
	}
}

func TestPreviewTokenExpiresAndPruningPreservesExpiredOutcome(t *testing.T) {
	now := time.Unix(2_000, 0)
	store := NewPreviewStore(func() time.Time { return now }, previewEntropy(2))
	expired := store.Issue("memory-forget", "note-1", time.Minute)

	now = now.Add(2 * time.Minute)
	current := store.Issue("memory-forget", "note-2", time.Minute)

	if err := store.Consume(expired, "memory-forget", "note-1"); !errors.Is(err, ErrPreviewExpired) {
		t.Fatalf("expired token error = %v", err)
	}
	if err := store.Consume(current, "memory-forget", "note-2"); err != nil {
		t.Fatalf("current token error = %v", err)
	}
}

func TestPreviewStoreEvictsEarliestExpiryAtCapacity(t *testing.T) {
	now := time.Unix(3_000, 0)
	store := NewPreviewStore(fixedPreviewClock(now), previewEntropy(previewStoreCapacity+1))
	earliest := store.Issue("workspace-close", "ws-earliest", time.Hour)
	for i := 1; i < previewStoreCapacity; i++ {
		store.Issue("workspace-close", "ws", 2*time.Hour)
	}

	newest := store.Issue("workspace-close", "ws-newest", 2*time.Hour)

	if err := store.Consume(earliest, "workspace-close", "ws-earliest"); !errors.Is(err, ErrPreviewUsed) {
		t.Fatalf("evicted token error = %v", err)
	}
	if err := store.Consume(newest, "workspace-close", "ws-newest"); err != nil {
		t.Fatalf("new token error = %v", err)
	}
}

func TestPreviewTokenUsesThirtyTwoOpaqueURLSafeBytes(t *testing.T) {
	store := NewPreviewStore(fixedPreviewClock(time.Unix(4_000, 0)), previewEntropy(1))

	token := store.Issue("workspace-close", "ws-1", time.Minute)

	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		t.Fatalf("decode token: %v", err)
	}
	if len(decoded) != previewTokenBytes {
		t.Fatalf("decoded token bytes = %d, want %d", len(decoded), previewTokenBytes)
	}
}

func TestPreviewUnknownTokenHasStableError(t *testing.T) {
	store := NewPreviewStore(fixedPreviewClock(time.Unix(5_000, 0)), previewEntropy(1))

	if err := store.Consume("not-issued", "workspace-close", "ws-1"); !errors.Is(err, ErrPreviewUnknown) {
		t.Fatalf("unknown token error = %v", err)
	}
}

func TestPreviewConcurrentConsumeSucceedsExactlyOnce(t *testing.T) {
	store := NewPreviewStore(fixedPreviewClock(time.Unix(6_000, 0)), previewEntropy(1))
	token := store.Issue("workspace-close", "ws-1", time.Minute)

	const consumers = 64
	start := make(chan struct{})
	results := make(chan error, consumers)
	var wg sync.WaitGroup
	wg.Add(consumers)
	for i := 0; i < consumers; i++ {
		go func() {
			defer wg.Done()
			<-start
			results <- store.Consume(token, "workspace-close", "ws-1")
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	successes := 0
	used := 0
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrPreviewUsed):
			used++
		default:
			t.Fatalf("unexpected consume error = %v", err)
		}
	}
	if successes != 1 || used != consumers-1 {
		t.Fatalf("successes = %d, used = %d; want 1 and %d", successes, used, consumers-1)
	}
}

func fixedPreviewClock(now time.Time) func() time.Time {
	return func() time.Time { return now }
}

func previewEntropy(tokens int) *bytes.Reader {
	data := make([]byte, tokens*previewTokenBytes)
	for token := 0; token < tokens; token++ {
		for i := 0; i < previewTokenBytes; i++ {
			data[token*previewTokenBytes+i] = byte(token + i + 1)
		}
	}
	return bytes.NewReader(data)
}
