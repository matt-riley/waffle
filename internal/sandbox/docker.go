package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/matt-riley/waffle/internal/id"
	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/tool"
)

// hostGOOS is the OS the waffle process runs on. A package var so tests can
// simulate a non-linux host without cross-compiling.
var hostGOOS = runtime.GOOS

// ResolveRunnerBinary returns the path to bind-mount as the container's
// `waffle runner` entrypoint. If explicit is set (from [sandbox]
// runner_binary) it is validated and used. Otherwise the running binary is
// used — but only on a linux host: on any other OS the running binary is a
// non-linux executable that dies with "exec format error" the instant the
// container starts, which surfaces ~10s later as a misleading "runner appears
// dead" timeout. Refuse up front with an actionable message instead (#42).
func ResolveRunnerBinary(explicit string) (string, error) {
	if explicit != "" {
		if err := ValidateRunnerBinary(explicit); err != nil {
			return "", err
		}
		return explicit, nil
	}
	if hostGOOS != "linux" {
		return "", fmt.Errorf(
			"sandbox: the running waffle binary is built for %s, but docker mode bind-mounts a linux "+
				"binary as the container entrypoint; set [sandbox] runner_binary to an absolute path to a "+
				"static linux build whose GOARCH matches your container image "+
				"(e.g. CGO_ENABLED=0 GOOS=linux GOARCH=<image arch> go build -o /abs/path/waffle-linux ./cmd/waffle)",
			hostGOOS)
	}
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("sandbox: locate waffle binary: %w", err)
	}
	// os.Executable is not guaranteed absolute; a relative path would make
	// docker read the -v mount source as a named volume. Canonicalize to an
	// absolute, symlink-resolved path (best effort) so the bind mount points
	// at the real file.
	if abs, err := filepath.Abs(self); err == nil {
		self = abs
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	return self, nil
}

// ValidateRunnerBinary checks an explicit runner binary is usable as a docker
// bind-mount source before any docker call: it must be an absolute path (a
// bare name like "waffle-linux" is silently read as a named volume, not a
// file) to an existing regular file. It does not verify the binary is a linux
// executable of the right arch — that only shows up at container start — so
// the arch note in ResolveRunnerBinary/README still matters.
func ValidateRunnerBinary(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf(
			"sandbox: [sandbox] runner_binary %q must be an absolute path "+
				"(docker reads a bare name as a named volume, not a file)", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("sandbox: [sandbox] runner_binary %q is not accessible: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("sandbox: [sandbox] runner_binary %q is not a regular file", path)
	}
	return nil
}

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
	// Resource limits protect the host from runaway sandbox workloads.
	Memory string
	CPUs   float64
	PIDs   int
	Disk   string
	// BrokerURL and Token, when set, let in-sandbox tools reach the host-side
	// credential broker — never a raw key. BrokerURL is exported as
	// WAFFLE_BROKER (not secret). Token is written to a 0600 file under
	// QueueDir (SessionTokenFileName) and never passed via docker -e (#106).
	BrokerURL string
	Token     string
	// SelfPath overrides the waffle binary to mount (default: this one).
	SelfPath          string
	FetchAllowPrivate []string
}

const (
	DefaultMemoryLimit = "2g"
	DefaultCPULimit    = 2.0
	DefaultPIDLimit    = 512

	// SessionTokenFileName is the host-side filename under QueueDir that holds
	// the broker session token. The queue dir is bind-mounted at
	// ContainerQueueMount, so the in-container path is ContainerSessionTokenPath.
	SessionTokenFileName = "session.token"
	// ContainerQueueMount is the in-container mount point for opts.QueueDir.
	ContainerQueueMount = "/waffle/queue"
	// ContainerSessionTokenPath is the conventional in-container path of the
	// session token file (QueueDir/SessionTokenFileName on the host).
	ContainerSessionTokenPath = ContainerQueueMount + "/" + SessionTokenFileName
	// EnvSessionTokenFile is an optional path-only env var pointing at the
	// token file. The value is a filesystem path, not the secret itself.
	EnvSessionTokenFile = "WAFFLE_SESSION_TOKEN_FILE"
)

// WriteSessionToken writes token to queueDir/SessionTokenFileName with mode
// 0600. A empty token is a no-op. Call before docker run so the bind-mounted
// queue dir exposes the token without putting it in Config.Env (#106).
func WriteSessionToken(queueDir, token string) error {
	if token == "" {
		return nil
	}
	if queueDir == "" {
		return fmt.Errorf("sandbox: queue dir required to write session token")
	}
	if err := os.MkdirAll(queueDir, 0o700); err != nil {
		return fmt.Errorf("sandbox: create queue dir for session token: %w", err)
	}
	path := filepath.Join(queueDir, SessionTokenFileName)
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		return fmt.Errorf("sandbox: write session token: %w", err)
	}
	// Re-assert mode in case a restrictive umask is not the only concern —
	// the secret must not be group/world readable.
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("sandbox: chmod session token: %w", err)
	}
	return nil
}

// RemoveSessionToken deletes the session token file from queueDir if present.
func RemoveSessionToken(queueDir string) {
	if queueDir == "" {
		return
	}
	_ = os.Remove(filepath.Join(queueDir, SessionTokenFileName))
}

// DockerExecutor is a tool.Toolbox whose tools execute inside a container.
type DockerExecutor struct {
	client    *Client
	container string
	queueDir  string
	defs      []llm.Tool

	// Timeout bounds one tool call end to end (queue round trip included).
	Timeout time.Duration
}

// StartDocker launches the sandbox container and connects the queue.
func StartDocker(ctx context.Context, opts DockerOpts) (*DockerExecutor, error) {
	selfPath, err := ResolveRunnerBinary(opts.SelfPath)
	if err != nil {
		return nil, err
	}
	opts.SelfPath = selfPath
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

	client, err := NewClient(opts.QueueDir)
	if err != nil {
		return nil, err
	}

	// Deliver the broker session token via a restricted file on the queue
	// bind-mount — never via docker run -e, which lands in Config.Env and
	// process listings (#106).
	if err := WriteSessionToken(opts.QueueDir, opts.Token); err != nil {
		_ = client.Close()
		return nil, err
	}

	args := dockerRunArgs(name, opts)
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		if opts.Disk != "" && StorageOptUnsupported(string(out)) {
			opts.Disk = ""
			out, err = exec.CommandContext(ctx, "docker", dockerRunArgs(name, opts)...).CombinedOutput()
		}
		if err != nil {
			RemoveSessionToken(opts.QueueDir)
			_ = client.Close()
			return nil, fmt.Errorf("sandbox: docker run: %w\n%s", err, strings.TrimSpace(string(out)))
		}
	}
	client.startedAt = time.Now()

	return &DockerExecutor{
		client:    client,
		container: name,
		queueDir:  opts.QueueDir,
		defs:      tool.BuiltinsWithFetch(opts.FetchAllowPrivate).Defs(),
		Timeout:   DefaultToolTimeout, // > bash's 10-minute cap; dead-runner detection is faster
	}, nil
}

// StorageOptUnsupported reports a Docker error that means the configured
// disk quota is unavailable for the active storage driver.
func StorageOptUnsupported(output string) bool {
	lower := strings.ToLower(output)
	return strings.Contains(lower, "storage-opt") && (strings.Contains(lower, "not supported") || strings.Contains(lower, "unsupported"))
}

// dockerRunArgs builds the docker run invocation; separated for testing.
func dockerRunArgs(name string, opts DockerOpts) []string {
	memory, cpus, pids := DockerLimits(opts.Memory, opts.CPUs, opts.PIDs)
	args := []string{
		"run", "-d", "--rm",
		"--name", name,
		"--network", opts.Network,
		"--memory", memory,
		"--memory-swap", memory,
		"--cpus", fmt.Sprintf("%g", cpus),
		"--pids-limit", fmt.Sprintf("%d", pids),
		"--security-opt", "no-new-privileges",
		"-v", opts.SelfPath + ":/usr/local/bin/waffle:ro",
		"-v", opts.QueueDir + ":/waffle/queue",
	}
	if opts.WorkDir != "" {
		args = append(args, "-v", opts.WorkDir+":/work", "-w", "/work")
	}
	if opts.Disk != "" {
		args = append(args, "--storage-opt", "size="+opts.Disk)
	}
	if opts.BrokerURL != "" {
		args = append(args,
			"--add-host", "waffle-host:host-gateway",
			"-e", "WAFFLE_BROKER="+opts.BrokerURL,
		)
		// Path only — never the token value (#106).
		if opts.Token != "" {
			args = append(args, "-e", EnvSessionTokenFile+"="+ContainerSessionTokenPath)
		}
	}
	args = append(args, opts.Image, "/usr/local/bin/waffle", "runner", "--queue", "/waffle/queue")
	for _, entry := range opts.FetchAllowPrivate {
		args = append(args, "--fetch-allow-private", entry)
	}
	return args
}

// DockerLimits fills in the conservative defaults shared by sandbox and workspace containers.
func DockerLimits(memory string, cpus float64, pids int) (string, float64, int) {
	if memory == "" {
		memory = DefaultMemoryLimit
	}
	if cpus <= 0 {
		cpus = DefaultCPULimit
	}
	if pids <= 0 {
		pids = DefaultPIDLimit
	}
	return memory, cpus, pids
}

// Defs implements tool.Toolbox: the sandbox serves the builtin toolset.
func (d *DockerExecutor) Defs() []llm.Tool { return d.defs }

// Run implements tool.Toolbox by proxying over the queue.
func (d *DockerExecutor) Run(ctx context.Context, name string, input json.RawMessage) (string, error) {
	return d.run(ctx, "", name, input)
}

// RunWithID dispatches using the model's durable tool-call identity so a
// host crash or drain mid-tool can reclaim completed container work via
// RepairWithReclaim, mirroring QueueToolbox (#285).
func (d *DockerExecutor) RunWithID(ctx context.Context, useID, name string, input json.RawMessage) (string, error) {
	return d.run(ctx, useID, name, input)
}

func (d *DockerExecutor) run(ctx context.Context, useID, name string, input json.RawMessage) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, d.Timeout)
	defer cancel()
	content, isError, err := d.client.Exec(ctx, useID, name, input)
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
	return d.CloseContext(ctx)
}

// CloseContext stops the runner and removes the container under the caller's
// cleanup deadline. It creates no replacement timeout, so a chat manager's
// single shutdown window reaches the concrete Docker commands unchanged.
func (d *DockerExecutor) CloseContext(ctx context.Context) error {
	var closeErr error
	if err := d.client.Shutdown(ctx); err != nil && ctx.Err() != nil {
		closeErr = errors.Join(closeErr, ctx.Err())
	}
	out, err := exec.CommandContext(ctx, "docker", "rm", "-f", d.container).CombinedOutput()
	if err != nil && strings.Contains(string(out), "No such container:") {
		err = nil
	} else if err != nil {
		err = fmt.Errorf("sandbox: docker rm: %w\n%s", err, strings.TrimSpace(string(out)))
	}
	RemoveSessionToken(d.queueDir)
	return errors.Join(closeErr, err, d.client.CloseContext(ctx))
}

var _ tool.Toolbox = (*DockerExecutor)(nil)
var _ tool.CallerToolbox = (*DockerExecutor)(nil)
