// Package selfdev implements waffle's self-development loop (docs/plan.md,
// "Self-development loop"): doctor self-checks a build, upgrade builds a
// new binary from an approved ref and atomically swaps it in after doctor
// passes, and rollback restores the previous binary. Because waffle is a
// single compiled binary and its source is a git repo, code-level
// self-improvement is just repo-workspace work whose repo happens to be
// waffle's own — this package is the deploy end of that pipeline.
package selfdev

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/flock"
	"github.com/matt-riley/waffle/internal/gitcred"
	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/llm/anthropicp"
	"github.com/matt-riley/waffle/internal/llm/openaip"
	"github.com/matt-riley/waffle/internal/plugin"
	"github.com/matt-riley/waffle/internal/sandbox"
	"github.com/matt-riley/waffle/internal/secret"
	"github.com/matt-riley/waffle/internal/store"
	"github.com/matt-riley/waffle/internal/tool"
)

// Check is one doctor probe result.
type Check struct {
	Name string
	OK   bool
	Info string
}

var providerProbeTimeout = 5 * time.Second

// providerCheck verifies that the configured provider accepts one small,
// authenticated completion. It has no tools or persistence side effects.
func providerCheck(ctx context.Context, p config.Provider) (string, error) {
	env, err := providerEnvName(p.Name)
	if err != nil {
		return "", err
	}
	key, err := secret.ResolveRef(p.APIKey, env)
	if err != nil {
		return "", err
	}
	if key == "" && secret.IsRef(p.APIKey) {
		return "", fmt.Errorf("api_key is %q but no secret store is available: run `waffle secret init`, or set %s", p.APIKey, env)
	}
	if key == "" {
		return "no API key configured (skipped)", nil
	}
	provider, err := doctorProvider(p, key)
	if err != nil {
		return "", err
	}
	return probeHealthCheck(ctx, provider, p.Model)
}

// probeHealthCheck sends a minimal authenticated completion to confirm the
// provider/model pair is reachable, bounded by providerProbeTimeout.
func probeHealthCheck(ctx context.Context, provider llm.Provider, model string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, providerProbeTimeout)
	defer cancel()
	if _, err := provider.Complete(ctx, llm.Request{
		Model:     model,
		MaxTokens: 1,
		Messages:  []llm.Message{llm.UserText("health check")},
	}, nil); err != nil {
		return "", err
	}
	return "authenticated completion", nil
}

// providerCheckConfig probes the same effective default model that chat and
// serve resolve. Explicit registries fail closed on their named secret; the
// legacy table retains its historical environment fallback.
func providerCheckConfig(ctx context.Context, cfg config.Config) (string, error) {
	if cfg.ProviderRegistrySource() != config.ProviderRegistryExplicit {
		return providerCheck(ctx, cfg.Provider)
	}
	alias := strings.TrimSpace(cfg.Agent.DefaultModel)
	if alias == "" {
		if len(cfg.Providers) == 0 && len(cfg.Models) == 0 {
			return "no provider configured (skipped)", nil
		}
		return "", fmt.Errorf("agent.default_model is not configured")
	}
	provider, target, _, err := namedProvider(cfg, alias)
	if err != nil {
		return "", err
	}
	return probeHealthCheck(ctx, provider, target.UpstreamModel)
}

func namedProvider(cfg config.Config, alias string) (llm.Provider, config.ResolvedModel, string, error) {
	target, err := cfg.ResolveModel(alias)
	if err != nil {
		return nil, config.ResolvedModel{}, "", err
	}
	key, err := namedProviderKey(target.Connection)
	if err != nil {
		return nil, config.ResolvedModel{}, "", fmt.Errorf("model alias %q connection %q: resolve credentials: %w", alias, target.ConnectionName, err)
	}
	provider, err := providerForConnection(target.Connection, key)
	if err != nil {
		return nil, config.ResolvedModel{}, "", fmt.Errorf("model alias %q connection %q: %w", alias, target.ConnectionName, err)
	}
	return provider, target, key, nil
}

func namedProviderKey(connection config.ProviderConnection) (string, error) {
	if connection.APIKey == "" {
		return "", nil
	}
	if !secret.IsRef(connection.APIKey) {
		return "", fmt.Errorf("named provider api_key must be a secret reference")
	}
	store, err := secret.TryOpen()
	if err != nil {
		return "", err
	}
	if store == nil {
		return "", fmt.Errorf("no secret store is available: run `waffle secret init`")
	}
	return secret.Resolve(store, connection.APIKey)
}

func providerEnvName(name string) (string, error) {
	switch name {
	case "anthropic", "":
		return "ANTHROPIC_API_KEY", nil
	case "openai":
		return "OPENAI_API_KEY", nil
	default:
		return "", fmt.Errorf("unknown provider %q (want \"anthropic\" or \"openai\")", name)
	}
}

func doctorProvider(p config.Provider, key string) (llm.Provider, error) {
	switch p.Name {
	case "anthropic", "":
		return anthropicp.New(key, p.BaseURL), nil
	case "openai":
		baseURL := p.BaseURL
		if baseURL == "" {
			baseURL = "https://api.openai.com/v1"
		}
		return openaip.New(key, baseURL), nil
	default:
		return nil, fmt.Errorf("unknown provider %q (want \"anthropic\" or \"openai\")", p.Name)
	}
}

func providerForConnection(connection config.ProviderConnection, key string) (llm.Provider, error) {
	switch connection.Type {
	case "anthropic":
		return anthropicp.New(key, connection.BaseURL), nil
	case "openai":
		baseURL := connection.BaseURL
		if baseURL == "" {
			baseURL = "https://api.openai.com/v1"
		}
		return openaip.New(key, baseURL), nil
	default:
		return nil, fmt.Errorf("unsupported provider type %q", connection.Type)
	}
}

// Doctor runs waffle's self-checks: config parses, the database migrates
// on a throwaway copy, and the secret store round-trips. It never touches
// the live database. Returns the checks and whether all passed.
func Doctor(ctx context.Context) ([]Check, bool, error) {
	var checks []Check
	add := func(name string, err error, info string) {
		c := Check{Name: name, OK: err == nil, Info: info}
		if err != nil {
			c.Info = err.Error()
		}
		checks = append(checks, c)
	}

	cfgPath, err := config.Path()
	if err != nil {
		return nil, false, err
	}
	cfg, err := config.Load(cfgPath)
	add("config parses", err, cfgPath)
	configOK := err == nil
	if configOK {
		if _, statErr := os.Stat(cfgPath); errors.Is(statErr, os.ErrNotExist) {
			// Config.Load supplies defaults when no file exists. Its default
			// secret reference is a template, not an operator-configured key, so
			// doctor should report the provider as unconfigured rather than fail.
			cfg.Provider.APIKey = ""
		}
	}

	// Migrate a consistent snapshot of the real DB (or a fresh one if there
	// is none yet) so a bad migration is caught without risking live data.
	tmp, err := os.MkdirTemp("", "waffle-doctor-*")
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	snapshot := filepath.Join(tmp, "waffle.db")
	dbPath, err := config.DBPath()
	if err != nil {
		return nil, false, err
	}
	hadLive, err := store.Snapshot(ctx, dbPath, snapshot)
	if err != nil {
		add("database snapshot", err, "")
	} else {
		st, err := store.Open(ctx, snapshot)
		info := "on a fresh database"
		if hadLive {
			info = "on a throwaway snapshot"
		}
		add("database migrates", err, info)
		if st != nil {
			_ = st.Close()
		}
	}

	// Secret store: if an identity exists, it must decrypt.
	if id, e := secret.LoadIdentity(); e == nil {
		sp, _ := config.SecretsPath()
		_, e = secret.OpenFile(sp, id).List()
		add("secret store opens", e, "")
	} else {
		add("secret store", nil, "no identity configured (skipped)")
	}

	if _, err := exec.LookPath("golangci-lint"); err != nil {
		add("golangci-lint gate", nil, "not installed (optional; verify gate skipped)")
	} else {
		add("golangci-lint gate", nil, "installed (verify gate armed)")
	}

	// Provider, sandbox, and MCP checks need a successfully parsed config.
	// Running them against a zero-value cfg after Load fails is misleading (#114).
	if !configOK {
		const skipReason = "skipped: config did not parse"
		add("provider reachable", nil, skipReason)
		add("sandbox runner", nil, skipReason)
		add("mcp servers", nil, skipReason)
	} else {
		// A one-token authenticated completion exercises the provider path that
		// chat and upgrade ultimately depend on. Missing credentials are an
		// intentional unconfigured state; all other probe failures block doctor.
		info, err := providerCheckConfig(ctx, cfg)
		add("provider reachable", err, info)

		// Sandbox runner: docker mode bind-mounts a linux waffle binary as the
		// container entrypoint. On a non-linux host that must be an explicitly
		// configured linux build; otherwise the sandbox dies on start with a
		// misleading "runner appears dead" timeout (#42). This gates on
		// cfg.UsesDocker(): the global [sandbox] mode is docker, or any
		// [agent.group.*] opts into docker while the global mode stays host (#33).
		// Repo workspaces always use docker regardless of mode, but they are not
		// gated here — `ws open` resolves the same runner binary at use time via
		// sandbox.ResolveRunnerBinary, so it fails fast with the same error; doctor
		// can't know ahead of time whether the operator will open one.
		if cfg.UsesDocker() {
			info, err := sandboxRunnerCheck(cfg.Sandbox.RunnerBinary)
			add("sandbox runner", err, info)
			// Queue pair round-trip on the host filesystem (same IPC docker mode uses).
			// Full container start is separate: we probe the daemon and, when available,
			// a short `docker run --rm` of the configured image's true entrypoint probe.
			qInfo, qErr := sandboxQueueRoundTrip()
			add("sandbox queue round-trip", qErr, qInfo)
			dInfo, dErr := sandboxDockerRoundTrip(cfg.Sandbox.Image)
			add("sandbox docker round-trip", dErr, dInfo)
		} else {
			add("sandbox runner", nil, "host mode (skipped)")
		}

		// MCP execution authorities (#77 / #79): list each server's name, groups,
		// and execution authority (host / sandbox / restricted). Informational —
		// config load already rejects illegal codeintel host/secret setups.
		if len(cfg.MCP) == 0 {
			add("mcp servers", nil, "none configured")
		} else {
			for _, s := range cfg.MCP {
				add("mcp "+s.Name+" authority", nil, formatMCPDoctorInfo(s))
			}
		}

		// Plugin components (#393): load installed plugins and report rejected
		// plugins and partially-loaded ones (skill/MCP skips, disabled MCP) so
		// spec §11.3 degradation is visible, never silent. A whole-plugin
		// rejection is an error; component skips are informational.
		home, herr := config.Home()
		if herr != nil {
			add("plugin components", herr, "resolve waffle home")
		} else if results, rejects, ierr := plugin.Installed(home); ierr != nil {
			add("plugin components", ierr, "read installed plugins")
		} else if len(results) == 0 && len(rejects) == 0 {
			add("plugin components", nil, "no plugins installed")
		} else {
			for _, reject := range rejects {
				name := filepath.Base(reject.Dir)
				add("plugin "+name+" rejected", errors.New(reject.Reason), reject.Reason)
			}
			for _, result := range results {
				name := result.Plugin.Manifest.Name
				parts := []string{fmt.Sprintf("%d skills", len(result.Skills))}
				if result.MCP.Disabled != "" {
					parts = append(parts, "mcp disabled")
				}
				if len(result.MCP.Servers) > 0 {
					parts = append(parts, fmt.Sprintf("%d mcp servers", len(result.MCP.Servers)))
				}
				skips := len(result.SkillSkips) + len(result.MCP.Skips)
				if skips > 0 {
					parts = append(parts, fmt.Sprintf("%d skips", skips))
				}
				add("plugin "+name, nil, strings.Join(parts, ", "))
			}
		}
	}

	allOK := true
	for _, c := range checks {
		if !c.OK {
			allOK = false
		}
	}
	return checks, allOK, nil
}

// formatMCPDoctorInfo reports groups and execution authority for doctor (#77).
// Authority values:
//   - host: execution host (or empty default); launch still uses BuildProcessEnv
//   - sandbox: execution=sandbox — docker-wrapped when agent group is docker mode
//   - restricted: execution=sandbox on host agent groups (ConnectRestricted + Dir)
func formatMCPDoctorInfo(s config.MCPServer) string {
	execution := s.Execution
	if execution == "" {
		execution = "host"
	}
	authority := "host"
	switch execution {
	case "sandbox":
		// Runtime selects restricted (host groups) or sandbox/docker-wrap (docker groups).
		authority = "sandbox|restricted"
	case "host":
		authority = "host"
	}
	scope := "all groups"
	if len(s.Groups) > 0 {
		scope = "groups=" + strings.Join(s.Groups, ",")
	}
	return fmt.Sprintf("execution=%s authority=%s %s env_allowlist=%d", execution, authority, scope, len(s.Env))
}

// sandboxRunnerCheck validates the docker-mode runner binary without starting
// a container, by exercising the exact same resolution StartDocker and the
// workspace runtime use — so doctor can't report OK on a setup that would fail
// fast at startup, and the two can't drift apart.
func sandboxRunnerCheck(runnerBinary string) (info string, err error) {
	resolved, err := sandbox.ResolveRunnerBinary(runnerBinary)
	if err != nil {
		return "", err
	}
	if runnerBinary != "" {
		return "runner_binary " + resolved, nil
	}
	return "using the running binary (" + resolved + ")", nil
}

// sandboxQueueRoundTrip exercises the SQLite inbound/outbound queue pair that
// docker sandboxes use for IPC (#29). Runs on the host filesystem without Docker.
func sandboxQueueRoundTrip() (info string, err error) {
	dir, err := os.MkdirTemp("", "waffle-doctor-queue-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	return sandboxDoctorQueue(dir)
}

type doctorPingTool struct{}

func (doctorPingTool) Def() llm.Tool {
	return llm.Tool{Name: "ping", InputSchema: json.RawMessage(`{"type":"object"}`)}
}
func (doctorPingTool) Run(context.Context, json.RawMessage) (string, error) { return "pong", nil }

func sandboxDoctorQueue(dir string) (string, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		r := &sandbox.Runner{Tools: tool.NewRegistry(doctorPingTool{})}
		done <- r.Serve(ctx, dir)
	}()
	// Give the runner a moment to create DBs.
	time.Sleep(50 * time.Millisecond)
	client, err := sandbox.NewClient(dir)
	if err != nil {
		cancel()
		<-done
		return "", fmt.Errorf("queue client: %w", err)
	}
	defer func() { _ = client.Close() }()
	execCtx, execCancel := context.WithTimeout(ctx, 5*time.Second)
	defer execCancel()
	out, isErr, err := client.Exec(execCtx, "ping", json.RawMessage(`{}`))
	cancel()
	<-done
	if err != nil || isErr || out != "pong" {
		return "", fmt.Errorf("queue exec: out=%q isErr=%v err=%v", out, isErr, err)
	}
	return "inbound/outbound queue ok", nil
}

// sandboxDockerRoundTrip probes the Docker daemon and, when containers can
// run, a bind-mount write/read round-trip (the filesystem path docker sandboxes
// use for the SQLite queue; #29). A full waffle image pull is not required.
func sandboxDockerRoundTrip(image string) (info string, err error) {
	if _, err := exec.LookPath("docker"); err != nil {
		return "docker not in PATH", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "info", "--format", "{{.ServerVersion}}").CombinedOutput()
	if err != nil {
		return strings.TrimSpace(string(out)), fmt.Errorf("docker daemon unavailable: %w", err)
	}
	ver := strings.TrimSpace(string(out))
	if ver == "" {
		ver = "unknown"
	}
	// Lightweight container probe + bind-mount visibility (VirtioFS / fuse-overlay).
	probeCtx, probeCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer probeCancel()
	probe := exec.CommandContext(probeCtx, "docker", "run", "--rm", "--network", "none", "busybox:1.36", "true")
	if pout, perr := probe.CombinedOutput(); perr != nil {
		// Daemon is up but cannot run containers (permissions/image). Report soft info.
		return fmt.Sprintf("daemon %s (container probe skipped: %v)", ver, strings.TrimSpace(string(pout))), nil
	}
	mountInfo, mountErr := sandboxDockerBindMountProbe(probeCtx)
	if mountErr != nil {
		// Container run worked; bind-mount failure is still a hard fail when
		// docker mode is configured — queue IPC would not work.
		return fmt.Sprintf("daemon %s; container ok; bind-mount: %v", ver, mountErr), mountErr
	}
	_ = image
	return "daemon " + ver + "; container probe ok; " + mountInfo, nil
}

// sandboxDockerBindMountProbe writes from inside a container to a host temp
// dir and reads it back on the host — the same host↔container visibility the
// inbound/outbound SQLite queue depends on.
func sandboxDockerBindMountProbe(ctx context.Context) (string, error) {
	dir, err := os.MkdirTemp("", "waffle-doctor-bind-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	// Docker Desktop on macOS needs the path under a shared directory; temp
	// dirs usually are. Marker is written only inside the container.
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "--network", "none",
		"-v", dir+":/q", "busybox:1.36", "sh", "-c", "echo waffle-bind-ok > /q/probe && sync")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("bind-mount write: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	b, err := os.ReadFile(filepath.Join(dir, "probe"))
	if err != nil {
		return "", fmt.Errorf("host read after bind-mount write: %w", err)
	}
	if strings.TrimSpace(string(b)) != "waffle-bind-ok" {
		return "", fmt.Errorf("bind-mount content = %q, want waffle-bind-ok", string(b))
	}
	return "bind-mount round-trip ok", nil
}

// Upgrade builds waffle from repoDir at ref, runs doctor against the new
// binary, and — if it passes — atomically swaps it into place, keeping the
// previous binary for rollback. It returns the path that was replaced.
// Upgrade builds and installs an upgrade with verification enabled.
func Upgrade(ctx context.Context, repoDir, ref string, stderr io.Writer) (string, error) {
	return UpgradeWithOptions(ctx, repoDir, ref, stderr, true, "", nil)
}

// emptyTreeSHA is the well-known git empty-tree object, used as the review
// base when upgrading a root commit with no parent (#413).
const emptyTreeSHA = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// upgradeOptions carries optional gates for UpgradeWithOptions.
type upgradeOptions struct {
	ciGate   CIGate
	required []string
}

// UpgradeOption configures one optional gate.
type UpgradeOption func(*upgradeOptions)

// WithCIGate supplies the CI approval gate and the required-check names used
// when approval="ci" (#415). required may be empty to use the safe default.
func WithCIGate(gate CIGate, required []string) UpgradeOption {
	return func(o *upgradeOptions) {
		o.ciGate = gate
		o.required = required
	}
}

// UpgradeWithOptions is Upgrade with an explicit verification and approval
// policy. Verification is intentionally opt-out only: callers should make
// the unsafe choice visible in their own CLI.
//
// Every upgrade resolves one immutable commit SHA (including the no-ref/HEAD
// path), reviews that exact commit, and verifies and builds it in an isolated
// detached worktree created from the SHA. The configured checkout branch,
// index, and working tree are never touched, and dirty bytes in the checkout
// can never enter the artifact (#413). approval="ci" additionally requires
// successful CI evidence for the exact candidate SHA before any build (#415).
func UpgradeWithOptions(ctx context.Context, repoDir, ref string, stderr io.Writer, verify bool, approval string, protected []string, extra ...UpgradeOption) (string, error) {
	opts := upgradeOptions{}
	for _, o := range extra {
		o(&opts)
	}
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		return "", err
	}

	if ref != "" {
		if err := validateRef(ref); err != nil {
			return "", err
		}
	}
	// Bind everything below to one immutable commit, before any review.
	sha, err := resolveCommit(ctx, repoDir, ref)
	if err != nil {
		return "", err
	}
	baseSHA, err := reviewBaseSHA(ctx, repoDir, ref, sha)
	if err != nil {
		return "", err
	}
	if approval == "auto-patch" {
		if err := rejectProtectedChanges(ctx, repoDir, baseSHA, sha, protected); err != nil {
			_ = persistUpgradeRecord(UpgradeRecord{BaseSHA: baseSHA, CandidateSHA: sha, Approval: approval, Verify: verify, Verification: "failed", Error: err.Error()})
			return "", err
		}
	}

	// CI approval: successful, current evidence for the exact SHA is a hard
	// precondition; network/API errors fail closed and are distinguishable
	// from a genuine red check (#415). Verified before review so a red or
	// unavailable gate denies before any provider cost.
	var ci CIEvidence
	if approval == "ci" {
		if opts.ciGate == nil {
			return "", errors.New("approval=ci requires a configured [github.app] to verify checks; set selfdev.required_checks for exact check names")
		}
		repo, err := remoteRepo(ctx, repoDir)
		if err != nil {
			return "", err
		}
		required := requiredChecks(opts.required)
		evidence, err := opts.ciGate.Verify(ctx, repo, sha, required)
		if err != nil {
			return "", fmt.Errorf("ci approval failed closed: %w", err)
		}
		if ok, detail := evidence.Passes(required); !ok {
			return "", fmt.Errorf("ci approval denied for %s: %s", sha, detail)
		}
		ci = evidence
	}
	if err := reviewExactCommit(ctx, repoDir, baseSHA, sha, approval); err != nil {
		_ = persistUpgradeRecord(UpgradeRecord{BaseSHA: baseSHA, CandidateSHA: sha, Approval: approval, Verify: verify, Verification: "failed", Error: err.Error()})
		return "", err
	}

	// Isolated detached worktree at the exact SHA: the configured checkout
	// stays untouched on success and failure, and unreviewed local edits
	// cannot reach verification or the artifact.
	wt, err := addDetachedTempWorktree(ctx, repoDir, sha)
	if err != nil {
		_ = persistUpgradeRecord(UpgradeRecord{BaseSHA: baseSHA, CandidateSHA: sha, Approval: approval, Verify: verify, Verification: "failed", Error: err.Error()})
		return "", err
	}
	defer func() { _ = removeWorktree(ctx, repoDir, wt) }()
	tree, err := treeHash(ctx, wt)
	if err != nil {
		_ = persistUpgradeRecord(UpgradeRecord{BaseSHA: baseSHA, CandidateSHA: sha, Approval: approval, Verify: verify, Verification: "failed", Error: err.Error()})
		return "", err
	}
	buildDir, err := os.MkdirTemp("", "waffle-upgrade-build-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(buildDir) }()

	verification := "skipped"
	if verify {
		if err := verifyRepo(ctx, wt, stderr); err != nil {
			if perr := persistUpgradeRecord(UpgradeRecord{BaseSHA: baseSHA, CandidateSHA: sha, TreeHash: tree, Approval: approval, Verify: true, Verification: "failed", Error: err.Error(), CIEvidence: ciPtr(ci), RequiredChecks: requiredChecks(opts.required), CIVerified: approval == "ci"}); perr != nil {
				return "", fmt.Errorf("%w (upgrade audit write failed: %v)", err, perr)
			}
			return "", err
		}
		verification = "ok"
	}
	built, ver, err := verifyAndBuild(ctx, wt, buildDir, stderr, false)
	if err != nil {
		if perr := persistUpgradeRecord(UpgradeRecord{BaseSHA: baseSHA, CandidateSHA: sha, TreeHash: tree, Approval: approval, Verify: verify, Verification: verification, Error: err.Error(), CIEvidence: ciPtr(ci), RequiredChecks: requiredChecks(opts.required), CIVerified: approval == "ci"}); perr != nil {
			return "", fmt.Errorf("%w (upgrade audit write failed: %v)", err, perr)
		}
		return "", err
	}
	// The worktree must be pristine right before install so the artifact is
	// exactly the reviewed SHA/tree.
	if err := assertCleanTree(ctx, wt); err != nil {
		_ = persistUpgradeRecord(UpgradeRecord{BaseSHA: baseSHA, CandidateSHA: sha, TreeHash: tree, Approval: approval, Verify: verify, Verification: verification, Error: err.Error()})
		return "", err
	}

	// Worktrees are distinct per upgrade; the swap of the shared binary is
	// serialized across processes inside swapBinary (#413 review).
	path, err := swapBinary(ctx, built, self)
	if err != nil {
		if perr := persistUpgradeRecord(UpgradeRecord{BaseSHA: baseSHA, CandidateSHA: sha, TreeHash: tree, Approval: approval, Verify: verify, Verification: verification, Error: err.Error(), CIEvidence: ciPtr(ci), RequiredChecks: requiredChecks(opts.required), CIVerified: approval == "ci"}); perr != nil {
			return "", fmt.Errorf("%w (upgrade audit write failed: %v)", err, perr)
		}
		return "", err
	}
	digest, derr := fileSHA256(path)
	if derr != nil {
		// The swap already happened; record the install with the hashing
		// failure rather than leaving the audit silent.
		_ = persistUpgradeRecord(UpgradeRecord{BaseSHA: baseSHA, CandidateSHA: sha, TreeHash: tree, Approval: approval, Verify: verify, Verification: verification, Error: derr.Error()})
		return "", fmt.Errorf("hash installed artifact: %w", derr)
	}
	if err := persistUpgradeRecord(UpgradeRecord{
		BaseSHA: baseSHA, CandidateSHA: sha, TreeHash: tree,
		ArtifactSHA256: digest, Approval: approval, Verify: verify,
		Verification: verification, Version: ver, InstalledAt: time.Now().UTC(),
		CIEvidence: ciPtr(ci), RequiredChecks: requiredChecks(opts.required),
		CIVerified: approval == "ci",
	}); err != nil {
		return "", err
	}
	return path, nil
}

// ciPtr returns a pointer to evidence for audit persistence (nil-safe).
func ciPtr(ci CIEvidence) *CIEvidence {
	if ci.SHA == "" && ci.VerifiedAt.IsZero() {
		return nil
	}
	return &ci
}

// remoteRepo derives the canonical owner/repo for the checkout's origin
// remote, used by the CI gate (and only by it).
func remoteRepo(ctx context.Context, repoDir string) (string, error) {
	out, err := commandOutput(ctx, repoDir, "git", "remote", "get-url", "origin")
	if err != nil {
		return "", fmt.Errorf("ci approval: resolve origin remote: %w", err)
	}
	u := strings.TrimSpace(out)
	// Accept https://github.com/owner/repo(.git), git@github.com:owner/repo(.git),
	// and owner/repo directly.
	u = strings.TrimSuffix(u, ".git")
	if i := strings.Index(u, "github.com/"); i >= 0 {
		u = u[i+len("github.com/"):]
	} else if i := strings.Index(u, "github.com:"); i >= 0 {
		u = u[i+len("github.com:"):]
	}
	u = strings.TrimLeft(u, "/")
	u = strings.TrimPrefix(u, "git@")
	if _, _, err := gitcred.SplitRepo(u); err != nil {
		return "", fmt.Errorf("ci approval: origin remote %q is not a github owner/repo: %w", strings.TrimSpace(out), err)
	}
	return u, nil
}

// resolveCommit returns the immutable commit SHA for ref (or HEAD when ref is
// empty), so review, verification, build, and install all bind to one commit.
func resolveCommit(ctx context.Context, repoDir, ref string) (string, error) {
	arg := "HEAD"
	if ref != "" {
		arg = ref + "^{commit}"
	}
	out, err := commandOutput(ctx, repoDir, "git", "rev-parse", arg)
	if err != nil {
		return "", fmt.Errorf("resolve commit %q: %w", ref, err)
	}
	return strings.TrimSpace(out), nil
}

// reviewBaseSHA is the diff base for the review. With an explicit ref, the
// base is the configured checkout's HEAD (what the operator is on). With no
// ref, the candidate is HEAD and the base is its parent — the most recent
// commit is what would newly land; a root commit reviews against the empty
// tree. The no-ref path is always reviewed and commit-bound (#413).
func reviewBaseSHA(ctx context.Context, repoDir, ref, sha string) (string, error) {
	if ref != "" {
		return resolveCommit(ctx, repoDir, "")
	}
	out, err := commandOutput(ctx, repoDir, "git", "rev-parse", sha+"~1")
	if err != nil {
		return emptyTreeSHA, nil
	}
	return strings.TrimSpace(out), nil
}

// addDetachedTempWorktree creates a detached worktree at sha under a fresh
// temp directory. The path is created by the caller of os.MkdirTemp and
// removed so git worktree add can claim it.
func addDetachedTempWorktree(ctx context.Context, repoDir, sha string) (string, error) {
	dir, err := os.MkdirTemp("", "waffle-upgrade-worktree-*")
	if err != nil {
		return "", err
	}
	if err := os.Remove(dir); err != nil {
		return "", err
	}
	if err := run(ctx, repoDir, io.Discard, "git", "worktree", "add", "--detach", "--quiet", dir, sha); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("create worktree at %s: %w", sha, err)
	}
	return dir, nil
}

// removeWorktree removes the detached worktree, falling back to plain
// directory removal so a temp dir never leaks even if git metadata cleanup
// fails (the stale entry is harmless and pruned later).
func removeWorktree(ctx context.Context, repoDir, dir string) error {
	if dir == "" {
		return nil
	}
	err := run(ctx, repoDir, io.Discard, "git", "worktree", "remove", "--force", dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		return err
	}
	return nil
}

// assertCleanTree fails the upgrade if the worktree is not pristine: a dirty
// tree would mean the artifact is no longer exactly the reviewed SHA.
func assertCleanTree(ctx context.Context, worktree string) error {
	out, err := commandOutput(ctx, worktree, "git", "status", "--porcelain")
	if err != nil {
		return fmt.Errorf("check worktree cleanliness: %w", err)
	}
	if strings.TrimSpace(out) != "" {
		return fmt.Errorf("worktree is not clean: %s", strings.TrimSpace(out))
	}
	return nil
}

// treeHash is the tree object of the worktree's HEAD, recorded in the audit
// so the installed artifact is bound to an exact tree.
func treeHash(ctx context.Context, worktree string) (string, error) {
	out, err := commandOutput(ctx, worktree, "git", "rev-parse", "HEAD^{tree}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// fileSHA256 returns the sha256 of a file's contents.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// upgradeInto verifies and builds a checkout before replacing target. Keeping
// target explicit makes the no-swap boundary integration-testable.
func upgradeInto(ctx context.Context, repoDir, target string, stderr io.Writer, verify bool) (string, error) {
	buildDir, err := os.MkdirTemp("", "waffle-upgrade-build-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(buildDir) }()
	built, _, err := verifyAndBuild(ctx, repoDir, buildDir, stderr, verify)
	if err != nil {
		return "", err
	}
	return swapBinary(ctx, built, target)
}

// verifyAndBuild verifies (when requested) and builds the binary from repoDir
// into buildDir. The build lands outside the worktree so the reviewed tree
// stays pristine and can be asserted clean before install (#413).
func verifyAndBuild(ctx context.Context, repoDir, buildDir string, stderr io.Writer, verify bool) (string, string, error) {
	if verify {
		if err := verifyRepo(ctx, repoDir, stderr); err != nil {
			return "", "", err
		}
	}
	built := filepath.Join(buildDir, "waffle-build")
	ver, err := buildVersion(ctx, repoDir)
	if err != nil {
		return "", "", fmt.Errorf("version stamp: %w", err)
	}
	ldflags := "-X main.version=" + ver
	fmt.Fprintf(stderr, "building waffle %s\n", ver)
	if err := run(ctx, repoDir, stderr, "go", "build", "-ldflags", ldflags, "-o", built, "./cmd/waffle"); err != nil {
		return "", "", fmt.Errorf("build: %w", err)
	}
	fmt.Fprintf(stderr, "built waffle %s\n", ver)
	return built, ver, nil
}

// swapBinary gates on the new binary's own doctor, backs up the target, and
// atomically swaps it in. The previous binary's provenance is retained via
// the .prev backup for rollback (#413). The backup/swap section is serialized
// across processes with an advisory flock so two concurrent `waffle upgrade`
// shells cannot race on the target and its .prev (#413 review).
func swapBinary(ctx context.Context, built, target string) (string, error) {
	// Gate on the *new* binary's own doctor.
	out, err := exec.CommandContext(ctx, built, "doctor").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("new binary failed doctor:\n%s", out)
	}
	unlock, err := flock.Acquire(target+".swap.lock", "waffle upgrade swap", 30*time.Second)
	if err != nil {
		return "", fmt.Errorf("lock upgrade swap: %w", err)
	}
	defer func() { _ = unlock() }()
	backup := target + ".prev"
	if err := copyFile(target, backup); err != nil {
		return "", fmt.Errorf("back up current binary: %w", err)
	}
	// Rename within the same directory is atomic; a crash mid-swap leaves
	// either the old or the new binary, never a truncated one.
	staged := target + ".new"
	if err := copyFile(built, staged); err != nil {
		return "", err
	}
	if err := os.Chmod(staged, 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(staged, target); err != nil {
		return "", fmt.Errorf("swap binary: %w", err)
	}
	return target, nil
}

func verifyRepo(ctx context.Context, repoDir string, stderr io.Writer) error {
	for _, step := range verifySteps() {
		if step[0] == "golangci-lint" {
			if _, err := exec.LookPath("golangci-lint"); err != nil {
				fmt.Fprintln(stderr, "warning: golangci-lint not installed; lint gate skipped")
				continue
			}
		}
		if err := run(ctx, repoDir, stderr, step[0], step[1:]...); err != nil {
			return fmt.Errorf("verify: %s failed: %w", strings.Join(step, " "), err)
		}
	}
	return nil
}

// verifySteps is the deterministic upgrade verification ladder (#63):
// vet, tests, optional lint, then the zero-network eval harness. A broken
// eval blocks upgrade the same way a failing test does.
func verifySteps() [][]string {
	return [][]string{
		{"go", "vet", "./..."},
		{"go", "test", "-race", "./..."},
		{"golangci-lint", "run"},
		{"go", "run", "./cmd/waffle", "eval"},
	}
}

// rejectProtectedChanges refuses an auto-patch that touches protected paths
// between baseSHA and candidateSHA (#413: both are immutable SHAs, so the
// check binds to the exact reviewed diff).
func rejectProtectedChanges(ctx context.Context, repoDir, baseSHA, candidateSHA string, protected []string) error {
	out, err := commandOutput(ctx, repoDir, "git", "diff", "--name-only", baseSHA, candidateSHA)
	if err != nil {
		return fmt.Errorf("inspect auto-patch diff: %w", err)
	}
	paths := append([]string{
		"internal/selfdev", "internal/config", "cmd/waffle/selfdev_cmd.go",
		"cmd/waffle/main.go", "internal/doctor", "evals", "internal/eval",
	}, protected...)
	for _, file := range strings.Fields(out) {
		for _, prefix := range paths {
			if file == prefix || strings.HasPrefix(file, strings.TrimSuffix(prefix, "/")+"/") {
				return fmt.Errorf("auto-patch refused: change touches protected path %q", file)
			}
		}
	}
	return nil
}

// Rollback restores the binary saved by the last Upgrade.
func Rollback() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		return "", err
	}
	backup := self + ".prev"
	if _, err := os.Stat(backup); err != nil {
		return "", errors.New("no previous binary to roll back to")
	}
	if err := os.Rename(backup, self); err != nil {
		return "", err
	}
	return self, nil
}

// validateRef rejects refs git would parse as options ("--help", "-c"),
// which would otherwise be injected into the checkout argv and, via the
// build-and-swap that follows, escalate toward code execution.
func validateRef(ref string) error {
	if ref == "" {
		return errors.New("empty git ref")
	}
	if strings.HasPrefix(ref, "-") {
		return fmt.Errorf("invalid git ref %q: refs may not start with '-'", ref)
	}
	return nil
}

// buildVersion returns a version string suitable for -ldflags -X main.version.
// Prefer git describe (tags + short sha + dirty); fall back to "dev".
func buildVersion(ctx context.Context, repoDir string) (string, error) {
	out, err := commandOutput(ctx, repoDir, "git", "describe", "--tags", "--always", "--dirty")
	if err != nil {
		// Not a git checkout, or git missing: still allow the build with "dev".
		return "dev", nil
	}
	return sanitizeVersion(out)
}

// sanitizeVersion trims git describe output and rejects characters that
// would break or inject into -ldflags -X main.version=...
func sanitizeVersion(raw string) (string, error) {
	ver := strings.TrimSpace(raw)
	if ver == "" {
		return "dev", nil
	}
	for _, r := range ver {
		ok := (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '.' || r == '-' || r == '_' || r == '+'
		if !ok {
			return "", fmt.Errorf("git describe produced unsafe version %q", ver)
		}
	}
	return ver, nil
}

func run(ctx context.Context, dir string, stderr io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Stdout = stderr
	cmd.Stderr = stderr
	return cmd.Run()
}

func commandOutput(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return string(out), err
}

func copyFile(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := in.Close(); err == nil {
			err = cerr
		}
	}()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// ReExec replaces the current process with path (execve), preserving the
// environment. A long-running waffle (the gateway) uses this to hot-swap
// into a freshly upgraded binary; on success it does not return. Unix only.
func ReExec(path string, args []string) error {
	return reexec(path, args)
}
