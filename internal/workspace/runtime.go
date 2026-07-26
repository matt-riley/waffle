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
	// ProxyURL is the host broker's egress proxy. Used for allowlist and
	// for none (when set) so proxy-aware clients hit the broker; the broker
	// remains the policy enforcement point.
	ProxyURL   string
	ProxyToken string
	// NetLockdown, when true, grants CAP_NET_ADMIN and sets WAFFLE_NET_LOCKDOWN
	// so the in-container runner drops the default route (host broker only).
	NetLockdown bool
	SelfPath    string // waffle binary to bind-mount
	Memory      string
	CPUs        float64
	PIDs        int
	Disk        string
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
	// Deliver broker session token via restricted file on the queue
	// bind-mount — never via docker run -e (#106).
	if err := sandbox.WriteSessionToken(opts.QueueDir, opts.Token); err != nil {
		return err
	}
	// Ensure the user-defined bridge for none/allowlist exists before run so
	// waffle-host:host-gateway can resolve (#95).
	if err := d.ensureNetwork(ctx, opts.Network); err != nil {
		return err
	}
	if err := d.docker(ctx, "volume", "create", opts.Volume); err != nil {
		return err
	}
	if err := d.docker(ctx, workspaceRunArgs(opts)...); err != nil {
		if opts.Disk != "" && sandbox.StorageOptUnsupported(err.Error()) {
			opts.Disk = ""
			return d.docker(ctx, workspaceRunArgs(opts)...)
		}
		return err
	}
	return nil
}

// ensureNetwork creates a user-defined Docker network when needed. Built-in
// modes (bridge/none/host) are left alone. Concurrent creates are ignored.
//
// For WorkspaceBrokerNetwork we prefer a normal bridge (not --internal):
// Docker's --internal mode also blocks host-gateway on Docker Desktop, which
// breaks the broker. Isolation for none/allowlist is enforced by the runner's
// netlock (drop default route, keep waffle-host) plus the broker egress proxy.
func (d DockerRuntime) ensureNetwork(ctx context.Context, name string) error {
	switch name {
	case "", "bridge", "none", "host":
		return nil
	}
	// If a stale network exists with the wrong shape, leave it; route lockdown
	// + proxy policy provide isolation regardless of Internal flag.
	out, err := exec.CommandContext(ctx, "docker", "network", "create", name).CombinedOutput()
	if err == nil {
		return nil
	}
	msg := strings.ToLower(string(out))
	if strings.Contains(msg, "already exists") {
		return nil
	}
	return fmt.Errorf("docker network create %s: %w\n%s", name, err, strings.TrimSpace(string(out)))
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
	}
	// none/allowlist: CAP_NET_ADMIN so the runner can drop the default route
	// while keeping waffle-host (broker) reachable (#95).
	if opts.NetLockdown {
		args = append(args, "--cap-add", "NET_ADMIN")
	}
	args = append(args,
		"-v", opts.SelfPath+":/usr/local/bin/waffle:ro",
		"-v", opts.QueueDir+":/waffle/queue",
		"-v", opts.Volume+":/work",
		"-w", "/work",
	)
	if opts.BrokerURL != "" || opts.ProxyURL != "" {
		args = append(args, "--add-host", "waffle-host:host-gateway")
		if opts.BrokerURL != "" {
			args = append(args, "-e", "WAFFLE_BROKER="+opts.BrokerURL)
			// Path only — never the token value (#106).
			if opts.Token != "" {
				args = append(args, "-e", sandbox.EnvSessionTokenFile+"="+sandbox.ContainerSessionTokenPath)
			}
		}
	}
	if opts.NetLockdown {
		args = append(args,
			"-e", "WAFFLE_NET_LOCKDOWN=1",
			"-e", "WAFFLE_NET_LOCKDOWN_HOST=waffle-host",
		)
	}
	if opts.ProxyURL != "" {
		proxyURL := opts.ProxyURL
		if opts.ProxyToken != "" {
			// Trailing colon: the token is the username and the password is
			// empty. Without it git sees a userinfo with no password, asks the
			// credential helper for one against the *proxy* host, and the
			// helper refuses every host but the repo's -- so the clone fails
			// with "could not read Password for http://wk_...@waffle-host".
			proxyURL = strings.Replace(proxyURL, "://", "://"+opts.ProxyToken+":@", 1)
		}
		// Uppercase and lowercase: curl/git honor the former; busybox wget
		// the latter. NO_PROXY keeps broker calls off the proxy path.
		noProxy := "waffle-host,localhost,127.0.0.1"
		args = append(args,
			"-e", "HTTP_PROXY="+proxyURL,
			"-e", "HTTPS_PROXY="+proxyURL,
			"-e", "ALL_PROXY="+proxyURL,
			"-e", "NO_PROXY="+noProxy,
			"-e", "http_proxy="+proxyURL,
			"-e", "https_proxy="+proxyURL,
			"-e", "all_proxy="+proxyURL,
			"-e", "no_proxy="+noProxy,
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
