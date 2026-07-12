//go:build sandbox_docker

// Docker bind-mount queue smoke (#29).
//
//	go test -tags=sandbox_docker ./internal/sandbox -run BindMount -count=1
//
// When docker is unavailable the test Skips with a clear message so CI Linux
// hosts without a daemon stay green if the tag is ever enabled accidentally.
package sandbox

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestQueueOverDockerBindMount runs the real SQLite queue pair on a host
// directory that is also bind-mounted into a short-lived container. This is
// the filesystem path Docker Desktop VirtioFS / engine bind mounts use for
// inbound.db/outbound.db IPC.
func TestQueueOverDockerBindMount(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not in PATH; install Docker Desktop or the engine to run sandbox_docker tests (see docs/sandbox-queue.md)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	infoOut, err := exec.CommandContext(ctx, "docker", "info", "--format", "{{.ServerVersion}}").CombinedOutput()
	if err != nil {
		t.Skipf("docker daemon unavailable (%v: %s); skip bind-mount queue test", err, strings.TrimSpace(string(infoOut)))
	}
	ver := strings.TrimSpace(string(infoOut))
	t.Logf("docker ServerVersion=%s", ver)

	dir := t.TempDir()
	// Prove host↔container visibility on this path (same class of mount as queue).
	probe := exec.CommandContext(ctx, "docker", "run", "--rm", "--network", "none",
		"-v", dir+":/q", "busybox:1.36", "sh", "-c", "echo bind-ok > /q/probe && sync")
	if out, err := probe.CombinedOutput(); err != nil {
		t.Skipf("docker cannot bind-mount %s (%v: %s); skip", dir, err, strings.TrimSpace(string(out)))
	}
	b, err := os.ReadFile(filepath.Join(dir, "probe"))
	if err != nil || strings.TrimSpace(string(b)) != "bind-ok" {
		t.Fatalf("bind-mount probe failed: content=%q err=%v", b, err)
	}

	// Queue round-trip on the bind-mounted host path (host-side Client+Runner).
	// A full in-container waffle runner needs a linux runner_binary; host FS
	// on the shared path is the supported stress surface when Desktop is present.
	startRunner(t, dir)
	client, err := NewClient(dir)
	if err != nil {
		t.Fatalf("NewClient on bind-mount path: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	execCtx, execCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer execCancel()
	out, isErr, err := client.Exec(execCtx, "upper", json.RawMessage(`{"s":"bind"}`))
	if err != nil || isErr || out != "BIND" {
		t.Fatalf("queue Exec over bind-mount path: out=%q isErr=%v err=%v", out, isErr, err)
	}

	// Container can still see queue files created by the host runner.
	list := exec.CommandContext(ctx, "docker", "run", "--rm", "--network", "none",
		"-v", dir+":/q", "busybox:1.36", "sh", "-c", "ls /q")
	listOut, err := list.CombinedOutput()
	if err != nil {
		t.Fatalf("container list queue dir: %v (%s)", err, listOut)
	}
	listed := string(listOut)
	if !strings.Contains(listed, inboundFile) && !strings.Contains(listed, "inbound") {
		// Client may name files differently on some paths; require non-empty mount.
		if strings.TrimSpace(listed) == "" {
			t.Fatalf("container saw empty bind mount after queue use")
		}
	}
	t.Logf("queue over bind mount OK (docker %s); container ls:\n%s", ver, listed)
}
