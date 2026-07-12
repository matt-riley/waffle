package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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

// fakeRuntime runs an in-process Runner per "container" instead of docker.
type fakeRuntime struct {
	mu       sync.Mutex
	tools    *scriptedBash
	cancels  map[string]context.CancelFunc
	events   []string
	opts     []ContainerOpts
	startErr error
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
	return &fakeRuntime{tools: tools, cancels: map[string]context.CancelFunc{}}
}

func (f *fakeRuntime) log(e string) {
	f.mu.Lock()
	f.events = append(f.events, e)
	f.mu.Unlock()
}

func (f *fakeRuntime) StartWorkspace(ctx context.Context, opts ContainerOpts) error {
	f.mu.Lock()
	f.opts = append(f.opts, opts)
	startErr := f.startErr
	f.mu.Unlock()
	f.log("start-workspace " + opts.Name + " image=" + opts.Image)
	if startErr != nil {
		return startErr
	}
	f.launch(opts.Name, opts.QueueDir)
	return nil
}

func (f *fakeRuntime) launch(name, queueDir string) {
	rctx, cancel := context.WithCancel(context.Background())
	f.mu.Lock()
	f.cancels[name] = cancel
	f.mu.Unlock()
	go func() {
		r := &sandbox.Runner{Tools: tool.NewRegistry(f.tools)}
		_ = r.Serve(rctx, queueDir)
	}()
}

func (f *fakeRuntime) StopContainer(ctx context.Context, name string) error {
	f.log("stop " + name)
	f.halt(name)
	return nil
}

func (f *fakeRuntime) StartContainer(ctx context.Context, name string) error {
	f.log("restart " + name)
	f.mu.Lock()
	if f.startErr != nil {
		err := f.startErr
		f.mu.Unlock()
		return err
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
	f.halt(name)
	return nil
}

func (f *fakeRuntime) RemoveVolume(ctx context.Context, name string) error {
	f.log("rmvol " + name)
	return nil
}

func (f *fakeRuntime) halt(name string) {
	f.mu.Lock()
	if cancel := f.cancels[name]; cancel != nil {
		cancel()
		delete(f.cancels, name)
	}
	f.mu.Unlock()
	time.Sleep(150 * time.Millisecond) // let the runner release the queue
}

func newTestManager(t *testing.T, tools *scriptedBash) (*Manager, *fakeRuntime) {
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
	mgr := NewManager(st, session.New(st), rt, t.TempDir())
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
	mgr, _ := newTestManager(t, tools)
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
	if mgr.Egress != "none" {
		t.Fatalf("egress not tightened: %q", mgr.Egress)
	}
	if mgr.IdleTimeout != 5*time.Minute {
		t.Fatalf("idle not tightened: %v", mgr.IdleTimeout)
	}
	if mgr.Hooks.AfterCreate != "echo setup" {
		t.Fatalf("hooks not merged: %+v", mgr.Hooks)
	}
	p := mgr.LastPolicy()
	if p == nil || !strings.Contains(p.PromptBlock(), "untrusted") {
		t.Fatalf("last policy = %#v", p)
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
	_, client, err := mgr.Open(context.Background(), "acme/widgets")
	if err != nil {
		t.Fatal(err)
	}
	_ = client.Close()
	if mgr.Egress != "none" {
		t.Fatalf("cache policy not applied: %q", mgr.Egress)
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
	if mgr.IdleTimeout != time.Minute {
		t.Fatalf("idle after reload = %v", mgr.IdleTimeout)
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
