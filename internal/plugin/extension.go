package plugin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Namespace is waffle's stable client-extension namespace (spec §8): the
// reverse-domain identifier under which all waffle-specific plugin data
// lives. Chosen in #394 and documented in docs/plan.md; keep it stable.
// Client-specific files may also live in a top-level directory of the same
// name (reserved; unused so far).
const Namespace = "dev.mattriley.waffle"

// WaffleExtension is the validated contents of extensions[Namespace]
// (spec §8.1). Everything is optional and evolves additively; unknown
// members are reported and ignored. This is the only channel for
// waffle-specific plugin policy — no new plugin.json top-level fields are
// ever introduced.
type WaffleExtension struct {
	// Skills maps skill names to activation overrides for plugin skills.
	// The skill_status table remains the final operator override.
	Skills map[string]WaffleSkillPolicy `json:"skills"`
	// MCP maps server names to policy overrides for plugin mcp.json
	// servers: execution/egress/groups/token. The #77/#79/#249 posture
	// still bounds their application (docker groups require broker egress,
	// unattended tiers stay deny-by-default).
	MCP map[string]WaffleMCPPolicy `json:"mcp"`
}

// WaffleSkillPolicy overrides one plugin skill's activation state.
type WaffleSkillPolicy struct {
	// Status is "active" or "inactive"; empty means no override.
	Status string `json:"status"`
}

// WaffleMCPPolicy overrides one plugin mcp.json server's waffle policy.
// The portable mcp.json itself carries none of these fields; they ship here
// so a plugin can assert them without weakening the restrictive defaults.
type WaffleMCPPolicy struct {
	// Execution is "host" or "sandbox" (stdio servers; default "host").
	Execution string `json:"execution"`
	// Egress is "broker" or "direct" (remote servers). Docker-mode groups
	// still refuse "direct" regardless of what the extension asserts.
	Egress string `json:"egress"`
	// Groups limits the server to named agent groups. Empty keeps the
	// default: all groups for stdio, main-only for remote.
	Groups []string `json:"groups"`
	// Token is a secret:// reference for a remote server's static bearer
	// credential (the only credential form plugin data may name).
	Token string `json:"token"`
}

// LoadWaffleExtension extracts and validates the waffle namespace from the
// manifest's extensions (spec §8.1). Foreign namespaces are ignored without
// validating their values. Malformed waffle-namespace data is reported in
// the returned warnings and ignored — it never rejects the plugin.
func LoadWaffleExtension(m Manifest) (ext WaffleExtension, warnings []string, err error) {
	raw, ok := m.Extensions[Namespace]
	if !ok {
		return WaffleExtension{}, nil, nil
	}
	if !jsonKind(raw, '{') {
		return WaffleExtension{}, []string{"waffle extension must be an object; ignored"}, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return WaffleExtension{}, []string{"waffle extension is not valid JSON; ignored"}, nil
	}
	for _, key := range sortedKeys(fields) {
		switch key {
		case "skills":
			skills, w := parseWaffleSkills(fields[key])
			warnings = append(warnings, w...)
			ext.Skills = skills
		case "mcp":
			mcpPolicy, w := parseWaffleMCP(fields[key])
			warnings = append(warnings, w...)
			ext.MCP = mcpPolicy
		default:
			warnings = append(warnings, fmt.Sprintf("unknown waffle extension member %q ignored", key))
		}
	}
	return ext, warnings, nil
}

// parseWaffleSkills decodes the skills object: each member is a closed
// WaffleSkillPolicy; a malformed member is reported and ignored while the
// rest still apply (component-level boundary).
func parseWaffleSkills(raw json.RawMessage) (map[string]WaffleSkillPolicy, []string) {
	out := map[string]WaffleSkillPolicy{}
	if !jsonKind(raw, '{') {
		return out, []string{"waffle extension skills must be an object; ignored"}
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return out, []string{"waffle extension skills is not valid JSON; ignored"}
	}
	var warnings []string
	for _, name := range sortedKeys(fields) {
		var policy WaffleSkillPolicy
		if err := decodeClosed(fields[name], &policy); err != nil {
			warnings = append(warnings, fmt.Sprintf("waffle skill policy for %q is malformed; ignored", name))
			continue
		}
		if policy.Status != "" && policy.Status != StatusActive && policy.Status != StatusInactive {
			warnings = append(warnings, fmt.Sprintf("waffle skill policy for %q has invalid status %q; ignored", name, policy.Status))
			continue
		}
		out[name] = policy
	}
	return out, warnings
}

// StatusActive/StatusInactive mirror skill's activation constants so the
// plugin package need not import internal/skill.
const (
	StatusActive   = "active"
	StatusInactive = "inactive"
)

// parseWaffleMCP decodes the mcp object: each member is a closed
// WaffleMCPPolicy; a malformed member is reported and ignored.
func parseWaffleMCP(raw json.RawMessage) (map[string]WaffleMCPPolicy, []string) {
	out := map[string]WaffleMCPPolicy{}
	if !jsonKind(raw, '{') {
		return out, []string{"waffle extension mcp must be an object; ignored"}
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return out, []string{"waffle extension mcp is not valid JSON; ignored"}
	}
	var warnings []string
	for _, name := range sortedKeys(fields) {
		var policy WaffleMCPPolicy
		if err := decodeClosed(fields[name], &policy); err != nil {
			warnings = append(warnings, fmt.Sprintf("waffle mcp policy for %q is malformed; ignored", name))
			continue
		}
		if policy.Execution != "" && policy.Execution != "host" && policy.Execution != "sandbox" {
			warnings = append(warnings, fmt.Sprintf("waffle mcp policy for %q has invalid execution %q; ignored", name, policy.Execution))
			continue
		}
		if policy.Egress != "" && policy.Egress != "broker" && policy.Egress != "direct" {
			warnings = append(warnings, fmt.Sprintf("waffle mcp policy for %q has invalid egress %q; ignored", name, policy.Egress))
			continue
		}
		out[name] = policy
	}
	return out, warnings
}

// decodeClosed decodes raw into target with a closed schema: any unknown
// field or trailing data is an error.
func decodeClosed(raw json.RawMessage, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("trailing data")
	}
	return nil
}
