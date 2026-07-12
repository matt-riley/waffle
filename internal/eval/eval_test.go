package eval

import (
	"bytes"
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/store"
)

func TestRegistryPassesOffline(t *testing.T) {
	var buf bytes.Buffer
	fails := RunAll(context.Background(), &buf, Registry())
	if fails != 0 {
		t.Fatalf("fails=%d\n%s", fails, buf.String())
	}
	if len(Registry()) < 6 {
		t.Fatalf("want at least 6 seed evals, got %d", len(Registry()))
	}
	// Six seed names must be present exactly.
	have := map[string]bool{}
	for _, c := range Registry() {
		have[c.Name] = true
	}
	for _, n := range SeedNames {
		if !have[n] {
			t.Fatalf("Registry missing seed %q", n)
		}
	}
}

func TestRegistryNamesMatchEvalsDir(t *testing.T) {
	if err := EnsureTOMLCovered(""); err != nil {
		t.Fatal(err)
	}
	names, err := DiscoverTOMLNames("")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) < 6 {
		t.Fatalf("want at least 6 evals/*.toml, got %v", names)
	}
	want := map[string]bool{}
	for _, n := range SeedNames {
		want[n] = true
	}
	for _, n := range names {
		if !want[n] {
			// Extra toml files must still be covered by Registry (EnsureTOMLCovered).
			continue
		}
		delete(want, n)
	}
	if len(want) > 0 {
		t.Fatalf("seed names missing from evals/*.toml: %v", want)
	}
}

func TestEvalZeroNetworkGuard(t *testing.T) {
	var attempted int
	restore := GuardHTTPTransport(func(*http.Request) { attempted++ })
	defer restore()
	var buf bytes.Buffer
	fails := RunAll(context.Background(), &buf, Registry())
	if fails != 0 {
		t.Fatalf("fails=%d\n%s", fails, buf.String())
	}
	if attempted != 0 {
		t.Fatalf("network attempted %d times under HTTP guard", attempted)
	}
}

func TestEvalNonZeroOnFail(t *testing.T) {
	cases := []Case{{
		Name: "always_fail",
		Run:  func(context.Context) error { return context.Canceled },
	}}
	var buf bytes.Buffer
	fails := RunAll(context.Background(), &buf, cases)
	if fails != 1 {
		t.Fatalf("fails=%d want 1", fails)
	}
	if !strings.Contains(buf.String(), "FAIL always_fail") {
		t.Fatalf("output=%q", buf.String())
	}
}

func TestEvalHistoryRecorded(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "e.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	started := time.Now().UTC()
	report := Run(ctx, Registry())
	if report.Failed != 0 {
		t.Fatalf("failed=%d\n%s", report.Failed, report.Text)
	}
	if err := RecordRun(ctx, st.DB, "test-v1", started, report); err != nil {
		t.Fatal(err)
	}
	hist, err := ListHistory(ctx, st.DB, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 1 || hist[0].Version != "test-v1" || hist[0].Passed < 6 {
		t.Fatalf("hist=%+v", hist)
	}
	var out bytes.Buffer
	FormatHistory(&out, hist)
	if !strings.Contains(out.String(), "test-v1") || !strings.Contains(out.String(), "passed") {
		t.Fatalf("format=%q", out.String())
	}
}

func TestLiveRegistrySkippedWithoutOptIn(t *testing.T) {
	t.Setenv("WAFFLE_EVAL_LIVE", "")
	if got := LiveRegistry(); len(got) != 0 {
		t.Fatalf("live cases without opt-in: %d", len(got))
	}
	t.Setenv("WAFFLE_EVAL_LIVE", "1")
	// Opt-in without a wired provider still yields empty live set (skip).
	if got := LiveRegistry(); len(got) != 0 {
		t.Fatalf("live cases without provider: %d", len(got))
	}
}

func TestCodeIntelFailureFallsBackToNativeSearchRead(t *testing.T) {
	if err := evalCodeIntelFallback(context.Background()); err != nil {
		t.Fatal(err)
	}
}
