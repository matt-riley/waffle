package workspace

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/matt-riley/waffle/internal/sandbox"
)

// ContainerOpts describes one workspace container. Unlike phase-4 sandbox
// containers these are not --rm: they stop on idle and start again on
// resume, keeping their named volume.
type ContainerOpts struct {
	Name      string
	Image     string
	Volume    string // named volume mounted at /work
	QueueDir  string // host queue dir mounted at /waffle/queue
	Network   string
	BrokerURL string
	Token     string
	SelfPath  string // waffle binary to bind-mount
	Memory    string
	CPUs      float64
	PIDs      int
	Disk      string
}

// Runtime abstracts the container engine so the lifecycle logic is
// testable without a Docker daemon.
type Runtime interface {
	StartWorkspace(ctx context.Context, opts ContainerOpts) error
	StopContainer(ctx context.Context, name string) error
	StartContainer(ctx context.Context, name string) error
	RemoveContainer(ctx context.Context, name string) error
	RemoveVolume(ctx context.Context, name string) error
}

// DockerRuntime drives the docker CLI.
type DockerRuntime struct{}

func (DockerRuntime) docker(ctx context.Context, args ...string) error {
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker %s: %w\n%s", args[0], err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (d DockerRuntime) StartWorkspace(ctx context.Context, opts ContainerOpts) error {
	// Resolve (and validate) the runner binary before creating the volume, so
	// a non-linux host without a configured runner_binary fails fast instead
	// of leaving an orphaned volume behind a dead container (#42).
	selfPath, err := sandbox.ResolveRunnerBinary(opts.SelfPath)
	if err != nil {
		return err
	}
	opts.SelfPath = selfPath
	if err := d.docker(ctx, "volume", "create", opts.Volume); err != nil {
		return err
	}
	if err := d.docker(ctx, workspaceRunArgs(opts)...); err != nil {
		if opts.Disk != "" && strings.Contains(strings.ToLower(err.Error()), "storage-opt") && strings.Contains(strings.ToLower(err.Error()), "not supported") {
			opts.Disk = ""
			return d.docker(ctx, workspaceRunArgs(opts)...)
		}
		return err
	}
	return nil
}

// workspaceRunArgs builds the docker run invocation; separated for testing.
func workspaceRunArgs(opts ContainerOpts) []string {
	memory, cpus, pids := sandbox.DockerLimits(opts.Memory, opts.CPUs, opts.PIDs)
	args := []string{
		"run", "-d",
		"--name", opts.Name,
		"--network", opts.Network,
		"--memory", memory,
		"--memory-swap", memory,
		"--cpus", fmt.Sprintf("%g", cpus),
		"--pids-limit", fmt.Sprintf("%d", pids),
		"--security-opt", "no-new-privileges",
		"-v", opts.SelfPath + ":/usr/local/bin/waffle:ro",
		"-v", opts.QueueDir + ":/waffle/queue",
		"-v", opts.Volume + ":/work",
		"-w", "/work",
	}
	if opts.BrokerURL != "" {
		args = append(args,
			"--add-host", "waffle-host:host-gateway",
			"-e", "WAFFLE_BROKER="+opts.BrokerURL,
			"-e", "WAFFLE_SESSION_TOKEN="+opts.Token,
		)
	}
	if opts.Disk != "" {
		args = append(args, "--storage-opt", "size="+opts.Disk)
	}
	return append(args, opts.Image, "/usr/local/bin/waffle", "runner", "--queue", "/waffle/queue")
}

func (d DockerRuntime) StopContainer(ctx context.Context, name string) error {
	return d.docker(ctx, "stop", name)
}

func (d DockerRuntime) StartContainer(ctx context.Context, name string) error {
	return d.docker(ctx, "start", name)
}

func (d DockerRuntime) RemoveContainer(ctx context.Context, name string) error {
	return d.docker(ctx, "rm", "-f", name)
}

func (d DockerRuntime) RemoveVolume(ctx context.Context, name string) error {
	return d.docker(ctx, "volume", "rm", name)
}
