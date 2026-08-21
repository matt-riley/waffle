//go:build sandbox_docker

package sandbox

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestDockerPIDLimitContainsForkBomb exercises the cgroup limit, rather than
// merely asserting that waffle renders --pids-limit in an argument slice.
// The bomb is deliberately confined to a short-lived, low-limit container.
func TestDockerPIDLimitContainsForkBomb(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not in PATH")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "docker", "info", "--format", "{{.ServerVersion}}").CombinedOutput(); err != nil {
		t.Skipf("docker daemon unavailable: %v (%s)", err, strings.TrimSpace(string(out)))
	}

	const limit = 32
	name := fmt.Sprintf("waffle-pids-test-%d", time.Now().UnixNano())
	defer func() { _ = exec.Command("docker", "rm", "-f", name).Run() }()
	// Keep asking for processes after the limit is reached. Docker/cgroups
	// must reject those forks while the host and test process remain usable.
	out, err := exec.CommandContext(ctx, "docker", "run", "-d", "--rm",
		"--name", name, "--network", "none", "--pids-limit", strconv.Itoa(limit),
		"alpine:3.20", "sh", "-c", "while :; do sleep 300 & done").CombinedOutput()
	if err != nil {
		t.Fatalf("start contained fork bomb: %v (%s)", err, strings.TrimSpace(string(out)))
	}

	deadline := time.Now().Add(15 * time.Second)
	maxSeen := 0
	for time.Now().Before(deadline) {
		state, stateErr := exec.CommandContext(ctx, "docker", "inspect", "--format",
			"{{.State.Status}} exit={{.State.ExitCode}} err={{.State.Error}}", name).CombinedOutput()
		if stateErr != nil {
			logs, _ := exec.CommandContext(ctx, "docker", "logs", name).CombinedOutput()
			t.Fatalf("container %s vanished before pressure could be observed: %v (%s)\nlogs: %s",
				name, stateErr, strings.TrimSpace(string(state)), strings.TrimSpace(string(logs)))
		}
		top, topErr := exec.CommandContext(ctx, "docker", "top", name).CombinedOutput()
		if topErr == nil {
			lines := strings.Split(strings.TrimSpace(string(top)), "\n")
			processes := len(lines) - 1 // docker top includes one header row
			if processes > maxSeen {
				maxSeen = processes
			}
			if processes > limit {
				t.Fatalf("container escaped PID limit: observed %d processes, limit %d", processes, limit)
			}
			if processes >= limit-2 {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if maxSeen < 8 {
		logs, _ := exec.CommandContext(ctx, "docker", "logs", name).CombinedOutput()
		state, _ := exec.CommandContext(ctx, "docker", "inspect", "--format",
			"{{.State.Status}} exit={{.State.ExitCode}} err={{.State.Error}} oom={{.State.OOMKilled}}", name).CombinedOutput()
		t.Fatalf("fork workload did not create enough pressure: max=%d state=%s logs=%s",
			maxSeen, strings.TrimSpace(string(state)), strings.TrimSpace(string(logs)))
	}

	var configured string
	inspect, err := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{.HostConfig.PidsLimit}}", name).CombinedOutput()
	if err != nil {
		t.Fatalf("inspect PID limit: %v (%s)", err, strings.TrimSpace(string(inspect)))
	}
	configured = strings.TrimSpace(string(inspect))
	if configured != strconv.Itoa(limit) {
		t.Fatalf("configured PID limit = %q, want %d", configured, limit)
	}
	t.Logf("fork workload contained: max observed processes=%d configured limit=%d", maxSeen, limit)
}
