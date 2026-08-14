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
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Config is the root of config.toml.
type Config struct {
	Gateway   Gateway                       `toml:"gateway"`
	Chat      Chat                          `toml:"chat"`
	Dashboard Dashboard                     `toml:"dashboard"`
	Provider  Provider                      `toml:"provider"`
	Providers map[string]ProviderConnection `toml:"providers"`
	Models    map[string]ModelTarget        `toml:"models"`
	Channel   Channels                      `toml:"channel"`
	Sandbox   Sandbox                       `toml:"sandbox"`
	Broker    Broker                        `toml:"broker"`
	Workspace Workspace                     `toml:"workspace"`
	Store     Store                         `toml:"store"`
	GitHub    GitHub                        `toml:"github"`
	MCP       []MCPServer                   `toml:"mcp"`
	Agent     Agent                         `toml:"agent"`
	Repo      Repo                          `toml:"repo"`
	Selfdev   Selfdev                       `toml:"selfdev"`
	Log       Log                           `toml:"log"`
	Limits    Limits                        `toml:"limits"`
	Jobs      JobPolicy                     `toml:"jobs"`
	Tools     Tools                         `toml:"tools"`
	Memory    Memory                        `toml:"memory"`
	// Policy is host-side action-level tool rules (#66). Declared as
	// [[policy.rule]] tables; unknown keys are rejected by Load.
	Policy PolicyConfig `toml:"policy"`
	// Intake is board-driven GitHub issue watchers under waffle serve (#51).
	Intake Intake `toml:"intake"`
	// CodeIntel configures optional structural-code tools (#79).
	CodeIntel CodeIntel `toml:"codeintel"`
	// API configures credentialed API faces (#254).
	API API `toml:"api"`
	// Search configures the optional web_search tool (#245). Each key is a
	// named search provider the broker fronts; the effective provider is the
	// sole entry, or the entry named "default". Absent config disables the
	// tool.
	Search map[string]SearchProvider `toml:"search"`

	// legacyProviderNormalized records that Providers and Models were derived
	// from the compatibility [provider] table. It keeps historical secret-value
	// handling confined to that representation; explicit named providers always
	// require connection-scoped secret references.
	legacyProviderNormalized bool
	// providerRegistryExplicit records the presence of [providers] or [models],
	// including an intentionally empty table. Map length cannot represent that
	// precedence decision after TOML decoding.
	providerRegistryExplicit bool
}

// Chat configures the local managed-chat client connection.
type Chat struct {
	Socket string `toml:"socket"`
}

// Dashboard configures the optional Waffle Desk web interface.
type Dashboard struct {
	Enabled          bool             `toml:"enabled"`
	SkillImportRoots []string         `toml:"skill_import_roots"`
	SkillGitHosts    []string         `toml:"skill_git_hosts"`
	Tailnet          DashboardTailnet `toml:"tailnet"`
}

// DashboardTailnet authorizes Desk requests that arrive through a
// `tailscale serve` reverse proxy running on this host. It does not change the
// bind address: gateway.status_listen stays loopback-only, which is what makes
// the Tailscale identity headers trustworthy in the first place.
type DashboardTailnet struct {
	Enabled bool `toml:"enabled"`
	// ServeHost is the exact MagicDNS name Serve answers on. Requests whose
	// Host does not match it are never admitted through this path.
	ServeHost string `toml:"serve_host"`
	// AllowedLogins are the Tailscale logins permitted to use Desk. These are
	// login names as tailscaled reports them (for example "user@github"), not
	// necessarily email addresses. Tagged devices send no login and are always
	// rejected.
	AllowedLogins []string `toml:"allowed_logins"`
}

// PolicyConfig holds [[policy.rule]] entries (#66).
// Also accepts [policy] rules = [{...}, ...] via the Rules field.
type PolicyConfig struct {
	// Rule is the list under [[policy.rule]].
	Rule []PolicyRule `toml:"rule"`
	// Rules is an alternate inline array form: [policy] rules = [{ name = "…", … }].
	Rules []PolicyRule `toml:"rules"`
}

// PolicyRule is one action-level allow/deny/require rule (#66).
type PolicyRule struct {
	// Name is a stable audit label (required).
	Name string `toml:"name"`
	// Tool is the tool name to match (e.g. "bash"). Empty means any tool only
	// when match or regex is set; a rule with tool, match, and regex all empty
	// is rejected at Load (need tool, match, or regex).
	Tool string `toml:"tool"`
	// Match is a bash command prefix (quote-aware token match).
	Match string `toml:"match"`
	// Regex matches the raw command string when set.
	Regex string `toml:"regex"`
	// Action is "allow", "deny", or "require".
	Action string `toml:"action"`
	// Requires is the predicate event (usually another rule's name) that must
	// have succeeded after the last write for action=require rules.
	Requires string `toml:"requires"`
	// Guidance is included in deny messages when [sandbox] enforcer = "feedback"
	// (and always used as the "because" text for require denials).
	Guidance string `toml:"guidance"`
}

// CodeIntel configures discovery of the six code-intelligence tools (#79).
type CodeIntel struct {
	// Enabled registers in-process text-fallback tools when true (default true).
	// Set false to omit them entirely (agent falls back to search/read).
	Enabled *bool `toml:"enabled"`
	// Root overrides the directory scanned by the fallback finder.
	// Empty uses the sandbox work_dir or current workspace when available.
	Root string `toml:"root"`
	// AllowHostMCP permits codeintel MCP servers with execution=host.
	// Default false — codeintel MCP should run sandboxed.
	AllowHostMCP bool `toml:"allow_host_mcp"`
	// Required fails agent build if no codeintel tools can be registered.
	Required bool `toml:"required"`
}

// CodeIntelEnabled reports whether the in-process fallback should register.
func (c Config) CodeIntelEnabled() bool {
	if c.CodeIntel.Enabled == nil {
		return true
	}
	return *c.CodeIntel.Enabled
}

// JobPolicy controls retries for unattended scheduled jobs. Durations use
// Go duration syntax (for example, "10s" or "5m").
type JobPolicy struct {
	MaxAttempts  int    `toml:"max_attempts"`
	BaseBackoff  string `toml:"base_backoff"`
	MaxBackoff   string `toml:"max_backoff"`
	StallTimeout string `toml:"stall_timeout"`
}

// ParseDuration accepts Go durations plus a convenient whole-day suffix for
// retention and lifecycle horizons (for example, "365d").
func ParseDuration(value string) (time.Duration, error) {
	if strings.HasSuffix(strings.ToLower(strings.TrimSpace(value)), "d") {
		n := strings.TrimSpace(value)[:len(strings.TrimSpace(value))-1]
		var days int64
		if _, err := fmt.Sscan(n, &days); err != nil || days <= 0 {
			return 0, fmt.Errorf("invalid day duration %q", value)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	return time.ParseDuration(value)
}

// requirePositiveDurations validates each named duration field, skipping
// (per the optional skip predicate) values that disable the setting rather
// than name a duration. format takes the field name then its raw value.
func requirePositiveDurations(fields map[string]string, skip func(string) bool, format string) error {
	for name, value := range fields {
		if skip != nil && skip(value) {
			continue
		}
		if d, err := ParseDuration(value); err != nil || d <= 0 {
			return fmt.Errorf(format, name, value)
		}
	}
	return nil
}

// Tools configures builtin tool network policy.
type Tools struct {
	Fetch Fetch `toml:"fetch"`
}

// Fetch configures the fetch tool's private-address escape hatch.
type Fetch struct {
	AllowPrivate []string `toml:"allow_private"`
}

// Memory controls writes to prompt-visible memory and skills.
type Memory struct {
	WriteGate string `toml:"write_gate"`
	// InjectBudget is the max bytes of MEMORY.md notes injected into the
	// system prompt (pinned first, then newest). Zero uses the package
	// default (8KiB).
	InjectBudget int `toml:"inject_budget"`
	// ReflectAfter is how long a session must be idle (no updates) before
	// gateway idle reflection writes a summary (#59). Empty or "0" disables
	// idle reflection. Example: "30m".
	ReflectAfter string `toml:"reflect_after"`
	// ReflectEvery is the idle-reflection poll interval under serve (#59).
	// Empty defaults to "5m" when ReflectAfter is set.
	ReflectEvery string `toml:"reflect_every"`
	// ReflectEveryTurns, when > 0, also reflects after this many turns
	// on active conversations that never go idle (#59).
	ReflectEveryTurns int `toml:"reflect_every_turns"`
}

// Selfdev configures the approval and verification policy for upgrades.
type Selfdev struct {
	Approval  string   `toml:"approval"`
	Verify    bool     `toml:"verify"`
	Protected []string `toml:"protected"`
	// RequiredChecks are the exact check names approval=ci verifies for the
	// candidate SHA (#415). Empty uses the safe default covering the primary
	// CI workflow.
	RequiredChecks []string `toml:"required_checks"`
}

// MCPServer is one Model Context Protocol server: a local command run over
// stdio, or a remote streamable-HTTP endpoint (#249). Exactly one of
// Command or URL is set; Load rejects any other combination.
type MCPServer struct {
	Name    string   `toml:"name"`
	Command string   `toml:"command"`
	Args    []string `toml:"args"`
	// URL is a remote MCP streamable-HTTP endpoint (alternative to
	// Command). Remote servers have no process to restrict; their network
	// posture comes from Egress and their credentials from the secret
	// store (Token reference or `waffle mcp login`).
	URL string `toml:"url"`
	// Execution is "host" or "sandbox" and applies to Command servers only.
	// Sandbox launches via the #77 restricted executor (ConnectRestricted);
	// when the agent group is docker mode the command is docker-wrapped
	// (network none, allowlisted env). URL servers reject execution="sandbox":
	// there is no process to sandbox, and reachability is Egress's job.
	Execution string `toml:"execution"`
	// Egress governs how a URL server is reached (#249): "broker" routes
	// through the gateway broker's egress proxy (allowlist + audit rows)
	// and is the default for docker-mode groups; "direct" dials the URL
	// from the host and is refused for docker-mode groups (it would be an
	// unaudited side channel out of a sandboxed tier). Empty resolves per
	// group mode: docker → broker, host → direct.
	Egress string `toml:"egress"`
	// Token is a secret:// reference to a static bearer credential sent as
	// Authorization: Bearer on every request to a URL server. OAuth tokens
	// obtained by `waffle mcp login` are stored under the canonical
	// mcp/<server> secret names instead. Raw values are rejected: config
	// holds references only.
	Token string `toml:"token"`
	// Groups limits this server to named agent groups. For Command servers
	// empty means all groups; for URL servers empty means the main tier
	// only — cron/issue/group are deny-by-default and must be named
	// explicitly (#249).
	Groups []string `toml:"groups"`
	// Tools is an optional declaration of the server's exposed tool names.
	// It permits launch filtering before the process is started.
	Tools []string `toml:"tools"`
	// Env names the only parent environment variables copied to the child
	// (Command servers only; rejected for URL servers).
	Env []string `toml:"env"`
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
	// Profiles are named agent postures (system/tools/sandbox/model) (#71).
	// The empty name or "main" matches current default behavior when unset.
	Profiles map[string]AgentProfile `toml:"profile"`
	// DefaultProfile is used when a run does not name a profile.
	DefaultProfile string `toml:"default_profile"`
	// DefaultModel is the model alias used when a run does not select one.
	DefaultModel string `toml:"default_model"`
	// UtilityModel is the model alias used for summarization and reflection.
	UtilityModel string `toml:"utility_model"`
}

// ProfileNameMax is the maximum length of a profile slug (#71).
const ProfileNameMax = 64

// DefaultMainSystemPrompt is the starter system prompt for a new
// [agent.profile.main]. Both entry points that can create one — `waffle setup`
// and Desk's setup checklist (#192) — use it, so a profile created from the
// browser is the same profile the CLI would have written.
const DefaultMainSystemPrompt = "You are the owner's personal assistant."

// slugNameRE is the allowed slug form shared by profile names, provider
// connection names, and model aliases: [a-z0-9-], 1–64 chars. Empty,
// whitespace, path separators, and shell metacharacters are rejected.
var slugNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$|^[a-z0-9]$`)

// AgentProfile is one named agent posture (#71).
// Profiles are a trust boundary: system prompt, model, sandbox mode, and
// tool allow/deny for a named posture used by chat, spawn_subagent, channel
// binds, and optional cron jobs.
type AgentProfile struct {
	// System is inline system text, or a path to a file when it starts with
	// "@" or looks like an existing path ending in .md. Empty leaves the
	// default system prompt; missing files and paths outside WAFFLE_HOME error.
	System string `toml:"system"`
	// Model overrides [provider].model when set.
	Model string `toml:"model"`
	// Sandbox is "host" or "docker"; empty inherits group policy.
	Sandbox string `toml:"sandbox"`
	// Tools filters the toolset for this profile.
	Tools ToolPolicy `toml:"tools"`
	// DenyPrefixes applies action-level bash prefix denials (#66).
	DenyPrefixes []string `toml:"deny_prefixes"`
	// Guidance is appended to action-level denials.
	Guidance string `toml:"guidance"`
	// MaxTokens overrides [provider].max_tokens when > 0.
	MaxTokens int `toml:"max_tokens"`
	// MaxIterations bounds the agent loop when > 0.
	MaxIterations int `toml:"max_iterations"`
	// AllowedChildren limits spawn_subagent profile= names when non-empty.
	AllowedChildren []string `toml:"allowed_children"`
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
	// DenyPrefixes denies bash command prefixes (#66).
	DenyPrefixes []string `toml:"deny_prefixes"`
	// Guidance is appended to action-level denial messages.
	Guidance string `toml:"guidance"`
	// FileRoots confines the builtin file tools (read_file, write_file,
	// edit_file, search) to these directory trees (#269). Empty inherits the
	// wider tier; a profile's roots must lie inside its group's.
	FileRoots []string `toml:"file_roots"`
}

// GroupCron is the reserved group name for scheduled (cron) sessions, the
// plan's canonical "risky context": unattended and often driven by external
// content. GroupIssue is board-driven issue intake (#51), similarly untrusted.
// GroupGroup is multi-party channel chats (Telegram groups, #34): mention-
// gated and treated as untrusted multi-party input. GroupMain is the owner's
// interactive 1:1 sessions.
const (
	GroupMain  = "main"
	GroupCron  = "cron"
	GroupIssue = "issue"
	GroupGroup = "group"
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
		Mode:      c.Sandbox.Mode,
		Allow:     c.Sandbox.Allow,
		Deny:      c.Sandbox.Deny,
		FileRoots: c.Sandbox.FileRoots,
	}
	if r.Mode == "" {
		r.Mode = "host"
	}
	g, ok := c.Agent.Groups[group]
	// Allow/Deny replace global tool lists; DenyPrefixes alone tightens without
	// lifting restricted-group defaults.
	explicitTools := ok && (len(g.Tools.Allow) > 0 || len(g.Tools.Deny) > 0)
	if ok {
		if g.Sandbox != "" {
			r.Mode = g.Sandbox
		}
		if explicitTools {
			r.Allow = g.Tools.Allow
			r.Deny = g.Tools.Deny
		}
		if len(g.Tools.DenyPrefixes) > 0 || g.Tools.Guidance != "" {
			r.DenyPrefixes = append([]string(nil), g.Tools.DenyPrefixes...)
			r.Guidance = g.Tools.Guidance
		}
		if len(g.Tools.FileRoots) > 0 {
			r.FileRoots = append([]string(nil), g.Tools.FileRoots...)
		}
	}
	// The restricted tiers get a filesystem boundary by default when there is
	// a work dir to point it at (#269): an unattended or multi-party session
	// with file tools but no boundary can read anything the owner can. An
	// explicit file_roots — global or per-group — is authoritative and opts out.
	if len(r.FileRoots) == 0 && restrictedDefaultGroup(group) && c.Sandbox.WorkDir != "" {
		r.FileRoots = []string{c.Sandbox.WorkDir}
	}
	// Unattended / multi-party tiers deny host bash and durable memory
	// writes by default; only an explicit tool policy for that group opts out.
	if restrictedDefaultGroup(group) && !explicitTools {
		r.Deny = AppendUnique(r.Deny, "bash")
		r.Deny = AppendUnique(r.Deny, "remember")
		r.Deny = AppendUnique(r.Deny, "memory_update")
		r.Deny = AppendUnique(r.Deny, "distill_skill")
		// Working-set mutation is owner-session only by default (#67).
		r.Deny = AppendUnique(r.Deny, "workspace_update")
		// web_search (#245): search results are attacker-influenceable text
		// pulled into exactly the unattended/multi-party tiers, so it is
		// deny-by-default there; an explicit tools.allow for the group opts in.
		r.Deny = AppendUnique(r.Deny, "web_search")
		// Public, permanent publish actions stay denied for unattended tiers
		// by default (#252): an injected issue body must not be able to
		// produce a public GitHub comment with no human in the loop.
		r.Deny = AppendUnique(r.Deny, "github_comment")
		// notify (#253): cron and issue keep it — they are the unattended
		// tiers that most need to reach the owner mid-run, and the decision
		// is deliberate (a scheduled job or issue intake may notify but a
		// multi-party chat must not be able to make waffle send the owner
		// arbitrary text). Only the group tier denies it by default.
		if group == GroupGroup {
			r.Deny = AppendUnique(r.Deny, "notify")
		}
	}
	if r.Mode == "docker" {
		r.Deny = AppendUnique(r.Deny, "remember")
		r.Deny = AppendUnique(r.Deny, "memory_update")
		r.Deny = AppendUnique(r.Deny, "distill_skill")
	}
	return r
}

// Profile returns the named agent profile, or a zero profile when missing.
// With no [agent.profile] section the effective profile is "main" (zero
// value), which matches historical default construction.
func (c Config) Profile(name string) (AgentProfile, bool) {
	if name == "" {
		name = c.Agent.DefaultProfile
	}
	if name == "" {
		name = "main"
	}
	if c.Agent.Profiles == nil {
		return AgentProfile{}, name == "main"
	}
	p, ok := c.Agent.Profiles[name]
	return p, ok || name == "main"
}

// ResolveProfileModel selects the provider model for a profile (#71).
//
//	"" or "default" → [provider].model
//	"utility"       → [provider].utility_model (error if unset)
//	any other value → used as an explicit model id on the same provider
func (c Config) ResolveProfileModel(p AgentProfile) (string, error) {
	m := strings.TrimSpace(p.Model)
	switch m {
	case "", "default":
		return c.Provider.Model, nil
	case "utility":
		if strings.TrimSpace(c.Provider.UtilityModel) == "" {
			return "", fmt.Errorf("profile model %q requires [provider] utility_model to be set", m)
		}
		return c.Provider.UtilityModel, nil
	default:
		return m, nil
	}
}

// ResolveProfileModelAlias resolves the model alias a profile actually runs
// on, under an explicit provider registry. It is the one place that mapping
// lives: the chat runtime and Desk's setup projection both call it, so a
// profile that will not start is reported as unresolvable in exactly the same
// terms it fails with (#192).
//
//	"" or "default" → [agent] default_model
//	"utility"       → [agent] utility_model (error if unset)
//	any other value → an alias that must exist in [model.*]
//
// Under the singular legacy [provider] table there are no aliases, so
// ResolveProfileModel answers instead.
func (c Config) ResolveProfileModelAlias(p AgentProfile) (string, error) {
	if c.ProviderRegistrySource() != ProviderRegistryExplicit {
		return c.ResolveProfileModel(p)
	}
	alias := strings.TrimSpace(p.Model)
	switch alias {
	case "", "default":
		alias = c.Agent.DefaultModel
	case "utility":
		alias = c.Agent.UtilityModel
		if alias == "" {
			return "", fmt.Errorf("profile model %q requires [agent] utility_model to be set", p.Model)
		}
	}
	if alias == "" {
		return "", fmt.Errorf("agent.default_model is not configured")
	}
	if _, err := c.ResolveModel(alias); err != nil {
		return "", err
	}
	return alias, nil
}

// ValidProfileName reports whether name is an allowed profile slug (#71).
func ValidProfileName(name string) bool {
	if name == "" || len(name) > ProfileNameMax {
		return false
	}
	return slugNameRE.MatchString(name)
}

// knownProfileTools are tool names that may appear in profile allow/deny.
// "*" is a wildcard allow-all (still subject to deny).
var knownProfileTools = map[string]bool{
	"*": true,
	// builtins
	"bash": true, "read_file": true, "write_file": true, "edit_file": true,
	"fetch": true, "search": true, "list_files": true,
	// web_search (#245): a host-side builtin routed through the broker's
	// credentialed API faces, offered only when [search] config exists. The
	// restricted tiers deny it by default (search pulls attacker-influenceable
	// text into untrusted contexts; an explicit tools.allow opts in).
	"web_search": true,
	// host memory / session / workset / spill
	"remember": true, "memory_update": true, "recall": true, "distill_skill": true,
	"workspace_update": true, "expand_output": true, "expand_context": true,
	"spawn_subagent": true,
	// Host-side GitHub tools (issue #252). Each mints a per-call installation
	// token carrying only the permission it needs, and none of those tokens
	// ever enters a container; every tool resolves the repo from the
	// session's workspace binding, never from tool input.
	"github_pr":          true,
	"github_pr_get":      true,
	"github_pr_diff":     true,
	"github_pr_comments": true,
	"github_comment":     true,
	"github_checks":      true,
	"github_issue_get":   true,
	// Sends a short owner notification mid-run (#253). Destination comes
	// from session origin only, never tool input. Tier availability is
	// explicit in AgentPolicy: cron/issue keep it deliberately (unattended
	// tiers need it most); group tier denies it by default.
	"notify": true,
	// codeintel
	"code_find_symbol": true, "code_references": true, "code_callers": true,
	"code_structure": true, "code_blast_radius": true, "code_suggest_tests": true,
}

// ValidProfileTool reports whether name may appear in a profile's tool
// allow/deny list. The structured profile editor uses it to refuse an unknown
// tool by name, instead of surfacing a raw config-load failure (#194).
// api_<face> names are accepted for any slug: face tools are generated per
// configured [[api.upstream]] entry, so the static list cannot enumerate
// them. Load-time validation additionally requires the face to exist.
func ValidProfileTool(name string) bool {
	if knownProfileTools[name] {
		return true
	}
	face, ok := strings.CutPrefix(name, "api_")
	return ok && face != "" && slugNameRE.MatchString(face)
}

// ProfileToolNames lists every tool a profile may name, so an editor can offer
// them rather than asking an operator to guess.
func ProfileToolNames() []string {
	names := make([]string, 0, len(knownProfileTools))
	for name := range knownProfileTools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func validateProfiles(path string, agent Agent, api API) error {
	if agent.DefaultProfile != "" && !ValidProfileName(agent.DefaultProfile) {
		return fmt.Errorf("agent.default_profile: invalid name %q (want slug [a-z0-9-] max %d)", agent.DefaultProfile, ProfileNameMax)
	}
	if err := detectDuplicateProfileTables(path); err != nil {
		return err
	}
	for name, p := range agent.Profiles {
		if !ValidProfileName(name) {
			return fmt.Errorf("agent.profile %q: invalid name (want slug [a-z0-9-] 1–%d chars; no whitespace, path separators, or shell metacharacters)", name, ProfileNameMax)
		}
		switch p.Sandbox {
		case "", "host", "docker":
		default:
			return fmt.Errorf("agent.profile %q: sandbox must be \"host\" or \"docker\", got %q", name, p.Sandbox)
		}
		if strings.TrimSpace(p.Model) != p.Model {
			return fmt.Errorf("agent.profile %q: model must not have leading/trailing whitespace", name)
		}
		if p.MaxTokens < 0 {
			return fmt.Errorf("agent.profile %q: max_tokens must be >= 0", name)
		}
		if p.MaxIterations < 0 {
			return fmt.Errorf("agent.profile %q: max_iterations must be >= 0", name)
		}
		apiFaces := map[string]bool{}
		for _, f := range api.Upstream {
			apiFaces[f.Name] = true
		}
		for _, list := range []struct {
			label string
			names []string
		}{
			{"allow", p.Tools.Allow},
			{"deny", p.Tools.Deny},
		} {
			for _, t := range list.names {
				if t == "" || !knownProfileTools[t] {
					// api_<face> names must name a configured face: a typo
					// would silently deny-by-default (#254).
					if face, ok := strings.CutPrefix(t, "api_"); ok && face != "" && slugNameRE.MatchString(face) {
						if apiFaces[face] {
							continue
						}
						return fmt.Errorf("agent.profile %q: tools.%s names api_%s but no [[api.upstream]] face %q is configured", name, list.label, face, face)
					}
					return fmt.Errorf("agent.profile %q: unknown tool %q in tools.%s", name, t, list.label)
				}
			}
		}
		for _, child := range p.AllowedChildren {
			if !ValidProfileName(child) {
				return fmt.Errorf("agent.profile %q: allowed_children entry %q is not a valid profile name", name, child)
			}
		}
		// System paths: missing files and escapes are checked at agent build
		// (needs WAFFLE_HOME). Inline text and empty system are fine here.
	}
	return nil
}

// APIFaceGrants returns the names of the configured faces ([[api.upstream]])
// the given group's resolved tool policy explicitly grants. Only a literal
// api_<name> allow entry counts: an empty allow list and the "*" wildcard do
// NOT grant faces — a face is deny-by-default for every tier, including
// main (#254). This is the group-policy view used when minting session
// tokens; host-side tool offer also applies the profile merge.
func (c Config) APIFaceGrants(group string) []string {
	pol := c.AgentPolicy(group)
	grants := make([]string, 0, len(pol.Allow))
	for _, name := range pol.Allow {
		face, ok := strings.CutPrefix(name, "api_")
		if !ok || face == "" || !slugNameRE.MatchString(face) {
			continue
		}
		if slices.Contains(pol.Deny, name) {
			continue // deny always wins
		}
		grants = append(grants, face)
	}
	return grants
}

// apiUpstreamAllowedKeys are the only keys [[api.upstream]] may carry.
var apiUpstreamAllowedKeys = map[string]bool{
	"name": true, "base_url": true, "header": true,
	"value": true, "methods": true, "paths": true,
}

// detectAPIUpstreamUnknownKeys rejects unknown keys inside [[api.upstream]]
// tables with the offending face named. The main decode reports undecoded
// keys without array indices, which cannot be attributed to a face; this
// second targeted decode can (#254).
func detectAPIUpstreamUnknownKeys(path string) error {
	var raw map[string]toml.Primitive
	meta, err := toml.DecodeFile(path, &raw)
	if err != nil {
		return nil // Load already reported the parse error
	}
	apiPrim, ok := raw["api"]
	if !ok {
		return nil
	}
	var apiTable map[string]toml.Primitive
	if err := meta.PrimitiveDecode(apiPrim, &apiTable); err != nil {
		return nil
	}
	upstreamPrim, ok := apiTable["upstream"]
	if !ok {
		return nil
	}
	var upstreams []toml.Primitive
	if err := meta.PrimitiveDecode(upstreamPrim, &upstreams); err != nil {
		return nil
	}
	for i, prim := range upstreams {
		var keys map[string]toml.Primitive
		if err := meta.PrimitiveDecode(prim, &keys); err != nil {
			continue
		}
		var u APIUpstream
		if err := meta.PrimitiveDecode(prim, &u); err != nil {
			continue
		}
		for key := range keys {
			if apiUpstreamAllowedKeys[key] {
				continue
			}
			face := u.Name
			if face == "" {
				face = fmt.Sprintf("[%d]", i)
			}
			return fmt.Errorf("api.upstream: face %q: unknown key %q", face, key)
		}
	}
	return nil
}

// apiHTTPMethodRE matches the HTTP methods a face may allowlist.
var apiHTTPMethodRE = regexp.MustCompile(`^[A-Z]+$`)

// validateAPIUpstreams enforces the strict face contract (#254): required
// fields, secret://-only credentials, explicit method and path allowlists,
// and unique names. Every error names the offending face.
func validateAPIUpstreams(a API) error {
	seen := map[string]bool{}
	for _, f := range a.Upstream {
		if !ValidProviderConnectionName(f.Name) {
			return fmt.Errorf("api.upstream: invalid face name %q (want slug [a-z0-9-] max %d)", f.Name, ProviderConnectionNameMax)
		}
		if seen[f.Name] {
			return fmt.Errorf("api.upstream: duplicate face name %q", f.Name)
		}
		seen[f.Name] = true
		base, err := url.Parse(f.BaseURL)
		if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
			return fmt.Errorf("api.upstream %q: base_url must be an absolute http(s) URL, got %q", f.Name, f.BaseURL)
		}
		if base.User != nil || base.RawQuery != "" || base.Fragment != "" {
			return fmt.Errorf("api.upstream %q: base_url must not carry userinfo, a query, or a fragment", f.Name)
		}
		if !httpTokenRE.MatchString(f.Header) {
			return fmt.Errorf("api.upstream %q: header %q is not a valid HTTP header name", f.Name, f.Header)
		}
		if !strings.HasPrefix(f.Value, "secret://") || strings.TrimPrefix(f.Value, "secret://") == "" {
			return fmt.Errorf("api.upstream %q: value must be a secret:// reference (real credentials live only in internal/secret)", f.Name)
		}
		if len(f.Methods) == 0 {
			return fmt.Errorf("api.upstream %q: methods allowlist is required (deny-by-default; no implicit allow-all)", f.Name)
		}
		for _, m := range f.Methods {
			if !apiHTTPMethodRE.MatchString(m) || !knownHTTPMethod(m) {
				return fmt.Errorf("api.upstream %q: method %q is not supported (want GET, POST, PUT, PATCH, DELETE, HEAD, or OPTIONS)", f.Name, m)
			}
		}
		if len(f.Paths) == 0 {
			return fmt.Errorf("api.upstream %q: paths allowlist is required (deny-by-default; no implicit allow-all)", f.Name)
		}
		for _, path := range f.Paths {
			if err := validFacePath(path); err != nil {
				return fmt.Errorf("api.upstream %q: path %q: %w", f.Name, path, err)
			}
		}
	}
	return nil
}

// knownHTTPMethod reports whether m is one of the methods a face may allow.
func knownHTTPMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}

// httpTokenRE matches an RFC 7230 token (a valid HTTP header field name).
var httpTokenRE = regexp.MustCompile(`^[!#$%&'*+.^_` + "`" + `|~0-9A-Za-z-]+$`)

// validFacePath validates one path-prefix allowlist entry: absolute, no
// traversal segments, no encoded separators, no backslash, no query or
// fragment. Refusal of traversal at request time is the enforcement; this
// keeps the allowlist itself unambiguous.
func validFacePath(path string) error {
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("must start with %q", "/")
	}
	if strings.ContainsAny(path, "\\?#%") {
		return fmt.Errorf("must not contain backslash, query, fragment, or percent-encoding")
	}
	for _, seg := range strings.Split(path, "/") {
		if seg == ".." {
			return fmt.Errorf("must not contain %q segments", "..")
		}
	}
	return nil
}

// detectDuplicateProfileTables rejects repeated [agent.profile.NAME] headers
// in the raw TOML (decoder last-wins would otherwise hide duplicates).
func detectDuplicateProfileTables(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil // Load already handled missing files
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "[agent.profile.") || !strings.HasSuffix(line, "]") {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(line, "[agent.profile."), "]")
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if seen[name] {
			return fmt.Errorf("agent.profile %q: duplicate profile name", name)
		}
		seen[name] = true
	}
	return nil
}

// ResolvedAgentPolicy is the effective execution posture for a group.
type ResolvedAgentPolicy struct {
	Mode         string // "host" or "docker"
	Allow        []string
	Deny         []string
	DenyPrefixes []string
	Guidance     string
	// FileRoots confines the builtin file tools in host mode (#269); empty
	// means no boundary.
	FileRoots []string
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

// restrictedDefaultGroup reports whether group is a reserved tier that
// inherits the unattended deny defaults (bash, remember, memory_update,
// distill_skill, workspace_update, github_comment).
func restrictedDefaultGroup(group string) bool {
	return group == GroupCron || group == GroupIssue || group == GroupGroup
}

// AppendUnique returns s with v appended, unless s already contains v. The
// input slice is never mutated in place.
func AppendUnique(s []string, v string) []string {
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
	// Memory, CPUs, and PIDs cap each docker container's host impact. The
	// PIDs value is also used as the host Bash process budget when the host
	// executor is selected; Linux enforces it with a delegated cgroup when
	// possible (see docs/plan.md).
	Memory string  `toml:"memory"`
	CPUs   float64 `toml:"cpus"`
	PIDs   int     `toml:"pids"`
	Disk   string  `toml:"disk"`
	// WorkDir on the host is mounted read-write at /work in the sandbox.
	WorkDir string `toml:"work_dir"`
	// Allow/Deny filter tools by name (empty allow = everything).
	Allow []string `toml:"allow"`
	Deny  []string `toml:"deny"`
	// FileRoots confines the builtin file tools to these directory trees in
	// host mode (#269). Empty means no boundary, except for the restricted
	// tiers, which fall back to WorkDir when it is set. Docker mode gets its
	// boundary from the container.
	FileRoots []string `toml:"file_roots"`
	// Enforcer controls how action-level [[policy.rule]] denials are
	// surfaced (#66): "none" (default) denies with a short message;
	// "feedback" includes the rule's guidance for the model to adjust.
	// Layered trust: agent-group tool allow/deny → action rules → sandbox.
	Enforcer string `toml:"enforcer"`
}

// Workspace controls network egress for repository workspaces. Egress is
// deny-by-default; allowlist routes HTTP(S) through the host broker.
type Workspace struct {
	Egress      string   `toml:"egress"`
	Allowlist   []string `toml:"allowlist"`
	IdleTimeout string   `toml:"idle_timeout"`
	CloseTTL    string   `toml:"close_ttl"`
	// Hooks are optional container shell commands at lifecycle points (#54).
	Hooks WorkspaceHooks `toml:"hooks"`
}

// WorkspaceHooks are host-configured lifecycle commands (also overridable
// per-repo via WAFFLE.md once #53 is present). Commands run inside the
// workspace container, never on the host.
type WorkspaceHooks struct {
	AfterCreate  string `toml:"after_create"`
	BeforeRun    string `toml:"before_run"`
	AfterRun     string `toml:"after_run"`
	BeforeRemove string `toml:"before_remove"`
	// Timeout bounds each hook (Go duration); empty defaults to 5m.
	Timeout string `toml:"timeout"`
}

// Intake configures board-driven GitHub issue watchers (#51).
type Intake struct {
	GitHub []GitHubWatch `toml:"github"`
}

// GitHubWatch is one per-repo issue watcher.
type GitHubWatch struct {
	Repo           string `toml:"repo"`
	Label          string `toml:"label"`
	MaxConcurrency int    `toml:"max_concurrency"`
	Deliver        string `toml:"deliver"`
	PollInterval   string `toml:"poll_interval"`
	// Token is a secret:// reference or empty to use GITHUB_TOKEN / gh default.
	Token string `toml:"token"`
}

// Store controls retention of conversation data. Zero means retain forever.
type Store struct {
	Retain string `toml:"retain"`
}

// GitHub configures optional GitHub App credentials for workspace git access.
type GitHub struct {
	App GitHubApp `toml:"app"`
}

type GitHubApp struct {
	AppID          int64  `toml:"app_id"`
	InstallationID int64  `toml:"installation_id"`
	PrivateKey     string `toml:"private_key"`
	BaseURL        string `toml:"base_url"`
}

// Broker configures the credential broker's HTTP listener; empty disables.
type Broker struct {
	Listen string `toml:"listen"`
}

// API configures credentialed API faces served by the broker at
// /api/<name>/<path> (#254). Absent config means no faces exist and the
// routes 404, matching how other optional broker subsystems behave.
type API struct {
	Upstream []APIUpstream `toml:"upstream"`
}

// SearchProvider is one configured web_search provider (#245). The
// credential is always a secret:// reference resolved host-side; the broker
// injects it and a sandbox never holds it.
type SearchProvider struct {
	// Type is the provider: "brave" or "tavily".
	Type string `toml:"type"`
	// BaseURL overrides the provider's API root; empty uses the provider
	// default.
	BaseURL string `toml:"base_url"`
	// APIKey is the credential reference, for example
	// "secret://search/brave/api-key". Literal credentials are rejected.
	APIKey string `toml:"api_key"`
	// MaxResults caps the ranked rows web_search returns (1..10, default 5).
	MaxResults int `toml:"max_results"`
}

// SearchDefaultMaxResults is the row cap when max_results is unset.
const SearchDefaultMaxResults = 5

// SearchMaxResults is the hard row cap.
const SearchMaxResults = 10

// SearchEffective resolves the provider the web_search tool uses: the sole
// configured provider, or the one named "default". Multiple providers without
// a "default" entry are an error — strict config, no permissive fallback
// (#245). ok is false when no search provider is configured.
func (c Config) SearchEffective() (name string, p SearchProvider, ok bool, err error) {
	if len(c.Search) == 0 {
		return "", SearchProvider{}, false, nil
	}
	if p, ok := c.Search["default"]; ok {
		return "default", p, true, nil
	}
	if len(c.Search) == 1 {
		for name, p := range c.Search {
			return name, p, true, nil
		}
	}
	names := make([]string, 0, len(c.Search))
	for name := range c.Search {
		names = append(names, name)
	}
	sort.Strings(names)
	return "", SearchProvider{}, false, fmt.Errorf("multiple [search] providers configured (%s) but none is named \"default\"; name one \"default\" or keep a single provider", strings.Join(names, ", "))
}

func validateSearch(search map[string]SearchProvider) error {
	for name, p := range search {
		if !slugNameRE.MatchString(name) {
			return fmt.Errorf("[search] provider name %q is not a valid slug (want [a-z0-9-])", name)
		}
		switch p.Type {
		case "brave", "tavily":
		default:
			return fmt.Errorf("[search.%s]: type must be \"brave\" or \"tavily\", got %q", name, p.Type)
		}
		if !strings.HasPrefix(p.APIKey, "secret://") {
			return fmt.Errorf("[search.%s]: api_key must be a secret:// reference (store it with `waffle secret set %s`); literal credentials are rejected", name, "search/"+name+"/api-key")
		}
		if p.BaseURL != "" {
			u, err := url.Parse(p.BaseURL)
			if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
				return fmt.Errorf("[search.%s]: base_url must be a valid http(s) URL, got %q", name, p.BaseURL)
			}
		}
		if p.MaxResults < 0 || p.MaxResults > SearchMaxResults {
			return fmt.Errorf("[search.%s]: max_results must be 0 (default %d) or 1..%d, got %d", name, SearchDefaultMaxResults, SearchMaxResults, p.MaxResults)
		}
	}
	return nil
}

// APIUpstream is one named credentialed API face (#254). The broker injects
// the resolved credential host-side and the caller never holds it, but the
// face is deny-by-default: both allowlists are required (a face missing
// either is a load error, never an implicit allow-all), and a session's tier
// must explicitly grant the face by naming api_<name> in its tool allow
// list — the "*" wildcard does not grant faces.
type APIUpstream struct {
	// Name routes the face: the broker serves /api/<name>/<path>.
	Name string `toml:"name"`
	// BaseURL is the real API root (e.g. https://api.example.com). The
	// request path is appended to its path, if any.
	BaseURL string `toml:"base_url"`
	// Header is the auth header to inject ("x-api-key" or "Authorization").
	Header string `toml:"header"`
	// Value is a secret:// reference to the credential. Real credential
	// values live only in internal/secret; a literal value here is a load
	// error.
	Value string `toml:"value"`
	// Methods is the explicit method allowlist (e.g. ["GET"]). Required.
	Methods []string `toml:"methods"`
	// Paths is the explicit path-prefix allowlist (e.g. ["/v1/weather"]).
	// Required; traversal and encoded separators are refused at request time.
	Paths []string `toml:"paths"`
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
	// MaxAttachmentBytes caps inbound attachment downloads; zero disables
	// them entirely (deny-by-default — attachments are inbound data from
	// other people). The cap is enforced before any fetch, so an oversized
	// attachment is refused without being downloaded. config.example.toml
	// documents a conservative example.
	MaxAttachmentBytes int64 `toml:"max_attachment_bytes"`
}

// Provider selects and configures the LLM backend.
type Provider struct {
	// Name is "anthropic" or "openai" (any OpenAI-compatible endpoint:
	// OpenAI, OpenRouter, Ollama, Gemini, vLLM, ...).
	Name  string `toml:"name"`
	Model string `toml:"model"`
	// UtilityModel, when set, is used for summarization and reflection (#61)
	// instead of Model. Empty keeps those calls on Model.
	UtilityModel string `toml:"utility_model"`
	// APIKey is a secret:// reference or empty to fall back to the
	// provider's conventional environment variable. Never a raw key.
	APIKey    string `toml:"api_key"`
	BaseURL   string `toml:"base_url"`
	MaxTokens int    `toml:"max_tokens"`
}

// ProviderConnection is one named set of provider credentials and endpoint
// settings. Type is either "anthropic" or "openai"; the latter covers any
// OpenAI-compatible endpoint.
type ProviderConnection struct {
	Type      string `toml:"type"`
	APIKey    string `toml:"api_key"`
	BaseURL   string `toml:"base_url"`
	MaxTokens int    `toml:"max_tokens"`
}

// ModelTarget maps a stable local alias to a provider connection and the
// upstream model identifier sent to that provider.
type ModelTarget struct {
	Provider  string `toml:"provider"`
	Model     string `toml:"model"`
	MaxTokens int    `toml:"max_tokens"`
}

// ResolvedModel is the complete deterministic target for one model alias.
type ResolvedModel struct {
	Alias          string
	ConnectionName string
	Connection     ProviderConnection
	UpstreamModel  string
	MaxTokens      int
}

// ProviderRegistrySource identifies how the effective provider registry was
// established. It preserves explicit-empty precedence without exposing
// mutable loader internals.
type ProviderRegistrySource string

const (
	ProviderRegistryNone     ProviderRegistrySource = "none"
	ProviderRegistryLegacy   ProviderRegistrySource = "legacy"
	ProviderRegistryExplicit ProviderRegistrySource = "explicit"
)

// ProviderRegistrySource reports whether provider connections were explicitly
// configured or normalized from the singular compatibility table. Programmatic
// configs containing registry entries are treated as explicit.
func (c Config) ProviderRegistrySource() ProviderRegistrySource {
	if c.providerRegistryExplicit {
		return ProviderRegistryExplicit
	}
	if c.legacyProviderNormalized {
		return ProviderRegistryLegacy
	}
	if len(c.Providers) > 0 || len(c.Models) > 0 {
		return ProviderRegistryExplicit
	}
	return ProviderRegistryNone
}

// ProviderConnectionNameMax is the maximum length of a provider connection
// name or model alias. These names are also used as secret-store path parts.
const ProviderConnectionNameMax = 64

// ValidProviderConnectionName reports whether name is safe for use as a
// provider registry key and secret-store path component.
func ValidProviderConnectionName(name string) bool {
	return len(name) <= ProviderConnectionNameMax && slugNameRE.MatchString(name)
}

// ValidModelAlias reports whether alias is a valid model catalog key.
func ValidModelAlias(alias string) bool {
	return len(alias) <= ProviderConnectionNameMax && slugNameRE.MatchString(alias)
}

// ResolveModel resolves alias to exactly one connection and upstream model.
func (c Config) ResolveModel(alias string) (ResolvedModel, error) {
	target, ok := c.Models[alias]
	if !ok {
		return ResolvedModel{}, fmt.Errorf("unknown model alias %q", alias)
	}
	connection, ok := c.Providers[target.Provider]
	if !ok {
		return ResolvedModel{}, fmt.Errorf("model alias %q references unknown provider %q", alias, target.Provider)
	}
	if err := validateProviderConnection(target.Provider, connection, c.legacyProviderNormalized); err != nil {
		return ResolvedModel{}, err
	}
	if strings.TrimSpace(target.Model) == "" {
		return ResolvedModel{}, fmt.Errorf("model alias %q: model is required", alias)
	}
	if target.MaxTokens < 0 {
		return ResolvedModel{}, fmt.Errorf("model alias %q: max_tokens must be >= 0", alias)
	}
	maxTokens := target.MaxTokens
	if maxTokens == 0 {
		maxTokens = connection.MaxTokens
	}
	return ResolvedModel{
		Alias:          alias,
		ConnectionName: target.Provider,
		Connection:     connection,
		UpstreamModel:  target.Model,
		MaxTokens:      maxTokens,
	}, nil
}

// Gateway configures the control plane.
type Gateway struct {
	// Listen is the bind address. Loopback by default: exposing the
	// gateway remotely is a deliberate, explicit decision.
	Listen string `toml:"listen"`
	// StatusListen is the unauthenticated local run-status API. It must stay
	// loopback-only to avoid exposing session metadata and usage remotely.
	// [dashboard.tailnet] can authorize proxied Desk requests on this listener
	// without moving the bind address; /status and /healthz remain loopback-only.
	StatusListen string `toml:"status_listen"`
}

// Log configures logging.
type Log struct {
	Level string `toml:"level"`
}

// Limits are optional safety budgets. Zero means unlimited.
type Limits struct {
	TokensPerDay          int `toml:"tokens_per_day"`
	RequestsPerHour       int `toml:"requests_per_hour"`
	AlertThresholdPercent int `toml:"alert_threshold_percent"`
	// TunnelBytesPerSession is the rolling-day byte budget for broker CONNECT
	// tunnels (#244). The broker relays tunnel bytes without inspection, so a
	// tunnel cannot be metered per request; the relay's io.Copy byte counts
	// are charged against this budget instead, so one CONNECT cannot bypass
	// RequestsPerHour with unbounded traffic. Both relay directions count
	// (client→upstream and upstream→client), so the budget bounds total tunnel
	// traffic, not just egress bytes.
	TunnelBytesPerSession int64             `toml:"tunnel_bytes_per_session"`
	Groups                map[string]Limits `toml:"group"`
}

func (c Config) LimitsFor(group string) Limits {
	if l, ok := c.Limits.Groups[group]; ok {
		return l
	}
	return Limits{TokensPerDay: c.Limits.TokensPerDay, RequestsPerHour: c.Limits.RequestsPerHour, AlertThresholdPercent: c.Limits.AlertThresholdPercent, TunnelBytesPerSession: c.Limits.TunnelBytesPerSession}
}

// Default returns the configuration waffle runs with when no file exists.
func Default() Config {
	return Config{
		Gateway: Gateway{Listen: "127.0.0.1:8420", StatusListen: "127.0.0.1:8422"},
		Channel: Channels{
			Telegram: Telegram{Token: "secret://telegram/bot-token"},
		},
		Sandbox: Sandbox{
			Mode:     "host",
			Image:    "buildpack-deps:bookworm-scm",
			Network:  "none",
			Memory:   "2g",
			CPUs:     2,
			PIDs:     512,
			Enforcer: "none",
		},
		Workspace: Workspace{Egress: "none", IdleTimeout: "30m", CloseTTL: "168h"},
		Store:     Store{Retain: "0"},
		Agent:     Agent{Subagents: true, Learn: true},
		Memory:    Memory{WriteGate: "auto", InjectBudget: 8192},
		Selfdev:   Selfdev{Approval: "manual", Verify: true},
		Log:       Log{Level: "info"},
		Jobs:      JobPolicy{MaxAttempts: 1, BaseBackoff: "10s", MaxBackoff: "10m", StallTimeout: "5m"},
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

// pathOverride is set by SetConfigPath (typically from the CLI --config/-c flag).
// Empty means no process-level override.
var pathOverride string

// SetConfigPath sets a process-level config path override. An empty path clears
// the override so Path falls back to WAFFLE_CONFIG or the default location.
// Intended for CLI flag handling and tests (use t.Cleanup to reset).
func SetConfigPath(p string) {
	pathOverride = p
}

// ResolvePath returns the config file path with precedence:
// flagPath (if non-empty) > WAFFLE_CONFIG env > $WAFFLE_HOME/config.toml.
func ResolvePath(flagPath string) (string, error) {
	if flagPath != "" {
		return flagPath, nil
	}
	if v := os.Getenv("WAFFLE_CONFIG"); v != "" {
		return v, nil
	}
	return homePath("config.toml")
}

// Path returns the location of config.toml, honoring SetConfigPath override,
// then WAFFLE_CONFIG, then the default under WAFFLE_HOME.
func Path() (string, error) {
	return ResolvePath(pathOverride)
}

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
	// Historically, decoding [provider] layered over these Anthropic defaults.
	// Decode it a second time with that baseline only when the legacy table is
	// actually present, so a provider-empty config remains a valid Installed
	// state without changing partial legacy configuration behavior.
	if meta.IsDefined("provider") {
		cfg = Default()
		cfg.Provider = legacyProviderDefaults()
		meta, err = toml.DecodeFile(path, &cfg)
		if err != nil {
			return Config{}, fmt.Errorf("parse %s: %w", path, err)
		}
	}
	// api.upstream faces report unknown keys with the offending face named;
	// every other unknown key is reported by path below.
	if err := detectAPIUpstreamUnknownKeys(path); err != nil {
		return Config{}, err
	}
	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return Config{}, fmt.Errorf("unknown keys in %s: %s", path, strings.Join(keys, ", "))
	}
	registryDefined := meta.IsDefined("providers") || meta.IsDefined("models")
	cfg.providerRegistryExplicit = registryDefined
	normalizeLegacyProvider(&cfg, registryDefined)
	if err := validateProviderRegistry(cfg); err != nil {
		return Config{}, err
	}
	if err := validateLoopbackListen(cfg.Gateway.StatusListen); err != nil {
		return Config{}, fmt.Errorf("gateway.status_listen: %w", err)
	}
	if err := validateDashboardTailnet(cfg.Dashboard); err != nil {
		return Config{}, fmt.Errorf("dashboard.tailnet: %w", err)
	}
	if err := validateChatSocket(cfg.Chat.Socket); err != nil {
		return Config{}, fmt.Errorf("chat.socket: %w", err)
	}
	if err := validateSandboxResources(cfg.Sandbox); err != nil {
		return Config{}, fmt.Errorf("sandbox resources: %w", err)
	}
	if err := validatePolicy(cfg.Policy); err != nil {
		return Config{}, err
	}
	if err := validateLimits(cfg.Limits); err != nil {
		return Config{}, fmt.Errorf("limits: %w", err)
	}
	if err := validateFetch(cfg.Tools.Fetch); err != nil {
		return Config{}, fmt.Errorf("tools.fetch: %w", err)
	}
	if err := validateSearch(cfg.Search); err != nil {
		return Config{}, err
	}
	if _, _, _, err := cfg.SearchEffective(); err != nil {
		return Config{}, err
	}

	if cfg.Jobs.MaxAttempts < 1 {
		return Config{}, fmt.Errorf("jobs.max_attempts must be at least 1")
	}
	if err := requirePositiveDurations(map[string]string{"jobs.base_backoff": cfg.Jobs.BaseBackoff, "jobs.max_backoff": cfg.Jobs.MaxBackoff, "jobs.stall_timeout": cfg.Jobs.StallTimeout}, nil, "%s must be a positive duration, got %q"); err != nil {
		return Config{}, err
	}
	if cfg.Memory.WriteGate != "auto" && cfg.Memory.WriteGate != "notify" && cfg.Memory.WriteGate != "review" {
		return Config{}, fmt.Errorf("memory.write_gate: must be auto, notify, or review")
	}
	if cfg.Memory.InjectBudget < 0 {
		return Config{}, fmt.Errorf("memory.inject_budget: must be >= 0")
	}
	// Empty or "0" disables the corresponding idle-reflection setting (#59).
	disablesEmptyOrZero := func(v string) bool { return v == "" || v == "0" }
	if err := requirePositiveDurations(map[string]string{
		"memory.reflect_after": cfg.Memory.ReflectAfter,
		"memory.reflect_every": cfg.Memory.ReflectEvery,
	}, disablesEmptyOrZero, "%s must be a positive duration (or \"0\" to disable), got %q"); err != nil {
		return Config{}, err
	}
	switch cfg.Selfdev.Approval {
	case "manual", "ci", "auto-patch":
	default:
		return Config{}, fmt.Errorf("selfdev.approval: unknown value %q (want \"manual\", \"ci\", or \"auto-patch\")", cfg.Selfdev.Approval)
	}
	if err := validateRequiredChecks(cfg.Selfdev.RequiredChecks); err != nil {
		return Config{}, err
	}
	if err := validateWorkspaceEgress(cfg.Workspace); err != nil {
		return Config{}, fmt.Errorf("workspace egress: %w", err)
	}
	disablesZero := func(v string) bool { return v == "0" }
	if err := requirePositiveDurations(map[string]string{"workspace.idle_timeout": cfg.Workspace.IdleTimeout, "workspace.close_ttl": cfg.Workspace.CloseTTL, "store.retain": cfg.Store.Retain}, disablesZero, "%s must be 0 or a positive duration, got %q"); err != nil {
		return Config{}, err
	}
	app := cfg.GitHub.App
	if app.AppID < 0 || app.InstallationID < 0 {
		return Config{}, errors.New("github.app ids must be positive")
	}
	if app.AppID != 0 || app.InstallationID != 0 || app.PrivateKey != "" {
		if app.AppID <= 0 || app.InstallationID <= 0 || app.PrivateKey == "" {
			return Config{}, errors.New("github.app requires app_id, installation_id, and private_key")
		}
	}
	for _, s := range cfg.MCP {
		// #249: command and url are the two transports; exactly one is set.
		hasCommand := strings.TrimSpace(s.Command) != ""
		hasURL := strings.TrimSpace(s.URL) != ""
		if hasCommand && hasURL {
			return Config{}, fmt.Errorf("mcp %q: command and url are mutually exclusive (configure exactly one)", s.Name)
		}
		if !hasCommand && !hasURL {
			return Config{}, fmt.Errorf("mcp %q: exactly one of command or url is required", s.Name)
		}
		if hasURL {
			if err := validateRemoteMCPServer(s); err != nil {
				return Config{}, err
			}
			continue
		}
		if s.Execution != "" && s.Execution != "host" && s.Execution != "sandbox" {
			return Config{}, fmt.Errorf("mcp %q: execution must be \"host\" or \"sandbox\", got %q", s.Name, s.Execution)
		}
		// Code-intelligence MCP isolation (#79): no secret env; sandbox by default.
		if err := validateCodeIntelMCP(s, cfg.CodeIntel.AllowHostMCP); err != nil {
			return Config{}, err
		}
	}
	if cfg.Workspace.Hooks.Timeout != "" {
		if d, err := ParseDuration(cfg.Workspace.Hooks.Timeout); err != nil || d <= 0 {
			return Config{}, fmt.Errorf("workspace.hooks.timeout must be a positive duration, got %q", cfg.Workspace.Hooks.Timeout)
		}
	}
	for i, w := range cfg.Intake.GitHub {
		if w.Repo == "" {
			return Config{}, fmt.Errorf("intake.github[%d]: repo is required", i)
		}
		if !repoRE.MatchString(w.Repo) {
			return Config{}, fmt.Errorf("intake.github[%d]: repo must be owner/name, got %q", i, w.Repo)
		}
		if w.MaxConcurrency < 1 {
			return Config{}, fmt.Errorf("intake.github[%d]: max_concurrency must be at least 1", i)
		}
		if w.PollInterval != "" {
			if d, err := ParseDuration(w.PollInterval); err != nil || d <= 0 {
				return Config{}, fmt.Errorf("intake.github[%d]: poll_interval must be a positive duration, got %q", i, w.PollInterval)
			}
		}
	}
	if err := validateProfiles(path, cfg.Agent, cfg.API); err != nil {
		return Config{}, err
	}
	if err := validateAPIUpstreams(cfg.API); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func validateChatSocket(socket string) error {
	if socket == "" {
		return nil
	}
	if strings.ContainsRune(socket, '\x00') {
		return errors.New("must not contain a NUL byte")
	}
	if !filepath.IsAbs(socket) {
		return errors.New("must be an absolute path")
	}
	if filepath.Clean(socket) != socket {
		return errors.New("must be a clean path")
	}
	return nil
}

func legacyProviderDefaults() Provider {
	return Provider{
		Name:      "anthropic",
		Model:     "claude-opus-4-8",
		APIKey:    "secret://anthropic/api-key",
		MaxTokens: 64000,
	}
}

func normalizeLegacyProvider(cfg *Config, registryDefined bool) {
	if registryDefined || strings.TrimSpace(cfg.Provider.Name) == "" {
		return
	}
	cfg.legacyProviderNormalized = true
	cfg.Providers = map[string]ProviderConnection{
		"default": {
			Type:      cfg.Provider.Name,
			APIKey:    cfg.Provider.APIKey,
			BaseURL:   cfg.Provider.BaseURL,
			MaxTokens: cfg.Provider.MaxTokens,
		},
	}
	cfg.Models = make(map[string]ModelTarget, 2)
	if strings.TrimSpace(cfg.Provider.Model) != "" {
		cfg.Models["default"] = ModelTarget{Provider: "default", Model: cfg.Provider.Model}
		if cfg.Agent.DefaultModel == "" {
			cfg.Agent.DefaultModel = "default"
		}
	}
	if strings.TrimSpace(cfg.Provider.UtilityModel) != "" {
		cfg.Models["utility"] = ModelTarget{Provider: "default", Model: cfg.Provider.UtilityModel}
		if cfg.Agent.UtilityModel == "" {
			cfg.Agent.UtilityModel = "utility"
		}
	}
}

func validateProviderRegistry(cfg Config) error {
	for name, connection := range cfg.Providers {
		if err := validateProviderConnection(name, connection, cfg.legacyProviderNormalized); err != nil {
			return err
		}
	}
	for alias, target := range cfg.Models {
		if !ValidModelAlias(alias) {
			return fmt.Errorf("invalid model alias %q (want slug [a-z0-9-] max %d)", alias, ProviderConnectionNameMax)
		}
		if strings.TrimSpace(target.Provider) == "" {
			return fmt.Errorf("model alias %q: provider is required", alias)
		}
		if _, ok := cfg.Providers[target.Provider]; !ok {
			return fmt.Errorf("model alias %q references unknown provider %q", alias, target.Provider)
		}
		if strings.TrimSpace(target.Model) == "" {
			return fmt.Errorf("model alias %q: model is required", alias)
		}
		if target.MaxTokens < 0 {
			return fmt.Errorf("model alias %q: max_tokens must be >= 0", alias)
		}
	}
	for field, alias := range map[string]string{
		"agent.default_model": cfg.Agent.DefaultModel,
		"agent.utility_model": cfg.Agent.UtilityModel,
	} {
		if alias == "" {
			continue
		}
		if _, ok := cfg.Models[alias]; !ok {
			return fmt.Errorf("%s references unknown model alias %q", field, alias)
		}
	}
	return nil
}

func validateProviderConnection(name string, connection ProviderConnection, allowLegacyAPIKey bool) error {
	if !ValidProviderConnectionName(name) {
		return fmt.Errorf("invalid connection name %q (want slug [a-z0-9-] max %d)", name, ProviderConnectionNameMax)
	}
	switch connection.Type {
	case "anthropic", "openai":
	default:
		return fmt.Errorf("provider connection %q: unsupported type %q (want \"anthropic\" or \"openai\")", name, connection.Type)
	}
	if connection.MaxTokens < 0 {
		return fmt.Errorf("provider connection %q: max_tokens must be >= 0", name)
	}
	expectedAPIKey := "secret://provider/" + name + "/api-key"
	if !allowLegacyAPIKey && connection.APIKey != "" && connection.APIKey != expectedAPIKey {
		return fmt.Errorf("provider connection %q: api_key must be empty or %s", name, expectedAPIKey)
	}
	return nil
}

var repoRE = regexp.MustCompile(`^[\w.-]+/[\w.-]+$`)

func validateLimits(l Limits) error {
	if l.TokensPerDay < 0 || l.RequestsPerHour < 0 {
		return errors.New("limits must be zero (unlimited) or positive")
	}
	if l.TunnelBytesPerSession < 0 {
		return errors.New("limits.tunnel_bytes_per_session must be zero (unlimited) or positive")
	}
	if l.AlertThresholdPercent < 0 || l.AlertThresholdPercent > 100 {
		return errors.New("alert_threshold_percent must be between 0 and 100")
	}
	for _, g := range l.Groups {
		if err := validateLimits(g); err != nil {
			return err
		}
	}
	return nil
}

func validateWorkspaceEgress(w Workspace) error {
	switch w.Egress {
	case "", "none", "allowlist", "full":
	default:
		return fmt.Errorf("egress must be none, allowlist, or full, got %q", w.Egress)
	}
	if w.Egress == "allowlist" && len(w.Allowlist) == 0 {
		return errors.New("allowlist egress requires at least one host")
	}
	for _, host := range w.Allowlist {
		host = strings.TrimSpace(strings.ToLower(host))
		if host == "" || strings.ContainsAny(host, "/?#") || net.ParseIP(host) != nil {
			return fmt.Errorf("invalid allowlist host %q", host)
		}
	}
	return nil
}

// checkNameRE rejects unknown or malformed required-check names at config
// load (#415): GitHub check/job names are alphanumeric, with separators like
// spaces, hyphens, slashes, dots, and parentheses.
var checkNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 _/().\-]+$`)

// validateRequiredChecks rejects unknown, empty, or duplicate entries so a
// typo in approval=ci's required_checks can never silently weaken the gate.
func validateRequiredChecks(checks []string) error {
	seen := map[string]bool{}
	for _, c := range checks {
		c = strings.TrimSpace(c)
		if c == "" {
			return errors.New("selfdev.required_checks: check names may not be empty")
		}
		if !checkNameRE.MatchString(c) {
			return fmt.Errorf("selfdev.required_checks: unknown or malformed check name %q", c)
		}
		if seen[c] {
			return fmt.Errorf("selfdev.required_checks: duplicate check name %q", c)
		}
		seen[c] = true
	}
	return nil
}

var memoryLimitRE = regexp.MustCompile(`(?i)^[1-9][0-9]*(b|k|m|g|t|kb|mb|gb|tb)$`)

func validateSandboxResources(s Sandbox) error {
	if !memoryLimitRE.MatchString(s.Memory) {
		return fmt.Errorf("memory must be a positive Docker size such as 2g, got %q", s.Memory)
	}
	if s.Disk != "" && !memoryLimitRE.MatchString(s.Disk) {
		return fmt.Errorf("disk must be a positive Docker size such as 10g, got %q", s.Disk)
	}

	if s.CPUs <= 0 {
		return fmt.Errorf("cpus must be positive, got %g", s.CPUs)
	}
	if s.PIDs <= 0 {
		return fmt.Errorf("pids must be positive, got %d", s.PIDs)
	}
	switch s.Enforcer {
	case "", "none", "feedback":
	default:
		return fmt.Errorf("enforcer must be none or feedback, got %q", s.Enforcer)
	}
	return nil
}

// PolicyRules returns the merged rule list from [[policy.rule]] and [policy].rules.
func (p PolicyConfig) PolicyRules() []PolicyRule {
	if len(p.Rule) == 0 {
		return p.Rules
	}
	if len(p.Rules) == 0 {
		return p.Rule
	}
	out := make([]PolicyRule, 0, len(p.Rule)+len(p.Rules))
	out = append(out, p.Rule...)
	out = append(out, p.Rules...)
	return out
}

func validatePolicy(p PolicyConfig) error {
	rules := p.PolicyRules()
	for i, r := range rules {
		label := "policy.rule"
		if strings.TrimSpace(r.Name) == "" {
			return fmt.Errorf("%s[%d]: name is required", label, i)
		}
		switch r.Action {
		case "allow", "deny", "require":
		default:
			return fmt.Errorf("%s[%d] %q: action must be allow, deny, or require, got %q", label, i, r.Name, r.Action)
		}
		if r.Action == "require" && strings.TrimSpace(r.Requires) == "" {
			return fmt.Errorf("%s[%d] %q: require action needs non-empty requires", label, i, r.Name)
		}
		if r.Regex != "" {
			if _, err := regexp.Compile(r.Regex); err != nil {
				return fmt.Errorf("%s[%d] %q: bad regex: %w", label, i, r.Name, err)
			}
		}
		if r.Tool == "" && r.Match == "" && r.Regex == "" {
			return fmt.Errorf("%s[%d] %q: need tool, match, or regex", label, i, r.Name)
		}
	}
	return nil
}

func validateFetch(f Fetch) error {
	for _, entry := range f.AllowPrivate {
		if _, _, err := net.ParseCIDR(entry); err == nil {
			continue
		}
		host, port, err := net.SplitHostPort(entry)
		if err != nil || host == "" || port == "" {
			return fmt.Errorf("allow_private entry %q must be a CIDR or host:port", entry)
		}
	}
	return nil
}

// validateDashboardTailnet fails closed on any incomplete tailnet Desk opt-in.
// Logins are not validated as email addresses: tailscaled reports SSO logins in
// forms like "user@github" that no email shape check would accept.
func validateDashboardTailnet(d Dashboard) error {
	if !d.Tailnet.Enabled {
		if d.Tailnet.ServeHost != "" || len(d.Tailnet.AllowedLogins) > 0 {
			return fmt.Errorf("serve_host and allowed_logins require enabled = true")
		}
		return nil
	}
	if !d.Enabled {
		return fmt.Errorf("requires dashboard.enabled = true")
	}
	host := strings.TrimSpace(d.Tailnet.ServeHost)
	if host == "" {
		return fmt.Errorf("serve_host is required")
	}
	if host != d.Tailnet.ServeHost {
		return fmt.Errorf("serve_host must not have leading or trailing whitespace")
	}
	if strings.ContainsAny(host, "/:@*") {
		return fmt.Errorf("serve_host %q must be a bare DNS name without scheme, port, or wildcard", host)
	}
	if net.ParseIP(host) != nil {
		return fmt.Errorf("serve_host %q must be a MagicDNS name, not an IP address", host)
	}
	if !strings.Contains(host, ".") {
		return fmt.Errorf("serve_host %q must be the fully qualified MagicDNS name", host)
	}
	if len(d.Tailnet.AllowedLogins) == 0 {
		return fmt.Errorf("allowed_logins must list at least one login")
	}
	seen := make(map[string]struct{}, len(d.Tailnet.AllowedLogins))
	for _, login := range d.Tailnet.AllowedLogins {
		if login == "" || strings.TrimSpace(login) != login || strings.ContainsAny(login, " \t") {
			return fmt.Errorf("allowed_logins entry %q must be a non-empty login without whitespace", login)
		}
		lowered := strings.ToLower(login)
		if _, duplicate := seen[lowered]; duplicate {
			return fmt.Errorf("allowed_logins entry %q is duplicated", login)
		}
		seen[lowered] = struct{}{}
	}
	return nil
}

func validateLoopbackListen(listen string) error {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("invalid listen address %q: %w", listen, err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("must bind a loopback address, got %q", listen)
	}
	return nil
}

// codeIntelToolNames mirrors codeintel.ToolNames for config-time validation
// without importing the codeintel package into config load.
var codeIntelToolNames = map[string]bool{
	"code_find_symbol":   true,
	"code_references":    true,
	"code_callers":       true,
	"code_structure":     true,
	"code_blast_radius":  true,
	"code_suggest_tests": true,
}

// mcpServerNameRE constrains url-server names to the secret-store path
// convention (mcp/<name>/...) so `waffle mcp login` token names are always
// valid. Command servers keep the historical free-form name.
var mcpServerNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// validateRemoteMCPServer enforces the #249 contract for url-based servers:
// a valid http(s) URL, no process-level execution policy, egress only
// "broker"/"direct", a token that is a secret:// reference or absent, and a
// server name usable as a secret-store path. Every violation is a load
// error naming the server — no permissive fallback.
func validateRemoteMCPServer(s MCPServer) error {
	if !mcpServerNameRE.MatchString(s.Name) {
		return fmt.Errorf("mcp %q: url servers need a lowercase [a-z0-9._-] name (OAuth tokens are stored as mcp/<name>/... in the secret store)", s.Name)
	}
	u, err := url.Parse(s.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("mcp %q: url must be an absolute http(s) URL, got %q", s.Name, s.URL)
	}
	if s.Execution != "" && s.Execution != "host" {
		return fmt.Errorf("mcp %q: execution=%q is not supported for url servers (there is no process to sandbox); use egress to control reachability", s.Name, s.Execution)
	}
	if s.Egress != "" && s.Egress != "broker" && s.Egress != "direct" {
		return fmt.Errorf("mcp %q: egress must be \"broker\" or \"direct\", got %q", s.Name, s.Egress)
	}
	if s.Token != "" && !strings.HasPrefix(s.Token, "secret://") {
		return fmt.Errorf("mcp %q: token must be a secret:// reference (raw credentials never live in config.toml)", s.Name)
	}
	if len(s.Args) > 0 {
		return fmt.Errorf("mcp %q: args apply to command servers only", s.Name)
	}
	if len(s.Env) > 0 {
		return fmt.Errorf("mcp %q: env applies to command servers only (remote servers receive no ambient environment)", s.Name)
	}
	return nil
}

func validateCodeIntelMCP(s MCPServer, allowHost bool) error {
	exposes := false
	for _, t := range s.Tools {
		base := t
		if i := strings.LastIndex(t, "__"); i >= 0 {
			base = t[i+2:]
		}
		if codeIntelToolNames[base] {
			exposes = true
			break
		}
	}
	// Name convention: servers named codeintel* are treated as codeintel even
	// without declared tools (fail closed on secrets/host).
	if !exposes && !strings.HasPrefix(strings.ToLower(s.Name), "codeintel") {
		return nil
	}
	for _, e := range s.Env {
		u := strings.ToUpper(e)
		if strings.Contains(u, "TOKEN") || strings.Contains(u, "SECRET") ||
			strings.Contains(u, "PASSWORD") || strings.Contains(u, "API_KEY") ||
			u == "WAFFLE_AGE_IDENTITY" {
			return fmt.Errorf("mcp %q: code-intelligence servers must not receive secret env %q", s.Name, e)
		}
	}
	if s.Execution == "" || s.Execution == "host" {
		if !allowHost {
			return fmt.Errorf("mcp %q: code-intelligence requires execution=\"sandbox\" (or [codeintel] allow_host_mcp = true)", s.Name)
		}
	}
	return nil
}
