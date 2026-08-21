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
)

// TestWorkspaceRunnerDropsCapabilitiesAfterLockdown is the adversarial proof
// for the egress boundary: after the workspace runner applies its lockdown it
// re-execs with an empty capability set, so untrusted container code cannot
// re-add IPv4 or IPv6 default routes. Gated like the other Docker suites:
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

	// PID 1 carries the empty effective set only once the setup phase has
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

	gw := strings.TrimSpace(dockerOut(t, ctx, "network", "inspect",
		"-f", "{{(index .IPAM.Config 0).Gateway}}", netName))
	for _, tc := range []struct{ name, cmd string }{
		{"restore ipv4 default route", "ip route add default via " + gw + " dev eth0"},
		{"restore ipv6 default route", "ip -6 route add default via fe80::1 dev eth0"},
	} {
		out, err := exec.CommandContext(ctx, "docker", "exec", name, "sh", "-c", tc.cmd).CombinedOutput()
		if err == nil {
			t.Fatalf("%s: succeeded after the capability drop", tc.name)
		}
		if !strings.Contains(strings.ToLower(string(out)), "not permitted") {
			t.Fatalf("%s: failed for an unexpected reason (want EPERM): %v (%s)", tc.name, err, out)
		}
	}
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

	gw := strings.TrimSpace(dockerOut(t, ctx, "network", "inspect",
		"-f", "{{(index .IPAM.Config 0).Gateway}}", netName))
	add := "ip route add default via " + gw + " dev eth0"
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	var lastOut []byte
	for time.Now().Before(deadline) {
		lastOut, lastErr = exec.CommandContext(ctx, "docker", "exec", name, "sh", "-c", add).CombinedOutput()
		if lastErr == nil {
			_, _ = exec.CommandContext(ctx, "docker", "exec", name, "sh", "-c",
				"ip route del default via "+gw+" dev eth0").CombinedOutput()
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("positive control failed: a NET_ADMIN container could not add a route (environment problem, not the fix): %v (%s)", lastErr, lastOut)
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
