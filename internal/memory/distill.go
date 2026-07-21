package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/matt-riley/waffle/internal/llm"
)

// DistillTool lets the agent turn a procedure it just worked out into a
// reusable skill (docs/plan.md, "Self-development loop" / hermes-style
// learning loop). Skills are agentskills.io-compatible SKILL.md dirs, so
// what waffle distills is portable.
//
// Distilled skills are written inactive (status: inactive) until the owner
// runs `waffle skills activate` (#65). Overwriting an active skill is refused
// without validation.
type DistillTool struct {
	WS         Workspace
	Gate       *Gate
	Provenance Provenance
	// ActiveCheck, when set, returns true if name is currently active.
	// Used to refuse overwrite of active skills. Nil uses frontmatter only.
	ActiveCheck func(name string) bool
}

func (t DistillTool) Def() llm.Tool {
	return llm.Tool{
		Name:        "distill_skill",
		Description: "Save a reusable skill after working out how to do something non-trivial, so next time it's one step. Writes skills/<name>/SKILL.md. Use for repeatable procedures (\"release the CLI\", \"refresh the staging DB\"), not one-off answers.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"name": {"type": "string", "description": "kebab-case skill name, e.g. \"release-cli\""},
				"description": {"type": "string", "description": "one line: when to use this skill"},
				"body": {"type": "string", "description": "the instructions, in Markdown — the steps to follow"}
			},
			"required": ["name", "description", "body"]
		}`),
	}
}

var skillNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

func (t DistillTool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	var in struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Body        string `json:"body"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("bad input: %w", err)
	}
	in.Name = strings.TrimSpace(in.Name)
	if !skillNameRE.MatchString(in.Name) {
		return "", errors.New("name must be kebab-case (lowercase, digits, hyphens)")
	}
	if strings.TrimSpace(in.Body) == "" {
		return "", errors.New("body is required")
	}
	// Validation gate (#65): reject empty/too-short skills and instruction-
	// shaped bodies that look like prompt-injection payloads.
	if err := validateSkillBody(in.Body); err != nil {
		return "", err
	}

	// Refuse overwrite of an active skill without validation (#65).
	path := filepath.Join(t.WS.SkillsDir(), in.Name, "SKILL.md")
	existed := fileExists(path)
	if existed {
		active := false
		if t.ActiveCheck != nil {
			active = t.ActiveCheck(in.Name)
		} else if raw, err := os.ReadFile(path); err == nil {
			active = skillFrontmatterActive(string(raw))
		}
		if active {
			return "", fmt.Errorf("cannot overwrite active skill %q without validation; deactivate or use waffle skills activate after review", in.Name)
		}
	}

	gate := t.Gate
	if gate == nil {
		gate = &Gate{Mode: "auto", WS: t.WS}
	}
	t.Provenance = provenanceFromContext(ctx, t.Provenance)
	// Always write inactive until explicit activation (#65), and force review
	// when write_gate is review (existing gate behavior).
	candidate := Candidate{Kind: "skill", Name: in.Name, Description: in.Description, Body: in.Body, Provenance: t.Provenance}
	c, err := gate.submit(ctx, candidate, func() error { return t.WS.writeSkillCandidate(candidate) })
	if err != nil {
		return "", err
	}
	if c.Status == "pending" {
		return fmt.Sprintf("skill candidate %s is pending owner approval (will be inactive until waffle skills activate)", c.ID), nil
	}
	verb := "saved new"
	if existed {
		verb = "updated"
	}
	return fmt.Sprintf("%s skill %q at %s — inactive until `waffle skills activate %s`", verb, in.Name, path, in.Name), nil
}

func (w Workspace) writeSkillCandidate(c Candidate) error {
	dir := filepath.Join(w.SkillsDir(), c.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, "SKILL.md")
	// Cannot overwrite active skill without validation (#65).
	if raw, err := os.ReadFile(path); err == nil && skillFrontmatterActive(string(raw)) {
		return fmt.Errorf("cannot overwrite active skill %q without validation", c.Name)
	}
	// status: inactive until waffle skills activate (#65).
	content := fmt.Sprintf("---\nname: %s\ndescription: %s\nstatus: inactive\nprovenance: %s\nsource_id: %s\ntrust_class: %s\nsession_id: %s\nchannel: %s\nuntrusted_context: %t\n---\n\n%s\n",
		c.Name, strconv.Quote(OneLine(c.Description)), c.Provenance.SourceKind, c.Provenance.SourceID, c.Provenance.TrustClass, c.Provenance.SessionID, c.Provenance.Channel, c.Provenance.UntrustedContext, strings.TrimSpace(c.Body))
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return err
	}
	return nil
}

// skillFrontmatterActive reports whether SKILL.md frontmatter status is
// active (or missing — pre-#65 skills default active).
func skillFrontmatterActive(raw string) bool {
	if !strings.HasPrefix(raw, "---\n") && raw != "---" {
		return true
	}
	rest := strings.TrimPrefix(raw, "---\n")
	fm, _, found := strings.Cut(rest, "\n---")
	if !found {
		return true
	}
	status := ""
	for _, line := range strings.Split(fm, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if strings.TrimSpace(key) == "status" {
			status = strings.Trim(strings.TrimSpace(value), `"'`)
		}
	}
	return status == "" || status == "active"
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// validateSkillBody enforces a minimal mechanical gate before distill writes (#65).
func validateSkillBody(body string) error {
	body = strings.TrimSpace(body)
	if len(body) < 20 {
		return errors.New("skill body too short to be a reusable procedure (need ≥20 chars)")
	}
	lower := strings.ToLower(body)
	for _, bad := range []string{
		"ignore previous instructions",
		"ignore all prior",
		"disregard system",
		"you are now",
		"<system>",
	} {
		if strings.Contains(lower, bad) {
			return fmt.Errorf("skill body rejected by validation gate (matches %q)", bad)
		}
	}
	return nil
}
