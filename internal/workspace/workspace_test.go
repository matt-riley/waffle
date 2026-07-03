package workspace

import (
	"context"
	"encoding/json"
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
	mu      sync.Mutex
	tools   *scriptedBash
	cancels map[string]context.CancelFunc
	events  []string
	opts    []ContainerOpts
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
	f.mu.Unlock()
	f.log("start-workspace " + opts.Name + " image=" + opts.Image)
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
	if !tools.ran("git clone https://github.com/matt-riley/waffle.git /work/repo") {
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

func TestNormalizeRepo(t *testing.T) {
	cases := []struct {
		in, repo, url string
		wantErr       bool
	}{
		{"matt-riley/waffle", "matt-riley/waffle", "https://github.com/matt-riley/waffle.git", false},
		{"matt-riley/waffle.git", "matt-riley/waffle", "https://github.com/matt-riley/waffle.git", false},
		{"https://github.com/matt-riley/waffle", "matt-riley/waffle", "https://github.com/matt-riley/waffle.git", false},
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
