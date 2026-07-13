package usage

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/store"
)

func TestReserveRequestAtAtomicallyEnforcesConcurrentCap(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	u := New(st)
	now := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := u.ReserveRequestAt(ctx, "concurrent", Limits{RequestsPerHour: 1}, now); err == nil {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	if allowed != 1 {
		t.Fatalf("allowed=%d want 1", allowed)
	}
}

func TestAlertThresholdDeliversOncePerPeriod(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	u := New(st)
	now := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)
	var notices []string
	deliver := func(_ context.Context, notice string) error {
		notices = append(notices, notice)
		return nil
	}

	for _, tokens := range []int{79, 1, 5} {
		if err := u.AddRequestAt(ctx, "session-1", llm.Usage{InputTokens: tokens}, now); err != nil {
			t.Fatal(err)
		}
		if err := u.Alert(ctx, "session-1", Limits{TokensPerDay: 100}, now, deliver); err != nil {
			t.Fatal(err)
		}
	}
	if len(notices) != 1 {
		t.Fatalf("notices = %v, want exactly one threshold notice", notices)
	}

	nextDay := now.Add(24 * time.Hour)
	if err := u.AddRequestAt(ctx, "session-1", llm.Usage{InputTokens: 80}, nextDay); err != nil {
		t.Fatal(err)
	}
	if err := u.Alert(ctx, "session-1", Limits{TokensPerDay: 100}, nextDay, deliver); err != nil {
		t.Fatal(err)
	}
	if len(notices) != 2 {
		t.Fatalf("notices after period reset = %v, want two", notices)
	}
}

func TestBudgetPeriodsResetAcrossReopenAndDeltasDoNotDoubleCount(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "waffle.db")
	st, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	u := New(st)
	now := time.Date(2026, 7, 13, 23, 30, 0, 0, time.UTC)
	previous := llm.Usage{}
	current := llm.Usage{InputTokens: 6, OutputTokens: 4}
	if err := u.AddDelta(ctx, "stable", previous, current); err != nil {
		t.Fatal(err)
	}
	// Re-observing the same cumulative usage records no second request/tokens.
	if err := u.AddDelta(ctx, "stable", current, current); err != nil {
		t.Fatal(err)
	}
	if err := u.Check(ctx, "stable", Limits{TokensPerDay: 10}, time.Now()); err == nil {
		t.Fatal("daily cap did not apply")
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st, err = store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	u = New(st)
	// The explicit-clock path proves both daily tokens and hourly requests
	// reset at their UTC boundaries after a store round-trip.
	if err := u.AddRequestAt(ctx, "hour-boundary", llm.Usage{}, now); err != nil {
		t.Fatal(err)
	}
	if err := u.Check(ctx, "hour-boundary", Limits{RequestsPerHour: 1}, now); err == nil {
		t.Fatal("request cap did not apply before boundary")
	}
	if err := u.Check(ctx, "hour-boundary", Limits{RequestsPerHour: 1}, now.Add(time.Hour)); err != nil {
		t.Fatalf("hour boundary did not reset request cap: %v", err)
	}
	if err := u.AddRequestAt(ctx, "day-boundary", llm.Usage{InputTokens: 10}, now); err != nil {
		t.Fatal(err)
	}
	if err := u.Check(ctx, "day-boundary", Limits{TokensPerDay: 10}, now.Add(15*time.Minute)); err == nil {
		t.Fatal("daily token cap reset before midnight")
	}
	if err := u.Check(ctx, "day-boundary", Limits{TokensPerDay: 10}, now.Add(45*time.Minute)); err != nil {
		t.Fatalf("day boundary did not reset token cap: %v", err)
	}
}
