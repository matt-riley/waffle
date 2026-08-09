package workspace

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/matt-riley/waffle/internal/sandbox"
)

func TestWorkspaceRunArgsUseAbsoluteBinaryPath(t *testing.T) {
	args := workspaceRunArgs(ContainerOpts{
		Name:      "waffle-ws-ab12",
		Image:     "buildpack-deps:bookworm-scm",
		Volume:    "waffle-vol",
		QueueDir:  "/home/u/.waffle/sandboxes/x",
		Network:   "bridge",
		SelfPath:  "/usr/local/bin/waffle",
		BrokerURL: "http://waffle-host:8421",
		Token:     "wk_abc",
	})
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--memory 2g",
		"--memory-swap 2g",
		"--cpus 2",
		"--pids-limit 512",
		"--security-opt no-new-privileges",
		"-e WAFFLE_BROKER=http://waffle-host:8421",
		"-e " + sandbox.EnvSessionTokenFile + "=" + sandbox.ContainerSessionTokenPath,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("workspace run args missing %q:\n%s", want, joined)
		}
	}
	if !strings.Contains(joined, "buildpack-deps:bookworm-scm /usr/local/bin/waffle runner --queue /waffle/queue") {
		t.Fatalf("workspace run args do not use absolute binary path:\n%s", joined)
	}
	// Session token must never appear as a docker -e value (#106).
	if strings.Contains(joined, "WAFFLE_SESSION_TOKEN=") {
		t.Errorf("workspace args must not pass WAFFLE_SESSION_TOKEN via -e:\n%s", joined)
	}
	if strings.Contains(joined, "wk_abc") {
		t.Errorf("workspace args must not contain the session token value:\n%s", joined)
	}
}

func TestContainerOptsCarriesRunnerBinary(t *testing.T) {
	m := &Manager{RunnerBinary: "/opt/waffle-linux"}
	ws := &Workspace{ID: "ws-1", Container: "waffle-ws-1", Volume: "waffle-ws-1", Image: "img"}
	opts := m.containerOpts(ws, "wk_tok", m.Egress)
	if opts.SelfPath != "/opt/waffle-linux" {
		t.Fatalf("containerOpts SelfPath = %q, want /opt/waffle-linux", opts.SelfPath)
	}
}

// TestEgressNetworkArgs asserts none/allowlist use the waffle-ws bridge (not
// Docker "none") so waffle-host:host-gateway works; full stays on bridge (#95).
func TestEgressNetworkArgs(t *testing.T) {
	ws := &Workspace{ID: "ws-1", Container: "waffle-ws-1", Volume: "waffle-ws-1", Image: "img"}
	cases := []struct {
		name      string
		egress    string
		network   string // Manager.Network override for full
		broker    string
		proxy     string
		wantNet   string
		wantHost  bool
		wantProxy bool
	}{
		{
			name:      "none default broker",
			egress:    "none",
			broker:    "http://waffle-host:8421",
			wantNet:   WorkspaceBrokerNetwork,
			wantHost:  true,
			wantProxy: true, // derived from BrokerURL
		},
		{
			name:      "empty egress is none",
			egress:    "",
			broker:    "http://waffle-host:8421",
			wantNet:   WorkspaceBrokerNetwork,
			wantHost:  true,
			wantProxy: true,
		},
		{
			name:      "allowlist with proxy",
			egress:    "allowlist",
			broker:    "http://waffle-host:8421",
			proxy:     "http://waffle-host:8421/egress",
			wantNet:   WorkspaceBrokerNetwork,
			wantHost:  true,
			wantProxy: true,
		},
		{
			name:      "full uses bridge",
			egress:    "full",
			broker:    "http://waffle-host:8421",
			wantNet:   "bridge",
			wantHost:  true, // broker still added when BrokerURL set
			wantProxy: false,
		},
		{
			name:      "full honors Manager.Network",
			egress:    "full",
			network:   "custom-bridge",
			broker:    "http://waffle-host:8421",
			wantNet:   "custom-bridge",
			wantHost:  true,
			wantProxy: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &Manager{
				Egress:    tc.egress,
				Network:   tc.network,
				BrokerURL: tc.broker,
				ProxyURL:  tc.proxy,
			}
			opts := m.containerOpts(ws, "wk_tok", m.Egress)
			if opts.Network != tc.wantNet {
				t.Fatalf("Network = %q, want %q", opts.Network, tc.wantNet)
			}
			if opts.Network == "none" {
				t.Fatal("workspace network must not be Docker mode \"none\" when broker is needed")
			}
			args := workspaceRunArgs(opts)
			joined := strings.Join(args, " ")
			if got := argValue(args, "--network"); got != tc.wantNet {
				t.Fatalf("--network = %q, want %q\n%s", got, tc.wantNet, joined)
			}
			hasHost := strings.Contains(joined, "--add-host waffle-host:host-gateway")
			if hasHost != tc.wantHost {
				t.Fatalf("add-host present=%v, want %v\n%s", hasHost, tc.wantHost, joined)
			}
			hasProxy := strings.Contains(joined, "HTTP_PROXY=")
			if hasProxy != tc.wantProxy {
				t.Fatalf("HTTP_PROXY present=%v, want %v\n%s", hasProxy, tc.wantProxy, joined)
			}
			if tc.wantProxy && !strings.Contains(joined, "NO_PROXY=waffle-host") {
				t.Fatalf("missing NO_PROXY=waffle-host:\n%s", joined)
			}
			if tc.wantProxy && !strings.Contains(joined, "http_proxy=") {
				t.Fatalf("missing lowercase http_proxy for busybox clients:\n%s", joined)
			}
		})
	}
}

func TestEgressNetworkHelper(t *testing.T) {
	if got := egressNetwork("none"); got != WorkspaceBrokerNetwork {
		t.Fatalf("none -> %q, want %q", got, WorkspaceBrokerNetwork)
	}
	if got := egressNetwork("allowlist"); got != WorkspaceBrokerNetwork {
		t.Fatalf("allowlist -> %q, want %q", got, WorkspaceBrokerNetwork)
	}
	if got := egressNetwork(""); got != WorkspaceBrokerNetwork {
		t.Fatalf("empty -> %q, want %q", got, WorkspaceBrokerNetwork)
	}
	if got := egressNetwork("full"); got != "bridge" {
		t.Fatalf("full -> %q, want bridge", got)
	}
	if WorkspaceBrokerNetwork == "none" {
		t.Fatal("WorkspaceBrokerNetwork must not be Docker mode none")
	}
}

func argValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// TestDockerWorkspaceBrokerHostReachable proves the none/allowlist network
// args can resolve waffle-host (not "Network unreachable"). Gated:
//
//	WAFFLE_TEST_DOCKER=1 go test ./internal/workspace -run TestDockerWorkspaceBrokerHostReachable -count=1 -v
func TestDockerWorkspaceBrokerHostReachable(t *testing.T) {
	if os.Getenv("WAFFLE_TEST_DOCKER") != "1" {
		t.Skip("set WAFFLE_TEST_DOCKER=1 to run Docker workspace network integration")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH")
	}
	ctx := context.Background()
	// Ensure the dedicated bridge exists the same way StartWorkspace does.
	if err := (DockerRuntime{}).ensureNetwork(ctx, WorkspaceBrokerNetwork); err != nil {
		t.Fatalf("ensure network: %v", err)
	}
	// Minimal probe container with workspace none-egress network args.
	name := "waffle-ws-netprobe-test"
	_ = exec.CommandContext(ctx, "docker", "rm", "-f", name).Run()
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", name).Run()
	})
	args := []string{
		"run", "--rm", "--name", name,
		"--network", WorkspaceBrokerNetwork,
		"--add-host", "waffle-host:host-gateway",
		"alpine:3.20",
		"getent", "hosts", "waffle-host",
	}
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("getent hosts waffle-host failed (host-gateway unreachable?): %v\n%s", err, out)
	}
	if strings.Contains(strings.ToLower(string(out)), "network unreachable") {
		t.Fatalf("waffle-host network unreachable:\n%s", out)
	}
	if strings.TrimSpace(string(out)) == "" {
		t.Fatalf("empty getent output for waffle-host")
	}
}

// TestDockerEgressNoneUsesShippedNetlock is the gated #95 isolation proof for
// workspace none egress: build a linux binary that calls the shipped
// netlock.LockdownExceptHost (not a shell reimplementation), run it with the
// same network/host-gateway/CAP_NET_ADMIN flags as workspaceRunArgs for none,
// and assert broker OK + raw external blocked.
//
//	WAFFLE_TEST_DOCKER=1 go test ./internal/workspace -run TestDockerEgressNoneUsesShippedNetlock -count=1 -v
func TestDockerEgressNoneUsesShippedNetlock(t *testing.T) {
	if os.Getenv("WAFFLE_TEST_DOCKER") != "1" {
		t.Skip("set WAFFLE_TEST_DOCKER=1 to run Docker workspace egress integration")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH")
	}

	broker := startMockBroker(t, nil)
	port := brokerPort(t, broker)
	brokerURL := "http://waffle-host:" + port

	ctx := context.Background()
	if err := (DockerRuntime{}).ensureNetwork(ctx, WorkspaceBrokerNetwork); err != nil {
		t.Fatalf("ensure network: %v", err)
	}

	m := &Manager{Egress: "none", BrokerURL: brokerURL, ProxyURL: brokerURL}
	ws := &Workspace{ID: "ws-none", Container: "waffle-ws-none-probe", Volume: "v", Image: "img"}
	opts := m.containerOpts(ws, "wk_test", m.Egress)
	if opts.Network != WorkspaceBrokerNetwork || !opts.NetLockdown {
		t.Fatalf("opts Network=%q NetLockdown=%v", opts.Network, opts.NetLockdown)
	}
	// Confirm production docker args include lockdown signals.
	joined := strings.Join(workspaceRunArgs(opts), " ")
	if !strings.Contains(joined, "NET_ADMIN") || !strings.Contains(joined, "WAFFLE_NET_LOCKDOWN=1") {
		t.Fatalf("workspaceRunArgs missing lockdown flags:\n%s", joined)
	}

	probe := buildLinuxNetlockProbe(t)
	name := "waffle-ws-shipped-netlock"
	_ = exec.CommandContext(ctx, "docker", "rm", "-f", name).Run()
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })

	args := []string{
		"run", "--rm", "--name", name,
		"--network", opts.Network, // WorkspaceBrokerNetwork
		"--cap-add", "NET_ADMIN",
		"--add-host", "waffle-host:host-gateway",
		"-v", probe + ":/probe:ro",
		"-e", "BROKER_URL=" + brokerURL,
		"-e", "EXTERNAL_URL=http://1.1.1.1/",
		"debian:bookworm-slim",
		"/probe",
	}
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("shipped netlock probe failed (a no-op LockdownExceptHost must fail this test): %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "broker=ok") {
		t.Fatalf("want broker=ok:\n%s", out)
	}
	if !strings.Contains(string(out), "external=blocked") {
		t.Fatalf("want external=blocked:\n%s", out)
	}
}

// TestDockerEgressAllowlistProxyPolicy is the gated #95 proof for allowlist
// HTTP proxy policy (broker face), independent of route lockdown.
//
//	WAFFLE_TEST_DOCKER=1 go test ./internal/workspace -run TestDockerEgressAllowlistProxyPolicy -count=1 -v
func TestDockerEgressAllowlistProxyPolicy(t *testing.T) {
	if os.Getenv("WAFFLE_TEST_DOCKER") != "1" {
		t.Skip("set WAFFLE_TEST_DOCKER=1 to run Docker workspace egress integration")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH")
	}
	broker := startMockBroker(t, map[string]bool{"example.com": true})
	port := brokerPort(t, broker)
	brokerURL := "http://waffle-host:" + port

	ctx := context.Background()
	if err := (DockerRuntime{}).ensureNetwork(ctx, WorkspaceBrokerNetwork); err != nil {
		t.Fatalf("ensure network: %v", err)
	}

	m := &Manager{Egress: "allowlist", BrokerURL: brokerURL, ProxyURL: brokerURL}
	ws := &Workspace{ID: "ws-al", Container: "waffle-ws-al-probe", Volume: "v", Image: "img"}
	opts := m.containerOpts(ws, "wk_test", m.Egress)
	if opts.Network != WorkspaceBrokerNetwork || !opts.NetLockdown {
		t.Fatalf("opts Network=%q NetLockdown=%v", opts.Network, opts.NetLockdown)
	}

	// Proxy allow/deny without shell theater: busybox wget + HTTP_PROXY only.
	name := "waffle-ws-proxy-policy"
	_ = exec.CommandContext(ctx, "docker", "rm", "-f", name).Run()
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })

	run := func(url string) ([]byte, error) {
		args := []string{
			"run", "--rm", "--name", name,
			"--network", opts.Network,
			"--add-host", "waffle-host:host-gateway",
			"-e", "HTTP_PROXY=" + brokerURL,
			"-e", "http_proxy=" + brokerURL,
			"-e", "NO_PROXY=waffle-host",
			"-e", "no_proxy=waffle-host",
			"alpine:3.20",
			"wget", "-q", "-O-", "--timeout=5", url,
		}
		return exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	}

	out, err := run("http://example.com/")
	if err != nil {
		t.Fatalf("allowlisted host should succeed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "allowed-ok") {
		t.Fatalf("body = %q, want allowed-ok", out)
	}
	out, err = run("http://not-allowlisted.example/")
	if err == nil {
		t.Fatalf("non-allowlisted host unexpectedly succeeded:\n%s", out)
	}
	msg := strings.ToLower(string(out) + err.Error())
	if !strings.Contains(msg, "403") && !strings.Contains(msg, "denied") && !strings.Contains(msg, "forbidden") {
		t.Fatalf("want proxy deny, got: %v\n%s", err, out)
	}
}

// startMockBroker serves as a host-side stand-in for the credential broker's
// HTTP face: GET / returns broker-ok; absolute proxy URLs are allowed only
// when host is in allow (nil/empty = deny all, matching egress=none).
func startMockBroker(t *testing.T, allow map[string]bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// HTTP proxy clients send absolute-form request lines.
		if r.URL.IsAbs() || strings.HasPrefix(r.URL.Path, "http://") || strings.HasPrefix(r.URL.Path, "https://") {
			host := strings.ToLower(r.URL.Hostname())
			if host == "" {
				// Path may be absolute form in RequestURI.
				u, err := url.Parse(r.RequestURI)
				if err == nil {
					host = strings.ToLower(u.Hostname())
				}
			}
			if allow != nil && allow[host] {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("allowed-ok"))
				return
			}
			http.Error(w, "egress host not allowlisted", http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("broker-ok"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func brokerPort(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Port()
}

// buildLinuxNetlockProbe compiles internal/netlock/probe (shipped LockdownExceptHost).
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
		t.Fatalf("go build ./internal/netlock/probe: %v\n%s", err, out)
	}
	return outBin
}

func TestContainerOptsNetLockdown(t *testing.T) {
	ws := &Workspace{ID: "ws-1", Container: "c", Volume: "v", Image: "img"}
	m := &Manager{Egress: "none", BrokerURL: "http://waffle-host:1"}
	opts := m.containerOpts(ws, "tok", m.Egress)
	if !opts.NetLockdown {
		t.Fatal("none should set NetLockdown")
	}
	args := workspaceRunArgs(opts)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--cap-add") || !strings.Contains(joined, "NET_ADMIN") {
		t.Fatalf("want NET_ADMIN: %s", joined)
	}
	if !strings.Contains(joined, "WAFFLE_NET_LOCKDOWN=1") {
		t.Fatalf("want lockdown env: %s", joined)
	}
	m.Egress = "full"
	opts = m.containerOpts(ws, "tok", m.Egress)
	if opts.NetLockdown {
		t.Fatal("full must not set NetLockdown")
	}
}

func TestGitHostFromURL(t *testing.T) {
	if got := gitHostFromURL("https://github.com/o/r.git"); got != "github.com" {
		t.Fatalf("got %q", got)
	}
	if got := gitHostFromURL("https://gitlab.example/a/b.git"); got != "gitlab.example" {
		t.Fatalf("got %q", got)
	}
}

// The broker session token is the proxy username. Git treats userinfo with no
// password as "ask the credential helper for one", and it asks against the
// proxy host -- which the helper refuses, because it only serves the repo's
// host. The clone then dies with "could not read Password for
// http://wk_...@waffle-host:8423". An explicit empty password stops git asking.
func TestWorkspaceProxyURLCarriesAnExplicitEmptyPassword(t *testing.T) {
	m := &Manager{
		Egress:    "none",
		Network:   "waffle-ws",
		BrokerURL: "http://waffle-host:8423",
		ProxyURL:  "http://waffle-host:8423/egress",
	}
	ws := &Workspace{ID: "ws-1", Repo: "owner/repo", Container: "c", Volume: "v", Image: "img"}
	args := workspaceRunArgs(m.containerOpts(ws, "wk_tok", m.Egress))
	joined := strings.Join(args, " ")

	want := "http://wk_tok:@waffle-host:8423/egress"
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		if !strings.Contains(joined, key+"="+want) {
			t.Fatalf("%s missing explicit empty password (want %s=%s):\n%s", key, key, want, joined)
		}
	}
	// The password-less form is what breaks the clone; it must not appear.
	if strings.Contains(joined, "wk_tok@waffle-host") {
		t.Fatalf("proxy URL still has userinfo without a password:\n%s", joined)
	}
}
