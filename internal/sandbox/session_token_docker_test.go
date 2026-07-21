//go:build sandbox_docker

package sandbox

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestDockerSessionTokenNotInInspectEnv asserts that the session-token delivery
// path used by dockerRunArgs does not place the token value in Config.Env
// (visible via docker inspect / process listings). Honest skip when Docker is
// unavailable (#106).
func TestDockerSessionTokenNotInInspectEnv(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not in PATH")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "docker", "info", "--format", "{{.ServerVersion}}").CombinedOutput(); err != nil {
		t.Skipf("docker daemon unavailable: %v (%s)", err, strings.TrimSpace(string(out)))
	}

	queueDir := t.TempDir()
	const token = "wk_inspect_must_not_leak"
	if err := WriteSessionToken(queueDir, token); err != nil {
		t.Fatal(err)
	}

	// Mirror the broker env shape from dockerRunArgs without requiring a
	// linux waffle binary: path-only TOKEN_FILE env + queue bind-mount.
	args := dockerRunArgs("unused", DockerOpts{
		Image:     "alpine:3.20",
		QueueDir:  queueDir,
		Network:   "none",
		SelfPath:  "/usr/local/bin/waffle",
		BrokerURL: "http://waffle-host:8421",
		Token:     token,
	})
	// Confirm the args builder itself never embeds the secret.
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "WAFFLE_SESSION_TOKEN=") || strings.Contains(joined, token) {
		t.Fatalf("dockerRunArgs leaked token into CLI args:\n%s", joined)
	}

	name := fmt.Sprintf("waffle-token-env-%d", time.Now().UnixNano())
	defer func() { _ = exec.Command("docker", "rm", "-f", name).Run() }()

	// Throwaway container with the same -e flags dockerRunArgs would emit.
	run := exec.CommandContext(ctx, "docker", "run", "-d", "--rm",
		"--name", name,
		"--network", "none",
		"-v", queueDir+":"+ContainerQueueMount,
		"-e", "WAFFLE_BROKER=http://waffle-host:8421",
		"-e", EnvSessionTokenFile+"="+ContainerSessionTokenPath,
		"alpine:3.20", "sleep", "30")
	if out, err := run.CombinedOutput(); err != nil {
		t.Fatalf("docker run: %v (%s)", err, strings.TrimSpace(string(out)))
	}

	inspect, err := exec.CommandContext(ctx, "docker", "inspect",
		"--format", "{{range .Config.Env}}{{println .}}{{end}}", name).CombinedOutput()
	if err != nil {
		t.Fatalf("docker inspect: %v (%s)", err, strings.TrimSpace(string(inspect)))
	}
	envBlock := string(inspect)
	if strings.Contains(envBlock, "WAFFLE_SESSION_TOKEN=") {
		t.Errorf("Config.Env contains WAFFLE_SESSION_TOKEN:\n%s", envBlock)
	}
	if strings.Contains(envBlock, token) {
		t.Errorf("Config.Env contains session token value:\n%s", envBlock)
	}
	if !strings.Contains(envBlock, EnvSessionTokenFile+"="+ContainerSessionTokenPath) {
		t.Errorf("Config.Env missing token-file path:\n%s", envBlock)
	}

	// Token is present on the bind-mounted queue, not in env.
	cat := exec.CommandContext(ctx, "docker", "exec", name, "cat", ContainerSessionTokenPath)
	got, err := cat.CombinedOutput()
	if err != nil {
		t.Fatalf("docker exec cat token: %v (%s)", err, strings.TrimSpace(string(got)))
	}
	if strings.TrimSpace(string(got)) != token {
		t.Errorf("container token file = %q, want %q", got, token)
	}
}
