package usage

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/store"
)

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
