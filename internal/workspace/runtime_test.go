package workspace

import (
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
