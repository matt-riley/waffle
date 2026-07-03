package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/matt-riley/waffle/internal/id"
	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/tool"
)

// DockerOpts configures one sandbox container.
type DockerOpts struct {
	// Image is any Docker image — no waffle-specific image is needed,
	// because the waffle binary is bind-mounted in and runs as the
	// entrypoint (`waffle runner`). Requires a static (CGO_ENABLED=0)
	// linux build of waffle; the default toolchain settings produce one.
	Image string
	// QueueDir on the host holds the queue pair; mounted at /waffle/queue.
	QueueDir string
	// WorkDir on the host, mounted read-write at /work (optional).
	WorkDir string
	// Network is the docker network mode: "none" (default) or "bridge".
	Network string
	// BrokerURL and Token, when set, are exported into the container as
	// WAFFLE_BROKER / WAFFLE_SESSION_TOKEN so in-sandbox tools can reach
	// the host-side credential broker — never a raw key.
	BrokerURL string
	Token     string
	// SelfPath overrides the waffle binary to mount (default: this one).
	SelfPath string
}

// DockerExecutor is a tool.Toolbox whose tools execute inside a container.
type DockerExecutor struct {
	client    *Client
	container string
	defs      []llm.Tool

	// Timeout bounds one tool call end to end (queue round trip included).
	Timeout time.Duration
}

// StartDocker launches the sandbox container and connects the queue.
func StartDocker(ctx context.Context, opts DockerOpts) (*DockerExecutor, error) {
	if opts.SelfPath == "" {
		self, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("sandbox: locate waffle binary: %w", err)
		}
		opts.SelfPath = self
	}
	if opts.Image == "" {
		opts.Image = "debian:stable-slim"
	}
	if opts.Network == "" {
		opts.Network = "none"
	}

	suffix, err := id.NewBytes(4)
	if err != nil {
		return nil, fmt.Errorf("sandbox: new id: %w", err)
	}
	name := "waffle-sb-" + suffix

	args := dockerRunArgs(name, opts)
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("sandbox: docker run: %w\n%s", err, strings.TrimSpace(string(out)))
	}

	client, err := NewClient(opts.QueueDir)
	if err != nil {
		_ = exec.Command("docker", "rm", "-f", name).Run()
		return nil, err
	}
	return &DockerExecutor{
		client:    client,
		container: name,
		defs:      tool.Builtins().Defs(),
		Timeout:   11 * time.Minute, // > bash's 10-minute cap
	}, nil
}

// dockerRunArgs builds the docker run invocation; separated for testing.
func dockerRunArgs(name string, opts DockerOpts) []string {
	args := []string{
		"run", "-d", "--rm",
		"--name", name,
		"--network", opts.Network,
		"-v", opts.SelfPath + ":/usr/local/bin/waffle:ro",
		"-v", opts.QueueDir + ":/waffle/queue",
	}
	if opts.WorkDir != "" {
		args = append(args, "-v", opts.WorkDir+":/work", "-w", "/work")
	}
	if opts.BrokerURL != "" {
		args = append(args,
			"--add-host", "waffle-host:host-gateway",
			"-e", "WAFFLE_BROKER="+opts.BrokerURL,
			"-e", "WAFFLE_SESSION_TOKEN="+opts.Token,
		)
	}
	return append(args, opts.Image, "/usr/local/bin/waffle", "runner", "--queue", "/waffle/queue")
}

// Defs implements tool.Toolbox: the sandbox serves the builtin toolset.
func (d *DockerExecutor) Defs() []llm.Tool { return d.defs }

// Run implements tool.Toolbox by proxying over the queue.
func (d *DockerExecutor) Run(ctx context.Context, name string, input json.RawMessage) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, d.Timeout)
	defer cancel()
	content, isError, err := d.client.Exec(ctx, name, input)
	if err != nil {
		return "", err
	}
	if isError {
		return "", fmt.Errorf("%s", strings.TrimPrefix(content, "error: "))
	}
	return content, nil
}

// Close stops the runner and removes the container.
func (d *DockerExecutor) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = d.client.Shutdown(ctx)
	out, err := exec.CommandContext(ctx, "docker", "rm", "-f", d.container).CombinedOutput()
	if err != nil && strings.Contains(string(out), "No such container:") {
		err = nil
	} else if err != nil {
		err = fmt.Errorf("sandbox: docker rm: %w\n%s", err, strings.TrimSpace(string(out)))
	}
	return errors.Join(err, d.client.Close())
}

var _ tool.Toolbox = (*DockerExecutor)(nil)
