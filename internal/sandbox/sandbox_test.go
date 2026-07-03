package sandbox

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/tool"
)

type upperTool struct{}

func (upperTool) Def() llm.Tool {
	return llm.Tool{Name: "upper", InputSchema: json.RawMessage(`{"type":"object","properties":{"s":{"type":"string"}}}`)}
}

func (upperTool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	var in struct {
		S string `json:"s"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", err
	}
	return strings.ToUpper(in.S), nil
}

type failTool struct{}

func (failTool) Def() llm.Tool {
	return llm.Tool{Name: "fail", InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func (failTool) Run(ctx context.Context, _ json.RawMessage) (string, error) {
	return "", context.DeadlineExceeded
}

// startRunner runs a Runner against dir until the test ends.
func startRunner(t *testing.T, dir string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		r := &Runner{Tools: tool.NewRegistry(upperTool{}, failTool{})}
		done <- r.Serve(ctx, dir)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("runner did not stop")
		}
	})
}

func TestQueueRoundTrip(t *testing.T) {
	dir := t.TempDir()
	startRunner(t, dir)

	client, err := NewClient(dir)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.Close() //nolint:errcheck // test teardown

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	out, isError, err := client.Exec(ctx, "upper", json.RawMessage(`{"s":"waffle"}`))
	if err != nil || isError || out != "WAFFLE" {
		t.Fatalf("Exec = %q, isError=%v, err=%v", out, isError, err)
	}

	// Tool errors come back as error results, not transport errors.
	out, isError, err = client.Exec(ctx, "fail", json.RawMessage(`{}`))
	if err != nil || !isError || !strings.Contains(out, "deadline") {
		t.Fatalf("fail tool = %q, isError=%v, err=%v", out, isError, err)
	}

	// Unknown tools too.
	out, isError, err = client.Exec(ctx, "nope", json.RawMessage(`{}`))
	if err != nil || !isError || !strings.Contains(out, "unknown tool") {
		t.Fatalf("unknown tool = %q, isError=%v, err=%v", out, isError, err)
	}
}

func TestRunnerResumesWithoutReexecuting(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := NewClient(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close() //nolint:errcheck // test teardown

	// First runner answers one request, then stops.
	r1ctx, r1cancel := context.WithCancel(ctx)
	r1done := make(chan error, 1)
	go func() {
		r := &Runner{Tools: tool.NewRegistry(upperTool{})}
		r1done <- r.Serve(r1ctx, dir)
	}()
	out, _, err := client.Exec(ctx, "upper", json.RawMessage(`{"s":"one"}`))
	if err != nil || out != "ONE" {
		t.Fatalf("first exec = %q, %v", out, err)
	}
	r1cancel()
	<-r1done

	// A second runner picks up only new work — the answered request stays
	// answered (INSERT OR IGNORE + high-water mark).
	startRunner(t, dir)
	out, _, err = client.Exec(ctx, "upper", json.RawMessage(`{"s":"two"}`))
	if err != nil || out != "TWO" {
		t.Fatalf("second exec = %q, %v", out, err)
	}
}

func TestShutdownStopsRunner(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		r := &Runner{Tools: tool.NewRegistry(upperTool{})}
		done <- r.Serve(context.Background(), dir)
	}()

	client, err := NewClient(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close() //nolint:errcheck // test teardown
	if err := client.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runner ignored shutdown")
	}
}

func TestDockerRunArgs(t *testing.T) {
	args := dockerRunArgs("waffle-sb-ab12", DockerOpts{
		Image:     "debian:stable-slim",
		QueueDir:  "/home/u/.waffle/sandboxes/x",
		WorkDir:   "/home/u/project",
		Network:   "none",
		SelfPath:  "/usr/local/bin/waffle",
		BrokerURL: "http://waffle-host:8421",
		Token:     "wk_abc",
	})
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--network none",
		"-v /usr/local/bin/waffle:/usr/local/bin/waffle:ro",
		"-v /home/u/.waffle/sandboxes/x:/waffle/queue",
		"-v /home/u/project:/work",
		"--add-host waffle-host:host-gateway",
		"-e WAFFLE_SESSION_TOKEN=wk_abc",
		"debian:stable-slim /usr/local/bin/waffle runner --queue /waffle/queue",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q:\n%s", want, joined)
		}
	}
}

func TestDockerCloseIgnoresAlreadyRemovedContainer(t *testing.T) {
	binDir := t.TempDir()
	docker := filepath.Join(binDir, "docker")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\necho 'Error response from daemon: No such container: waffle-sb-gone' >&2\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	client, err := NewClient(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	executor := &DockerExecutor{client: client, container: "waffle-sb-gone"}
	if err := executor.Close(); err != nil {
		t.Fatalf("Close returned an error for an already-removed container: %v", err)
	}
}
