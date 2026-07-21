package workspace

import (
	"context"
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
