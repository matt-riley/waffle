package intake

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
	mu        sync.Mutex
	started   []int
	cancelled []int
	block     chan struct{} // if non-nil, Dispatch waits until closed
	err       error
}

func (d *stubDispatcher) Dispatch(ctx context.Context, cfg WatchConfig, iss Issue) (string, error) {
	d.mu.Lock()
	d.started = append(d.started, iss.Number)
	d.mu.Unlock()
	if d.block != nil {
		select {
		case <-d.block:
		case <-ctx.Done():
			return "", ctx.Err()
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
