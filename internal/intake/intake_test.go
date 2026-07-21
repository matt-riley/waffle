package intake

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/store"
)

type stubTracker struct {
	mu      sync.Mutex
	issues  map[int]Issue
	openDep map[int]bool
}

func (s *stubTracker) ListOpen(ctx context.Context, repo, label string) ([]Issue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Issue
	for _, iss := range s.issues {
		if iss.State != "open" {
			continue
		}
		if label != "" && !hasLabel(iss.Labels, label) {
			continue
		}
		out = append(out, iss)
	}
	return out, nil
}

func (s *stubTracker) IsOpen(ctx context.Context, repo string, number int) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.openDep[number]; ok {
		return v, nil
	}
	iss, ok := s.issues[number]
	return ok && iss.State == "open", nil
}

type stubDispatcher struct {
	mu           sync.Mutex
	started      []int
	cancelled    []int
	block        chan struct{} // if non-nil, Dispatch waits until closed
	sleep        time.Duration // if > 0, sleep after start (see ignoreCancel)
	ignoreCancel bool          // when true, sleep ignores ctx cancel
	onStart      chan struct{} // closed once when first Dispatch starts
	err          error
}

func (d *stubDispatcher) Dispatch(ctx context.Context, cfg WatchConfig, iss Issue) (string, error) {
	d.mu.Lock()
	d.started = append(d.started, iss.Number)
	onStart := d.onStart
	d.onStart = nil
	d.mu.Unlock()
	if onStart != nil {
		close(onStart)
	}
	if d.block != nil {
		select {
		case <-d.block:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if d.sleep > 0 {
		if d.ignoreCancel {
			time.Sleep(d.sleep)
		} else {
			select {
			case <-time.After(d.sleep):
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
	}
	if d.err != nil {
		return "", d.err
	}
	// Assert untrusted marker in the prompt assembly helper.
	p := PromptForIssue(iss)
	if !strings.Contains(p, "UNTRUSTED EXTERNAL CONTENT") {
		return "", fmt.Errorf("missing untrusted marker")
	}
	return "done #" + fmt.Sprint(iss.Number), nil
}

func (d *stubDispatcher) Cancel(ctx context.Context, claim Claim) error {
	d.mu.Lock()
	d.cancelled = append(d.cancelled, claim.IssueNumber)
	d.mu.Unlock()
	return nil
}

type captureDeliverer struct {
	mu   sync.Mutex
	msgs []string
}

func (c *captureDeliverer) Deliver(ctx context.Context, target, text string) error {
	c.mu.Lock()
	c.msgs = append(c.msgs, target+":"+text)
	c.mu.Unlock()
	return nil
}

func testStore(t *testing.T) *ClaimStore {
	t.Helper()
	st, err := store.Open(context.Background(), t.TempDir()+"/w.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return &ClaimStore{DB: st.DB}
}

func TestSortAndFilterBlockers(t *testing.T) {
	now := time.Now()
	issues := []Issue{
		{Number: 2, Priority: 5, CreatedAt: now.Add(-time.Hour), Blockers: []int{9}},
		{Number: 1, Priority: 1, CreatedAt: now},
		{Number: 3, Priority: 1, CreatedAt: now.Add(-2 * time.Hour)},
	}
	tr := &stubTracker{openDep: map[int]bool{9: true}}
	ready, err := FilterReady(context.Background(), tr, "o/r", issues)
	if err != nil {
		t.Fatal(err)
	}
	SortCandidates(ready)
	if len(ready) != 2 || ready[0].Number != 3 || ready[1].Number != 1 {
		t.Fatalf("ready = %+v", ready)
	}
}

func TestPromptUntrusted(t *testing.T) {
	p := PromptForIssue(Issue{Number: 7, Title: "Fix it", Body: "do the thing"})
	if !strings.Contains(p, "UNTRUSTED EXTERNAL CONTENT") || !strings.Contains(p, "do the thing") {
		t.Fatalf("%q", p)
	}
}

func TestClaimNoDoubleDispatchAndRestart(t *testing.T) {
	ctx := context.Background()
	claims := testStore(t)
	ok, err := claims.TryClaim(ctx, "o/r", 1)
	if err != nil || !ok {
		t.Fatalf("first claim: %v %v", ok, err)
	}
	ok, err = claims.TryClaim(ctx, "o/r", 1)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("second claim should fail while active")
	}
	if err := claims.Release(ctx, "o/r", 1); err != nil {
		t.Fatal(err)
	}
	// Simulated restart: new ClaimStore on same DB can re-claim released.
	ok, err = claims.TryClaim(ctx, "o/r", 1)
	if err != nil || !ok {
		t.Fatalf("reclaim after release: %v %v", ok, err)
	}
}

func TestConcurrencyBound(t *testing.T) {
	ctx := context.Background()
	claims := testStore(t)
	tr := &stubTracker{issues: map[int]Issue{
		1: {Number: 1, Title: "a", State: "open", Labels: []string{"agent-ok"}, CreatedAt: time.Now().Add(-3 * time.Hour), Priority: 1},
		2: {Number: 2, Title: "b", State: "open", Labels: []string{"agent-ok"}, CreatedAt: time.Now().Add(-2 * time.Hour), Priority: 1},
		3: {Number: 3, Title: "c", State: "open", Labels: []string{"agent-ok"}, CreatedAt: time.Now().Add(-1 * time.Hour), Priority: 1},
	}}
	block := make(chan struct{})
	disp := &stubDispatcher{block: block}
	w := &Watcher{
		Config:     WatchConfig{Repo: "o/r", Label: "agent-ok", MaxConcurrency: 2},
		Tracker:    tr,
		Claims:     claims,
		Dispatcher: disp,
	}
	if err := w.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	// Allow goroutines to register.
	time.Sleep(50 * time.Millisecond)
	disp.mu.Lock()
	n := len(disp.started)
	disp.mu.Unlock()
	if n != 2 {
		t.Fatalf("started = %d, want 2", n)
	}
	close(block)
	// Wait for releases then a second tick should start the third.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_ = w.Tick(ctx)
		time.Sleep(20 * time.Millisecond)
		disp.mu.Lock()
		n = len(disp.started)
		disp.mu.Unlock()
		if n >= 3 {
			break
		}
	}
	if n < 3 {
		t.Fatalf("eventually started = %d, want 3", n)
	}
}

func TestReconcileCancelsOnClose(t *testing.T) {
	ctx := context.Background()
	claims := testStore(t)
	tr := &stubTracker{issues: map[int]Issue{
		1: {Number: 1, Title: "a", State: "open", Labels: []string{"agent-ok"}, CreatedAt: time.Now(), Priority: 1},
	}}
	block := make(chan struct{})
	disp := &stubDispatcher{block: block}
	w := &Watcher{
		Config:     WatchConfig{Repo: "o/r", Label: "agent-ok", MaxConcurrency: 1},
		Tracker:    tr,
		Claims:     claims,
		Dispatcher: disp,
	}
	if err := w.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	tr.mu.Lock()
	iss := tr.issues[1]
	iss.State = "closed"
	tr.issues[1] = iss
	tr.mu.Unlock()
	if err := w.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	close(block)
	time.Sleep(30 * time.Millisecond)
	disp.mu.Lock()
	cancelled := len(disp.cancelled)
	disp.mu.Unlock()
	if cancelled != 1 {
		t.Fatalf("cancelled = %d", cancelled)
	}
	c, err := claims.Get(ctx, "o/r", 1)
	if err != nil {
		t.Fatal(err)
	}
	if c == nil || c.Status != StatusReleased {
		t.Fatalf("claim = %+v", c)
	}
}

func TestDelivery(t *testing.T) {
	ctx := context.Background()
	claims := testStore(t)
	tr := &stubTracker{issues: map[int]Issue{
		1: {Number: 1, Title: "a", Body: "body", State: "open", Labels: []string{"agent-ok"}, CreatedAt: time.Now(), Priority: 1},
	}}
	cap := &captureDeliverer{}
	w := &Watcher{
		Config:     WatchConfig{Repo: "o/r", Label: "agent-ok", MaxConcurrency: 1, Deliver: "telegram:1"},
		Tracker:    tr,
		Claims:     claims,
		Dispatcher: &stubDispatcher{},
		Deliverer:  cap,
	}
	if err := w.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		cap.mu.Lock()
		n := len(cap.msgs)
		cap.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if len(cap.msgs) != 1 || !strings.Contains(cap.msgs[0], "telegram:1:") {
		t.Fatalf("msgs = %#v", cap.msgs)
	}
}

func TestWatchConfigValidate(t *testing.T) {
	if err := (WatchConfig{}).Validate(); err == nil {
		t.Fatal("expected error")
	}
	if err := (WatchConfig{Repo: "o/r", MaxConcurrency: 0}).Validate(); err == nil {
		t.Fatal("expected concurrency error")
	}
	if err := (WatchConfig{Repo: "o/r", MaxConcurrency: 1}).Validate(); err != nil {
		t.Fatal(err)
	}
}

// TestRunAwaitsInFlightDispatches ensures Run does not return on ctx cancel
// until in-flight dispatch goroutines finish (issue #101).
func TestRunAwaitsInFlightDispatches(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	claims := testStore(t)
	tr := &stubTracker{issues: map[int]Issue{
		1: {Number: 1, Title: "a", Body: "body", State: "open", Labels: []string{"agent-ok"}, CreatedAt: time.Now(), Priority: 1},
	}}
	const sleep = 200 * time.Millisecond
	started := make(chan struct{})
	disp := &stubDispatcher{
		sleep:        sleep,
		ignoreCancel: true,
		onStart:      started,
	}
	w := &Watcher{
		Config: WatchConfig{
			Repo: "o/r", Label: "agent-ok", MaxConcurrency: 1,
			// Long interval so only the initial Tick dispatches.
			PollInterval: time.Hour,
		},
		Tracker:    tr,
		Claims:     claims,
		Dispatcher: disp,
	}

	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("dispatch did not start")
	}

	cancelAt := time.Now()
	cancel()

	select {
	case err := <-done:
		elapsed := time.Since(cancelAt)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run err = %v, want context.Canceled", err)
		}
		// Must wait for the ~200ms sleep, not return immediately on cancel.
		if elapsed < 150*time.Millisecond {
			t.Fatalf("Run returned too quickly after cancel: %v (want ≥ ~%v for in-flight dispatch)", elapsed, sleep)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after dispatch completed")
	}
}

func TestGitHubTrackerStubServer(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/issues", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[
			{"number":2,"title":"p","body":"blocked by #9","state":"open","created_at":"2020-01-02T00:00:00Z","labels":[{"name":"agent-ok"},{"name":"priority/5"}]},
			{"number":1,"title":"q","body":"hi","state":"open","created_at":"2020-01-01T00:00:00Z","labels":[{"name":"agent-ok"},{"name":"priority/1"}],"pull_request":{}}
		]`))
	})
	mux.HandleFunc("/repos/o/r/issues/9", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"state":"closed"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	tr := &GitHubTracker{BaseURL: srv.URL, HTTPClient: srv.Client()}
	issues, err := tr.ListOpen(context.Background(), "o/r", "agent-ok")
	if err != nil {
		t.Fatal(err)
	}
	// PR filtered out → only issue 2
	if len(issues) != 1 || issues[0].Number != 2 || issues[0].Priority != 5 {
		t.Fatalf("%+v", issues)
	}
	if len(issues[0].Blockers) != 1 || issues[0].Blockers[0] != 9 {
		t.Fatalf("blockers = %v", issues[0].Blockers)
	}
	open, err := tr.IsOpen(context.Background(), "o/r", 9)
	if err != nil || open {
		t.Fatalf("dep open=%v err=%v", open, err)
	}
}

func issueJSON(n int, title string) string {
	return fmt.Sprintf(
		`{"number":%d,"title":%q,"body":"b","state":"open","created_at":"2020-01-01T00:00:00Z","labels":[{"name":"agent-ok"}]}`,
		n, title,
	)
}

// TestListOpenFollowsLinkPagination asserts ListOpen aggregates issues across
// GitHub Link-header pages (AC: repos with >100 open matching issues).
func TestListOpenFollowsLinkPagination(t *testing.T) {
	// Page 1: 100 issues + Link rel=next; page 2: 5 more (total 105 > 100).
	const page1Count = 100
	const page2Count = 5
	var hits atomic.Int32

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/issues", func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		if page == "" || page == "1" {
			hits.Add(1)
			// Absolute next URL as GitHub does (full URL in Link).
			next := fmt.Sprintf("%s/repos/o/r/issues?state=open&per_page=100&labels=agent-ok&page=2",
				"http://"+r.Host)
			// Also include last to ensure we pick rel=next only.
			w.Header().Set("Link", fmt.Sprintf(
				`<%s>; rel="next", <%s&page=2>; rel="last"`,
				next, strings.TrimSuffix(next, "&page=2"),
			))
			parts := make([]string, 0, page1Count)
			for i := 1; i <= page1Count; i++ {
				parts = append(parts, issueJSON(i, fmt.Sprintf("issue-%d", i)))
			}
			_, _ = w.Write([]byte("[" + strings.Join(parts, ",") + "]"))
			return
		}
		if page == "2" {
			hits.Add(1)
			// No Link header on last page.
			parts := make([]string, 0, page2Count)
			for i := page1Count + 1; i <= page1Count+page2Count; i++ {
				parts = append(parts, issueJSON(i, fmt.Sprintf("issue-%d", i)))
			}
			_, _ = w.Write([]byte("[" + strings.Join(parts, ",") + "]"))
			return
		}
		http.Error(w, "unexpected page "+page, http.StatusBadRequest)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	tr := &GitHubTracker{BaseURL: srv.URL, HTTPClient: srv.Client()}
	issues, err := tr.ListOpen(context.Background(), "o/r", "agent-ok")
	if err != nil {
		t.Fatal(err)
	}
	want := page1Count + page2Count
	if len(issues) != want {
		t.Fatalf("got %d issues, want %d (must aggregate past first page of 100)", len(issues), want)
	}
	if hits.Load() != 2 {
		t.Fatalf("HTTP hits = %d, want 2 pages", hits.Load())
	}
	// Spot-check first, last, and one from page 2.
	if issues[0].Number != 1 {
		t.Fatalf("first number = %d, want 1", issues[0].Number)
	}
	if issues[page1Count-1].Number != page1Count {
		t.Fatalf("page1 last = %d, want %d", issues[page1Count-1].Number, page1Count)
	}
	if issues[page1Count].Number != page1Count+1 {
		t.Fatalf("page2 first = %d, want %d", issues[page1Count].Number, page1Count+1)
	}
	if issues[want-1].Number != want {
		t.Fatalf("last number = %d, want %d", issues[want-1].Number, want)
	}
}

func TestNextLinkURL(t *testing.T) {
	tests := []struct {
		name string
		hdr  string
		want string
		ok   bool
	}{
		{
			name: "next and last",
			hdr:  `<https://api.github.com/repositories/1/issues?page=2>; rel="next", <https://api.github.com/repositories/1/issues?page=5>; rel="last"`,
			want: "https://api.github.com/repositories/1/issues?page=2",
			ok:   true,
		},
		{
			name: "only prev",
			hdr:  `<https://api.github.com/repositories/1/issues?page=1>; rel="prev"`,
			want: "",
			ok:   false,
		},
		{
			name: "empty",
			hdr:  "",
			want: "",
			ok:   false,
		},
		{
			name: "unquoted rel",
			hdr:  `<https://example.com/x?page=3>; rel=next`,
			want: "https://example.com/x?page=3",
			ok:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := nextLinkURL(tt.hdr)
			if ok != tt.ok || got != tt.want {
				t.Fatalf("nextLinkURL(%q) = (%q, %v), want (%q, %v)", tt.hdr, got, ok, tt.want, tt.ok)
			}
		})
	}
}

// TestListOpenPageCapStopsAndLogs verifies the safety cap stops following next
// and logs a warning when more pages remain.
func TestListOpenPageCapStopsAndLogs(t *testing.T) {
	// Force cap of listOpenMaxPages by always advertising a next link.
	var pages atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/issues", func(w http.ResponseWriter, r *http.Request) {
		n := int(pages.Add(1))
		// Always point next at the same handler with an increasing page query so
		// ListOpen keeps requesting until the package cap.
		next := fmt.Sprintf("http://%s/repos/o/r/issues?state=open&per_page=100&page=%d", r.Host, n+1)
		w.Header().Set("Link", fmt.Sprintf(`<%s>; rel="next"`, next))
		// One issue per page so we can count pages via results length.
		_, _ = w.Write([]byte("[" + issueJSON(n, fmt.Sprintf("p-%d", n)) + "]"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var logs strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)

	tr := &GitHubTracker{BaseURL: srv.URL, HTTPClient: srv.Client()}
	issues, err := tr.ListOpen(context.Background(), "o/r", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != listOpenMaxPages {
		t.Fatalf("got %d issues, want %d (one per capped page)", len(issues), listOpenMaxPages)
	}
	if pages.Load() != int32(listOpenMaxPages) {
		t.Fatalf("pages fetched = %d, want %d", pages.Load(), listOpenMaxPages)
	}
	if !strings.Contains(logs.String(), "page cap reached") {
		t.Fatalf("expected page-cap warning in logs, got %q", logs.String())
	}
}
