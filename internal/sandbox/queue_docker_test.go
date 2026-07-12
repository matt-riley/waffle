//go:build sandbox_docker

// Real Docker bind-mount queue acceptance harness (#29).
//
// Run on macOS with Docker Desktop/VirtioFS enabled:
//
//	WAFFLE_SANDBOX_DOCKER_SECONDS=120 go test -tags=sandbox_docker ./internal/sandbox -run DockerBindMount -count=1 -timeout 10m -v
package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const dockerHeartbeatInterval = 2 * time.Second

type dockerQueueHarness struct {
	name    string
	dir     string
	client  *Client
	version string
}

func TestDockerBindMountContainerRunnerStress(t *testing.T) {
	h := startDockerQueueHarness(t)
	duration := dockerStressDuration(t)
	t.Logf("Docker=%s host=%s/%s duration=%s", h.version, runtime.GOOS, runtime.GOARCH, duration)

	ctx, cancel := context.WithTimeout(context.Background(), duration+45*time.Second)
	defer cancel()
	var completed atomic.Int64
	errCh := make(chan error, 16)
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		worker := worker
		wg.Add(1)
		go func() {
			defer wg.Done()
			for seq := 0; time.Since(h.client.startedAt) < duration; seq++ {
				useID := fmt.Sprintf("docker-%d-%d", worker, seq)
				input, _ := json.Marshal(map[string]string{"path": fmt.Sprintf("/tmp/%s", useID), "content": useID})
				out, isErr, err := h.client.Exec(ctx, useID, "write_file", input)
				if err != nil || isErr || !strings.Contains(out, "wrote") {
					errCh <- fmt.Errorf("%s: out=%q isErr=%v err=%w", useID, out, isErr, err)
					return
				}
				completed.Add(1)
			}
		}()
	}

	// Observe the heartbeat row while load crosses the host/container mount.
	var heartbeatTimes []time.Time
	lastSeen := time.Time{}
	for time.Since(h.client.startedAt) < duration {
		ts, err := h.client.lastHealth(ctx)
		if err != nil {
			t.Fatalf("heartbeat probe under stress: %v", err)
		}
		if !ts.IsZero() && !ts.Equal(lastSeen) {
			heartbeatTimes = append(heartbeatTimes, ts)
			lastSeen = ts
		}
		time.Sleep(200 * time.Millisecond)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	if completed.Load() == 0 {
		t.Fatal("containerized runner completed no requests")
	}

	var requests, uniqueUses, results int64
	if err := h.client.inbound.QueryRow(`SELECT COUNT(*), COUNT(DISTINCT tool_use_id) FROM requests WHERE tool_use_id LIKE 'docker-%'`).Scan(&requests, &uniqueUses); err != nil {
		t.Fatal(err)
	}
	if err := h.client.outbound.QueryRow(`SELECT COUNT(*) FROM results WHERE request_id > 0`).Scan(&results); err != nil {
		t.Fatal(err)
	}
	if requests != completed.Load() || uniqueUses != requests || results != requests {
		t.Fatalf("exactly-once violation: completed=%d requests=%d unique_use_ids=%d results=%d", completed.Load(), requests, uniqueUses, results)
	}
	if len(heartbeatTimes) < 2 {
		t.Fatalf("observed %d distinct container heartbeats, want at least 2", len(heartbeatTimes))
	}
	for i := 1; i < len(heartbeatTimes); i++ {
		if gap := heartbeatTimes[i].Sub(heartbeatTimes[i-1]); gap > 2*dockerHeartbeatInterval {
			t.Fatalf("container heartbeat gap %s exceeds poll interval x2 (%s)", gap, 2*dockerHeartbeatInterval)
		}
	}
}

func TestDockerBindMountKillMidWriteIntegrity(t *testing.T) {
	h := startDockerQueueHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	// Keep host inserts and container result writes active, then SIGKILL the
	// actual runner container without allowing SQLite/process cleanup.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 500; i++ {
			input, _ := json.Marshal(map[string]string{"path": fmt.Sprintf("/tmp/kill-%d", i), "content": strings.Repeat("x", 4096)})
			_, _, _ = h.client.Exec(ctx, fmt.Sprintf("kill-%d", i), "write_file", input)
		}
	}()
	time.Sleep(750 * time.Millisecond)
	if out, err := exec.CommandContext(ctx, "docker", "kill", h.name).CombinedOutput(); err != nil {
		t.Fatalf("docker kill runner: %v (%s)", err, out)
	}
	cancel()
	<-done
	_ = h.client.Close()

	for _, item := range []struct{ name, schema string }{{inboundFile, inboundSchema}, {outboundFile, outboundSchema}} {
		db, err := openQueueDB(filepath.Join(h.dir, item.name), item.schema)
		if err != nil {
			t.Fatalf("reopen %s after docker kill: %v", item.name, err)
		}
		var result string
		err = db.QueryRow(`PRAGMA integrity_check`).Scan(&result)
		_ = db.Close()
		if err != nil || result != "ok" {
			t.Fatalf("%s integrity after docker kill = %q, err=%v", item.name, result, err)
		}
	}
}

func startDockerQueueHarness(t *testing.T) *dockerQueueHarness {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not in PATH; run on macOS Docker Desktop for VirtioFS evidence")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	info, err := exec.CommandContext(ctx, "docker", "info", "--format", "{{.ServerVersion}}|{{.Architecture}}|{{json .DriverStatus}}").CombinedOutput()
	if err != nil {
		t.Skipf("docker daemon unavailable: %v (%s)", err, strings.TrimSpace(string(info)))
	}
	parts := strings.SplitN(strings.TrimSpace(string(info)), "|", 3)
	arch := runtime.GOARCH
	if len(parts) > 1 {
		switch parts[1] {
		case "x86_64":
			arch = "amd64"
		case "aarch64":
			arch = "arm64"
		default:
			arch = parts[1]
		}
	}
	bin := filepath.Join(t.TempDir(), "waffle-linux")
	build := exec.CommandContext(ctx, "go", "build", "-o", bin, "./cmd/waffle")
	build.Dir = repoRoot(t)
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build linux runner: %v (%s)", err, out)
	}
	dir := t.TempDir()
	client, err := NewClient(dir)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("waffle-queue-%d", time.Now().UnixNano())
	run := exec.CommandContext(ctx, "docker", "run", "-d", "--name", name, "--network", "none",
		"-v", dir+":/waffle/queue", "-v", bin+":/waffle/waffle:ro", "busybox:1.36",
		"/waffle/waffle", "runner", "--queue", "/waffle/queue")
	if out, err := run.CombinedOutput(); err != nil {
		_ = client.Close()
		t.Fatalf("start containerized runner: %v (%s)", err, out)
	}
	h := &dockerQueueHarness{name: name, dir: dir, client: client, version: strings.TrimSpace(string(info))}
	t.Cleanup(func() {
		_ = client.Close()
		_ = exec.Command("docker", "rm", "-f", name).Run()
	})
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if ts, err := client.lastHealth(context.Background()); err == nil && !ts.IsZero() {
			return h
		}
		time.Sleep(200 * time.Millisecond)
	}
	logs, _ := exec.Command("docker", "logs", name).CombinedOutput()
	t.Fatalf("containerized runner emitted no heartbeat: %s", logs)
	return nil
}

func dockerStressDuration(t *testing.T) time.Duration {
	t.Helper()
	if raw := os.Getenv("WAFFLE_SANDBOX_DOCKER_SECONDS"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 5 {
			t.Fatalf("WAFFLE_SANDBOX_DOCKER_SECONDS=%q must be an integer >=5", raw)
		}
		return time.Duration(n) * time.Second
	}
	return 10 * time.Second
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(filepath.Join(dir, "..", ".."))
}
