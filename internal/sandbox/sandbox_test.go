package sandbox

import (
	"context"
	"database/sql"
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

type bigOutputTool struct{}

func (bigOutputTool) Def() llm.Tool {
	return llm.Tool{Name: "big", InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func (bigOutputTool) Run(ctx context.Context, _ json.RawMessage) (string, error) {
	// > OutputLimit to exercise runner truncation before the DB write.
	return strings.Repeat("X", 100*1024) + "END", nil
}

// startRunner runs a Runner against dir until the test ends.
func startRunner(t *testing.T, dir string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		r := &Runner{Tools: tool.NewRegistry(upperTool{}, failTool{}, bigOutputTool{})}
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

	defer func() {
		if err := client.Close(); err != nil {
			t.Errorf("close client: %v", err)
		}
	}()

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
	defer func() {
		if err := client.Close(); err != nil {
			t.Errorf("close client: %v", err)
		}
	}()

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
	defer func() {
		if err := client.Close(); err != nil {
			t.Errorf("close client: %v", err)
		}
	}()
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
		"--memory 2g",
		"--memory-swap 2g",
		"--cpus 2",
		"--pids-limit 512",
		"--security-opt no-new-privileges",
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

func TestRunnerEnforcesTruncationBeforeOutboundWrite(t *testing.T) {
	dir := t.TempDir()
	startRunner(t, dir)

	client, err := NewClient(dir)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			t.Errorf("close client: %v", err)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	out, isError, err := client.Exec(ctx, "big", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Exec big: %v", err)
	}
	if isError {
		t.Fatalf("big returned error: %s", out)
	}
	if len(out) > tool.OutputLimit+200 {
		t.Fatalf("runner did not truncate; got %d bytes", len(out))
	}
	if !strings.Contains(out, "truncated") || !strings.Contains(out, "X") {
		t.Errorf("expected truncation marker and content, got: %q", out[:min(100, len(out))])
	}
}

func TestClientExecDetectsDeadRunnerEarly(t *testing.T) {
	dir := t.TempDir()
	client, err := NewClient(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			t.Errorf("close client: %v", err)
		}
	}()
	// Shrink the detection windows so the test stays fast (defaults are
	// 10s per call / 60s cold-start allowance).
	client.noHealthWait = 300 * time.Millisecond
	client.startupWait = 1 * time.Second

	// No runner started; with heartbeat detection we should fail fast
	// rather than block for the whole ctx.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	start := time.Now()
	_, _, err = client.Exec(ctx, "anything", json.RawMessage(`{}`))
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected error from dead runner")
	}
	if !strings.Contains(err.Error(), "runner appears dead") {
		t.Fatalf("expected 'runner appears dead' error, got: %v", err)
	}
	// Should detect shortly after the startup window, not wait for ctx.
	if elapsed > 10*time.Second {
		t.Fatalf("took too long to detect dead runner: %s", elapsed)
	}
	if elapsed < client.startupWait {
		t.Fatalf("detected too fast (before cold-start allowance): %s", elapsed)
	}
}

// TestExecToleratesColdStartFirstHeartbeat is a regression test for the
// cold-container false positive: the runner's first heartbeat only appears
// well after the per-Exec no-health window, but the container (client) has
// only just started, so Exec must keep polling instead of declaring death.
func TestExecToleratesColdStartFirstHeartbeat(t *testing.T) {
	dir := t.TempDir()
	client, err := NewClient(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			t.Errorf("close client: %v", err)
		}
	}()
	// Tiny per-Exec window (the old, buggy trigger) but a generous
	// cold-start allowance anchored to client/container start.
	client.noHealthWait = 200 * time.Millisecond
	client.startupWait = 10 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	type result struct {
		out     string
		isError bool
		err     error
	}
	resCh := make(chan result, 1)
	go func() {
		out, isError, err := client.Exec(ctx, "upper", json.RawMessage(`{"s":"cold"}`))
		resCh <- result{out, isError, err}
	}()

	// Let Exec sit well past noHealthWait with no runner heartbeat at all;
	// the old Exec-relative logic would have declared the runner dead here.
	time.Sleep(700 * time.Millisecond)
	select {
	case r := <-resCh:
		t.Fatalf("Exec returned before the runner started: %q, isError=%v, err=%v", r.out, r.isError, r.err)
	default:
	}

	// The "container" finishes booting: the runner comes up and serves the
	// pending request.
	startRunner(t, dir)
	select {
	case r := <-resCh:
		if r.err != nil || r.isError || r.out != "COLD" {
			t.Fatalf("Exec after cold start = %q, isError=%v, err=%v", r.out, r.isError, r.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Exec did not complete after runner started")
	}
}

// TestExecPersistentProbeFailuresFailFast covers the contended-queue case
// where the health probe can never run: a dead runner must still be
// detected — past the probe-failure budget Exec fails fast instead of
// blocking until ctx or the full tool timeout. probeTimeout of 1ns makes
// every probe fail deterministically with a deadline error, like sustained
// busy_timeout contention would.
func TestExecPersistentProbeFailuresFailFast(t *testing.T) {
	dir := t.TempDir()
	client, err := NewClient(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			t.Errorf("close client: %v", err)
		}
	}()
	client.probeTimeout = 1 * time.Nanosecond
	client.probeFailWindow = 500 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	start := time.Now()
	_, _, err = client.Exec(ctx, "anything", json.RawMessage(`{}`))
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected error from dead runner under probe failures")
	}
	if !strings.Contains(err.Error(), "runner appears dead") || !strings.Contains(err.Error(), "health probe failing") {
		t.Fatalf("expected probe-failure dead-runner error, got: %v", err)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("probe failures disabled dead-runner detection: took %s", elapsed)
	}
}

// TestExecDetectsDeadRunnerAfterTransientLockContention holds a real
// exclusive lock on outbound.db for a while (every probe and result poll
// errors, as under busy_timeout contention), then releases it. Probe
// errors during the contended stretch must not permanently disable
// dead-runner detection: once the lock clears, the missing runner is
// detected and Exec returns instead of blocking until ctx.
func TestExecDetectsDeadRunnerAfterTransientLockContention(t *testing.T) {
	dir := t.TempDir()
	client, err := NewClient(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			t.Errorf("close client: %v", err)
		}
	}()
	client.noHealthWait = 100 * time.Millisecond
	client.startupWait = 200 * time.Millisecond
	client.probeTimeout = 100 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	locker, err := sql.Open("sqlite", "file:"+dir+"/"+outboundFile)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := locker.Close(); err != nil {
			t.Errorf("close locker: %v", err)
		}
	}()
	conn, err := locker.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close conn: %v", err)
		}
	}()
	if _, err := conn.ExecContext(ctx, "BEGIN EXCLUSIVE"); err != nil {
		t.Fatalf("BEGIN EXCLUSIVE: %v", err)
	}
	release := time.AfterFunc(1500*time.Millisecond, func() {
		_, _ = conn.ExecContext(ctx, "ROLLBACK")
	})
	defer release.Stop()

	start := time.Now()
	_, _, err = client.Exec(ctx, "anything", json.RawMessage(`{}`))
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected error from dead runner")
	}
	if !strings.Contains(err.Error(), "runner appears dead") {
		t.Fatalf("expected 'runner appears dead' error, got: %v", err)
	}
	// Detection should land shortly after the lock is released (a few
	// busy_timeout-bounded queries at most), nowhere near the 60s ctx.
	if elapsed > 20*time.Second {
		t.Fatalf("probe failures disabled dead-runner detection: took %s", elapsed)
	}
}

func TestResolveRunnerBinaryExplicitWins(t *testing.T) {
	// An explicit, valid runner binary is honored regardless of host OS.
	restore := hostGOOS
	hostGOOS = "darwin"
	defer func() { hostGOOS = restore }()

	bin := filepath.Join(t.TempDir(), "waffle-linux")
	if err := os.WriteFile(bin, []byte("elf"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveRunnerBinary(bin)
	if err != nil {
		t.Fatalf("explicit runner binary rejected: %v", err)
	}
	if got != bin {
		t.Errorf("got %q, want %q", got, bin)
	}
}

func TestResolveRunnerBinaryRejectsBadExplicitPath(t *testing.T) {
	// A relative path (docker would read it as a named volume) is refused, on
	// any host OS, before it ever reaches docker.
	restore := hostGOOS
	hostGOOS = "linux"
	defer func() { hostGOOS = restore }()

	if _, err := ResolveRunnerBinary("waffle-linux"); err == nil {
		t.Error("relative runner_binary accepted, want error")
	}
	if _, err := ResolveRunnerBinary(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("missing absolute runner_binary accepted, want error")
	}
}

func TestValidateRunnerBinary(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "waffle-linux")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ValidateRunnerBinary(file); err != nil {
		t.Errorf("valid absolute file rejected: %v", err)
	}
	if err := ValidateRunnerBinary("waffle-linux"); err == nil {
		t.Error("relative path accepted")
	}
	if err := ValidateRunnerBinary(dir); err == nil {
		t.Error("directory accepted, want file")
	}
	if err := ValidateRunnerBinary(filepath.Join(dir, "nope")); err == nil {
		t.Error("missing file accepted")
	}
}

func TestResolveRunnerBinaryRefusesNonLinuxWithoutConfig(t *testing.T) {
	restore := hostGOOS
	hostGOOS = "darwin"
	defer func() { hostGOOS = restore }()

	_, err := ResolveRunnerBinary("")
	if err == nil {
		t.Fatal("want refusal on a non-linux host with no runner_binary")
	}
	if !strings.Contains(err.Error(), "runner_binary") || !strings.Contains(err.Error(), "linux") {
		t.Errorf("error should name runner_binary and linux; got: %v", err)
	}
}

func TestResolveRunnerBinaryUsesSelfOnLinux(t *testing.T) {
	restore := hostGOOS
	hostGOOS = "linux"
	defer func() { hostGOOS = restore }()

	got, err := ResolveRunnerBinary("")
	if err != nil {
		t.Fatalf("linux host rejected: %v", err)
	}
	if got == "" {
		t.Error("want the running binary path, got empty")
	}
}

func TestDuplicateExecIsAbsorbedAndReclaimable(t *testing.T) {
	dir := t.TempDir()
	startRunner(t, dir)
	client, err := NewClient(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			t.Errorf("close client: %v", err)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	first, _, err := client.Exec(ctx, "tool-call-1", "upper", json.RawMessage(`{"s":"once"}`))
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := client.Exec(ctx, "tool-call-1", "upper", json.RawMessage(`{"s":"different"}`))
	if err != nil {
		t.Fatal(err)
	}
	if first != second || first != "ONCE" {
		t.Fatalf("duplicate results = %q, %q", first, second)
	}
	var count int
	if err := client.inbound.QueryRow(`SELECT count(*) FROM requests WHERE tool_use_id = 'tool-call-1'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("request count = %d, want 1", count)
	}
	got, err := client.Reclaim(ctx, []string{"tool-call-1", "missing"})
	if err != nil {
		t.Fatal(err)
	}
	if got["tool-call-1"].Content != "ONCE" || len(got) != 1 {
		t.Fatalf("reclaim = %#v", got)
	}
}

func TestLegacyPopulatedQueueMigratesNullableToolUseID(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, inboundFile))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE requests (id INTEGER PRIMARY KEY AUTOINCREMENT, tool TEXT NOT NULL, input TEXT NOT NULL, created_at TEXT NOT NULL) STRICT`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO requests(tool,input,created_at) VALUES ('upper','{"s":"legacy"}','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	client, err := NewClient(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	var count, nullIDs int
	if err := client.inbound.QueryRow(`SELECT COUNT(*), SUM(tool_use_id IS NULL) FROM requests`).Scan(&count, &nullIDs); err != nil {
		t.Fatal(err)
	}
	if count != 1 || nullIDs != 1 {
		t.Fatalf("legacy rows count=%d null tool_use_id=%d", count, nullIDs)
	}
	if _, err := client.inbound.Exec(`INSERT INTO requests(tool_use_id,tool,input,created_at) VALUES ('new-id','upper','{}','2026-01-01T00:00:01Z')`); err != nil {
		t.Fatalf("new identity after additive migration: %v", err)
	}
}

func TestNewClientReclaimsCompletedOutputAfterHostRestart(t *testing.T) {
	dir := t.TempDir()
	startRunner(t, dir)
	first, err := NewClient(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	content, _, err := first.Exec(ctx, "restart-use", "upper", json.RawMessage(`{"s":"durable"}`))
	if err != nil || content != "DURABLE" {
		t.Fatalf("first result=%q err=%v", content, err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	resumed, err := NewClient(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resumed.Close() }()
	reclaimed, err := resumed.Reclaim(ctx, []string{"restart-use"})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := reclaimed["restart-use"]
	if !ok || result.Content != "DURABLE" || result.IsError {
		t.Fatalf("reclaimed=%#v", reclaimed)
	}
}
