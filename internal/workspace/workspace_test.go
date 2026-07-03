package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/llm"
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
	s.mu.Unlock()
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
	t.Cleanup(func() { st.Close() }) //nolint:errcheck // test teardown
	rt := newFakeRuntime(tools)
	mgr := NewManager(st, session.New(st), rt, t.TempDir())
	mgr.ExecTimeout = 10 * time.Second
	mgr.MintToken = func(ctx context.Context, sessionID string) string { return "wk_test" }
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
	defer client.Close() //nolint:errcheck // test teardown

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
	defer client2.Close() //nolint:errcheck // test teardown
	if ws2.ID != ws.ID {
		t.Errorf("second open made a new workspace: %s vs %s", ws2.ID, ws.ID)
	}
}

func TestOpenRefreshesBrokerTokenForExistingWorkspace(t *testing.T) {
	ctx := context.Background()
	mgr, rt := newTestManager(t, &scriptedBash{})
	token := "wk_first"
	mgr.MintToken = func(context.Context, string) string { return token }

	ws, client, err := mgr.Open(ctx, "matt-riley/waffle")
	if err != nil {
		t.Fatal(err)
	}
	client.Close() //nolint:errcheck // reopening with a new broker token

	token = "wk_second"
	resumed, client, err := mgr.Open(ctx, "matt-riley/waffle")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer client.Close() //nolint:errcheck // test teardown
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
	defer client.Close() //nolint:errcheck // test teardown

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
	client.Close() //nolint:errcheck // switching to resume

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
	defer client2.Close() //nolint:errcheck // test teardown
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
	client.Close() //nolint:errcheck // manager reconnects in Close

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
	client.Close() //nolint:errcheck // manager reconnects in Close

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
	client.Close() //nolint:errcheck // manager reconnects in Close
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
	client.Close() //nolint:errcheck
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
	client.Close() //nolint:errcheck // switching to idle

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
	client.Close() //nolint:errcheck // manager reconnects in Close

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
				client.Close() //nolint:errcheck // test cleanup
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
