package netlock_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/sandbox"
)

// TestWorkspaceRunnerDropsCapabilitiesAfterLockdown is the adversarial proof
// for the egress boundary: after the workspace runner applies its lockdown it
// re-execs with only the DAC capability pair, so untrusted container code
// cannot re-add IPv4 or IPv6 default routes. Gated like the other Docker suites:
//
//	WAFFLE_TEST_DOCKER=1 go test ./internal/netlock -run TestWorkspaceRunnerDrops -count=1 -v
func TestWorkspaceRunnerDropsCapabilitiesAfterLockdown(t *testing.T) {
	skipUnlessDocker(t)
	ctx := context.Background()
	bin := buildLinuxWaffle(t)

	netName := "waffle-capdrop-test"
	name := fmt.Sprintf("waffle-capdrop-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", name).Run()
		_ = exec.Command("docker", "network", "rm", netName).Run()
	})
	_ = exec.CommandContext(ctx, "docker", "network", "rm", netName).Run()
	if out, err := exec.CommandContext(ctx, "docker", "network", "create", netName).CombinedOutput(); err != nil {
		t.Fatalf("docker network create: %v (%s)", err, out)
	}

	qdir := t.TempDir()
	// The cap-less runner opens the queue before the host-side client
	// exists, so the dir must be cross-uid accessible before docker run.
	if err := os.Chmod(qdir, 0o777); err != nil {
		t.Fatal(err)
	}
	run := exec.CommandContext(ctx, "docker", "run", "-d", "--name", name,
		"--network", netName,
		"--cap-add", "NET_ADMIN",
		"--security-opt", "no-new-privileges",
		"--add-host", "waffle-host:host-gateway",
		"-e", "WAFFLE_NET_LOCKDOWN=1",
		"-e", "WAFFLE_NET_LOCKDOWN_HOST=waffle-host",
		"-v", qdir+":/waffle/queue",
		"-v", bin+":/waffle/waffle:ro",
		"busybox:1.36",
		"/waffle/waffle", "runner", "--queue", "/waffle/queue")
	if out, err := run.CombinedOutput(); err != nil {
		t.Fatalf("docker run: %v (%s)", err, out)
	}

	// PID 1 carries the empty capability set once the setup phase has
	// re-exec'd into the serving process.
	deadline := time.Now().Add(30 * time.Second)
	dropped := false
	for time.Now().Before(deadline) {
		out, err := exec.CommandContext(ctx, "docker", "exec", name, "cat", "/proc/1/status").CombinedOutput()
		if err == nil && strings.Contains(string(out), "CapEff:\t0000000000000000") {
			dropped = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !dropped {
		logs, _ := exec.CommandContext(ctx, "docker", "logs", name).CombinedOutput()
		t.Fatalf("runner never reached an empty capability set\nlogs:\n%s", logs)
	}

	// The Bash tool execs "bash"; busybox ships only sh. The wrapper is test
	// scaffolding — the adversarial commands below still run through the
	// runner's own queue.
	wrapper := filepath.Join(t.TempDir(), "bash-wrapper")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec /bin/sh \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.CommandContext(ctx, "docker", "cp", wrapper, name+":/bin/bash").CombinedOutput(); err != nil {
		t.Fatalf("install bash wrapper: %v (%s)", err, out)
	}

	gw := strings.TrimSpace(dockerOut(t, ctx, "network", "inspect",
		"-f", "{{(index .IPAM.Config 0).Gateway}}", netName))
	// Drive the runner's own Bash tool through the shared queue so the
	// commands execute as children of the dropped-capability PID 1. docker
	// exec would instead start a fresh process carrying the container's
	// original NET_ADMIN set, bypassing the drop entirely. Both attempts run
	// in one queue round trip: the bind-mounted queue can serve one request
	// reliably before cross-OS sqlite locking staleness kicks in.
	client, err := sandbox.NewClient(qdir)
	if err != nil {
		t.Fatalf("open queue client: %v", err)
	}
	defer func() { _ = client.Close() }()
	cmd := "ip route add default via " + gw + " dev eth0; echo V4=$?; " +
		"ip -6 route add default dev eth0; echo V6=$?"
	out, _, err := bashViaQueue(ctx, client, cmd)
	if err != nil {
		t.Fatalf("queue exec: %v", err)
	}
	if strings.Contains(out, "V4=0") {
		t.Fatalf("restore ipv4 default route: succeeded after the capability drop\n%s", out)
	}
	if !strings.Contains(strings.ToLower(out), "not permitted") {
		t.Fatalf("restore ipv4 default route: failed for an unexpected reason (want EPERM): %s", out)
	}
	// The kernel may lack IPv6 in some environments; a zero V6 exit would be
	// the only meaningful failure (a route added back without capabilities).
	if strings.Contains(out, "V6=0") {
		t.Fatalf("restore ipv6 default route: succeeded after the capability drop\n%s", out)
	}
}

// bashViaQueue runs one bash command through the runner's queue with a short
// retry window for the moment between the re-exec and the serving loop.
func bashViaQueue(ctx context.Context, client *sandbox.Client, cmd string) (string, bool, error) {
	input := fmt.Sprintf(`{"command": %q}`, cmd)
	var out string
	var isError bool
	var err error
	for range 25 {
		out, isError, err = client.Exec(ctx, "bash", input)
		if err == nil || !strings.Contains(err.Error(), "runner") {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	return out, isError, err
}

// TestWorkspaceRunnerKeepsCapabilitiesWithoutLockdown is the positive
// control: the same image and mounts without WAFFLE_NET_LOCKDOWN keep
// NET_ADMIN, so the refusals above are attributable to the drop itself.
func TestWorkspaceRunnerKeepsCapabilitiesWithoutLockdown(t *testing.T) {
	skipUnlessDocker(t)
	ctx := context.Background()
	bin := buildLinuxWaffle(t)

	netName := "waffle-capdrop-control"
	name := fmt.Sprintf("waffle-control-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", name).Run()
		_ = exec.Command("docker", "network", "rm", netName).Run()
	})
	_ = exec.CommandContext(ctx, "docker", "network", "rm", netName).Run()
	if out, err := exec.CommandContext(ctx, "docker", "network", "create", netName).CombinedOutput(); err != nil {
		t.Fatalf("docker network create: %v (%s)", err, out)
	}

	qdir := t.TempDir()
	if err := os.Chmod(qdir, 0o777); err != nil {
		t.Fatal(err)
	}
	run := exec.CommandContext(ctx, "docker", "run", "-d", "--name", name,
		"--network", netName,
		"--cap-add", "NET_ADMIN",
		"--add-host", "waffle-host:host-gateway",
		"-v", qdir+":/waffle/queue",
		"-v", bin+":/waffle/waffle:ro",
		"busybox:1.36",
		"/waffle/waffle", "runner", "--queue", "/waffle/queue")
	if out, err := run.CombinedOutput(); err != nil {
		t.Fatalf("docker run: %v (%s)", err, out)
	}

	// A fresh container already has a default route, so adding one cannot
	// serve as the positive control (EEXIST). Adding an address exercises
	// the same CAP_NET_ADMIN check without colliding with existing state.
	addAddr := "ip addr add 198.51.100.1/32 dev eth0"
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	var lastOut []byte
	for time.Now().Before(deadline) {
		lastOut, lastErr = exec.CommandContext(ctx, "docker", "exec", name, "sh", "-c", addAddr).CombinedOutput()
		if lastErr == nil {
			_, _ = exec.CommandContext(ctx, "docker", "exec", name, "sh", "-c",
				"ip addr del 198.51.100.1/32 dev eth0").CombinedOutput()
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("positive control failed: a NET_ADMIN container could not add an address (environment problem, not the fix): %v (%s)", lastErr, lastOut)
}

func skipUnlessDocker(t *testing.T) {
	t.Helper()
	if os.Getenv("WAFFLE_TEST_DOCKER") != "1" {
		t.Skip("set WAFFLE_TEST_DOCKER=1 to run Docker netlock integration")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "docker", "info", "--format", "{{.ServerVersion}}").CombinedOutput(); err != nil {
		t.Skipf("docker daemon unavailable: %v (%s)", err, out)
	}
}

func buildLinuxWaffle(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	outBin := filepath.Join(dir, "waffle-linux")
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	modRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "../.."))
	cmd := exec.Command("go", "build", "-o", outBin, "./cmd/waffle")
	cmd.Dir = modRoot
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/waffle: %v\n%s\nmodRoot=%s", err, out, modRoot)
	}
	return outBin
}

func dockerOut(t *testing.T, ctx context.Context, args ...string) string {
	t.Helper()
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %v: %v (%s)", args, err, out)
	}
	return string(out)
}
