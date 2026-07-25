package providerconfig

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/matt-riley/waffle/internal/config"
)

// Agent profiles are a trust boundary, so they are edited through the same
// locked, staged, journalled transaction as provider credentials (#194).
// Nothing here accepts raw TOML: every field is typed, and the document editor
// only ever rewrites the profile's own table, leaving unrelated config bytes
// untouched.

// ErrProfileNotFound is returned when an edit or delete names a profile that
// is not configured.
var ErrProfileNotFound = errors.New("agent profile does not exist")

// ProfileRequest is one structured profile edit. It mirrors
// config.AgentProfile field for field; there is no free-form escape hatch.
type ProfileRequest struct {
	Name            string
	System          string
	Model           string
	Sandbox         string
	Allow           []string
	Deny            []string
	DenyPrefixes    []string
	Guidance        string
	MaxTokens       int
	MaxIterations   int
	AllowedChildren []string
}

// AgentProfile renders the request as the config type it will become, so
// validation and preview run against the same value that gets written.
func (r ProfileRequest) AgentProfile() config.AgentProfile {
	return config.AgentProfile{
		System:          strings.TrimSpace(r.System),
		Model:           strings.TrimSpace(r.Model),
		Sandbox:         strings.TrimSpace(r.Sandbox),
		Guidance:        strings.TrimSpace(r.Guidance),
		MaxTokens:       r.MaxTokens,
		MaxIterations:   r.MaxIterations,
		DenyPrefixes:    trimmedList(r.DenyPrefixes),
		AllowedChildren: trimmedList(r.AllowedChildren),
		Tools: config.ToolPolicy{
			Allow: trimmedList(r.Allow),
			Deny:  trimmedList(r.Deny),
		},
	}
}

func trimmedList(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// PutProfile creates or replaces one agent profile.
func (m *Manager) PutProfile(ctx context.Context, req ProfileRequest, mode CommitMode) (MutationResult, error) {
	return runWithCommitMode(ctx, mode, func(modeCtx context.Context) error {
		return m.putProfile(modeCtx, req)
	})
}

func (m *Manager) putProfile(ctx context.Context, req ProfileRequest) (err error) {
	lease, err := m.acquire(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lease.Release()) }()
	if err := m.recoverLocked(ctx); err != nil {
		return err
	}

	name := strings.TrimSpace(req.Name)
	if !config.ValidProfileName(name) {
		return fmt.Errorf("invalid profile name %q (want slug [a-z0-9-] max %d)", name, config.ProfileNameMax)
	}
	profile := req.AgentProfile()
	if err := validateProfileFields(profile); err != nil {
		return err
	}

	before, err := m.capture(ctx)
	if err != nil {
		return err
	}
	if err := ensureCanonicalManagedSource(before.configBytes, before.cfg); err != nil {
		return err
	}
	// Narrowing is checked before staging so a widening edit never reaches
	// the filesystem, and again on the reloaded candidate below.
	if err := config.ValidateProfileNarrows(before.cfg.AgentPolicy(ProfileGroup(before.cfg, name)), profile); err != nil {
		return err
	}

	configStage, candidate, err := m.stageConfig(before.configBytes, func(doc *tomlDocument, cfg config.Config) error {
		writeProfileTable(doc, name, profile)
		return nil
	})
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(configStage) }()

	written, ok := candidate.Agent.Profiles[name]
	if !ok {
		return fmt.Errorf("semantic profile write failed for %q", name)
	}
	// Re-validate what the file actually parses back to. The document editor
	// is textual, so the authoritative check is on the reloaded candidate.
	if err := config.ValidateProfileNarrows(
		candidate.AgentPolicy(ProfileGroup(candidate, name)), written); err != nil {
		return err
	}
	if err := validateProfileCandidate(before.cfg, candidate, name); err != nil {
		return err
	}
	return m.commit(ctx, before, configStage, "", candidate, "")
}

// RemoveProfile deletes a profile. extraReferences carries references the
// caller found outside config.toml (open sessions, workspaces, scheduled
// jobs); they are refused with the same message as config-level ones (#194 AC4).
func (m *Manager) RemoveProfile(ctx context.Context, name string, extraReferences []string, mode CommitMode) (MutationResult, error) {
	return runWithCommitMode(ctx, mode, func(modeCtx context.Context) error {
		return m.removeProfile(modeCtx, name, extraReferences)
	})
}

func (m *Manager) removeProfile(ctx context.Context, name string, extraReferences []string) (err error) {
	lease, err := m.acquire(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lease.Release()) }()
	if err := m.recoverLocked(ctx); err != nil {
		return err
	}

	name = strings.TrimSpace(name)
	if !config.ValidProfileName(name) {
		return fmt.Errorf("invalid profile name %q", name)
	}
	before, err := m.capture(ctx)
	if err != nil {
		return err
	}
	if err := ensureCanonicalManagedSource(before.configBytes, before.cfg); err != nil {
		return err
	}
	if _, ok := before.cfg.Agent.Profiles[name]; !ok {
		return fmt.Errorf("%w: %q", ErrProfileNotFound, name)
	}

	refs := append(ProfileReferences(before.cfg, name), trimmedList(extraReferences)...)
	sort.Strings(refs)
	if len(refs) > 0 {
		return fmt.Errorf("%w: %q is still used by %s", ErrReferenced, name, strings.Join(refs, ", "))
	}

	configStage, candidate, err := m.stageConfig(before.configBytes, func(doc *tomlDocument, cfg config.Config) error {
		doc.deleteTableTree("agent.profile." + name)
		return nil
	})
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(configStage) }()
	if _, stillPresent := candidate.Agent.Profiles[name]; stillPresent {
		return fmt.Errorf("semantic profile removal failed for %q", name)
	}
	if err := validateProfileCandidate(before.cfg, candidate, name); err != nil {
		return err
	}
	return m.commit(ctx, before, configStage, "", candidate, "")
}

// ProfileGroup resolves which agent group a profile is measured against: its
// own when a group of that name exists, otherwise the main interactive group.
// This matches the chat runtime and the posture projection (#155).
func ProfileGroup(cfg config.Config, name string) string {
	if _, isGroup := cfg.Agent.Groups[name]; isGroup {
		return name
	}
	return config.GroupMain
}

// ProfileReferences lists the config-level references to a profile, so a
// delete can name what still depends on it.
func ProfileReferences(cfg config.Config, name string) []string {
	var refs []string
	if cfg.Agent.DefaultProfile == name {
		refs = append(refs, "agent.default_profile")
	}
	for _, other := range SortedKeys(cfg.Agent.Profiles) {
		if other == name {
			continue
		}
		for _, child := range cfg.Agent.Profiles[other].AllowedChildren {
			if child == name {
				refs = append(refs, "agent.profile."+other+".allowed_children")
				break
			}
		}
	}
	return refs
}

func validateProfileFields(profile config.AgentProfile) error {
	if profile.Sandbox != "" && profile.Sandbox != "host" && profile.Sandbox != "docker" {
		return fmt.Errorf("profile sandbox must be host or docker, got %q", profile.Sandbox)
	}
	if profile.MaxTokens < 0 {
		return fmt.Errorf("profile max_tokens cannot be negative")
	}
	if profile.MaxIterations < 0 {
		return fmt.Errorf("profile max_iterations cannot be negative")
	}
	for _, child := range profile.AllowedChildren {
		if !config.ValidProfileName(child) {
			return fmt.Errorf("profile allowed_children contains invalid name %q", child)
		}
	}
	// Tool names are identifiers. Refusing anything else here keeps shell
	// metacharacters and paths out of the policy tables entirely.
	for field, values := range map[string][]string{
		"tools.allow": profile.Tools.Allow,
		"tools.deny":  profile.Tools.Deny,
	} {
		for _, value := range values {
			if !validToolIdentifier(value) {
				return fmt.Errorf("profile %s contains invalid tool name %q", field, value)
			}
			// Refuse an unknown tool by name here rather than letting the
			// staged config fail to load with a lower-level message.
			if !config.ValidProfileTool(value) {
				return fmt.Errorf("profile %s names unknown tool %q", field, value)
			}
		}
	}
	return nil
}

func validToolIdentifier(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_' || r == '-' || r == '.' || r == '*':
		default:
			return false
		}
	}
	return true
}

// validateProfileCandidate asserts a profile edit changed only the profile
// table it claimed to. Providers, models, and every other profile must survive
// byte-for-byte in the reloaded config (#194 AC6).
func validateProfileCandidate(before, candidate config.Config, changed string) error {
	if !equalMaps(before.Providers, candidate.Providers) {
		return errors.New("profile edit altered provider connections")
	}
	if !equalMaps(before.Models, candidate.Models) {
		return errors.New("profile edit altered model aliases")
	}
	if before.Agent.DefaultProfile != candidate.Agent.DefaultProfile ||
		before.Agent.DefaultModel != candidate.Agent.DefaultModel ||
		before.Agent.UtilityModel != candidate.Agent.UtilityModel {
		return errors.New("profile edit altered agent defaults")
	}
	if len(before.Agent.Groups) != len(candidate.Agent.Groups) {
		return errors.New("profile edit altered agent groups")
	}
	for name, group := range before.Agent.Groups {
		other, ok := candidate.Agent.Groups[name]
		if !ok || group.Sandbox != other.Sandbox {
			return errors.New("profile edit altered agent group " + name)
		}
	}
	for name := range before.Agent.Profiles {
		if name == changed {
			continue
		}
		if _, ok := candidate.Agent.Profiles[name]; !ok {
			return errors.New("profile edit removed unrelated profile " + name)
		}
	}
	return nil
}

// writeProfileTable rewrites one profile's table in place. Keys the request
// leaves empty are deleted rather than written blank, so a profile never
// accumulates dead settings.
func writeProfileTable(doc *tomlDocument, name string, profile config.AgentProfile) {
	table := "agent.profile." + name
	doc.ensureTable(table)
	doc.setOptional(table, "system", profile.System)
	doc.setOptional(table, "model", profile.Model)
	doc.setOptional(table, "sandbox", profile.Sandbox)
	doc.setOptional(table, "guidance", profile.Guidance)
	doc.setOptionalInt(table, "max_tokens", profile.MaxTokens)
	doc.setOptionalInt(table, "max_iterations", profile.MaxIterations)
	doc.setOptionalStrings(table, "deny_prefixes", profile.DenyPrefixes)
	doc.setOptionalStrings(table, "allowed_children", profile.AllowedChildren)

	tools := table + ".tools"
	if len(profile.Tools.Allow) == 0 && len(profile.Tools.Deny) == 0 {
		doc.deleteTableTree(tools)
		return
	}
	doc.ensureTable(tools)
	doc.setOptionalStrings(tools, "allow", profile.Tools.Allow)
	doc.setOptionalStrings(tools, "deny", profile.Tools.Deny)
}

func renderStringArray(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, strconv.Quote(value))
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}
