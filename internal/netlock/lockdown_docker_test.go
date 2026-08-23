package netlock_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestLockdownExceptHostBlocksExternal is a Docker-gated proof that the
// shipped LockdownExceptHost function (via internal/netlock/probe) makes
// raw external HTTP fail while the host broker remains reachable.
//
//	WAFFLE_TEST_DOCKER=1 go test ./internal/netlock -run TestLockdownExceptHostBlocksExternal -count=1 -v
func TestLockdownExceptHostBlocksExternal(t *testing.T) {
	if os.Getenv("WAFFLE_TEST_DOCKER") != "1" {
		t.Skip("set WAFFLE_TEST_DOCKER=1 to run Docker netlock integration")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH")
	}

	// Bind all interfaces: containers reach the broker through host-gateway,
	// which is not loopback on Linux CI runners.
	broker := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "broker-ok")
	}))
	l, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	broker.Listener = l
	broker.Start()
	t.Cleanup(broker.Close)
	_, port, err := net.SplitHostPort(broker.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	brokerURL := "http://waffle-host:" + port

	probeBin := buildLinuxNetlockProbe(t)

	netName := "waffle-netlock-test"
	ctx := context.Background()
	_ = exec.CommandContext(ctx, "docker", "network", "rm", netName).Run()
	if out, err := exec.CommandContext(ctx, "docker", "network", "create", netName).CombinedOutput(); err != nil {
		if !strings.Contains(strings.ToLower(string(out)), "already exists") {
			t.Fatalf("network create: %v\n%s", err, out)
		}
	}
	t.Cleanup(func() { _ = exec.Command("docker", "network", "rm", netName).Run() })

	name := "waffle-netlock-probe"
	_ = exec.CommandContext(ctx, "docker", "rm", "-f", name).Run()
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })

	args := []string{
		"run", "--rm", "--name", name,
		"--network", netName,
		"--cap-add", "NET_ADMIN",
		"--add-host", "waffle-host:host-gateway",
		"-v", probeBin + ":/probe:ro",
		"-e", "BROKER_URL=" + brokerURL,
		"-e", "EXTERNAL_URL=http://1.1.1.1/",
		"debian:bookworm-slim",
		"/probe",
	}
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("shipped netlock probe failed (no-op LockdownExceptHost must fail): %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "broker=ok") {
		t.Fatalf("want broker=ok in output:\n%s", out)
	}
	if !strings.Contains(string(out), "external=blocked") {
		t.Fatalf("want external=blocked in output:\n%s", out)
	}
}

func buildLinuxNetlockProbe(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	outBin := filepath.Join(dir, "probe")
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	modRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "../.."))
	cmd := exec.Command("go", "build", "-o", outBin, "./internal/netlock/probe")
	cmd.Dir = modRoot
	cmd.Env = append(os.Environ(), "GOOS=linux", "CGO_ENABLED=0", "GOARCH="+runtime.GOARCH)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build ./internal/netlock/probe: %v\n%s\nmodRoot=%s", err, out, modRoot)
	}
	return outBin
}
