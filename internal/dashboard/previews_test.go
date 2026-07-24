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

	if err := store.Consume(earliest, "workspace-close", "ws-earliest"); !errors.Is(err, ErrPreviewEvicted) {
		t.Fatalf("evicted token error = %v", err)
	}
	if err := store.Consume(newest, "workspace-close", "ws-newest"); err != nil {
		t.Fatalf("new token error = %v", err)
	}
}

func TestPreviewIssueRejectsNonPositiveTTLWithoutChangingCapacity(t *testing.T) {
	tests := map[string]time.Duration{
		"zero":     0,
		"negative": -time.Second,
	}
	for name, ttl := range tests {
		t.Run(name, func(t *testing.T) {
			store := NewPreviewStore(
				fixedPreviewClock(time.Unix(3_500, 0)),
				previewEntropy(previewStoreCapacity+1),
			)
			tokens := make([]string, 0, previewStoreCapacity)
			for i := 0; i < previewStoreCapacity; i++ {
				tokens = append(tokens, store.Issue("workspace-close", "ws-existing", time.Hour))
			}

			requirePreviewIssuePanic(t, func() {
				store.Issue("workspace-close", "ws-invalid", ttl)
			})

			if got := len(store.entries); got != previewStoreCapacity {
				t.Fatalf("live entries after invalid TTL = %d, want %d", got, previewStoreCapacity)
			}
			if got := len(store.outcomes); got != 0 {
				t.Fatalf("terminal outcomes after invalid TTL = %d, want 0", got)
			}
			for _, token := range tokens {
				if _, exists := store.entries[token]; !exists {
					t.Fatalf("invalid TTL removed existing token %q", token)
				}
			}
		})
	}
}

func TestPreviewIssueEntropyFailureAtCapacityPreservesExistingTokens(t *testing.T) {
	store := NewPreviewStore(
		fixedPreviewClock(time.Unix(3_600, 0)),
		previewEntropy(previewStoreCapacity),
	)
	tokens := make([]string, 0, previewStoreCapacity)
	for i := 0; i < previewStoreCapacity; i++ {
		tokens = append(tokens, store.Issue("workspace-close", "ws-existing", time.Hour))
	}

	requirePreviewIssuePanic(t, func() {
		store.Issue("workspace-close", "ws-new", time.Hour)
	})

	if got := len(store.entries); got != previewStoreCapacity {
		t.Fatalf("live entries after entropy failure = %d, want %d", got, previewStoreCapacity)
	}
	if got := len(store.outcomes); got != 0 {
		t.Fatalf("terminal outcomes after entropy failure = %d, want 0", got)
	}
	for _, token := range tokens {
		if err := store.Consume(token, "workspace-close", "ws-existing"); err != nil {
			t.Fatalf("existing token after entropy failure: %v", err)
		}
	}
}

func TestPreviewIssueBoundsRepeatedEntropyCollisionsAndUnlocksStore(t *testing.T) {
	store := NewPreviewStore(
		fixedPreviewClock(time.Unix(3_700, 0)),
		repeatingPreviewEntropy{value: 42},
	)
	token := store.Issue("workspace-close", "ws-existing", time.Hour)

	panicResult := make(chan any, 1)
	go func() {
		defer func() {
			panicResult <- recover()
		}()
		store.Issue("workspace-close", "ws-collision", time.Hour)
	}()

	select {
	case recovered := <-panicResult:
		if recovered == nil {
			t.Fatal("repeated entropy collisions did not panic")
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("repeated entropy collisions did not finish within the bounded interval")
	}

	if err := store.Consume(token, "workspace-close", "ws-existing"); err != nil {
		t.Fatalf("store remained unusable after collision exhaustion: %v", err)
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

type repeatingPreviewEntropy struct {
	value byte
}

func (r repeatingPreviewEntropy) Read(buffer []byte) (int, error) {
	for i := range buffer {
		buffer[i] = r.value
	}
	return len(buffer), nil
}

func requirePreviewIssuePanic(t *testing.T, issue func()) {
	t.Helper()
	var recovered any
	func() {
		defer func() {
			recovered = recover()
		}()
		issue()
	}()
	if recovered == nil {
		t.Fatal("preview issuance did not panic")
	}
}
