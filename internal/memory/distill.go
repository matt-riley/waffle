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
type DistillTool struct {
	WS         Workspace
	Gate       *Gate
	Provenance Provenance
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

	gate := t.Gate
	if gate == nil {
		gate = &Gate{Mode: "auto", WS: t.WS}
	}
	candidate := Candidate{Kind: "skill", Name: in.Name, Description: in.Description, Body: in.Body, Provenance: t.Provenance}
	c, err := gate.submit(candidate, func() error { return t.WS.writeSkillCandidate(candidate) })
	if err != nil {
		return "", err
	}
	if c.Status == "pending" {
		return fmt.Sprintf("skill candidate %s is pending owner approval", c.ID), nil
	}
	return fmt.Sprintf("%s skill %q at %s — available as /skill %s", skillVerb(t.WS, in.Name), in.Name, filepath.Join(t.WS.SkillsDir(), in.Name, "SKILL.md"), in.Name), nil
}

func skillVerb(w Workspace, name string) string {
	if fileExists(filepath.Join(w.SkillsDir(), name, "SKILL.md")) {
		return "updated"
	}
	return "saved new"
}

func (w Workspace) writeSkillCandidate(c Candidate) error {
	dir := filepath.Join(w.SkillsDir(), c.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	content := fmt.Sprintf("---\nname: %s\ndescription: %s\nprovenance: %s\nsource_id: %s\ntrust_class: %s\n---\n\n%s\n",
		c.Name, strconv.Quote(oneLine(c.Description)), c.Provenance.SourceKind, c.Provenance.SourceID, c.Provenance.TrustClass, strings.TrimSpace(c.Body))
	path := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return err
	}
	return nil
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
