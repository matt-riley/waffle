package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Posture is the observable shape of a profile's trust boundary: the system
// prompt it was given and the layered tool policy it runs under (#193).
//
// This package deliberately owns the resolution so the runtime, Desk, and the
// profile editor all read the same answer. A parallel reimplementation in a
// handler would be a second trust boundary that can drift from the first.

// System prompt source kinds.
const (
	// SystemPromptDefault means the profile sets no System of its own and
	// inherits the workspace's base prompt.
	SystemPromptDefault = "default"
	// SystemPromptInline means System is literal text in config.toml.
	SystemPromptInline = "inline"
	// SystemPromptFile means System resolved from a file under WAFFLE_HOME.
	SystemPromptFile = "file"
)

// SystemPrompt is a profile's resolved system text plus where it came from.
type SystemPrompt struct {
	// Text is the resolved prompt body, empty for SystemPromptDefault.
	Text string
	// Kind is one of the SystemPrompt* constants.
	Kind string
	// Path names the source file for SystemPromptFile, always relative to
	// WAFFLE_HOME. The absolute path, and therefore the home location, is
	// never carried here: callers project this straight to a browser (#193).
	Path string
}

// ResolveProfileSystem resolves a profile's System field. A value starting
// with "@", or ending in ".md", is a file under WAFFLE_HOME; anything else is
// inline text. Files outside WAFFLE_HOME are refused.
//
// This is the single resolution used by the chat runtime and by Desk's
// posture view, so what Desk shows is what the agent was actually told.
func ResolveProfileSystem(system string) (SystemPrompt, error) {
	trimmed := strings.TrimSpace(system)
	if trimmed == "" {
		return SystemPrompt{Kind: SystemPromptDefault}, nil
	}

	path := strings.TrimPrefix(trimmed, "@")
	isFile := strings.HasPrefix(trimmed, "@") || strings.HasSuffix(path, ".md")
	if !isFile {
		return SystemPrompt{Text: trimmed, Kind: SystemPromptInline}, nil
	}

	home, err := Home()
	if err != nil {
		return SystemPrompt{}, fmt.Errorf("profile system file: %w", err)
	}
	homeAbs, err := filepath.Abs(home)
	if err != nil {
		return SystemPrompt{}, fmt.Errorf("profile system file: %w", err)
	}
	// Relative paths resolve under WAFFLE_HOME; absolute paths must still sit
	// under it.
	if !filepath.IsAbs(path) {
		path = filepath.Join(homeAbs, path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return SystemPrompt{}, fmt.Errorf("profile system file: %w", err)
	}
	rel, err := filepath.Rel(homeAbs, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return SystemPrompt{}, fmt.Errorf("profile system file %q is outside WAFFLE_HOME", path)
	}
	body, err := os.ReadFile(abs)
	if err != nil {
		return SystemPrompt{}, fmt.Errorf("profile system file: %w", err)
	}
	return SystemPrompt{
		Text: strings.TrimSpace(string(body)),
		Kind: SystemPromptFile,
		Path: filepath.ToSlash(rel),
	}, nil
}

// ToolLayer is one narrowing contribution to a policy, expressed in this
// package's own terms. Repo policy (WAFFLE.md) arrives this way so that
// internal/config keeps no dependency on the repo-policy or tool packages.
type ToolLayer struct {
	Allow        []string
	Deny         []string
	DenyPrefixes []string
	Guidance     string
}

// IsZero reports whether the layer contributes nothing.
func (l ToolLayer) IsZero() bool {
	return len(l.Allow) == 0 && len(l.Deny) == 0 &&
		len(l.DenyPrefixes) == 0 && l.Guidance == ""
}

// PolicyLayer is one step of the layered resolution, recorded so a reader can
// see which tier contributed a restriction rather than only the flat result.
type PolicyLayer struct {
	// Name is the tier: "group", "profile", or "repo".
	Name string
	// Applied is false when the tier exists but changed nothing.
	Applied bool
	// Allow, Deny, DenyPrefixes, Guidance, and Sandbox are that tier's own
	// contribution, not the running total.
	Sandbox      string
	Allow        []string
	Deny         []string
	DenyPrefixes []string
	Guidance     string
	// Result is the running policy after this tier is applied.
	Result ResolvedAgentPolicy
}

// LayeredPolicy is the full derivation of an effective agent policy.
type LayeredPolicy struct {
	Layers    []PolicyLayer
	Effective ResolvedAgentPolicy
}

// ApplyProfilePolicy narrows base by a profile. This is the one implementation
// of profile narrowing: the chat runtime resolves permissions through it, Desk
// projects it, and the profile editor validates against it (#193, #194).
//
// Allow replaces (a profile names the toolset it wants), while Deny and
// DenyPrefixes only ever accumulate — a profile can never lift a denial its
// group imposed.
func ApplyProfilePolicy(base ResolvedAgentPolicy, profile AgentProfile) ResolvedAgentPolicy {
	out := base
	out.Allow = append([]string(nil), base.Allow...)
	out.Deny = append([]string(nil), base.Deny...)
	out.DenyPrefixes = append([]string(nil), base.DenyPrefixes...)

	if profile.Sandbox != "" {
		out.Mode = profile.Sandbox
	}
	if len(profile.Tools.Allow) > 0 {
		out.Allow = append([]string(nil), profile.Tools.Allow...)
	}
	for _, deny := range profile.Tools.Deny {
		out.Deny = AppendUnique(out.Deny, deny)
	}
	for _, prefix := range profile.DenyPrefixes {
		out.DenyPrefixes = AppendUnique(out.DenyPrefixes, prefix)
	}
	for _, prefix := range profile.Tools.DenyPrefixes {
		out.DenyPrefixes = AppendUnique(out.DenyPrefixes, prefix)
	}
	return out
}

// ApplyToolLayer tightens base by a repo-supplied layer. Unlike a profile, a
// repo may never replace the allowlist: WAFFLE.md can only intersect it, so a
// repository cannot grant its checkout a tool the host withheld.
func ApplyToolLayer(base ResolvedAgentPolicy, layer ToolLayer) ResolvedAgentPolicy {
	out := base
	out.Allow = append([]string(nil), base.Allow...)
	out.Deny = append([]string(nil), base.Deny...)
	out.DenyPrefixes = append([]string(nil), base.DenyPrefixes...)

	if len(layer.Allow) > 0 {
		if len(out.Allow) == 0 {
			// No host allowlist means "everything the toolbox has", so the repo
			// list is itself the intersection.
			out.Allow = append([]string(nil), layer.Allow...)
		} else {
			kept := make([]string, 0, len(out.Allow))
			for _, name := range out.Allow {
				if containsString(layer.Allow, name) {
					kept = append(kept, name)
				}
			}
			out.Allow = kept
		}
	}
	for _, deny := range layer.Deny {
		out.Deny = AppendUnique(out.Deny, deny)
	}
	for _, prefix := range layer.DenyPrefixes {
		out.DenyPrefixes = AppendUnique(out.DenyPrefixes, prefix)
	}
	if layer.Guidance != "" {
		out.Guidance = layer.Guidance
	}
	return out
}

// LayeredAgentPolicy derives the effective policy for a group/profile pair and
// records each tier's own contribution. repo is optional; pass nil when no
// repository policy is in scope (for example a profile viewed outside a
// workspace).
func (c Config) LayeredAgentPolicy(group, profileName string, repo *ToolLayer) LayeredPolicy {
	groupPolicy := c.AgentPolicy(group)
	layered := LayeredPolicy{Layers: []PolicyLayer{{
		Name:         "group",
		Applied:      true,
		Sandbox:      groupPolicy.Mode,
		Allow:        groupPolicy.Allow,
		Deny:         groupPolicy.Deny,
		DenyPrefixes: groupPolicy.DenyPrefixes,
		Guidance:     groupPolicy.Guidance,
		Result:       groupPolicy,
	}}}

	profile, _ := c.Profile(profileName)
	profileResult := ApplyProfilePolicy(groupPolicy, profile)
	layered.Layers = append(layered.Layers, PolicyLayer{
		Name:         "profile",
		Applied:      !samePolicy(groupPolicy, profileResult),
		Sandbox:      profile.Sandbox,
		Allow:        profile.Tools.Allow,
		Deny:         profile.Tools.Deny,
		DenyPrefixes: append(append([]string(nil), profile.DenyPrefixes...), profile.Tools.DenyPrefixes...),
		Guidance:     profile.Guidance,
		Result:       profileResult,
	})

	result := profileResult
	if repo != nil {
		repoResult := ApplyToolLayer(profileResult, *repo)
		layered.Layers = append(layered.Layers, PolicyLayer{
			Name:         "repo",
			Applied:      !samePolicy(profileResult, repoResult),
			Allow:        repo.Allow,
			Deny:         repo.Deny,
			DenyPrefixes: repo.DenyPrefixes,
			Guidance:     repo.Guidance,
			Result:       repoResult,
		})
		result = repoResult
	}

	layered.Effective = result
	return layered
}

func samePolicy(a, b ResolvedAgentPolicy) bool {
	return a.Mode == b.Mode && a.Guidance == b.Guidance &&
		sameStringSet(a.Allow, b.Allow) && sameStringSet(a.Deny, b.Deny) &&
		sameStringSet(a.DenyPrefixes, b.DenyPrefixes)
}

// sameStringSet compares tool and prefix lists as sets. These lists are
// unordered policy: a profile that restates its group's allowlist in a
// different order has changed nothing, and must not be reported as having
// applied a restriction.
func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	counts := make(map[string]int, len(a))
	for _, value := range a {
		counts[value]++
	}
	for _, value := range b {
		counts[value]--
		if counts[value] < 0 {
			return false
		}
	}
	return true
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
