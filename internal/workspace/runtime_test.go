package workspace

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
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
	opts := m.containerOpts(ws, "wk_tok")
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
			opts := m.containerOpts(ws, "wk_tok")
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

// TestDockerEgressNoneBrokerOKExternalDenied is the gated #95 proof for
// egress=none: host broker is reachable while a proxy-aware probe to an
// external host is denied by the broker (empty allowlist). Uses shipped
// containerOpts + workspaceRunArgs proxy/network settings.
//
//	WAFFLE_TEST_DOCKER=1 go test ./internal/workspace -run TestDockerEgressNoneBrokerOKExternalDenied -count=1 -v
func TestDockerEgressNoneBrokerOKExternalDenied(t *testing.T) {
	if os.Getenv("WAFFLE_TEST_DOCKER") != "1" {
		t.Skip("set WAFFLE_TEST_DOCKER=1 to run Docker workspace egress integration")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH")
	}
	// Mock host broker: direct GETs succeed; absolute proxy URLs are denied (none allowlist).
	broker := startMockBroker(t, nil)
	port := brokerPort(t, broker)
	brokerURL := "http://waffle-host:" + port

	ctx := context.Background()
	if err := (DockerRuntime{}).ensureNetwork(ctx, WorkspaceBrokerNetwork); err != nil {
		t.Fatalf("ensure network: %v", err)
	}

	m := &Manager{
		Egress:    "none",
		BrokerURL: brokerURL,
		ProxyURL:  brokerURL, // absolute-URL HTTP proxy face on same mock
	}
	ws := &Workspace{ID: "ws-none", Container: "waffle-ws-none-probe", Volume: "v", Image: "alpine:3.20"}
	opts := m.containerOpts(ws, "wk_test")
	if opts.Network != WorkspaceBrokerNetwork {
		t.Fatalf("Network = %q, want %q", opts.Network, WorkspaceBrokerNetwork)
	}
	if opts.ProxyURL == "" {
		t.Fatal("none egress must set ProxyURL for deny-all proxy path")
	}

	// 1) Broker reachable without proxy env (NO_PROXY path).
	out, err := dockerRunProbe(ctx, t, opts, []string{
		"wget", "-q", "-O-", "--timeout=5", brokerURL + "/",
	}, false)
	if err != nil {
		t.Fatalf("broker probe failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "broker-ok") {
		t.Fatalf("broker body = %q, want broker-ok", out)
	}

	// 2) External host via proxy must be denied under empty allowlist.
	out, err = dockerRunProbe(ctx, t, opts, []string{
		"wget", "-q", "-O-", "--timeout=5", "http://example.com/",
	}, true)
	if err == nil {
		t.Fatalf("external probe unexpectedly succeeded under none:\n%s", out)
	}
	msg := strings.ToLower(string(out) + err.Error())
	if !strings.Contains(msg, "403") && !strings.Contains(msg, "denied") && !strings.Contains(msg, "forbidden") {
		t.Fatalf("external probe error should be proxy deny, got: %v\n%s", err, out)
	}
}

// TestDockerEgressAllowlistDeniesNonAllowlistedHost is the gated #95 proof for
// allowlist: allowlisted host succeeds through the proxy; other hosts fail.
//
//	WAFFLE_TEST_DOCKER=1 go test ./internal/workspace -run TestDockerEgressAllowlistDeniesNonAllowlistedHost -count=1 -v
func TestDockerEgressAllowlistDeniesNonAllowlistedHost(t *testing.T) {
	if os.Getenv("WAFFLE_TEST_DOCKER") != "1" {
		t.Skip("set WAFFLE_TEST_DOCKER=1 to run Docker workspace egress integration")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH")
	}
	// Allow only example.com through the mock proxy.
	broker := startMockBroker(t, map[string]bool{"example.com": true})
	port := brokerPort(t, broker)
	brokerURL := "http://waffle-host:" + port

	ctx := context.Background()
	if err := (DockerRuntime{}).ensureNetwork(ctx, WorkspaceBrokerNetwork); err != nil {
		t.Fatalf("ensure network: %v", err)
	}

	m := &Manager{
		Egress:    "allowlist",
		BrokerURL: brokerURL,
		ProxyURL:  brokerURL,
	}
	ws := &Workspace{ID: "ws-al", Container: "waffle-ws-al-probe", Volume: "v", Image: "alpine:3.20"}
	opts := m.containerOpts(ws, "wk_test")

	// Allowlisted host through proxy → 200 allowed-ok.
	out, err := dockerRunProbe(ctx, t, opts, []string{
		"wget", "-q", "-O-", "--timeout=5", "http://example.com/",
	}, true)
	if err != nil {
		t.Fatalf("allowlisted host should succeed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "allowed-ok") {
		t.Fatalf("allowlisted body = %q, want allowed-ok", out)
	}

	// Non-allowlisted host → 403.
	out, err = dockerRunProbe(ctx, t, opts, []string{
		"wget", "-q", "-O-", "--timeout=5", "http://not-allowlisted.example/",
	}, true)
	if err == nil {
		t.Fatalf("non-allowlisted host unexpectedly succeeded:\n%s", out)
	}
	msg := strings.ToLower(string(out) + err.Error())
	if !strings.Contains(msg, "403") && !strings.Contains(msg, "denied") && !strings.Contains(msg, "forbidden") {
		t.Fatalf("non-allowlisted error should be proxy deny, got: %v\n%s", err, out)
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

// dockerRunProbe runs a one-shot alpine container using the network / host
// alias / proxy settings from shipped workspaceRunArgs. withProxy applies
// the proxy env from opts; without proxy, only network+host alias (broker
// direct). Command is the container argv after the image.
func dockerRunProbe(ctx context.Context, t *testing.T, opts ContainerOpts, command []string, withProxy bool) ([]byte, error) {
	t.Helper()
	name := opts.Name + "-probe"
	_ = exec.CommandContext(ctx, "docker", "rm", "-f", name).Run()
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })

	// Build from shipped args builder, then rewrite to a foreground probe
	// (no -d, no volume mounts, alpine command override).
	probeOpts := opts
	probeOpts.Name = name
	probeOpts.Image = "alpine:3.20"
	probeOpts.Volume = ""
	probeOpts.QueueDir = ""
	probeOpts.SelfPath = ""
	probeOpts.Token = ""
	if !withProxy {
		probeOpts.ProxyURL = ""
		probeOpts.ProxyToken = ""
	}
	// Minimal args matching production network posture.
	args := []string{"run", "--rm", "--name", name, "--network", probeOpts.Network}
	args = append(args, "--add-host", "waffle-host:host-gateway")
	if withProxy && probeOpts.ProxyURL != "" {
		proxyURL := probeOpts.ProxyURL
		if probeOpts.ProxyToken != "" {
			proxyURL = strings.Replace(proxyURL, "://", "://"+probeOpts.ProxyToken+"@", 1)
		}
		noProxy := "waffle-host,localhost,127.0.0.1"
		for _, e := range []string{
			"HTTP_PROXY=" + proxyURL,
			"HTTPS_PROXY=" + proxyURL,
			"ALL_PROXY=" + proxyURL,
			"NO_PROXY=" + noProxy,
			"http_proxy=" + proxyURL,
			"https_proxy=" + proxyURL,
			"all_proxy=" + proxyURL,
			"no_proxy=" + noProxy,
		} {
			args = append(args, "-e", e)
		}
	}
	args = append(args, "alpine:3.20")
	args = append(args, command...)
	return exec.CommandContext(ctx, "docker", args...).CombinedOutput()
}
