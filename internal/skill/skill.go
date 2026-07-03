// Package skill loads SKILL.md skills (docs/plan.md, "Skills & memory").
// The format is agentskills.io-compatible — a directory per skill holding a
// SKILL.md with YAML frontmatter — so skills written for hermes-agent or
// openclaw port straight over.
package skill

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Skill is one discovered skill.
type Skill struct {
	Name        string
	Description string
	Path        string // the SKILL.md file
}

// Discover finds skills under dir (each in its own subdirectory). A missing
// dir means no skills, not an error.
func Discover(dir string) ([]Skill, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var skills []Skill
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(dir, e.Name(), "SKILL.md")
		body, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		s := Skill{Name: e.Name(), Path: path}
		s.Name, s.Description = parseFrontmatter(string(body), e.Name())
		skills = append(skills, s)
	}
	return skills, nil
}

// Body returns the skill's full SKILL.md content, frontmatter stripped.
func (s Skill) Body() (string, error) {
	raw, err := os.ReadFile(s.Path)
	if err != nil {
		return "", err
	}
	_, body := splitFrontmatter(string(raw))
	return strings.TrimSpace(body), nil
}

// Find returns the named skill from skills.
func Find(skills []Skill, name string) (Skill, bool) {
	for _, s := range skills {
		if s.Name == name {
			return s, true
		}
	}
	return Skill{}, false
}

// Index renders the skill list for the system prompt.
func Index(skills []Skill) string {
	if len(skills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n<skills>\nThe user can invoke these with /skill <name>; you can load one yourself by reading its SKILL.md path when the task matches its description.\n")
	for _, s := range skills {
		fmt.Fprintf(&b, "- %s: %s (%s)\n", s.Name, s.Description, s.Path)
	}
	b.WriteString("</skills>\n")
	return b.String()
}

// splitFrontmatter separates a leading "---\n...\n---" block from the body.
func splitFrontmatter(raw string) (frontmatter, body string) {
	if !strings.HasPrefix(raw, "---\n") && raw != "---" {
		return "", raw
	}
	rest := strings.TrimPrefix(raw, "---\n")
	fm, body, found := strings.Cut(rest, "\n---")
	if !found {
		return "", raw
	}
	return fm, strings.TrimPrefix(body, "\n")
}

// parseFrontmatter pulls name and description out of the frontmatter. It
// reads simple "key: value" lines — enough for the agentskills.io fields —
// rather than pulling in a YAML dependency.
func parseFrontmatter(raw, fallbackName string) (name, description string) {
	name = fallbackName
	fm, _ := splitFrontmatter(raw)
	for _, line := range strings.Split(fm, "\n") {
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		switch strings.TrimSpace(key) {
		case "name":
			if value != "" {
				name = value
			}
		case "description":
			description = value
		}
	}
	return name, description
}
