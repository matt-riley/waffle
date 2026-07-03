package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/matt-riley/waffle/internal/llm"
)

// DistillTool lets the agent turn a procedure it just worked out into a
// reusable skill (docs/plan.md, "Self-development loop" / hermes-style
// learning loop). Skills are agentskills.io-compatible SKILL.md dirs, so
// what waffle distills is portable.
type DistillTool struct {
	WS Workspace
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

	dir := filepath.Join(t.WS.SkillsDir(), in.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	content := fmt.Sprintf("---\nname: %s\ndescription: %s\n---\n\n%s\n",
		in.Name, in.Description, strings.TrimSpace(in.Body))
	path := filepath.Join(dir, "SKILL.md")
	existed := fileExists(path)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", err
	}
	verb := "saved new"
	if existed {
		verb = "updated"
	}
	return fmt.Sprintf("%s skill %q at %s — available as /skill %s", verb, in.Name, path, in.Name), nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
