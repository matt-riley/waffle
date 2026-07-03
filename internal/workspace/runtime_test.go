package workspace

import (
	"strings"
	"testing"
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
	if !strings.Contains(joined, "buildpack-deps:bookworm-scm /usr/local/bin/waffle runner --queue /waffle/queue") {
		t.Fatalf("workspace run args do not use absolute binary path:\n%s", joined)
	}
}
