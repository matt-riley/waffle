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
			if _, err := u.ReserveRequestAt(ctx, "concurrent", "anthropic", Limits{RequestsPerHour: 1}, now, 0, false); err == nil {
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

func TestTokenReservationsAreAtomicDurableAndReconciled(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "waffle.db")
	st, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	u := New(st)
	now := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	limits := Limits{TokensPerDay: 100, RequestsPerHour: 10}

	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	var reservations []int
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			reserved, err := u.ReserveRequestAt(ctx, "streams", "anthropic", limits, now, 60, false)
			if err == nil {
				mu.Lock()
				reservations = append(reservations, reserved)
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	if len(reservations) != 1 || reservations[0] != 60 {
		t.Fatalf("reservations=%v", reservations)
	}

	// No reconciliation models an aborted stream: its reservation stays charged.
	if _, err := u.ReserveRequestAt(ctx, "streams", "anthropic", limits, now, 50, false); err == nil {
		t.Fatal("aborted reservation did not protect cap")
	}
	if err := u.ReconcileReservationAt(ctx, "streams", now, 60, llm.Usage{InputTokens: 4, OutputTokens: 6}); err != nil {
		t.Fatal(err)
	}
	if reserved, err := u.ReserveRequestAt(ctx, "streams", "anthropic", limits, now, 90, false); err != nil || reserved != 90 {
		t.Fatalf("lower actual usage did not restore capacity: reserved=%d err=%v", reserved, err)
	}

	rows, err := u.List(ctx, "streams")
	if err != nil {
		t.Fatal(err)
	}
	var day Row
	for _, row := range rows {
		if row.Period == "day" {
			day = row
		}
	}
	if day.InputTokens+day.OutputTokens != 10 || day.ReservedTokens != 90 || day.Requests != 2 {
		t.Fatalf("day=%+v", day)
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
	if _, err := u.ReserveRequestAt(ctx, "streams", "anthropic", limits, now.Add(24*time.Hour), 100, false); err != nil {
		t.Fatalf("new day did not reset reservation budget: %v", err)
	}
}

func TestMissingDeclaredMaximumReservesRemainingAllowance(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	u := New(st)
	now := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	reserved, err := u.ReserveRequestAt(ctx, "unknown", "anthropic", Limits{TokensPerDay: 100}, now, 0, true)
	if err != nil || reserved != 100 {
		t.Fatalf("reserved=%d err=%v", reserved, err)
	}
	if _, err := u.ReserveRequestAt(ctx, "unknown", "anthropic", Limits{TokensPerDay: 100}, now, 1, false); err == nil {
		t.Fatal("remaining allowance reservation was bypassed")
	}
}

func TestReconcileReservationSaturatesPersistedCounters(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	u := New(st)
	now := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	if err := u.AddRequestAt(ctx, "saturated", llm.Usage{InputTokens: 1, OutputTokens: 2}, now); err != nil {
		t.Fatal(err)
	}
	reserved, err := u.ReserveRequestAt(ctx, "saturated", "anthropic", Limits{}, now, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	maxInt := int(^uint(0) >> 1)
	if err := u.ReconcileReservationAt(ctx, "saturated", now, reserved, llm.Usage{InputTokens: maxInt, OutputTokens: maxInt}); err != nil {
		t.Fatal(err)
	}
	rows, err := u.List(ctx, "saturated")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.InputTokens != maxInt || row.OutputTokens != maxInt {
			t.Fatalf("row did not saturate as integers: %+v", row)
		}
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

// TestAddPersistsCacheCounters pins that the store records the provider's
// cache counters as raw columns alongside uncached input.
func TestAddPersistsCacheCounters(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	u := New(st)
	now := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	if err := u.AddRequestAt(ctx, "cached", llm.Usage{InputTokens: 10, OutputTokens: 5, CacheCreationInputTokens: 20, CacheReadInputTokens: 30}, now); err != nil {
		t.Fatal(err)
	}
	rows, err := u.List(ctx, "cached")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want day+hour", len(rows))
	}
	for _, row := range rows {
		if row.InputTokens != 10 || row.OutputTokens != 5 || row.CacheCreationInputTokens != 20 || row.CacheReadInputTokens != 30 {
			t.Fatalf("row = %+v, want raw cache counters persisted", row)
		}
	}
	// Accumulation across requests adds each counter independently.
	if err := u.AddRequestAt(ctx, "cached", llm.Usage{InputTokens: 1, CacheReadInputTokens: 7}, now); err != nil {
		t.Fatal(err)
	}
	rows, err = u.List(ctx, "cached")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.InputTokens != 11 || row.CacheReadInputTokens != 37 {
			t.Fatalf("accumulated row = %+v", row)
		}
	}
}

// TestAddTokensAtPersistsCacheCounters pins the streaming-proxy path used by
// sandboxed traffic: cache counters arrive with the final usage observation.
func TestAddTokensAtPersistsCacheCounters(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	u := New(st)
	now := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	if err := u.AddTokensAt(ctx, "tokens", llm.Usage{InputTokens: 4, CacheReadInputTokens: 100}, now); err != nil {
		t.Fatal(err)
	}
	rows, err := u.List(ctx, "tokens")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.InputTokens != 4 || row.CacheReadInputTokens != 100 || row.Requests != 0 {
			t.Fatalf("row = %+v", row)
		}
	}
	// Zeroed counters record nothing.
	if err := u.AddTokensAt(ctx, "tokens", llm.Usage{}, now); err != nil {
		t.Fatal(err)
	}
	rows, err = u.List(ctx, "tokens")
	if err != nil {
		t.Fatal(err)
	}
	var readTotal int
	for _, row := range rows {
		if row.Period == "day" {
			readTotal = row.CacheReadInputTokens
		}
	}
	if readTotal != 100 {
		t.Fatalf("zeroed observation recorded tokens: %d", readTotal)
	}
}

// TestCheckBindsOnTrueCostNotPreCacheCounts pins the acceptance criterion
// that budgets bind on true cost: 500 cache-read tokens bill at 0.1x, so a
// session is nowhere near a 100-token budget even though its pre-cache count
// (550) would blow straight through it.
func TestCheckBindsOnTrueCostNotPreCacheCounts(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	u := New(st)
	now := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	if err := u.AddRequestAt(ctx, "cheap-reads", llm.Usage{InputTokens: 50, CacheReadInputTokens: 500}, now); err != nil {
		t.Fatal(err)
	}
	// True cost = 50 + 500*0.1 = 100, which exactly exhausts the budget.
	if err := u.Check(ctx, "cheap-reads", Limits{TokensPerDay: 100}, now); err == nil {
		t.Fatal("budget did not bind at true cost 100")
	}
	if err := u.Check(ctx, "cheap-reads", Limits{TokensPerDay: 101}, now); err != nil {
		t.Fatalf("budget bound too early at true cost 100: %v", err)
	}
}

// TestAlertBindsOnTrueCostNotPreCacheCounts pins the Alert threshold on the
// same weighted arithmetic: 4000 cache-read tokens bill at 400 true-cost
// tokens, so a 1000-token budget at a 50% threshold (500) is not crossed
// even though the pre-cache count (4040) is eight times past it.
func TestAlertBindsOnTrueCostNotPreCacheCounts(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	u := New(st)
	now := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	if err := u.AddRequestAt(ctx, "alert-cheap", llm.Usage{InputTokens: 40, CacheReadInputTokens: 4000}, now); err != nil {
		t.Fatal(err)
	}
	var notices []string
	deliver := func(_ context.Context, notice string) error {
		notices = append(notices, notice)
		return nil
	}
	// True cost 40 + 4000*0.1 = 440 < 500: no alert, even though the naive
	// count of 4040 would have tripped the threshold many times over.
	if err := u.Alert(ctx, "alert-cheap", Limits{TokensPerDay: 1000, AlertThresholdPercent: 50}, now, deliver); err != nil {
		t.Fatal(err)
	}
	if len(notices) != 0 {
		t.Fatalf("notices = %v, want none below true-cost threshold", notices)
	}
	// A sibling session at the true-cost boundary (60 + 4400*0.1 = 500) does
	// alert: the threshold is real, just weighted.
	if err := u.AddRequestAt(ctx, "alert-boundary", llm.Usage{InputTokens: 60, CacheReadInputTokens: 4400}, now); err != nil {
		t.Fatal(err)
	}
	if err := u.Alert(ctx, "alert-boundary", Limits{TokensPerDay: 1000, AlertThresholdPercent: 50}, now, deliver); err != nil {
		t.Fatal(err)
	}
	if len(notices) != 1 {
		t.Fatalf("notices = %v, want the true-cost boundary alert", notices)
	}
}

// TestReserveReconcileBindOnTrueCost pins the reserve/reconcile path: a
// reservation is checked against true cost, and reconciliation carries the
// cache counters into the row and releases the reservation.
func TestReserveReconcileBindOnTrueCost(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	u := New(st)
	now := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	// True cost 43: 10 + 20*1.25 + 30*0.1 + 5.
	if err := u.AddRequestAt(ctx, "rec", llm.Usage{InputTokens: 10, OutputTokens: 5, CacheCreationInputTokens: 20, CacheReadInputTokens: 30}, now); err != nil {
		t.Fatal(err)
	}
	// Declared 60: 43+60=103 > 100, refused.
	if _, err := u.ReserveRequestAt(ctx, "rec", "anthropic", Limits{TokensPerDay: 100}, now, 60, false); err == nil {
		t.Fatal("reservation passed under true-cost binding")
	}
	// Declared 50: 43+50=93 <= 100, allowed.
	reserved, err := u.ReserveRequestAt(ctx, "rec", "anthropic", Limits{TokensPerDay: 100}, now, 50, false)
	if err != nil || reserved != 50 {
		t.Fatalf("reserved=%d err=%v", reserved, err)
	}
	// Reconcile with the same cached counters: the row accumulates to raw
	// 20/10/40/60 and the reservation releases.
	if err := u.ReconcileReservationAt(ctx, "rec", now, 50, llm.Usage{InputTokens: 10, OutputTokens: 5, CacheCreationInputTokens: 20, CacheReadInputTokens: 30}); err != nil {
		t.Fatal(err)
	}
	rows, err := u.List(ctx, "rec")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Period == "day" {
			if row.InputTokens != 20 || row.OutputTokens != 10 || row.CacheCreationInputTokens != 40 || row.CacheReadInputTokens != 60 || row.ReservedTokens != 0 {
				t.Fatalf("day row = %+v", row)
			}
		}
	}
	// A zeroed actual usage leaves the reservation charged (abort path) and
	// records no cache counters.
	u3 := New(st)
	if _, err := u3.ReserveRequestAt(ctx, "abort", "anthropic", Limits{TokensPerDay: 100}, now, 40, false); err != nil {
		t.Fatal(err)
	}
	if err := u3.ReconcileReservationAt(ctx, "abort", now, 40, llm.Usage{}); err != nil {
		t.Fatal(err)
	}
	rows, err = u3.List(ctx, "abort")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.InputTokens != 0 || row.CacheReadInputTokens != 0 || row.ReservedTokens != 40 {
			t.Fatalf("abort row = %+v, want reservation retained with zeroed counters", row)
		}
	}
}

// TestReconcileSaturatesCacheCounters pins the saturation guards on the new
// columns: a huge cache observation clamps at MaxInt instead of wrapping.
func TestReconcileSaturatesCacheCounters(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	u := New(st)
	now := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	reserved, err := u.ReserveRequestAt(ctx, "sat", "anthropic", Limits{}, now, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	maxInt := int(^uint(0) >> 1)
	if err := u.ReconcileReservationAt(ctx, "sat", now, reserved, llm.Usage{CacheCreationInputTokens: maxInt, CacheReadInputTokens: maxInt}); err != nil {
		t.Fatal(err)
	}
	rows, err := u.List(ctx, "sat")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.CacheCreationInputTokens != maxInt || row.CacheReadInputTokens != maxInt {
			t.Fatalf("row did not saturate: %+v", row)
		}
	}
}

// TestCheckPricesOpenAICacheReadsAtHalfRate pins finding 1 of the #247
// review: usage rows attributed to an OpenAI-compatible provider price
// cache reads at the OpenAI-compatible 0.5x multiplier, not Anthropic's
// 0.1x. 500 cache-read tokens bill 250 at 0.5x (plus 50 uncached = 300),
// whereas the same row under the old hardcoded Anthropic model would have
// billed only 100.
func TestCheckPricesOpenAICacheReadsAtHalfRate(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	u := New(st)
	now := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	if err := u.AddRequestAt(ctx, "openai-reads", llm.Usage{InputTokens: 50, CacheReadInputTokens: 500, Provider: "openai"}, now); err != nil {
		t.Fatal(err)
	}
	// True cost = 50 + 500*0.5 = 300.
	if err := u.Check(ctx, "openai-reads", Limits{TokensPerDay: 300}, now); err == nil {
		t.Fatal("budget did not bind at OpenAI true cost 300")
	}
	if err := u.Check(ctx, "openai-reads", Limits{TokensPerDay: 301}, now); err != nil {
		t.Fatalf("budget bound too early at OpenAI true cost 300: %v", err)
	}
	// The same counters under the Anthropic model would cost 100, so a
	// 101-token budget must still bind: the row really is priced at 0.5x.
	if err := u.Check(ctx, "openai-reads", Limits{TokensPerDay: 101}, now); err == nil {
		t.Fatal("OpenAI row priced below its 0.5x true cost")
	}
}

// TestLegacyRowsWithoutProviderPriceAtAnthropicModel pins that rows written
// before provider attribution existed (or by callers that never learned the
// provider) fall back to the Anthropic model: cache reads bill at 0.1x.
func TestLegacyRowsWithoutProviderPriceAtAnthropicModel(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	u := New(st)
	now := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	// A legacy row inserted without a provider column value takes the
	// migration default ('anthropic').
	if _, err := st.DB.ExecContext(ctx, `
		INSERT INTO usage (session_id, period, period_start, requests, input_tokens, output_tokens, cache_creation_input_tokens, cache_read_input_tokens)
		VALUES ('legacy', 'day', ?, 1, 50, 0, 0, 500), ('legacy', 'hour', ?, 1, 50, 0, 0, 500)`,
		period(now, 24*time.Hour), period(now, time.Hour)); err != nil {
		t.Fatal(err)
	}
	// True cost = 50 + 500*0.1 = 100.
	if err := u.Check(ctx, "legacy", Limits{TokensPerDay: 100}, now); err == nil {
		t.Fatal("legacy row did not bind at Anthropic true cost 100")
	}
	if err := u.Check(ctx, "legacy", Limits{TokensPerDay: 101}, now); err != nil {
		t.Fatalf("legacy row priced above Anthropic true cost 100: %v", err)
	}
	// Attribution from a caller that never learned the provider (empty
	// Usage.Provider) behaves identically.
	if err := u.AddRequestAt(ctx, "unattributed", llm.Usage{InputTokens: 50, CacheReadInputTokens: 500}, now); err != nil {
		t.Fatal(err)
	}
	if err := u.Check(ctx, "unattributed", Limits{TokensPerDay: 100}, now); err == nil {
		t.Fatal("unattributed row did not bind at Anthropic true cost 100")
	}
	if err := u.Check(ctx, "unattributed", Limits{TokensPerDay: 101}, now); err != nil {
		t.Fatalf("unattributed row priced above Anthropic true cost 100: %v", err)
	}
}

// TestCheckBindsPerProviderPrice pins that budget limits bind on the
// per-provider price of a row's cache tokens: identical counters bill at
// the provider's own cache-read multiplier, and an empty attribution
// (legacy) falls back to the Anthropic model.
func TestCheckBindsPerProviderPrice(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		wantCost int // 50 uncached + 500 cache-read at the provider's rate
	}{
		{name: "anthropic", provider: "anthropic", wantCost: 100}, // 50 + 500*0.1
		{name: "openai", provider: "openai", wantCost: 300},       // 50 + 500*0.5
		{name: "legacy empty", provider: "", wantCost: 100},       // falls back to Anthropic
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = st.Close() }()
			u := New(st)
			now := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
			session := "price-" + tc.name
			if err := u.AddRequestAt(ctx, session, llm.Usage{InputTokens: 50, CacheReadInputTokens: 500, Provider: tc.provider}, now); err != nil {
				t.Fatal(err)
			}
			rows, err := u.List(ctx, session)
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range rows {
				if row.Period == "day" {
					want := tc.provider
					if want == "" {
						want = "anthropic"
					}
					if row.Provider != want {
						t.Fatalf("day row provider = %q, want %q", row.Provider, want)
					}
				}
			}
			if err := u.Check(ctx, session, Limits{TokensPerDay: tc.wantCost}, now); err == nil {
				t.Fatalf("budget did not bind at %s true cost %d", tc.provider, tc.wantCost)
			}
			if err := u.Check(ctx, session, Limits{TokensPerDay: tc.wantCost + 1}, now); err != nil {
				t.Fatalf("budget bound too early at %s true cost %d: %v", tc.provider, tc.wantCost, err)
			}
		})
	}
}

// TestReserveReconcilePricesPerProvider pins the broker path: reservations
// record the upstream's provider type, reconciliation keeps it on the row,
// and subsequent budget checks price that row's cache tokens with the
// provider's own model.
func TestReserveReconcilePricesPerProvider(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	u := New(st)
	now := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	reserved, err := u.ReserveRequestAt(ctx, "openai-res", "openai", Limits{TokensPerDay: 100}, now, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := u.ReconcileReservationAt(ctx, "openai-res", now, reserved, llm.Usage{InputTokens: 50, CacheReadInputTokens: 500, Provider: "openai"}); err != nil {
		t.Fatal(err)
	}
	rows, err := u.List(ctx, "openai-res")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Period == "day" {
			if row.Provider != "openai" || row.CacheReadInputTokens != 500 {
				t.Fatalf("day row = %+v, want provider openai with 500 cache-read tokens", row)
			}
		}
	}
	// True cost 50 + 500*0.5 = 300 > 100: the openai-priced row binds hard.
	if err := u.Check(ctx, "openai-res", Limits{TokensPerDay: 300}, now); err == nil {
		t.Fatal("reconciled OpenAI row did not bind at 0.5x true cost")
	}
	if err := u.Check(ctx, "openai-res", Limits{TokensPerDay: 301}, now); err != nil {
		t.Fatalf("reconciled OpenAI row bound too early: %v", err)
	}
	// An Anthropic reservation for the same counters would cost only 100.
	u2 := New(st)
	if _, err := u2.ReserveRequestAt(ctx, "anthropic-res", "anthropic", Limits{TokensPerDay: 100}, now, 0, false); err != nil {
		t.Fatal(err)
	}
	// Reconcile with an unattributed observation (legacy capture path):
	// the row falls back to Anthropic pricing.
	rows2, err := u2.List(ctx, "anthropic-res")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows2 {
		if row.Period == "day" && row.Provider != "anthropic" {
			t.Fatalf("anthropic reservation row provider = %q, want anthropic", row.Provider)
		}
	}
}

// TestAlertPricesPerProvider pins the Alert threshold on the per-provider
// true cost: an OpenAI session crosses a 50% threshold only when 0.5x-
// weighted usage passes it, not when the Anthropic 0.1x weight would keep
// it under.
func TestAlertPricesPerProvider(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	u := New(st)
	now := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	var notices []string
	deliver := func(_ context.Context, notice string) error {
		notices = append(notices, notice)
		return nil
	}
	// True cost 40 + 4000*0.5 = 2040 >= 1000*50% = 500: alerts.
	if err := u.AddRequestAt(ctx, "alert-openai", llm.Usage{InputTokens: 40, CacheReadInputTokens: 4000, Provider: "openai"}, now); err != nil {
		t.Fatal(err)
	}
	if err := u.Alert(ctx, "alert-openai", Limits{TokensPerDay: 1000, AlertThresholdPercent: 50}, now, deliver); err != nil {
		t.Fatal(err)
	}
	if len(notices) != 1 {
		t.Fatalf("notices = %v, want the OpenAI true-cost alert", notices)
	}
	// The same counters under the Anthropic model (440) stay under the 500
	// threshold, so provider attribution is what made the alert fire.
	if err := u.AddRequestAt(ctx, "alert-anthropic", llm.Usage{InputTokens: 40, CacheReadInputTokens: 4000, Provider: "anthropic"}, now); err != nil {
		t.Fatal(err)
	}
	if err := u.Alert(ctx, "alert-anthropic", Limits{TokensPerDay: 1000, AlertThresholdPercent: 50}, now, deliver); err != nil {
		t.Fatal(err)
	}
	if len(notices) != 1 {
		t.Fatalf("notices = %v, want no Anthropic alert below its true-cost threshold", notices)
	}
}

// TestMixedProviderDayPricesEachProvider is the regression for the #247
// review finding that a budget key routing requests to both Anthropic and
// OpenAI-compatible upstreams in one period merged their counters into a
// single row priced at the last writer's multipliers. Rows are keyed per
// provider (migration 0030), so each provider's cache tokens price with its
// own model and the day sum binds on the true per-provider total.
func TestMixedProviderDayPricesEachProvider(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	u := New(st)
	now := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	session := "mixed-providers"
	// Anthropic: 1000 cache-read tokens bill at 0.1x = 100.
	if err := u.AddRequestAt(ctx, session, llm.Usage{InputTokens: 50, CacheReadInputTokens: 1000, Provider: "anthropic"}, now); err != nil {
		t.Fatal(err)
	}
	// OpenAI: 500 cache-read tokens bill at 0.5x = 250. Total true cost 350.
	if err := u.AddRequestAt(ctx, session, llm.Usage{InputTokens: 50, CacheReadInputTokens: 500, Provider: "openai"}, now); err != nil {
		t.Fatal(err)
	}
	// Another identical Anthropic request accumulates on the Anthropic row only.
	if err := u.AddRequestAt(ctx, session, llm.Usage{InputTokens: 50, CacheReadInputTokens: 1000, Provider: "anthropic"}, now); err != nil {
		t.Fatal(err)
	}
	rows, err := u.List(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	var dayAnthropic, dayOpenAI int
	for _, row := range rows {
		if row.Period != "day" {
			continue
		}
		switch row.Provider {
		case "anthropic":
			dayAnthropic++
			if row.Requests != 2 || row.CacheReadInputTokens != 2000 {
				t.Fatalf("anthropic day row = %+v, want 2 requests and 2000 cache-read tokens", row)
			}
		case "openai":
			dayOpenAI++
			if row.Requests != 1 || row.CacheReadInputTokens != 500 {
				t.Fatalf("openai day row = %+v, want 1 request and 500 cache-read tokens", row)
			}
		default:
			t.Fatalf("unexpected day row provider %q", row.Provider)
		}
	}
	if dayAnthropic != 1 || dayOpenAI != 1 {
		t.Fatalf("day rows per provider: anthropic=%d openai=%d, want 1 each", dayAnthropic, dayOpenAI)
	}
	// True cost: 2*50 + 2000*0.1 + 50 + 500*0.5 = 100 + 200 + 50 + 250 = 600.
	// If the row had merged and repriced everything at the last writer
	// (openai), the billed total would be 650; at the first writer
	// (anthropic), 400. Either way 600 pins the per-provider sum.
	if err := u.Check(ctx, session, Limits{TokensPerDay: 600}, now); err == nil {
		t.Fatal("budget did not bind on the per-provider true cost 600")
	}
	if err := u.Check(ctx, session, Limits{TokensPerDay: 601}, now); err != nil {
		t.Fatalf("budget bound too early at per-provider true cost 600: %v", err)
	}
}

// TestMixedProviderReserveReconcilePricesEachProvider pins the broker path:
// reservations for different upstreams in the same period stay on their own
// provider rows through reconciliation, and the combined day binds on the
// sum of each provider's true cost.
func TestMixedProviderReserveReconcilePricesEachProvider(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	u := New(st)
	now := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	session := "mixed-reserve"
	reserved, err := u.ReserveRequestAt(ctx, session, "anthropic", Limits{TokensPerDay: 10000}, now, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := u.ReconcileReservationAt(ctx, session, now, reserved, llm.Usage{InputTokens: 100, CacheReadInputTokens: 1000, Provider: "anthropic"}); err != nil {
		t.Fatal(err)
	}
	reserved, err = u.ReserveRequestAt(ctx, session, "openai", Limits{TokensPerDay: 10000}, now, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := u.ReconcileReservationAt(ctx, session, now, reserved, llm.Usage{InputTokens: 100, CacheReadInputTokens: 1000, Provider: "openai"}); err != nil {
		t.Fatal(err)
	}
	// True cost 100 + 1000*0.1 + 100 + 1000*0.5 = 200 + 100 + 500 = 800.
	if err := u.Check(ctx, session, Limits{TokensPerDay: 800}, now); err == nil {
		t.Fatal("budget did not bind on combined per-provider true cost 800")
	}
	if err := u.Check(ctx, session, Limits{TokensPerDay: 801}, now); err != nil {
		t.Fatalf("budget bound too early at combined per-provider true cost 800: %v", err)
	}
}
