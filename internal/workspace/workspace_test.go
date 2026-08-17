package workspace

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/chat"
	"github.com/matt-riley/waffle/internal/hooks"
	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/repopolicy"
	"github.com/matt-riley/waffle/internal/sandbox"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/store"
	"github.com/matt-riley/waffle/internal/tool"
)

// scriptedBash records commands and returns scripted output, playing the
// container side of the queue without any container.
type scriptedBash struct {
	mu       sync.Mutex
	commands []string
	// outputs maps a substring of the command to its response.
	outputs map[string]string
	// failing maps a substring to an error message.
	failing map[string]string
	// delays maps a substring to a sleep that respects ctx (timeout tests).
	delays map[string]time.Duration
	// hostExecWouldPanic, when true, panics if Run is invoked — proves hooks
	// never fall back to host os/exec in these tests (they go through the queue).
	viaQueue bool
}

func (s *scriptedBash) Def() llm.Tool {
	return llm.Tool{Name: "bash", InputSchema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}}}`)}
}

func (s *scriptedBash) Run(ctx context.Context, input json.RawMessage) (string, error) {
	var in struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", err
	}
	s.mu.Lock()
	s.commands = append(s.commands, in.Command)
	s.viaQueue = true
	s.mu.Unlock()
	for k, d := range s.delays {
		if strings.Contains(in.Command, k) {
			select {
			case <-time.After(d):
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
	}
	for k, msg := range s.failing {
		if strings.Contains(in.Command, k) {
			return "", fmt.Errorf("%s", msg)
		}
	}
	for k, out := range s.outputs {
		if strings.Contains(in.Command, k) {
			return out, nil
		}
	}
	return "", nil
}

func (s *scriptedBash) ran(substr string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.commands {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

func (s *scriptedBash) count(substr string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	var count int
	for _, c := range s.commands {
		if strings.Contains(c, substr) {
			count++
		}
	}
	return count
}

// Test timing seams (#335): the manager's inspection probe and the runner's
// heartbeat/poll cadences are shortened from their production values (100ms
// probe, 2s heartbeat, 100ms poll) so idle-restore inspection tests drive the
// same state-machine transitions without paying seconds of wall clock per
// restart.
const (
	testRunnerHeartbeatInterval = 20 * time.Millisecond
	testRunnerPollInterval      = 5 * time.Millisecond
	testInspectionProbeInterval = 5 * time.Millisecond
)

// fakeRuntime runs an in-process Runner per "container" instead of docker.
type fakeRuntime struct {
	mu           sync.Mutex
	tools        *scriptedBash
	cancels      map[string]context.CancelFunc
	events       []string
	opts         []ContainerOpts
	startErr     error
	stopErr      error
	restartDelay time.Duration
	// failStartOn, when > 0, fails the Nth StartWorkspace call (1-indexed)
	// so tests can let the initial start succeed and a later restart fail.
	failStartOn int
	// removeFail makes RemoveContainer fail (docker rm error), so tests can
	// exercise the egress-restart retry path.
	removeFail bool
	// absent tracks containers removed since their last start, so
	// StartContainer can simulate Docker's "No such container" error.
	absent map[string]bool
	// done is closed when the named runner's Serve goroutine returns.
	done map[string]chan struct{}
}

type revocationTracker struct {
	mu       sync.Mutex
	sessions []string
}

func (r *revocationTracker) revoke(sessionID string) {
	r.mu.Lock()
	r.sessions = append(r.sessions, sessionID)
	r.mu.Unlock()
}

func (r *revocationTracker) seen(sessionID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, got := range r.sessions {
		if got == sessionID {
			return true
		}
	}
	return false
}

func newFakeRuntime(tools *scriptedBash) *fakeRuntime {
	return &fakeRuntime{tools: tools, cancels: map[string]context.CancelFunc{}, absent: map[string]bool{}, done: map[string]chan struct{}{}}
}

func (f *fakeRuntime) log(e string) {
	f.mu.Lock()
	f.events = append(f.events, e)
	f.mu.Unlock()
}

func (f *fakeRuntime) hasEventPrefix(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, event := range f.events {
		if strings.HasPrefix(event, prefix) {
			return true
		}
	}
	return false
}

func (f *fakeRuntime) StartWorkspace(ctx context.Context, opts ContainerOpts) error {
	f.mu.Lock()
	f.opts = append(f.opts, opts)
	startErr := f.startErr
	fail := f.failStartOn > 0 && len(f.opts) == f.failStartOn
	f.mu.Unlock()
	f.log("start-workspace " + opts.Name + " image=" + opts.Image)
	if startErr != nil || fail {
		return errors.New("docker unavailable")
	}
	f.launch(opts.Name, opts.QueueDir)
	return nil
}

func (f *fakeRuntime) launch(name, queueDir string) {
	rctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	f.mu.Lock()
	// StartContainer relaunches without halt. Cancel the previous Serve so
	// it cannot keep the queue DBs open after this name is reused.
	if prev := f.cancels[name]; prev != nil {
		prev()
	}
	f.cancels[name] = cancel
	f.done[name] = finished
	delay := f.restartDelay
	f.mu.Unlock()
	go func() {
		defer close(finished)
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-rctx.Done():
				return
			}
		}
		r := &sandbox.Runner{Tools: tool.NewRegistry(f.tools), HeartbeatInterval: testRunnerHeartbeatInterval, PollInterval: testRunnerPollInterval}
		_ = r.Serve(rctx, queueDir)
	}()
}

func (f *fakeRuntime) StopContainer(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.log("stop " + name)
	f.mu.Lock()
	err := f.stopErr
	f.mu.Unlock()
	if err != nil {
		return err
	}
	f.halt(name)
	return nil
}

func writeStaleRunnerHeartbeat(t *testing.T, queueDir string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(queueDir, "outbound.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	_, err = db.Exec(`INSERT OR REPLACE INTO results (request_id, content, is_error, created_at) VALUES (-1, 'alive', 0, ?)`, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
}

func (f *fakeRuntime) StartContainer(ctx context.Context, name string) error {
	f.log("restart " + name)
	f.mu.Lock()
	if f.startErr != nil {
		err := f.startErr
		f.mu.Unlock()
		return err
	}
	if f.absent[name] {
		delete(f.absent, name)
		f.mu.Unlock()
		return fmt.Errorf("docker start: exit status 1\nError response from daemon: No such container: %s", name)
	}
	var queueDir string
	for _, o := range f.opts {
		if o.Name == name {
			queueDir = o.QueueDir
		}
	}
	f.mu.Unlock()
	f.launch(name, queueDir)
	return nil
}

func TestDefaultImageIncludesGit(t *testing.T) {
	mgr, _ := newTestManager(t, &scriptedBash{})
	if mgr.DefaultImage != "buildpack-deps:bookworm-scm" {
		t.Fatalf("DefaultImage = %q, want an image containing Git", mgr.DefaultImage)
	}
}

func (f *fakeRuntime) RemoveContainer(ctx context.Context, name string) error {
	f.log("rm " + name)
	f.mu.Lock()
	removeFail := f.removeFail
	f.mu.Unlock()
	if removeFail {
		return fmt.Errorf("docker rm: exit status 1\nError response from daemon: could not remove container")
	}
	f.mu.Lock()
	f.absent[name] = true
	f.mu.Unlock()
	f.halt(name)
	return nil
}

func (f *fakeRuntime) RemoveVolume(ctx context.Context, name string) error {
	f.log("rmvol " + name)
	return nil
}

func (f *fakeRuntime) halt(name string) {
	f.mu.Lock()
	cancel := f.cancels[name]
	done := f.done[name]
	delete(f.cancels, name)
	delete(f.done, name)
	f.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done == nil {
		return
	}
	// Wait for Serve to close its queue DBs. A fixed sleep was both slow
	// (150ms per container remove) and still racy when Serve outlasted it.
	// Bound the wait: modernc sqlite can ignore a canceled QueryContext.
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
}

func (f *fakeRuntime) haltAll() {
	f.mu.Lock()
	names := make([]string, 0, len(f.cancels))
	for name := range f.cancels {
		names = append(names, name)
	}
	f.mu.Unlock()
	for _, name := range names {
		f.halt(name)
	}
}

func newTestManager(t *testing.T, tools *scriptedBash) (*Manager, *fakeRuntime) {
	t.Helper()
	t.Parallel()
	return newManagerFixture(t, tools)
}

// newSerialTestManager is for tests that inspect the process-wide
// workspaceLifecycleLocks registry and cannot run beside other lifecycle tests.
func newSerialTestManager(t *testing.T, tools *scriptedBash) (*Manager, *fakeRuntime) {
	t.Helper()
	return newManagerFixture(t, tools)
}

func newManagerFixture(t *testing.T, tools *scriptedBash) (*Manager, *fakeRuntime) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	rt := newFakeRuntime(tools)
	t.Cleanup(rt.haltAll)
	mgr := NewManager(st, session.New(st), rt, t.TempDir())
	mgr.InspectionProbeInterval = testInspectionProbeInterval
	mgr.ExecTimeout = 10 * time.Second
	mgr.MintToken = func(ctx context.Context, sessionID string) (string, error) { return "wk_test", nil }
	mgr.BrokerURL = "http://waffle-host:8421"
	return mgr, rt
}

func TestOpenClonesAndBindsSession(t *testing.T) {
	ctx := context.Background()
	tools := &scriptedBash{outputs: map[string]string{}}
	mgr, rt := newTestManager(t, tools)

	ws, client, err := mgr.Open(ctx, "matt-riley/waffle")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			t.Errorf("close client: %v", err)
		}
	}()

	if ws.Repo != "matt-riley/waffle" || ws.URL != "https://github.com/matt-riley/waffle.git" {
		t.Errorf("ws = %+v", ws)
	}
	if ws.SessionID == "" || ws.Status != StatusOpen {
		t.Errorf("ws = %+v", ws)
	}
	if !tools.ran("credential.helper '!waffle git-credential'") {
		t.Error("credential helper not configured")
	}
	// Without this git asks the helper for a host only, and the broker refuses
	// a credential whose path does not match the repo the session is bound to.
	if !tools.ran("credential.useHttpPath true") {
		t.Errorf("git would not send the repo path to the credential helper; commands = %v", tools.commands)
	}
	if !tools.ran("git clone -- 'https://github.com/matt-riley/waffle.git' /work/repo") {
		t.Errorf("clone not run; commands = %v", tools.commands)
	}
	// Broker env reached the container opts.
	if rt.opts[0].Token != "wk_test" || rt.opts[0].BrokerURL != "http://waffle-host:8421" {
		t.Errorf("opts = %+v", rt.opts[0])
	}

	// Re-opening the same repo resumes rather than duplicating.
	ws2, client2, err := mgr.Open(ctx, "matt-riley/waffle")
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer func() {
		if err := client2.Close(); err != nil {
			t.Errorf("close client: %v", err)
		}
	}()
	if ws2.ID != ws.ID {
		t.Errorf("second open made a new workspace: %s vs %s", ws2.ID, ws.ID)
	}
}

func TestTouchPersistsWorkspaceActivity(t *testing.T) {
	mgr, _ := newTestManager(t, &scriptedBash{})
	ws, client, err := mgr.Open(context.Background(), "matt-riley/waffle")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			t.Errorf("close client: %v", err)
		}
	}()
	before := ws.LastActive
	if err := mgr.Touch(context.Background(), ws.ID); err != nil {
		t.Fatal(err)
	}
	got, err := mgr.Get(context.Background(), ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastActive.IsZero() || got.LastActive.Equal(before) {
		t.Fatalf("last_active = %q, before %q", got.LastActive, before)
	}
}

func TestReaperIdlesOnlyStaleWorkspace(t *testing.T) {
	mgr, rt := newTestManager(t, &scriptedBash{})
	old, client, err := mgr.Open(context.Background(), "matt-riley/waffle")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("close client: %v", err)
	}
	if _, err := mgr.DB.Exec(`UPDATE workspaces SET last_active = ?`, time.Now().Add(-2*time.Hour).UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	r := &Reaper{Manager: mgr, IdleTimeout: time.Hour, Now: func() time.Time { return time.Now() }}
	if err := r.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := mgr.Get(context.Background(), old.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusIdle {
		t.Fatalf("status = %q, want idle", got.Status)
	}
	if !strings.Contains(strings.Join(rt.events, "\n"), "stop "+old.Container) {
		t.Fatalf("events = %v", rt.events)
	}
	resumed, resumedClient, err := mgr.Open(context.Background(), old.Repo)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := resumedClient.Close(); err != nil {
			t.Errorf("close client: %v", err)
		}
	}()
	if resumed.ID != old.ID || resumed.Status != StatusOpen {
		t.Fatalf("resume = %+v", resumed)
	}
}

func TestReaperLeavesRecentWorkspaceOpen(t *testing.T) {
	mgr, rt := newTestManager(t, &scriptedBash{})
	ws, client, err := mgr.Open(context.Background(), "matt-riley/recent")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("close client: %v", err)
	}
	r := &Reaper{Manager: mgr, IdleTimeout: time.Hour, Now: time.Now}
	if err := r.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := mgr.Get(context.Background(), ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusOpen || strings.Contains(strings.Join(rt.events, "\n"), "stop "+ws.Container) {
		t.Fatalf("status=%s events=%v", got.Status, rt.events)
	}
}

func TestReaperClosesCleanTTLWorkspace(t *testing.T) {
	mgr, _ := newTestManager(t, &scriptedBash{})
	ws, client, err := mgr.Open(context.Background(), "matt-riley/clean")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("close client: %v", err)
	}
	if _, err := mgr.DB.Exec(`UPDATE workspaces SET last_active = ?`, time.Now().Add(-48*time.Hour).UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	r := &Reaper{Manager: mgr, CloseTTL: time.Hour, Now: time.Now}
	if err := r.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := mgr.Get(context.Background(), ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusClosed {
		t.Fatalf("status=%s, want closed", got.Status)
	}
}

func TestReaperKeepsDirtyWorkspaceAndNotifies(t *testing.T) {
	tools := &scriptedBash{outputs: map[string]string{"git status --porcelain": " M important.txt"}}
	mgr, _ := newTestManager(t, tools)
	ws, client, err := mgr.Open(context.Background(), "matt-riley/waffle")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("close client: %v", err)
	}
	if _, err := mgr.DB.Exec(`UPDATE workspaces SET last_active = ?`, time.Now().Add(-2*time.Hour).UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	var notified string
	r := &Reaper{Manager: mgr, CloseTTL: time.Hour, Now: time.Now, Notify: func(_ context.Context, got Workspace, msg string) error { notified = got.ID + ":" + msg; return nil }}
	if err := r.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := mgr.Get(context.Background(), ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status == StatusClosed {
		t.Fatal("dirty workspace was closed")
	}
	if notified == "" {
		t.Fatal("dirty workspace did not notify")
	}
}

func TestReaperClosesCleanWorkspaceWithoutForce(t *testing.T) {
	mgr, _ := newTestManager(t, &scriptedBash{})
	ws, client, err := mgr.Open(context.Background(), "matt-riley/waffle")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("close client: %v", err)
	}
	if _, err := mgr.DB.Exec(`UPDATE workspaces SET last_active = ?`, time.Now().Add(-2*time.Hour).UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	r := &Reaper{Manager: mgr, CloseTTL: time.Hour, Now: time.Now}
	if err := r.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := mgr.Get(context.Background(), ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusClosed {
		t.Fatalf("status = %q, want closed", got.Status)
	}
}

func TestReaperKeepsDirtyTTLWorkspaceAndNotifies(t *testing.T) {
	tools := &scriptedBash{outputs: map[string]string{"git status --porcelain": " M important.txt"}}
	mgr, _ := newTestManager(t, tools)
	ws, client, err := mgr.Open(context.Background(), "matt-riley/waffle")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("close client: %v", err)
	}
	if _, err := mgr.DB.Exec(`UPDATE workspaces SET last_active = ?`, time.Now().Add(-48*time.Hour).UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	notified := false
	r := &Reaper{Manager: mgr, CloseTTL: time.Hour, Now: time.Now, Notify: func(_ context.Context, got Workspace, msg string) error {
		notified = got.ID == ws.ID && strings.Contains(msg, "kept")
		return nil
	}}
	if err := r.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := mgr.Get(context.Background(), ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status == StatusClosed || !notified {
		t.Fatalf("status=%s notified=%v", got.Status, notified)
	}
}

// fakeSweepManager records Idle/Close calls and fails a configured workspace id (#110).
type fakeSweepManager struct {
	items      []Workspace
	failIdle   string
	idleCalls  []string
	closeCalls []string
}

func (f *fakeSweepManager) List(context.Context) ([]Workspace, error) {
	out := make([]Workspace, len(f.items))
	copy(out, f.items)
	return out, nil
}

func (f *fakeSweepManager) Idle(_ context.Context, id string) error {
	f.idleCalls = append(f.idleCalls, id)
	if id == f.failIdle {
		return fmt.Errorf("simulated idle failure for %s", id)
	}
	// Reflect idle status for subsequent close checks in the same pass.
	for i := range f.items {
		if f.items[i].ID == id {
			f.items[i].Status = StatusIdle
		}
	}
	return nil
}

func (f *fakeSweepManager) Close(_ context.Context, id string, _ bool) (*CloseReport, error) {
	f.closeCalls = append(f.closeCalls, id)
	for i := range f.items {
		if f.items[i].ID == id {
			f.items[i].Status = StatusClosed
		}
	}
	return nil, nil
}

// TestReaperSweepContinuesAfterIdleError: middle workspace Idle fails; first
// and third still get Idle; Sweep returns a joined error naming the failed id (#110).
func TestReaperSweepContinuesAfterIdleError(t *testing.T) {
	stale := time.Now().Add(-2 * time.Hour).UTC()
	fake := &fakeSweepManager{
		failIdle: "ws-mid",
		items: []Workspace{
			{ID: "ws-first", Repo: "o/a", Status: StatusOpen, LastActive: stale, UpdatedAt: stale},
			{ID: "ws-mid", Repo: "o/b", Status: StatusOpen, LastActive: stale, UpdatedAt: stale},
			{ID: "ws-third", Repo: "o/c", Status: StatusOpen, LastActive: stale, UpdatedAt: stale},
		},
	}
	r := &Reaper{
		Manager:     fake,
		IdleTimeout: time.Hour,
		Now:         time.Now,
	}
	err := r.Sweep(context.Background())
	if err == nil {
		t.Fatal("Sweep should return joined error when middle Idle fails")
	}
	if !strings.Contains(err.Error(), "ws-mid") {
		t.Fatalf("joined error should mention failed id: %v", err)
	}
	if !strings.Contains(err.Error(), "idle workspace") {
		t.Fatalf("joined error should mention idle: %v", err)
	}
	wantIdle := []string{"ws-first", "ws-mid", "ws-third"}
	if len(fake.idleCalls) != 3 {
		t.Fatalf("idle calls = %v, want all three workspaces", fake.idleCalls)
	}
	for i, id := range wantIdle {
		if fake.idleCalls[i] != id {
			t.Fatalf("idleCalls[%d]=%q, want %q (full=%v)", i, fake.idleCalls[i], id, fake.idleCalls)
		}
	}
	// First and third should have been marked idle despite mid failure.
	for _, id := range []string{"ws-first", "ws-third"} {
		found := false
		for _, ws := range fake.items {
			if ws.ID == id {
				found = true
				if ws.Status != StatusIdle {
					t.Errorf("%s status=%q, want idle", id, ws.Status)
				}
			}
		}
		if !found {
			t.Errorf("workspace %s missing from fake items", id)
		}
	}
	// Mid remains open because Idle failed.
	for _, ws := range fake.items {
		if ws.ID == "ws-mid" && ws.Status != StatusOpen {
			t.Errorf("ws-mid status=%q, want open (Idle failed)", ws.Status)
		}
	}
}

// TestReaperSweepContinuesAfterNotifyError: notify failure on a dirty workspace
// is joined and does not abort the rest of the pass (#110).
func TestReaperSweepContinuesAfterNotifyError(t *testing.T) {
	stale := time.Now().Add(-48 * time.Hour).UTC()
	fake := &fakeSweepManager{
		items: []Workspace{
			{ID: "ws-a", Repo: "o/a", Status: StatusIdle, LastActive: stale, UpdatedAt: stale},
			{ID: "ws-b", Repo: "o/b", Status: StatusIdle, LastActive: stale, UpdatedAt: stale},
		},
	}
	// Override Close to report dirty for first workspace only.
	closing := &dirtyFirstCloser{inner: fake, dirtyID: "ws-a"}
	var notified []string
	r := &Reaper{
		Manager:  closing,
		CloseTTL: time.Hour,
		Now:      time.Now,
		Notify: func(_ context.Context, got Workspace, msg string) error {
			notified = append(notified, got.ID)
			return fmt.Errorf("notify transport down")
		},
	}
	err := r.Sweep(context.Background())
	if err == nil {
		t.Fatal("Sweep should return joined error when notify fails")
	}
	if !strings.Contains(err.Error(), "ws-a") {
		t.Fatalf("error should mention notify target: %v", err)
	}
	// Second workspace still closed.
	if len(closing.inner.closeCalls) < 2 {
		t.Fatalf("close calls = %v, want both workspaces processed", closing.inner.closeCalls)
	}
	if len(notified) == 0 || notified[0] != "ws-a" {
		t.Fatalf("notified = %v", notified)
	}
	for _, ws := range fake.items {
		if ws.ID == "ws-b" && ws.Status != StatusClosed {
			t.Errorf("ws-b status=%q, want closed after notify failure on ws-a", ws.Status)
		}
	}
}

// dirtyFirstCloser wraps a SweepManager so Close on dirtyID returns a dirty report.
type dirtyFirstCloser struct {
	inner   *fakeSweepManager
	dirtyID string
}

func (d *dirtyFirstCloser) List(ctx context.Context) ([]Workspace, error) {
	return d.inner.List(ctx)
}

func (d *dirtyFirstCloser) Idle(ctx context.Context, id string) error {
	return d.inner.Idle(ctx, id)
}

func (d *dirtyFirstCloser) Close(ctx context.Context, id string, force bool) (*CloseReport, error) {
	if id == d.dirtyID {
		d.inner.closeCalls = append(d.inner.closeCalls, id)
		return &CloseReport{Dirty: " M file.txt"}, fmt.Errorf("workspace %s has unsaved work", id)
	}
	return d.inner.Close(ctx, id, force)
}

func TestOpenRefreshesBrokerTokenForExistingWorkspace(t *testing.T) {
	ctx := context.Background()
	mgr, rt := newTestManager(t, &scriptedBash{})
	token := "wk_first"
	mgr.MintToken = func(context.Context, string) (string, error) { return token, nil }

	ws, client, err := mgr.Open(ctx, "matt-riley/waffle")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("close client: %v", err)
	}

	token = "wk_second"
	resumed, client, err := mgr.Open(ctx, "matt-riley/waffle")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			t.Errorf("close client: %v", err)
		}
	}()
	if resumed.ID != ws.ID {
		t.Fatalf("reopen created workspace %s, want %s", resumed.ID, ws.ID)
	}
	rt.mu.Lock()
	opts := append([]ContainerOpts(nil), rt.opts...)
	rt.mu.Unlock()
	if len(opts) < 2 || opts[len(opts)-1].Token != "wk_second" {
		t.Fatalf("container starts = %+v, want refreshed broker token", opts)
	}
}

func TestOpenAdoptsDevcontainerImage(t *testing.T) {
	ctx := context.Background()
	tools := &scriptedBash{outputs: map[string]string{
		"devcontainer.json": `{"image": "golang:1.25"}`,
	}}
	mgr, rt := newTestManager(t, tools)

	ws, client, err := mgr.Open(ctx, "matt-riley/waffle")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			t.Errorf("close client: %v", err)
		}
	}()

	if ws.Image != "golang:1.25" {
		t.Errorf("image = %q, want devcontainer image", ws.Image)
	}
	// Two container starts: default image, then the adopted one.
	if len(rt.opts) != 2 || rt.opts[1].Image != "golang:1.25" {
		t.Errorf("container starts = %+v", rt.opts)
	}
	// Volume is shared across the restart.
	if rt.opts[0].Volume != rt.opts[1].Volume {
		t.Error("volume changed during adoption")
	}
}

func TestCloneFailureCleansUp(t *testing.T) {
	ctx := context.Background()
	tools := &scriptedBash{failing: map[string]string{"git clone": "fatal: repository not found"}}
	mgr, rt := newTestManager(t, tools)

	_, _, err := mgr.Open(ctx, "matt-riley/nope")
	if err == nil || !strings.Contains(err.Error(), "repository not found") {
		t.Fatalf("err = %v", err)
	}
	joined := strings.Join(rt.events, "\n")
	if !strings.Contains(joined, "rm waffle-ws-") || !strings.Contains(joined, "rmvol waffle-ws-") {
		t.Errorf("no cleanup after failed clone:\n%s", joined)
	}
	if list, _ := mgr.List(ctx); len(list) != 0 {
		t.Errorf("failed workspace persisted: %v", list)
	}
}

func TestIdleResumeCycle(t *testing.T) {
	ctx := context.Background()
	tools := &scriptedBash{}
	mgr, _ := newTestManager(t, tools)

	ws, client, err := mgr.Open(ctx, "matt-riley/waffle")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("close client: %v", err)
	}

	if err := mgr.Idle(ctx, ws.ID); err != nil {
		t.Fatalf("Idle: %v", err)
	}
	got, _ := mgr.Get(ctx, ws.ID)
	if got.Status != StatusIdle {
		t.Errorf("status = %s", got.Status)
	}

	resumed, client2, err := mgr.Resume(ctx, ws.ID)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	defer func() {
		if err := client2.Close(); err != nil {
			t.Errorf("close client: %v", err)
		}
	}()
	if resumed.Status != StatusOpen {
		t.Errorf("resumed status = %s", resumed.Status)
	}
	// The queue still works after resume.
	if err := mgr.bash(ctx, client2, "true"); err != nil {
		t.Errorf("post-resume exec: %v", err)
	}
}

func TestCloseRefusesUnpushedWork(t *testing.T) {
	ctx := context.Background()
	tools := &scriptedBash{outputs: map[string]string{
		"git status --porcelain": " M main.go",
		"git log --oneline":      "abc123 wip",
	}}
	mgr, _ := newTestManager(t, tools)

	ws, client, err := mgr.Open(ctx, "matt-riley/waffle")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("close client: %v", err)
	}

	report, err := mgr.Close(ctx, ws.ID, false)
	if err == nil {
		t.Fatal("Close succeeded despite dirty tree")
	}
	if report.Dirty == "" || report.Unpushed == "" {
		t.Errorf("report = %+v", report)
	}
	if got, _ := mgr.Get(ctx, ws.ID); got.Status == StatusClosed {
		t.Error("workspace closed despite refusal")
	}

	// Force closes anyway.
	if _, err := mgr.Close(ctx, ws.ID, true); err != nil {
		t.Fatalf("forced Close: %v", err)
	}
	if got, _ := mgr.Get(ctx, ws.ID); got.Status != StatusClosed {
		t.Errorf("status = %s", got.Status)
	}
}

func TestInspectCloseReportsDirtyAndUnpushedWithoutTeardown(t *testing.T) {
	ctx := context.Background()
	tools := &scriptedBash{outputs: map[string]string{
		"git status --porcelain": " M main.go",
		"git log --oneline":      "abc123 wip",
	}}
	mgr, rt := newTestManager(t, tools)
	var minted, revoked int
	mgr.MintToken = func(context.Context, string) (string, error) {
		minted++
		return "wk_test", nil
	}
	mgr.RevokeSession = func(string) { revoked++ }

	ws, client, err := mgr.Open(ctx, "matt-riley/waffle")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	beforeEvents := len(rt.events)
	beforeMinted := minted

	report, err := mgr.InspectClose(ctx, ws.ID)
	if err != nil {
		t.Fatalf("InspectClose: %v", err)
	}
	if report.Dirty != "M main.go" || report.Unpushed != "abc123 wip" {
		t.Fatalf("report = %+v", report)
	}
	if minted != beforeMinted || revoked != 0 {
		t.Fatalf("inspection changed credentials: minted=%d revoked=%d", minted, revoked)
	}
	if events := strings.Join(rt.events[beforeEvents:], "\n"); strings.Contains(events, "rm ") || strings.Contains(events, "rmvol ") {
		t.Fatalf("inspection tore down workspace: %s", events)
	}
	got, err := mgr.Get(ctx, ws.ID)
	if err != nil || got.Status != StatusOpen {
		t.Fatalf("workspace after inspection = %+v, %v", got, err)
	}
}

func TestInspectCloseOpenWorkspaceDoesNotTouchLifecycleState(t *testing.T) {
	ctx := context.Background()
	mgr, rt := newTestManager(t, &scriptedBash{})
	ws, client, err := mgr.Open(ctx, "matt-riley/waffle")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := mgr.Get(ctx, ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeEvents := len(rt.events)

	if _, err := mgr.InspectClose(ctx, ws.ID); err != nil {
		t.Fatalf("InspectClose: %v", err)
	}
	got, err := mgr.Get(ctx, ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != before.Status || !got.UpdatedAt.Equal(before.UpdatedAt) || !got.LastActive.Equal(before.LastActive) {
		t.Fatalf("inspection changed lifecycle state: before=%+v after=%+v", before, got)
	}
	if events := rt.events[beforeEvents:]; len(events) != 0 {
		t.Fatalf("open inspection changed runtime state: %v", events)
	}
}

func TestInspectCloseRestoresIdleWorkspaceWithoutRecreatingContainer(t *testing.T) {
	ctx := context.Background()
	tools := &scriptedBash{outputs: map[string]string{
		"git status --porcelain": " M main.go",
		"git log --oneline":      "abc123 wip",
	}}
	mgr, rt := newTestManager(t, tools)
	var minted, revoked int
	mgr.MintToken = func(context.Context, string) (string, error) {
		minted++
		return "wk_test", nil
	}
	mgr.RevokeSession = func(string) { revoked++ }
	ws, client, err := mgr.Open(ctx, "matt-riley/waffle")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Idle(ctx, ws.ID); err != nil {
		t.Fatal(err)
	}
	before, err := mgr.Get(ctx, ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeEvents, beforeMinted, beforeRevoked := len(rt.events), minted, revoked

	report, err := mgr.InspectClose(ctx, ws.ID)
	if err != nil {
		t.Fatalf("InspectClose: %v", err)
	}
	if report.Dirty != "M main.go" || report.Unpushed != "abc123 wip" {
		t.Fatalf("report = %+v", report)
	}
	got, err := mgr.Get(ctx, ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusIdle || !got.UpdatedAt.Equal(before.UpdatedAt) || !got.LastActive.Equal(before.LastActive) {
		t.Fatalf("inspection changed idle lifecycle state: before=%+v after=%+v", before, got)
	}
	if minted != beforeMinted || revoked != beforeRevoked {
		t.Fatalf("inspection changed credentials: minted=%d revoked=%d", minted, revoked)
	}
	events := rt.events[beforeEvents:]
	want := []string{"restart " + ws.Container, "stop " + ws.Container}
	if strings.Join(events, "\n") != strings.Join(want, "\n") {
		t.Fatalf("idle inspection events = %v, want %v", events, want)
	}
}

func TestInspectCloseWaitsForFreshIdleRunnerHeartbeat(t *testing.T) {
	ctx := context.Background()
	mgr, rt := newTestManager(t, &scriptedBash{})
	ws, client, err := mgr.Open(ctx, "matt-riley/waffle")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Idle(ctx, ws.ID); err != nil {
		t.Fatal(err)
	}
	writeStaleRunnerHeartbeat(t, mgr.queueDir(ws.ID))
	rt.mu.Lock()
	rt.restartDelay = 300 * time.Millisecond
	rt.mu.Unlock()
	before, err := mgr.Get(ctx, ws.ID)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := mgr.InspectClose(ctx, ws.ID); err != nil {
		t.Fatalf("InspectClose with stale heartbeat: %v", err)
	}
	got, err := mgr.Get(ctx, ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusIdle || !got.UpdatedAt.Equal(before.UpdatedAt) || !got.LastActive.Equal(before.LastActive) {
		t.Fatalf("inspection changed idle lifecycle state: before=%+v after=%+v", before, got)
	}
}

func TestWaitForInspectionHeartbeatAllowsSupportedColdStart(t *testing.T) {
	t.Parallel()
	if inspectionRunnerReadyTimeout != time.Minute {
		t.Fatalf("inspection runner readiness timeout = %s, want sandbox cold-start allowance %s", inspectionRunnerReadyTimeout, time.Minute)
	}
	startedAt := time.Date(2026, time.July, 23, 22, 30, 0, 0, time.UTC)
	ticks := make(chan time.Time, 1)
	ticks <- startedAt.Add(16 * time.Second)
	queries := 0
	err := waitForInspectionHeartbeat(context.Background(), startedAt, inspectionRunnerReadyTimeout,
		func(context.Context) (time.Time, error) {
			queries++
			if queries == 1 {
				return time.Time{}, nil
			}
			return startedAt.Add(16 * time.Second), nil
		}, ticks)
	if err != nil {
		t.Fatalf("wait for heartbeat arriving inside the supported window: %v", err)
	}
}

func TestInspectCloseIdleRestoresWithCanceledCaller(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tools := &scriptedBash{delays: map[string]time.Duration{"git status --porcelain": 10 * time.Second}}
	mgr, rt := newTestManager(t, tools)
	ws, client, err := mgr.Open(context.Background(), "matt-riley/waffle")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Idle(context.Background(), ws.ID); err != nil {
		t.Fatal(err)
	}
	before, err := mgr.Get(context.Background(), ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeEvents := len(rt.events)
	result := make(chan error, 1)
	go func() {
		_, err := mgr.InspectClose(ctx, ws.ID)
		result <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !tools.ran("git status --porcelain") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !tools.ran("git status --porcelain") {
		t.Fatal("inspection command never started")
	}
	cancel()
	if err := <-result; err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("InspectClose error = %v, want canceled inspection", err)
	}
	got, err := mgr.Get(context.Background(), ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusIdle || !got.UpdatedAt.Equal(before.UpdatedAt) || !got.LastActive.Equal(before.LastActive) {
		t.Fatalf("canceled inspection changed idle lifecycle state: before=%+v after=%+v", before, got)
	}
	events := rt.events[beforeEvents:]
	want := []string{"restart " + ws.Container, "stop " + ws.Container}
	if strings.Join(events, "\n") != strings.Join(want, "\n") {
		t.Fatalf("canceled inspection events = %v, want %v", events, want)
	}
}

func TestInspectCloseIdleRestorationFailureRefusesWithoutTeardown(t *testing.T) {
	ctx := context.Background()
	tools := &scriptedBash{outputs: map[string]string{"git status --porcelain": " M main.go"}}
	mgr, rt := newTestManager(t, tools)
	ws, client, err := mgr.Open(ctx, "matt-riley/waffle")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Idle(ctx, ws.ID); err != nil {
		t.Fatal(err)
	}
	rt.mu.Lock()
	rt.stopErr = errors.New("restore failed")
	rt.mu.Unlock()
	beforeEvents := len(rt.events)

	report, err := mgr.InspectClose(ctx, ws.ID)
	if err == nil || !strings.Contains(err.Error(), "restore idle") {
		t.Fatalf("InspectClose error = %v, want restoration failure", err)
	}
	if report == nil || report.Dirty != "M main.go" {
		t.Fatalf("inspection evidence lost: %+v", report)
	}
	if events := strings.Join(rt.events[beforeEvents:], "\n"); strings.Contains(events, "rm ") || strings.Contains(events, "rmvol ") {
		t.Fatalf("restoration failure tore down workspace: %s", events)
	}
	got, getErr := mgr.Get(ctx, ws.ID)
	if getErr != nil || got.Status == StatusClosed {
		t.Fatalf("workspace after failed inspection = %+v, %v", got, getErr)
	}
}

func TestCloseWithoutForceReusesInspectClose(t *testing.T) {
	ctx := context.Background()
	tools := &scriptedBash{outputs: map[string]string{
		"git status --porcelain": " M main.go",
		"git log --oneline":      "abc123 wip",
	}}
	mgr, rt := newTestManager(t, tools)
	ws, client, err := mgr.Open(ctx, "matt-riley/waffle")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	beforeEvents := len(rt.events)

	report, err := mgr.Close(ctx, ws.ID, false)
	if err == nil || !strings.Contains(err.Error(), "unsaved work") {
		t.Fatalf("Close error = %v, want refusal", err)
	}
	if report.Dirty != "M main.go" || report.Unpushed != "abc123 wip" {
		t.Fatalf("report = %+v", report)
	}
	if tools.count("git status --porcelain") != 1 || tools.count("git log --oneline") != 1 {
		t.Fatalf("Close did not perform one shared inspection: commands=%v", tools.commands)
	}
	if events := strings.Join(rt.events[beforeEvents:], "\n"); strings.Contains(events, "rm ") || strings.Contains(events, "rmvol ") {
		t.Fatalf("refused close tore down workspace: %s", events)
	}
}

func TestCloseIdleCleanStartsContainerOnce(t *testing.T) {
	ctx := context.Background()
	tools := &scriptedBash{outputs: map[string]string{
		"git status --porcelain": "",
		"git log --oneline":      "",
	}}
	mgr, rt := newTestManager(t, tools)
	// Disable credential refresh so resume would only StartContainer — the
	// regression is a second start after inspection (#148).
	mgr.MintToken = nil
	ws, client, err := mgr.Open(ctx, "matt-riley/waffle")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Idle(ctx, ws.ID); err != nil {
		t.Fatal(err)
	}
	beforeEvents := len(rt.events)

	report, transitioned, err := mgr.CloseTransition(ctx, ws.ID, false)
	if err != nil {
		t.Fatalf("CloseTransition: %v", err)
	}
	if !transitioned || report == nil {
		t.Fatalf("CloseTransition = %+v transitioned=%v", report, transitioned)
	}
	events := rt.events[beforeEvents:]
	var restarts int
	for _, event := range events {
		if strings.HasPrefix(event, "restart ") {
			restarts++
		}
		if strings.HasPrefix(event, "start-workspace ") {
			t.Fatalf("idle clean close recreated workspace: %v", events)
		}
	}
	if restarts != 1 {
		t.Fatalf("idle clean close start count = %d, want 1; events=%v", restarts, events)
	}
	if !strings.Contains(strings.Join(events, "\n"), "rm "+ws.Container) {
		t.Fatalf("close did not remove container: %v", events)
	}
}

func TestInspectCloseClosedWorkspaceIsExplicit(t *testing.T) {
	ctx := context.Background()
	mgr, _ := newTestManager(t, &scriptedBash{})
	ws, client, err := mgr.Open(ctx, "matt-riley/waffle")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Close(ctx, ws.ID, true); err != nil {
		t.Fatal(err)
	}
	report, err := mgr.InspectClose(ctx, ws.ID)
	if !errors.Is(err, ErrWorkspaceAlreadyClosed) || report == nil || report.Dirty != "" || report.Unpushed != "" {
		t.Fatalf("InspectClose(closed) = %+v, %v", report, err)
	}
}

func TestLifecycleLockRegistryRetiresQuiescentKeys(t *testing.T) {
	t.Parallel()
	var registry lifecycleLockRegistry
	for i := range 256 {
		lock := registry.lock(fmt.Sprintf("quiescent-%d", i))
		lock.Lock()
		runtime.Gosched()
		lock.Unlock()
	}
	if got := lifecycleRegistrySize(&registry); got != 0 {
		t.Fatalf("quiescent registry entries = %d, want 0", got)
	}
}

func TestLifecycleLockRegistryRetiresFailedWorkspaceCalls(t *testing.T) {
	ctx := context.Background()
	mgr, _ := newSerialTestManager(t, &scriptedBash{})

	for i := range 64 {
		id := fmt.Sprintf("missing-%d", i)
		if _, err := mgr.InspectClose(ctx, id); !errors.Is(err, ErrWorkspaceNotFound) {
			t.Fatalf("InspectClose(%q) error = %v", id, err)
		}
		if _, _, err := mgr.CloseTransition(ctx, id, false); !errors.Is(err, ErrWorkspaceNotFound) {
			t.Fatalf("CloseTransition(%q) error = %v", id, err)
		}
		if err := mgr.Idle(ctx, id); !errors.Is(err, ErrWorkspaceNotFound) {
			t.Fatalf("Idle(%q) error = %v", id, err)
		}
		if _, _, err := mgr.Resume(ctx, id); !errors.Is(err, ErrWorkspaceNotFound) {
			t.Fatalf("Resume(%q) error = %v", id, err)
		}
	}
	if got := lifecycleRegistrySize(&workspaceLifecycleLocks); got != 0 {
		t.Fatalf("failed lifecycle calls retained %d registry entries", got)
	}
}

func TestLifecycleLockRegistryKeepsOneEntryThroughContendedHandoff(t *testing.T) {
	t.Parallel()
	var registry lifecycleLockRegistry
	const key = "contended"

	holder := registry.lock(key)
	holder.Lock()
	waiter := registry.lock(key)
	if holder.entry != waiter.entry {
		t.Fatal("contended callers received different keyed mutexes")
	}

	waiterAcquired := make(chan struct{})
	releaseWaiter := make(chan struct{})
	waiterDone := make(chan struct{})
	go func() {
		waiter.Lock()
		close(waiterAcquired)
		<-releaseWaiter
		waiter.Unlock()
		close(waiterDone)
	}()

	holder.Unlock()
	<-waiterAcquired
	if got := lifecycleRegistryRefs(&registry, key); got != 1 {
		t.Fatalf("registry refs during handoff = %d, want 1", got)
	}
	if entry := lifecycleRegistryEntry(&registry, key); entry != waiter.entry {
		t.Fatal("registry deleted or replaced a lock during handoff")
	}

	close(releaseWaiter)
	<-waiterDone
	if got := lifecycleRegistrySize(&registry); got != 0 {
		t.Fatalf("registry entries after handoff = %d, want 0", got)
	}
}

func TestLifecycleLockRegistryConcurrentStressRetiresAllKeys(t *testing.T) {
	t.Parallel()
	var registry lifecycleLockRegistry
	const (
		workers = 24
		keys    = 11
		rounds  = 200
	)

	var activeMu sync.Mutex
	active := make(map[string]bool)
	errs := make(chan string, workers*rounds)
	var wg sync.WaitGroup
	for worker := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for round := range rounds {
				key := fmt.Sprintf("stress-%d", (worker+round)%keys)
				lock := registry.lock(key)
				lock.Lock()
				activeMu.Lock()
				if active[key] {
					errs <- key
				}
				active[key] = true
				activeMu.Unlock()
				runtime.Gosched()
				activeMu.Lock()
				active[key] = false
				activeMu.Unlock()
				lock.Unlock()
			}
		}()
	}
	wg.Wait()
	close(errs)
	for key := range errs {
		t.Errorf("concurrent critical sections for key %q", key)
	}
	if got := lifecycleRegistrySize(&registry); got != 0 {
		t.Fatalf("stress registry entries = %d, want 0", got)
	}
}

func TestInspectCloseGuardedSerializesPreviewAcceptanceAgainstClose(t *testing.T) {
	ctx := context.Background()
	mgr, rt := newSerialTestManager(t, &scriptedBash{})
	ws, client, err := mgr.Open(ctx, "matt-riley/waffle")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}

	acceptStarted := make(chan struct{})
	releaseAccept := make(chan struct{})
	inspectDone := make(chan error, 1)
	go func() {
		_, err := mgr.InspectCloseGuarded(ctx, ws.ID, func(report *CloseReport) error {
			if report.Dirty != "" || report.Unpushed != "" {
				return errors.New("unexpected close evidence")
			}
			close(acceptStarted)
			<-releaseAccept
			return nil
		})
		inspectDone <- err
	}()
	<-acceptStarted

	closer := &Manager{
		DB:           mgr.DB,
		Sessions:     mgr.Sessions,
		Runtime:      mgr.Runtime,
		QueueRoot:    mgr.QueueRoot,
		DefaultImage: mgr.DefaultImage,
		Network:      mgr.Network,
		ExecTimeout:  mgr.ExecTimeout,
		MintToken:    mgr.MintToken,
		BrokerURL:    mgr.BrokerURL,
	}
	type closeResult struct {
		transitioned bool
		err          error
	}
	closeDone := make(chan closeResult, 1)
	go func() {
		_, transitioned, err := closer.CloseTransition(ctx, ws.ID, true)
		closeDone <- closeResult{transitioned: transitioned, err: err}
	}()
	waitForLifecycleRegistryRefs(t, &workspaceLifecycleLocks, ws.ID, 2)
	if rt.hasEventPrefix("rm ") {
		t.Fatal("CloseTransition from a second manager escaped guarded preview")
	}

	close(releaseAccept)
	if err := <-inspectDone; err != nil {
		t.Fatalf("InspectCloseGuarded: %v", err)
	}
	result := <-closeDone
	if result.err != nil || !result.transitioned {
		t.Fatalf("CloseTransition after preview = %+v", result)
	}
	if got := lifecycleRegistryRefs(&workspaceLifecycleLocks, ws.ID); got != 0 {
		t.Fatalf("workspace lifecycle refs after close = %d, want 0", got)
	}
}

func TestCloseTransitionReportsExactlyOneConcurrentTransition(t *testing.T) {
	ctx := context.Background()
	mgr, _ := newTestManager(t, &scriptedBash{})
	ws, client, err := mgr.Open(ctx, "matt-riley/waffle")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	type result struct {
		transitioned bool
		err          error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() {
			<-start
			_, transitioned, err := mgr.CloseTransition(ctx, ws.ID, false)
			results <- result{transitioned: transitioned, err: err}
		}()
	}
	close(start)

	var transitioned int
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatalf("CloseTransition: %v", result.err)
		}
		if result.transitioned {
			transitioned++
		}
	}
	if transitioned != 1 {
		t.Fatalf("concurrent transitioned count = %d, want one", transitioned)
	}
	if report, err := mgr.Close(ctx, ws.ID, false); err != nil || report != nil {
		t.Fatalf("backward-compatible Close(closed) = %+v, %v", report, err)
	}
}

func lifecycleRegistrySize(registry *lifecycleLockRegistry) int {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	return len(registry.entries)
}

func lifecycleRegistryRefs(registry *lifecycleLockRegistry, key string) int {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if entry := registry.entries[key]; entry != nil {
		return entry.refs
	}
	return 0
}

func lifecycleRegistryEntry(registry *lifecycleLockRegistry, key string) *lifecycleLockEntry {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	return registry.entries[key]
}

func waitForLifecycleRegistryRefs(
	t *testing.T,
	registry *lifecycleLockRegistry,
	key string,
	want int,
) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		if got := lifecycleRegistryRefs(registry, key); got == want {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("lifecycle registry refs for %q did not reach %d", key, want)
		default:
			runtime.Gosched()
		}
	}
}

func TestCloseRefusesCommitsWithoutUpstream(t *testing.T) {
	ctx := context.Background()
	tools := &scriptedBash{outputs: map[string]string{
		"git log --oneline HEAD --not --remotes": "abc123 local commit",
	}}
	mgr, rt := newTestManager(t, tools)

	ws, client, err := mgr.Open(ctx, "matt-riley/waffle")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("close client: %v", err)
	}

	report, err := mgr.Close(ctx, ws.ID, false)
	if err == nil {
		t.Fatal("Close succeeded despite a commit not present on any remote")
	}
	if report.Unpushed != "abc123 local commit" {
		t.Fatalf("Unpushed = %q", report.Unpushed)
	}
	if strings.Contains(strings.Join(rt.events, "\n"), "rmvol ") {
		t.Fatal("workspace volume removed despite unpushed work")
	}
}

func TestCloseAbortsWhenIdleWorkspaceCannotResume(t *testing.T) {
	ctx := context.Background()
	mgr, rt := newTestManager(t, &scriptedBash{})

	ws, client, err := mgr.Open(ctx, "matt-riley/waffle")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("close client: %v", err)
	}
	if err := mgr.Idle(ctx, ws.ID); err != nil {
		t.Fatal(err)
	}
	rt.mu.Lock()
	rt.startErr = errors.New("docker unavailable")
	rt.mu.Unlock()

	if _, err := mgr.Close(ctx, ws.ID, false); err == nil || !strings.Contains(err.Error(), "docker unavailable") {
		t.Fatalf("Close error = %v, want resume failure", err)
	}
	if strings.Contains(strings.Join(rt.events, "\n"), "rmvol ") {
		t.Fatal("workspace volume removed after resume failure")
	}
	if got, err := mgr.Get(ctx, ws.ID); err != nil || got.Status == StatusClosed {
		t.Fatalf("workspace after failed close = %+v, %v", got, err)
	}
}

// TestCloseRestoresIdleOnRefusal exercises the restore-to-idle path for
// an originally-idle workspace when safety check refuses (addresses review
// feedback that this new behavior lacked coverage).
func TestCloseRestoresIdleOnRefusal(t *testing.T) {
	ctx := context.Background()
	mgr, _ := newTestManager(t, &scriptedBash{outputs: map[string]string{
		"cd /work/repo && git status --porcelain": " M file\n",
	}})

	ws, client, err := mgr.Open(ctx, "matt-riley/waffle")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("close client: %v", err)
	}
	if err := mgr.Idle(ctx, ws.ID); err != nil {
		t.Fatal(err)
	}

	// Should refuse and restore to idle (not leave open)
	_, err = mgr.Close(ctx, ws.ID, false)
	if err == nil || !strings.Contains(err.Error(), "unsaved work") {
		t.Fatalf("expected refusal, got %v", err)
	}
	got, err := mgr.Get(ctx, ws.ID)
	if err != nil || got.Status != StatusIdle {
		t.Fatalf("after refuse for idle ws, status=%v err=%v (want idle)", got.Status, err)
	}
}

func TestIdleRevokesWorkspaceSessionToken(t *testing.T) {
	ctx := context.Background()
	mgr, _ := newTestManager(t, &scriptedBash{})
	revoke := &revocationTracker{}
	mgr.RevokeSession = revoke.revoke

	ws, client, err := mgr.Open(ctx, "matt-riley/waffle")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("close client: %v", err)
	}

	if err := mgr.Idle(ctx, ws.ID); err != nil {
		t.Fatal(err)
	}
	if !revoke.seen(ws.SessionID) {
		t.Fatalf("session %s was not revoked on idle", ws.SessionID)
	}
}

func TestCloseRevokesWorkspaceSessionToken(t *testing.T) {
	ctx := context.Background()
	mgr, _ := newTestManager(t, &scriptedBash{})
	revoke := &revocationTracker{}
	mgr.RevokeSession = revoke.revoke

	ws, client, err := mgr.Open(ctx, "matt-riley/waffle")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("close client: %v", err)
	}

	if _, err := mgr.Close(ctx, ws.ID, true); err != nil {
		t.Fatal(err)
	}
	if !revoke.seen(ws.SessionID) {
		t.Fatalf("session %s was not revoked on close", ws.SessionID)
	}
}

// TestOpenConcurrentRaceResumesWinner exercises the "resume the winner on
// unique-index insert failure" path that protects against duplicate
// workspaces during concurrent Open (addresses review request for
// deterministic coverage of the race fix).
func TestOpenConcurrentRaceResumesWinner(t *testing.T) {
	ctx := context.Background()
	tools := &scriptedBash{outputs: map[string]string{}}
	mgr, _ := newTestManager(t, tools)

	var wg sync.WaitGroup
	results := make([]struct {
		ws  *Workspace
		err error
	}, 2)

	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ws, client, err := mgr.Open(ctx, "matt-riley/waffle")
			results[i].err = err
			if client != nil {
				if err := client.Close(); err != nil {
					t.Errorf("close client: %v", err)
				}
			}
			results[i].ws = ws
		}(i)
	}
	wg.Wait()

	success := 0
	var id string
	for _, r := range results {
		if r.err == nil && r.ws != nil {
			success++
			if id == "" {
				id = r.ws.ID
			} else if id != r.ws.ID {
				t.Errorf("concurrent opens produced different workspaces: %s vs %s", id, r.ws.ID)
			}
		}
	}
	if success != 2 {
		t.Errorf("expected both concurrent Open calls to succeed (resume winner on UNIQUE), got %d successes", success)
	}
	// If the INSERT-fail resume path was taken, we still end up with one workspace.
	if list, err := mgr.List(ctx); err != nil || len(list) != 1 {
		t.Errorf("after concurrent open, workspaces = %d (want 1), err=%v", len(list), err)
	}
}

func TestForRepoReturnsNotFoundSentinel(t *testing.T) {
	ctx := context.Background()
	mgr, _ := newTestManager(t, &scriptedBash{})

	if _, err := mgr.ForRepo(ctx, "matt-riley/none"); !errors.Is(err, ErrWorkspaceNotFound) {
		t.Fatalf("ForRepo on empty DB = %v, want ErrWorkspaceNotFound", err)
	}
	if _, err := mgr.Get(ctx, "ws-none"); !errors.Is(err, ErrWorkspaceNotFound) {
		t.Fatalf("Get on empty DB = %v, want ErrWorkspaceNotFound", err)
	}
}

// TestOpenLookupErrorDoesNotCreateWorkspace covers issue #16: a lookup
// error that is NOT "workspace not found" (e.g. transient SQLITE_BUSY)
// must abort Open rather than fall through to creating a duplicate
// session/container/volume.
func TestOpenLookupErrorDoesNotCreateWorkspace(t *testing.T) {
	ctx := context.Background()
	mgr, rt := newTestManager(t, &scriptedBash{})

	// Run the one-time index DDL first so the injected failure below only
	// affects the ForRepo lookup inside Open.
	if err := mgr.ensureActiveRepoIndex(ctx); err != nil {
		t.Fatal(err)
	}
	// Inject a non-not-found lookup failure: ForRepo now errors with
	// "no such table", standing in for any transient DB error.
	if _, err := mgr.DB.ExecContext(ctx, `DROP TABLE workspaces`); err != nil {
		t.Fatal(err)
	}

	_, _, err := mgr.Open(ctx, "matt-riley/waffle")
	if err == nil || errors.Is(err, ErrWorkspaceNotFound) {
		t.Fatalf("Open with failing lookup err = %v, want a lookup error", err)
	}
	if !strings.Contains(err.Error(), "look up workspace") {
		t.Errorf("err = %v, want it to identify the lookup failure", err)
	}
	rt.mu.Lock()
	starts := len(rt.opts)
	events := strings.Join(rt.events, "\n")
	rt.mu.Unlock()
	if starts != 0 || strings.Contains(events, "start-workspace") {
		t.Errorf("Open created container(s) despite lookup failure; events:\n%s", events)
	}
}

// TestIsUniqueConstraintErrorRealDriver forces real constraint violations
// through modernc.org/sqlite and checks the structural detection (issue
// #26): UNIQUE violations match, other constraints don't.
func TestIsUniqueConstraintErrorRealDriver(t *testing.T) {
	ctx := context.Background()
	mgr, _ := newTestManager(t, &scriptedBash{})

	sess, err := mgr.Sessions.Create(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	insert := func(id, repo, status string) error {
		_, err := mgr.DB.ExecContext(ctx, `
			INSERT INTO workspaces (id, repo, url, image, container, volume, session_id, status, created_at, updated_at)
			VALUES (?, ?, 'https://x/y.git', 'img', 'c', 'v', ?, ?, ?, ?)`,
			id, repo, sess.ID, status, now(), now())
		return err
	}
	if err := insert("ws-one", "matt-riley/waffle", StatusOpen); err != nil {
		t.Fatal(err)
	}

	// Second active workspace for the same repo violates the partial
	// unique index idx_workspaces_repo_active.
	uniqueErr := insert("ws-two", "matt-riley/waffle", StatusOpen)
	if uniqueErr == nil {
		t.Fatal("second active insert for the same repo succeeded, want UNIQUE violation")
	}
	if !isUniqueConstraintError(uniqueErr) {
		t.Errorf("isUniqueConstraintError(%v) = false, want true", uniqueErr)
	}
	// Detection must survive wrapping (errors.As unwraps).
	if !isUniqueConstraintError(fmt.Errorf("insert workspace: %w", uniqueErr)) {
		t.Errorf("wrapped unique error not detected: %v", uniqueErr)
	}

	// A CHECK violation from the same driver must NOT match.
	checkErr := insert("ws-three", "matt-riley/other", "bogus")
	if checkErr == nil {
		t.Fatal("insert with bogus status succeeded, want CHECK violation")
	}
	if isUniqueConstraintError(checkErr) {
		t.Errorf("isUniqueConstraintError(%v) = true for CHECK violation, want false", checkErr)
	}
}

func TestIsUniqueConstraintErrorFallback(t *testing.T) {
	t.Parallel()
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		// Non-typed errors fall back to a case-insensitive substring
		// check without requiring the column/index name.
		{errors.New("UNIQUE constraint failed: workspaces.repo"), true},
		{errors.New("unique constraint failed"), true},
		{errors.New("CHECK constraint failed: status"), false},
		{errors.New("database is locked"), false},
	}
	for _, c := range cases {
		if got := isUniqueConstraintError(c.err); got != c.want {
			t.Errorf("isUniqueConstraintError(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

func TestNormalizeRepo(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, repo, url string
		wantErr       bool
	}{
		{"matt-riley/waffle", "matt-riley/waffle", "https://github.com/matt-riley/waffle.git", false},
		{"matt-riley/waffle.git", "matt-riley/waffle", "https://github.com/matt-riley/waffle.git", false},
		{"https://github.com/matt-riley/waffle", "matt-riley/waffle", "https://github.com/matt-riley/waffle.git", false},
		{"https://github.com/matt-riley/waffle?tab=readme", "", "", true},
		{"https://github.com/matt-riley/waffle;echo-pwned", "", "", true},
		{"not a repo", "", "", true},
		{"a/b/c", "", "", true},
	}
	for _, c := range cases {
		repo, url, err := normalizeRepo(c.in)
		if c.wantErr != (err != nil) {
			t.Errorf("normalizeRepo(%q) err = %v", c.in, err)
			continue
		}
		if repo != c.repo || url != c.url {
			t.Errorf("normalizeRepo(%q) = %q, %q", c.in, repo, url)
		}
	}
}

func TestOpenWithProfileStoresOnWorkspace(t *testing.T) {
	ctx := context.Background()
	tools := &scriptedBash{outputs: map[string]string{}}
	mgr, _ := newTestManager(t, tools)
	ws, client, err := mgr.OpenWithProfile(ctx, "matt-riley/profiled", "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	if ws.Profile != "reviewer" {
		t.Fatalf("profile = %q", ws.Profile)
	}
	again, err := mgr.Get(ctx, ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.Profile != "reviewer" {
		t.Fatalf("persisted profile = %q", again.Profile)
	}
	// Resume preserves profile (Open ignores new profile when existing).
	resumed, client2, err := mgr.OpenWithProfile(ctx, "matt-riley/profiled", "other")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client2.Close() }()
	if resumed.Profile != "reviewer" {
		t.Fatalf("resume profile = %q, want stored reviewer", resumed.Profile)
	}
}

func TestUnparsableRepoPolicyAtOpen(t *testing.T) {
	// Unclosed front matter → Parse error at open (#53).
	tools := &scriptedBash{outputs: map[string]string{
		"cat /work/repo/WAFFLE.md": "---\nbad line without colon\n",
	}}
	mgr, _ := newTestManager(t, tools)
	_, _, err := mgr.Open(context.Background(), "acme/widgets")
	if err == nil || !strings.Contains(err.Error(), "repo policy") {
		t.Fatalf("expected repo policy error, got %v", err)
	}
	if _, err := mgr.ForRepo(context.Background(), "acme/widgets"); !errors.Is(err, ErrWorkspaceNotFound) {
		t.Fatalf("unusable workspace leaked: %v", err)
	}
}

func TestRepoPolicyTightensEgressIdleHooks(t *testing.T) {
	tools := &scriptedBash{outputs: map[string]string{
		"cat /work/repo/WAFFLE.md": `---
egress: none
idle_timeout: 5m
hooks.after_create: echo setup
hooks.before_run: true
---
Follow repo rules.
`,
	}}
	mgr, rt := newTestManager(t, tools)
	mgr.Egress = "full"
	mgr.IdleTimeout = 30 * time.Minute
	mgr.Hooks = hooks.Config{Timeout: time.Minute}
	ws, client, err := mgr.Open(context.Background(), "acme/widgets")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	if ws.Status != StatusOpen {
		t.Fatalf("status = %s", ws.Status)
	}
	// The shared Manager is never mutated: host settings stay the immutable
	// default for other workspaces (#282).
	if mgr.Egress != "full" {
		t.Fatalf("manager egress mutated: %q, want host default", mgr.Egress)
	}
	if mgr.IdleTimeout != 30*time.Minute {
		t.Fatalf("manager idle mutated: %v, want host default", mgr.IdleTimeout)
	}
	if mgr.Hooks.AfterCreate != "" {
		t.Fatalf("manager hooks mutated: %+v", mgr.Hooks)
	}
	// The tightening is per-open: this workspace's container restarted on
	// the tightened network and the effective egress is durable on the row.
	if len(rt.opts) < 2 || rt.opts[1].Network != WorkspaceBrokerNetwork {
		t.Fatalf("container starts = %+v, want restart on the tightened network", rt.opts)
	}
	if ws.Egress != "none" {
		t.Fatalf("workspace egress = %q, want tightened 'none'", ws.Egress)
	}
	p := mgr.LastPolicy()
	if p == nil || !strings.Contains(p.PromptBlock(), "untrusted") {
		t.Fatalf("last policy = %#v", p)
	}
}

// TestRepoPolicyIsolationAcrossRepos is the #282 regression: opening a
// restrictive repo must not tighten egress/idle/hooks for a second,
// unrelated workspace opened later.
func TestRepoPolicyIsolationAcrossRepos(t *testing.T) {
	// One shared Manager, exactly like `waffle serve`: opening a restrictive
	// repo must not tighten egress/idle for a second, unrelated repo opened
	// later on the same Manager (#282). The fake answers are cleared between
	// opens so the loose repo reads an absent WAFFLE.md.
	tools := &scriptedBash{outputs: map[string]string{
		"cat /work/repo/WAFFLE.md": `---
egress: none
idle_timeout: 5m
---
Follow repo rules.
`,
	}}
	mgr, rt := newTestManager(t, tools)
	mgr.Egress = "full"
	mgr.IdleTimeout = 30 * time.Minute
	ws1, client1, err := mgr.Open(context.Background(), "acme/tight")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client1.Close() }()
	if ws1.Egress != "none" {
		t.Fatalf("tight repo egress = %q", ws1.Egress)
	}
	if ws1.IdleTimeout != "5m0s" {
		t.Fatalf("tight repo idle = %q, want 5m0s", ws1.IdleTimeout)
	}
	// The Manager must still hold host defaults: nothing leaked.
	if mgr.Egress != "full" || mgr.IdleTimeout != 30*time.Minute {
		t.Fatalf("manager mutated by tight open: %q %v", mgr.Egress, mgr.IdleTimeout)
	}

	tools.mu.Lock()
	tools.outputs = map[string]string{}
	tools.mu.Unlock()
	ws2, client2, err := mgr.Open(context.Background(), "acme/loose")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client2.Close() }()
	if ws2.Egress != "full" {
		t.Fatalf("loose repo egress = %q, want host 'full'", ws2.Egress)
	}
	if ws2.IdleTimeout != "" {
		t.Fatalf("loose repo idle = %q, want empty (host applies)", ws2.IdleTimeout)
	}
	// tight open = 2 starts (initial bridge + egress-restart on waffle-ws),
	// loose open = 1 start on the host bridge.
	if len(rt.opts) != 3 || rt.opts[2].Network != "bridge" {
		t.Fatalf("loose repo container starts = %+v, want single bridge start", rt.opts)
	}
	if mgr.IdleTimeout != 30*time.Minute {
		t.Fatalf("manager idle mutated: %v", mgr.IdleTimeout)
	}
	for _, e := range rt.events {
		if e == "rm "+ws2.Container {
			t.Fatalf("loose repo container %q was restarted despite no policy; events=%v", ws2.Container, rt.events)
		}
	}
}

func TestAbsentRepoPolicyLeavesManagerUnchanged(t *testing.T) {
	tools := &scriptedBash{outputs: map[string]string{}}
	mgr, _ := newTestManager(t, tools)
	mgr.Egress = "full"
	mgr.IdleTimeout = 20 * time.Minute
	hostHooks := hooks.Config{AfterCreate: "host-setup", Timeout: time.Minute}
	mgr.Hooks = hostHooks
	_, client, err := mgr.Open(context.Background(), "acme/widgets")
	if err != nil {
		t.Fatal(err)
	}
	_ = client.Close()
	if mgr.Egress != "full" || mgr.IdleTimeout != 20*time.Minute {
		t.Fatalf("egress/idle changed: %q %v", mgr.Egress, mgr.IdleTimeout)
	}
	if mgr.Hooks.AfterCreate != "host-setup" {
		t.Fatalf("hooks changed: %+v", mgr.Hooks)
	}
	if mgr.LastPolicy() != nil {
		t.Fatal("expected nil last policy")
	}
}

func TestPolicyCacheReloadBetweenSessions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "WAFFLE.md")
	if err := os.WriteFile(path, []byte("---\negress: none\n---\nv1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tools := &scriptedBash{}
	mgr, _ := newTestManager(t, tools)
	mgr.Egress = "full"
	mgr.PolicyCache = repopolicy.NewCache(dir)
	// Open without container policy (empty cat); cache supplies policy.
	ws, client, err := mgr.Open(context.Background(), "acme/widgets")
	if err != nil {
		t.Fatal(err)
	}
	_ = client.Close()
	if ws.Egress != "none" {
		t.Fatalf("workspace egress = %q, want cache-tightened 'none'", ws.Egress)
	}
	if mgr.Egress != "full" {
		t.Fatalf("manager egress mutated: %q, want host default", mgr.Egress)
	}
	// Simulate next session: rewrite policy, Cache.Get reloads by mtime.
	time.Sleep(15 * time.Millisecond)
	if err := os.WriteFile(path, []byte("---\negress: none\nidle_timeout: 1m\n---\nv2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mgr.IdleTimeout = 10 * time.Minute
	p, err := mgr.LoadRepoPolicy(context.Background(), nil)
	if err != nil || p == nil || p.Body != "v2" {
		t.Fatalf("reload = %#v err=%v", p, err)
	}
	if mgr.IdleTimeout != 10*time.Minute {
		t.Fatalf("manager idle mutated by load: %v", mgr.IdleTimeout)
	}
}

func TestAfterCreateHookFailureRefusesWorkspace(t *testing.T) {
	tools := &scriptedBash{
		outputs: map[string]string{},
		failing: map[string]string{"go mod download": "hook boom"},
	}
	mgr, _ := newTestManager(t, tools)
	mgr.Hooks = hooks.Config{AfterCreate: "go mod download"}
	_, _, err := mgr.Open(context.Background(), "acme/widgets")
	if err == nil {
		t.Fatal("expected after_create failure")
	}
	if !strings.Contains(err.Error(), "after_create") && !strings.Contains(err.Error(), "hook") {
		t.Fatalf("err = %v", err)
	}
	if _, err := mgr.ForRepo(context.Background(), "acme/widgets"); !errors.Is(err, ErrWorkspaceNotFound) {
		t.Fatalf("ForRepo = %v", err)
	}
}

func TestBeforeRemoveHookDoesNotBlockClose(t *testing.T) {
	tools := &scriptedBash{outputs: map[string]string{
		"git status": "",
		"git log":    "",
	}, failing: map[string]string{"cleanup.sh": "nope"}}
	mgr, _ := newTestManager(t, tools)
	mgr.Hooks = hooks.Config{BeforeRemove: "./cleanup.sh"}
	ws, client, err := mgr.Open(context.Background(), "acme/widgets")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Close(context.Background(), ws.ID, true); err != nil {
		t.Fatalf("close: %v", err)
	}
	if !tools.ran("cleanup.sh") {
		t.Fatal("before_remove did not run")
	}
}

func TestHookRunsViaSandboxClientExec(t *testing.T) {
	// Hooks must reach scriptedBash through the queue client, not host os/exec (#54).
	tools := &scriptedBash{}
	mgr, _ := newTestManager(t, tools)
	mgr.Hooks = hooks.Config{AfterCreate: "go mod download"}
	ws, client, err := mgr.Open(context.Background(), "acme/widgets")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	if !tools.ran("go mod download") {
		t.Fatal("after_create not executed via sandbox bash tool")
	}
	if !tools.viaQueue {
		t.Fatal("hook did not go through queue toolbox")
	}
	_ = ws
}

func TestHookTimeoutViaSandbox(t *testing.T) {
	tools := &scriptedBash{
		delays: map[string]time.Duration{"sleep-hook": 500 * time.Millisecond},
	}
	mgr, _ := newTestManager(t, tools)
	mgr.Hooks = hooks.Config{BeforeRun: "sleep-hook", Timeout: 40 * time.Millisecond}
	ws, client, err := mgr.Open(context.Background(), "acme/widgets")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	res, err := mgr.RunHookFor(context.Background(), client, hooks.BeforeRun, ws.ID, ws.SessionID)
	if err == nil {
		t.Fatal("expected before_run timeout")
	}
	if res.Err == nil {
		t.Fatal("expected res.Err on timeout")
	}
}

func TestBeforeRunFailureAborts(t *testing.T) {
	tools := &scriptedBash{failing: map[string]string{"git fetch": "fetch failed"}}
	mgr, _ := newTestManager(t, tools)
	mgr.Hooks = hooks.Config{BeforeRun: "git fetch --all"}
	ws, client, err := mgr.Open(context.Background(), "acme/widgets")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	_, err = mgr.RunHookFor(context.Background(), client, hooks.BeforeRun, ws.ID, ws.SessionID)
	if err == nil || !strings.Contains(err.Error(), "before_run") {
		t.Fatalf("expected before_run fatal error, got %v", err)
	}
}

func TestAfterRunFailureLoggedAndProceeds(t *testing.T) {
	tools := &scriptedBash{failing: map[string]string{"git status": "status failed"}}
	mgr, _ := newTestManager(t, tools)
	mgr.Hooks = hooks.Config{AfterRun: "git status"}
	ws, client, err := mgr.Open(context.Background(), "acme/widgets")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	// after_run is non-fatal: RunHookFor returns nil error.
	res, err := mgr.RunHookFor(context.Background(), client, hooks.AfterRun, ws.ID, ws.SessionID)
	if err != nil {
		t.Fatalf("after_run should not fail RunHookFor: %v", err)
	}
	if res.Err == nil {
		t.Fatal("expected res.Err recorded")
	}
	// hook_logs row must exist for session debuggability.
	var n int
	var point, errText string
	row := mgr.DB.QueryRow(`SELECT COUNT(*), MAX(point), MAX(error) FROM hook_logs WHERE session_id = ?`, ws.SessionID)
	if err := row.Scan(&n, &point, &errText); err != nil {
		t.Fatal(err)
	}
	if n < 1 || point != "after_run" || errText == "" {
		t.Fatalf("hook_logs = n=%d point=%q err=%q", n, point, errText)
	}
}

func TestBeforeRemoveFailureLoggedAndProceeds(t *testing.T) {
	tools := &scriptedBash{
		outputs: map[string]string{"git status": "", "git log": ""},
		failing: map[string]string{"export.sh": "export failed"},
	}
	mgr, _ := newTestManager(t, tools)
	mgr.Hooks = hooks.Config{BeforeRemove: "./export.sh"}
	ws, client, err := mgr.Open(context.Background(), "acme/widgets")
	if err != nil {
		t.Fatal(err)
	}
	_ = client.Close()
	if _, err := mgr.Close(context.Background(), ws.ID, true); err != nil {
		t.Fatalf("close blocked by before_remove: %v", err)
	}
	var n int
	if err := mgr.DB.QueryRow(`SELECT COUNT(*) FROM hook_logs WHERE point = 'before_remove' AND session_id = ?`, ws.SessionID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n < 1 {
		t.Fatal("expected before_remove hook_logs row")
	}
	got, err := mgr.Get(context.Background(), ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusClosed {
		t.Fatalf("status = %s", got.Status)
	}
}

func TestNoHooksRegression(t *testing.T) {
	tools := &scriptedBash{}
	mgr, _ := newTestManager(t, tools)
	// Empty hooks config: open/close must work as before.
	ws, client, err := mgr.Open(context.Background(), "acme/widgets")
	if err != nil {
		t.Fatal(err)
	}
	_ = client.Close()
	if _, err := mgr.Close(context.Background(), ws.ID, true); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := mgr.DB.QueryRow(`SELECT COUNT(*) FROM hook_logs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("unexpected hook_logs with no hooks: %d", n)
	}
}

// probeSweepManager is a fakeSweepManager that also reports in-process
// activity, standing in for a *Manager whose last_active write failed (#260).
type probeSweepManager struct {
	fakeSweepManager
	activeIDs map[string]time.Time
}

func (p *probeSweepManager) ActiveSince(id string, since time.Time) bool {
	last, ok := p.activeIDs[id]
	return ok && !last.Before(since)
}

func TestReaperSkipsIdleWhenProcessSawActivityAfterAFailedWrite(t *testing.T) {
	stale := time.Now().Add(-2 * time.Hour).UTC()
	fake := &probeSweepManager{
		fakeSweepManager: fakeSweepManager{
			items: []Workspace{
				{ID: "ws-busy", Repo: "o/busy", Status: StatusOpen, LastActive: stale, UpdatedAt: stale},
				{ID: "ws-idle", Repo: "o/idle", Status: StatusOpen, LastActive: stale, UpdatedAt: stale},
			},
		},
		// ws-busy was used a moment ago; only its last_active write was lost.
		activeIDs: map[string]time.Time{"ws-busy": time.Now().UTC()},
	}
	r := &Reaper{Manager: fake, IdleTimeout: time.Hour, Now: time.Now}
	if err := r.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(fake.idleCalls, "ws-busy") {
		t.Errorf("idled a workspace this process was actively using: %v", fake.idleCalls)
	}
	if !slices.Contains(fake.idleCalls, "ws-idle") {
		t.Errorf("idle calls = %v, want the genuinely idle workspace stopped", fake.idleCalls)
	}
}

func TestNoteActivityWarnsAndFallsBackWhenTouchFails(t *testing.T) {
	mgr, _ := newTestManager(t, &scriptedBash{})
	// A closed handle stands in for a busy database or a shutdown race.
	if err := mgr.DB.Close(); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(previous)

	before := time.Now().UTC()
	mgr.noteActivity(context.Background(), "ws-1", "o/r")

	body := logs.String()
	for _, want := range []string{"workspace activity write failed", "workspace=ws-1", "repo=o/r"} {
		if !strings.Contains(body, want) {
			t.Fatalf("logs missing %q: %s", want, body)
		}
	}
	if !mgr.ActiveSince("ws-1", before) {
		t.Error("a failed activity write left no fallback record, so the reaper can still idle an active workspace")
	}
	if mgr.ActiveSince("ws-1", time.Now().UTC().Add(time.Minute)) {
		t.Error("ActiveSince reported activity newer than it saw")
	}
	if mgr.ActiveSince("ws-other", before) {
		t.Error("ActiveSince reported activity for an untouched workspace")
	}
}

func TestNoteActivityClearsFallbackAfterASuccessfulWrite(t *testing.T) {
	mgr, _ := newTestManager(t, &scriptedBash{})
	ws, client, err := mgr.Open(context.Background(), "matt-riley/activity")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("close client: %v", err)
	}

	mgr.activityMu.Lock()
	mgr.activity = map[string]time.Time{ws.ID: time.Now().UTC()}
	mgr.activityMu.Unlock()

	mgr.noteActivity(context.Background(), ws.ID, ws.Repo)
	// last_active is authoritative again, so the fallback must not keep
	// protecting the workspace from future sweeps.
	if mgr.ActiveSince(ws.ID, time.Now().UTC().Add(-time.Hour)) {
		t.Error("a successful activity write left a stale fallback record behind")
	}
}

// TestActivityFallbackKeepsNewestOverlappingWrite covers the ordering hazard in
// #260: activity callbacks for one workspace overlap and do not finish in the
// order they started, so a slower older successful write must not erase the
// record of newer activity whose write failed.
func TestActivityFallbackKeepsNewestOverlappingWrite(t *testing.T) {
	t.Parallel()
	mgr := &Manager{}
	older := time.Now().UTC().Add(-time.Second)
	newer := older.Add(500 * time.Millisecond)

	// The newer callback's Touch failed; the older one's then succeeds.
	mgr.recordActivity("ws-1", newer)
	mgr.clearActivityUpTo("ws-1", older)
	if !mgr.ActiveSince("ws-1", newer) {
		t.Error("an older successful write erased newer activity, so the reaper can idle a workspace still in use")
	}

	// A successful write that does cover the record clears it.
	mgr.clearActivityUpTo("ws-1", newer.Add(time.Millisecond))
	if mgr.ActiveSince("ws-1", older) {
		t.Error("a covering successful write left the fallback behind")
	}

	// An older failed write never backdates a newer record.
	mgr.recordActivity("ws-2", newer)
	mgr.recordActivity("ws-2", older)
	if !mgr.ActiveSince("ws-2", newer) {
		t.Error("an older failed write backdated newer activity")
	}
}

func TestCloseForgetsFallbackActivity(t *testing.T) {
	mgr, _ := newTestManager(t, &scriptedBash{})
	ws, client, err := mgr.Open(context.Background(), "matt-riley/closing")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("close client: %v", err)
	}
	// A workspace closed without first being idled or successfully touched
	// must not leave its fallback record behind.
	mgr.recordActivity(ws.ID, time.Now().UTC())

	if _, err := mgr.Close(context.Background(), ws.ID, true); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if mgr.ActiveSince(ws.ID, time.Now().UTC().Add(-time.Hour)) {
		t.Error("closed workspace kept an unreachable activity record")
	}
}

// TestReaperHonorsPerWorkspaceIdleTimeout is the #282 reaper regression: a
// repo that tightens idle below the host default idles only its own
// workspace, while a sibling workspace keeps the host idle.
func TestReaperHonorsPerWorkspaceIdleTimeout(t *testing.T) {
	tools := &scriptedBash{outputs: map[string]string{
		"cat /work/repo/WAFFLE.md": `---
idle_timeout: 1m
---
tight repo
`,
	}}
	mgr, _ := newTestManager(t, tools)
	mgr.IdleTimeout = time.Hour
	tight, c1, err := mgr.Open(context.Background(), "acme/tight")
	if err != nil {
		t.Fatal(err)
	}
	_ = c1.Close()
	// The loose repo declares no policy: clear the fake's answers so the
	// second open reads an absent WAFFLE.md.
	tools.mu.Lock()
	tools.outputs = map[string]string{}
	tools.mu.Unlock()
	loose, c2, err := mgr.Open(context.Background(), "acme/loose")
	if err != nil {
		t.Fatal(err)
	}
	_ = c2.Close()

	if tight.IdleTimeout != "1m0s" {
		t.Fatalf("tight workspace idle = %q, want repo-tightened 1m0s", tight.IdleTimeout)
	}
	if loose.IdleTimeout != "" {
		t.Fatalf("loose workspace idle = %q, want empty (host applies)", loose.IdleTimeout)
	}
	// Age both workspaces beyond the repo's 1m but well under the host hour.
	cut := time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339Nano)
	if _, err := mgr.DB.Exec(`UPDATE workspaces SET last_active = ?`, cut); err != nil {
		t.Fatal(err)
	}
	if err := (&Reaper{Manager: mgr, IdleTimeout: time.Hour, Now: time.Now}).Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	gotTight, err := mgr.Get(context.Background(), tight.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotTight.Status != StatusIdle {
		t.Fatalf("tight workspace status = %q, want idle on its own 1m policy", gotTight.Status)
	}
	gotLoose, err := mgr.Get(context.Background(), loose.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotLoose.Status != StatusOpen {
		t.Fatalf("loose workspace status = %q, want open (host hour not reached)", gotLoose.Status)
	}
}

func TestOpenDevcontainerAdoptionFailureCleansUp(t *testing.T) {
	ctx := context.Background()
	tools := &scriptedBash{outputs: map[string]string{
		"devcontainer.json": `{"image": "golang:1.25"}`,
	}}
	mgr, rt := newTestManager(t, tools)
	var tracker revocationTracker
	mgr.RevokeSession = tracker.revoke

	// The initial container start succeeds; the adoption restart (second
	// StartWorkspace) fails.
	rt.failStartOn = 2

	ws, _, err := mgr.Open(ctx, "matt-riley/waffle")
	if err == nil {
		t.Fatalf("Open = %+v, want adoption failure", ws)
	}
	if !strings.Contains(err.Error(), "adopt devcontainer image") {
		t.Fatalf("Open error = %v, want devcontainer adoption failure", err)
	}
	// The original container was removed before the failed restart, so the
	// volume and broker token must be cleaned up like the setup path (#283).
	if !rt.hasEventPrefix("rmvol ") {
		t.Fatalf("volume not removed after failed adoption; events = %v", rt.events)
	}
	tracker.mu.Lock()
	revoked := len(tracker.sessions)
	tracker.mu.Unlock()
	if revoked == 0 {
		t.Fatal("broker session not revoked after failed adoption")
	}
}

// TestResumeClearsRemovedRepoIdleTimeout is the Greptile follow-up: when a
// repo drops its idle_timeout between open and resume, the stale per-workspace
// timeout must be cleared so the reaper falls back to the host default.
func TestResumeClearsRemovedRepoIdleTimeout(t *testing.T) {
	ctx := context.Background()
	tools := &scriptedBash{outputs: map[string]string{
		"cat /work/repo/WAFFLE.md": `---
idle_timeout: 1m
---
v1
`,
	}}
	mgr, _ := newTestManager(t, tools)
	mgr.IdleTimeout = time.Hour
	ws, client, err := mgr.Open(ctx, "acme/tight")
	if err != nil {
		t.Fatal(err)
	}
	_ = client.Close()
	if ws.IdleTimeout != "1m0s" {
		t.Fatalf("idle after open = %q, want repo-tightened", ws.IdleTimeout)
	}
	// The repo now declares no idle_timeout.
	tools.mu.Lock()
	tools.outputs = map[string]string{"cat /work/repo/WAFFLE.md": "v2\n"}
	tools.mu.Unlock()
	if err := mgr.Idle(ctx, ws.ID); err != nil {
		t.Fatal(err)
	}
	resumed, resumedClient, err := mgr.Resume(ctx, ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resumedClient.Close() }()
	if resumed.IdleTimeout != "" {
		t.Fatalf("idle after resume = %q, want cleared (host default applies)", resumed.IdleTimeout)
	}
	row, err := mgr.Get(ctx, ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.IdleTimeout != "" {
		t.Fatalf("stored idle after resume = %q, want cleared", row.IdleTimeout)
	}
}

// TestResumeRestartsContainerWhenRepoTightensEgress is the Greptile follow-up:
// if the repo tightens egress while the workspace is idle, resume must
// recreate the container on the tightened network — storing the value alone
// would leave the running container with wider access.
func TestResumeRestartsContainerWhenRepoTightensEgress(t *testing.T) {
	ctx := context.Background()
	tools := &scriptedBash{outputs: map[string]string{
		"cat /work/repo/WAFFLE.md": "v1 (no egress)\n",
	}}
	mgr, rt := newTestManager(t, tools)
	mgr.Egress = "full"
	ws, client, err := mgr.Open(ctx, "acme/tight")
	if err != nil {
		t.Fatal(err)
	}
	_ = client.Close()
	if ws.Egress != "full" {
		t.Fatalf("egress after open = %q, want host 'full' (no repo tightening)", ws.Egress)
	}
	startsBefore := len(rt.opts)

	// The repo now tightens egress while the workspace is idle.
	tools.mu.Lock()
	tools.outputs = map[string]string{"cat /work/repo/WAFFLE.md": "---\negress: none\n---\nv2\n"}
	tools.mu.Unlock()
	if err := mgr.Idle(ctx, ws.ID); err != nil {
		t.Fatal(err)
	}
	resumed, resumedClient, err := mgr.Resume(ctx, ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resumedClient.Close() }()
	if resumed.Egress != "none" {
		t.Fatalf("egress after resume = %q, want tightened 'none'", resumed.Egress)
	}
	// The container must have been recreated on the tightened network.
	if len(rt.opts) != startsBefore+2 {
		t.Fatalf("container starts = %d (was %d), want resume start + egress restart", len(rt.opts), startsBefore)
	}
	last := rt.opts[len(rt.opts)-1]
	if last.Network != WorkspaceBrokerNetwork {
		t.Fatalf("final container network = %q, want the netlocked waffle-ws bridge", last.Network)
	}
}

// TestResumeEgressRestartFailureRevertsToIdle is the Greptile follow-up: when
// the egress-tightening restart fails after the old container was removed,
// the workspace row must not claim "open" — it reverts to idle so the next
// resume recreates the container from the surviving volume.
func TestResumeEgressRestartFailureRevertsToIdle(t *testing.T) {
	ctx := context.Background()
	tools := &scriptedBash{outputs: map[string]string{
		"cat /work/repo/WAFFLE.md": "v1 (no egress)\n",
	}}
	mgr, rt := newTestManager(t, tools)
	mgr.Egress = "full"
	ws, client, err := mgr.Open(ctx, "acme/tight")
	if err != nil {
		t.Fatal(err)
	}
	_ = client.Close()
	// The repo tightens egress while idle; the resume's egress-restart (the
	// 3rd container start: open, resume, egress restart) fails.
	tools.mu.Lock()
	tools.outputs = map[string]string{"cat /work/repo/WAFFLE.md": "---\negress: none\n---\nv2\n"}
	tools.mu.Unlock()
	rt.failStartOn = 3
	if err := mgr.Idle(ctx, ws.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := mgr.Resume(ctx, ws.ID); err == nil || !strings.Contains(err.Error(), "policy egress") {
		t.Fatalf("Resume = %v, want egress-restart failure", err)
	}
	got, err := mgr.Get(ctx, ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusIdle {
		t.Fatalf("status after failed egress restart = %q, want idle (container is gone)", got.Status)
	}
}

// TestResumeWithoutTokenRecreatesAbsentContainer is the Greptile follow-up:
// with MintToken unset, a failed egress-restart leaves the container absent
// and the workspace idle; the next resume must recreate it (not fail forever
// on "No such container").
func TestResumeWithoutTokenRecreatesAbsentContainer(t *testing.T) {
	ctx := context.Background()
	tools := &scriptedBash{outputs: map[string]string{
		"cat /work/repo/WAFFLE.md": "v1 (no egress)\n",
	}}
	mgr, rt := newTestManager(t, tools)
	mgr.Egress = "full"
	mgr.MintToken = nil
	ws, client, err := mgr.Open(ctx, "acme/tight")
	if err != nil {
		t.Fatal(err)
	}
	_ = client.Close()
	// The repo tightens egress while idle; the egress-restart (2nd
	// StartWorkspace: open + restart) fails after removing the container.
	tools.mu.Lock()
	tools.outputs = map[string]string{"cat /work/repo/WAFFLE.md": "---\negress: none\n---\nv2\n"}
	tools.mu.Unlock()
	rt.failStartOn = 2
	if err := mgr.Idle(ctx, ws.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := mgr.Resume(ctx, ws.ID); err == nil || !strings.Contains(err.Error(), "policy egress") {
		t.Fatalf("first Resume = %v, want egress-restart failure", err)
	}
	got, err := mgr.Get(ctx, ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusIdle {
		t.Fatalf("status after failed restart = %q, want idle", got.Status)
	}
	// The next resume (no MintToken) must recreate the absent container.
	rt.failStartOn = 0
	resumed, resumedClient, err := mgr.Resume(ctx, ws.ID)
	if err != nil {
		t.Fatalf("second Resume = %v, want container recreated", err)
	}
	defer func() { _ = resumedClient.Close() }()
	if resumed.Status != StatusOpen {
		t.Fatalf("status after recovery resume = %q, want open", resumed.Status)
	}
	// open (bridge), failed egress-restart (waffle-ws), recovery recreate
	// (stored bridge), enforced egress-restart (waffle-ws).
	if len(rt.opts) != 4 || rt.opts[3].Network != WorkspaceBrokerNetwork {
		t.Fatalf("container starts = %+v, want the tightened-network recreate", rt.opts)
	}
}

// TestResumeDoesNotPersistUnenforcedEgress is the Greptile follow-up: if the
// egress-tightening restart fails at container removal, the row must keep the
// old posture (the container still runs it) so the next resume retries the
// enforcement instead of comparing against a stored tightening the container
// never got.
func TestResumeDoesNotPersistUnenforcedEgress(t *testing.T) {
	ctx := context.Background()
	tools := &scriptedBash{outputs: map[string]string{
		"cat /work/repo/WAFFLE.md": "v1 (no egress)\n",
	}}
	mgr, rt := newTestManager(t, tools)
	mgr.Egress = "full"
	mgr.MintToken = nil
	ws, client, err := mgr.Open(ctx, "acme/tight")
	if err != nil {
		t.Fatal(err)
	}
	_ = client.Close()
	tools.mu.Lock()
	tools.outputs = map[string]string{"cat /work/repo/WAFFLE.md": "---\negress: none\n---\nv2\n"}
	tools.mu.Unlock()
	if err := mgr.Idle(ctx, ws.ID); err != nil {
		t.Fatal(err)
	}
	// The replacement fails at docker rm: the old container (still on "full")
	// survives, so the row must NOT claim "none".
	rt.removeFail = true
	if _, _, err := mgr.Resume(ctx, ws.ID); err == nil || !strings.Contains(err.Error(), "policy egress") {
		t.Fatalf("Resume = %v, want egress-restart failure", err)
	}
	got, err := mgr.Get(ctx, ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Egress != "full" {
		t.Fatalf("row egress after failed replacement = %q, want old 'full' (container never switched)", got.Egress)
	}
	// The next resume retries and enforces the tightening.
	rt.removeFail = false
	resumed, resumedClient, err := mgr.Resume(ctx, ws.ID)
	if err != nil {
		t.Fatalf("second Resume = %v, want enforced tightening", err)
	}
	defer func() { _ = resumedClient.Close() }()
	if resumed.Egress != "none" {
		t.Fatalf("egress after retry = %q, want 'none'", resumed.Egress)
	}
	if len(rt.opts) == 0 || rt.opts[len(rt.opts)-1].Network != WorkspaceBrokerNetwork {
		t.Fatalf("final container network = %+v, want the netlocked bridge", rt.opts)
	}
}

func TestClassifyCloneErrorMapsSafeStableCodes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		msg  string
		code string
		want string
	}{
		{
			name: "egress denied",
			msg:  "git clone -- https://github.com/o/r.git /work/repo: error: exit status 128\nfatal: unable to access: egress host not allowlisted",
			code: "repo_egress_denied",
			want: "the repo host is not permitted by the egress policy (git exited with status 128)",
		},
		{
			name: "credential refused",
			msg:  "git clone -- https://github.com/o/r.git /work/repo: error: exit status 128\ngit-credential: broker: 403 Forbidden: session is scoped to \"o/r\"; refusing credentials for \"other/r\"",
			code: "repo_credential_refused",
			want: "the git credential was refused for the requested repo (git exited with status 128)",
		},
		{
			name: "no binding",
			msg:  "git clone -- https://github.com/o/r.git /work/repo: error: exit status 128\nsession is not bound to a repo workspace; refusing git credentials",
			code: "repo_not_bound",
			want: "no workspace binding for this session (git exited with status 128)",
		},
		{
			name: "plain clone failure carries exit status",
			msg:  "git clone -- https://github.com/o/r.git /work/repo: error: exit status 128\nfatal: could not read from remote repository.",
			code: "repo_clone_failed",
			want: "the repository clone failed (git exited with status 128)",
		},
		{
			name: "clone failure without a status stays generic",
			msg:  "git clone -- https://github.com/o/r.git /work/repo: boom",
			code: "repo_clone_failed",
			want: "the repository clone failed",
		},
		{
			// The runner prefix is the first line only; "exit status" text in
			// the command's own output must not be mistaken for it (#385 review).
			name: "exit status inside command output is ignored",
			msg:  "git clone -- https://github.com/o/r.git /work/repo: fatal: helper said \"exit status 1\" and nothing else",
			code: "repo_clone_failed",
			want: "the repository clone failed",
		},
		{
			// The runner prefix follows the command text on the first line.
			name: "runner prefix mid-first-line is honored",
			msg:  "git clone -- https://github.com/o/r.git /work/repo: error: exit status 128\nfatal: cannot create pipe",
			code: "repo_clone_failed",
			want: "the repository clone failed (git exited with status 128)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyCloneError(errors.New(tc.msg))
			var stable *chat.StableError
			if !errors.As(err, &stable) {
				t.Fatalf("classifyCloneError(%q) = %v, want a chat.StableError", tc.msg, err)
			}
			if stable.Code != tc.code {
				t.Fatalf("code = %q, want %q", stable.Code, tc.code)
			}
			if stable.SafeMessage() != tc.want {
				t.Fatalf("SafeMessage = %q, want %q", stable.SafeMessage(), tc.want)
			}
			// The raw output stays reachable on the host via Error/Unwrap,
			// but never in the safe message.
			if strings.Contains(stable.SafeMessage(), "github.com") || strings.Contains(stable.SafeMessage(), "/work/repo") {
				t.Fatalf("safe message leaks clone detail: %q", stable.SafeMessage())
			}
			if !strings.Contains(err.Error(), tc.msg) {
				t.Fatalf("host Error() lost the raw cause: %q", err.Error())
			}
		})
	}
}

// TestReadFileReadsRepoRelativeFile pins the project-context file surface
// (#478): the file is read from the running workspace via the inspection
// queue, symlinks resolve beneath the repo root, and traversal or missing
// files fail closed.
func TestReadFileReadsRepoRelativeFile(t *testing.T) {
	ctx := context.Background()
	tools := &scriptedBash{outputs: map[string]string{
		"test -f":  "/work/repo/README.md",
		"readlink": "/work/repo/README.md",
		"wc -c":    "8",
		"base64":   base64.StdEncoding.EncodeToString([]byte("# Readme")),
	}}
	mgr, _ := newTestManager(t, tools)
	ws, client, err := mgr.Open(ctx, "matt-riley/waffle")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = client.Close() }()
	_ = ws

	content, err := mgr.ReadFile(ctx, ws.ID, "README.md")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(content) != "# Readme" {
		t.Fatalf("content = %q", content)
	}
	if !tools.ran("readlink -f") || !tools.ran("base64 --wrap=0") {
		t.Fatalf("commands = %+v", tools.commands)
	}
}

func TestReadFileRejectsTraversalAndMissing(t *testing.T) {
	ctx := context.Background()
	tools := &scriptedBash{outputs: map[string]string{}}
	mgr, _ := newTestManager(t, tools)
	ws, client, err := mgr.Open(ctx, "matt-riley/waffle")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = client.Close() }()

	for _, bad := range []string{"../etc/passwd", "/etc/passwd", "a\\b.md", "a;rm -rf /"} {
		if _, err := mgr.ReadFile(ctx, ws.ID, bad); err == nil {
			t.Errorf("ReadFile(%q) should fail closed", bad)
		}
	}
	// A missing file maps to ErrProjectFileMissing.
	tools2 := &scriptedBash{failing: map[string]string{"test -f": "cat: no such file or directory"}}
	mgr2, _ := newManagerFixture(t, tools2)
	ws2, client2, err := mgr2.Open(ctx, "matt-riley/waffle")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client2.Close() }()
	if _, err := mgr2.ReadFile(ctx, ws2.ID, "missing.md"); err == nil {
		t.Fatal("ReadFile of a missing file should fail")
	}
}
