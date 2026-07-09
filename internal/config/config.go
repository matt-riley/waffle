// Package config loads waffle's configuration from $WAFFLE_HOME/config.toml.
//
// The config file is deliberately small (docs/plan.md, "minimal config"):
// it names the trust boundaries — listen address, agent-group policies,
// secret references — and nothing else. Behavior belongs in code, secrets
// belong in the secret store, and the file is optional: waffle runs with
// defaults until one exists.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config is the root of config.toml.
type Config struct {
	Gateway  Gateway     `toml:"gateway"`
	Provider Provider    `toml:"provider"`
	Channel  Channels    `toml:"channel"`
	Sandbox  Sandbox     `toml:"sandbox"`
	Broker   Broker      `toml:"broker"`
	MCP      []MCPServer `toml:"mcp"`
	Agent    Agent       `toml:"agent"`
	Repo     Repo        `toml:"repo"`
	Log      Log         `toml:"log"`
}

// MCPServer is one Model Context Protocol server run over stdio.
type MCPServer struct {
	Name    string   `toml:"name"`
	Command string   `toml:"command"`
	Args    []string `toml:"args"`
}

// Agent tunes agent behavior.
type Agent struct {
	// Subagents enables the spawn_subagent tool.
	Subagents bool `toml:"subagents"`
	// Learn enables the distill_skill tool (the learning loop).
	Learn bool `toml:"learn"`
	// Groups carries per-agent-group trust posture (docs/plan.md, design
	// principle 4: "risky contexts — repo workspaces, scheduled jobs — run
	// in sandboxes with explicit tool policies"). Keyed by group name, e.g.
	// [agent.group.main], [agent.group.cron]. Unlisted groups fall back to
	// documented defaults (see Config.AgentPolicy).
	Groups map[string]AgentGroup `toml:"group"`
}

// AgentGroup is one agent group's trust posture: where its tools run and
// which tools it may use. It is owner-authored host config, so a group's
// explicit tool policy is authoritative for that group — it replaces the
// global [sandbox] allow/deny rather than intersecting it (an operator can,
// for example, opt a group back into a tool the global policy omits).
// Untrusted, repo-supplied policy that may only *tighten* is a separate
// concern (#53).
type AgentGroup struct {
	// Sandbox is "host" or "docker"; empty inherits [sandbox].mode.
	Sandbox string `toml:"sandbox"`
	// Tools filters the toolset for sessions in this group.
	Tools ToolPolicy `toml:"tools"`
}

// ToolPolicy is an allow/deny tool filter expressed in config.
type ToolPolicy struct {
	Allow []string `toml:"allow"`
	Deny  []string `toml:"deny"`
}

// GroupCron is the reserved group name for scheduled (cron) sessions, the
// plan's canonical "risky context": unattended and often driven by external
// content. GroupMain is the owner's interactive sessions.
const (
	GroupMain = "main"
	GroupCron = "cron"
)

// AgentPolicy resolves the effective sandbox mode and tool policy for a group.
// An explicit [agent.group.<name>] wins. Otherwise the global [sandbox]
// applies, except that the unattended cron group defaults to denying host
// `bash` — the plan tiers scheduled jobs below the owner's interactive
// sessions, and inheriting host shell unattended is exactly the hole that
// default closes. A group's explicit *tool policy* replaces the global
// allow/deny (it is authoritative for that group — not intersected or
// unioned), and only such an explicit cron tool policy opts out of the
// default bash deny. Merely configuring a group's sandbox mode (or adding an
// [agent.group.cron] section with no tool policy) does NOT drop the safety
// default — otherwise setting `sandbox` for cron would silently re-enable
// host shell for unattended jobs.
func (c Config) AgentPolicy(group string) ResolvedAgentPolicy {
	r := ResolvedAgentPolicy{
		Mode:  c.Sandbox.Mode,
		Allow: c.Sandbox.Allow,
		Deny:  c.Sandbox.Deny,
	}
	if r.Mode == "" {
		r.Mode = "host"
	}
	g, ok := c.Agent.Groups[group]
	explicitTools := ok && (len(g.Tools.Allow) > 0 || len(g.Tools.Deny) > 0)
	if ok {
		if g.Sandbox != "" {
			r.Mode = g.Sandbox
		}
		if explicitTools {
			r.Allow = g.Tools.Allow
			r.Deny = g.Tools.Deny
		}
	}
	// The unattended cron tier denies host bash by default; only an explicit
	// cron tool policy opts out of that safety default.
	if group == GroupCron && !explicitTools {
		r.Deny = appendUnique(r.Deny, "bash")
	}
	return r
}

// ResolvedAgentPolicy is the effective execution posture for a group.
type ResolvedAgentPolicy struct {
	Mode  string // "host" or "docker"
	Allow []string
	Deny  []string
}

// UsesDocker reports whether any tier runs tools in docker: the global
// [sandbox] mode, or any [agent.group.*] that opts into it. Groups without an
// explicit sandbox inherit the global mode, so those two sources cover every
// resolved policy. Used by `waffle doctor` to decide whether the runner-binary
// check applies even when the global mode is host.
func (c Config) UsesDocker() bool {
	if c.Sandbox.Mode == "docker" {
		return true
	}
	for _, g := range c.Agent.Groups {
		if g.Sandbox == "docker" {
			return true
		}
	}
	return false
}

func appendUnique(s []string, v string) []string {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(append([]string(nil), s...), v)
}

// Repo configures the self-development loop's view of waffle's own source.
type Repo struct {
	// Dir is a local checkout of waffle used by `waffle upgrade`.
	Dir string `toml:"dir"`
}

// Sandbox names the trust boundary for tool execution (docs/plan.md,
// "Sandboxing & IPC"). Policy is enforced host-side either way.
type Sandbox struct {
	// Mode is "host" (tools run in-process) or "docker" (tools run in a
	// container via waffle runner).
	Mode string `toml:"mode"`
	// Image for docker mode; any image works — the waffle binary is
	// bind-mounted in.
	Image string `toml:"image"`
	// RunnerBinary is a linux build of waffle to bind-mount as the
	// container's `waffle runner` entrypoint. Required for docker mode on a
	// non-linux host, where the running binary is the wrong executable
	// format; empty uses the running binary (correct only on linux).
	RunnerBinary string `toml:"runner_binary"`
	// Network for docker mode: "none" (default) or "bridge".
	Network string `toml:"network"`
	// WorkDir on the host is mounted read-write at /work in the sandbox.
	WorkDir string `toml:"work_dir"`
	// Allow/Deny filter tools by name (empty allow = everything).
	Allow []string `toml:"allow"`
	Deny  []string `toml:"deny"`
}

// Broker configures the credential broker's HTTP listener; empty disables.
type Broker struct {
	Listen string `toml:"listen"`
}

// Channels configures messaging surfaces for waffle serve.
type Channels struct {
	Telegram Telegram `toml:"telegram"`
}

// Telegram is the Telegram bot channel.
type Telegram struct {
	Enabled bool `toml:"enabled"`
	// Token is a secret:// reference (or empty to use TELEGRAM_BOT_TOKEN).
	Token string `toml:"token"`
	// BaseURL overrides the Bot API endpoint; for tests and proxies.
	BaseURL string `toml:"base_url"`
}

// Provider selects and configures the LLM backend.
type Provider struct {
	// Name is "anthropic" or "openai" (any OpenAI-compatible endpoint:
	// OpenAI, OpenRouter, Ollama, a running workweave/router, ...).
	Name  string `toml:"name"`
	Model string `toml:"model"`
	// APIKey is a secret:// reference or empty to fall back to the
	// provider's conventional environment variable. Never a raw key.
	APIKey    string `toml:"api_key"`
	BaseURL   string `toml:"base_url"`
	MaxTokens int    `toml:"max_tokens"`
}

// Gateway configures the control plane.
type Gateway struct {
	// Listen is the bind address. Loopback by default: exposing the
	// gateway remotely is a deliberate, explicit decision.
	Listen string `toml:"listen"`
}

// Log configures logging.
type Log struct {
	Level string `toml:"level"`
}

// Default returns the configuration waffle runs with when no file exists.
func Default() Config {
	return Config{
		Gateway: Gateway{Listen: "127.0.0.1:8420"},
		Provider: Provider{
			Name:      "anthropic",
			Model:     "claude-opus-4-8",
			APIKey:    "secret://anthropic/api-key",
			MaxTokens: 64000,
		},
		Channel: Channels{
			Telegram: Telegram{Token: "secret://telegram/bot-token"},
		},
		Sandbox: Sandbox{
			Mode:    "host",
			Image:   "buildpack-deps:bookworm-scm",
			Network: "none",
		},
		Agent: Agent{Subagents: true, Learn: true},
		Log:   Log{Level: "info"},
	}
}

// Home returns waffle's state directory: $WAFFLE_HOME if set, else ~/.waffle.
func Home() (string, error) {
	if h := os.Getenv("WAFFLE_HOME"); h != "" {
		return h, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".waffle"), nil
}

func homePath(name string) (string, error) {
	h, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, name), nil
}

// Path returns the location of config.toml.
func Path() (string, error) { return homePath("config.toml") }

// DBPath returns the location of waffle's SQLite database.
func DBPath() (string, error) { return homePath("waffle.db") }

// SecretsPath returns the location of the encrypted secret store.
func SecretsPath() (string, error) { return homePath("secrets.age") }

// Load reads the config file at path, layered over Default. A missing file
// is not an error, but unknown keys are rejected: they are almost always
// typos, and a silently ignored policy key would be a security bug.
func Load(path string) (Config, error) {
	cfg := Default()
	meta, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return Config{}, fmt.Errorf("unknown keys in %s: %s", path, strings.Join(keys, ", "))
	}
	return cfg, nil
}
