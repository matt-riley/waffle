package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writePlugin(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if strings.HasSuffix(rel, "/") {
			if err := os.MkdirAll(path, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDiscoverSkillsBasic(t *testing.T) {
	root := t.TempDir()
	writePlugin(t, root, map[string]string{
		"plugin.json":                        manifestJSON(t, baseFields()),
		"skills/summarize/SKILL.md":          "---\nname: summarize\ndescription: Summarizes things.\n---\n\nBody.\n",
		"skills/release/SKILL.md":            "---\nname: release\ndescription: Releases a build.\n---\n\nBody.\n",
		"skills/summarize/scripts/helper.sh": "#!/bin/sh\n",
	})

	skills, skips, err := DiscoverSkills(root)
	if err != nil {
		t.Fatalf("DiscoverSkills: %v", err)
	}
	if len(skips) != 0 {
		t.Errorf("skips = %v, want none", skips)
	}
	if len(skills) != 2 {
		t.Fatalf("skills = %d, want 2", len(skills))
	}
	if skills[0].Name != "release" || skills[1].Name != "summarize" {
		t.Errorf("skills sorted wrong: %+v", skills)
	}
	if skills[0].Description == "" || skills[1].Description == "" {
		t.Errorf("descriptions missing: %+v", skills)
	}
	for _, s := range skills {
		want := filepath.Join(resolved(t, root), "skills", s.Name, "SKILL.md")
		if s.Path != want {
			t.Errorf("Path = %q, want %q", s.Path, want)
		}
	}
}

func TestDiscoverSkillsNestedNotDiscovered(t *testing.T) {
	root := t.TempDir()
	writePlugin(t, root, map[string]string{
		"plugin.json":                manifestJSON(t, baseFields()),
		"skills/deploy/SKILL.md":     "---\nname: deploy\ndescription: d\n---\n\nBody.\n",
		"skills/deploy/sub/SKILL.md": "---\nname: sub\ndescription: nested\n---\n\nBody.\n",
	})
	skills, skips, err := DiscoverSkills(root)
	if err != nil {
		t.Fatalf("DiscoverSkills: %v", err)
	}
	if len(skills) != 1 || skills[0].Name != "deploy" {
		t.Errorf("skills = %+v, want only deploy (no recursion)", skills)
	}
	if len(skips) != 0 {
		t.Errorf("skips = %v, want none", skips)
	}
}

func TestDiscoverSkillsMissingOrInvalidLocation(t *testing.T) {
	root := t.TempDir()
	writePlugin(t, root, map[string]string{"plugin.json": manifestJSON(t, baseFields())})
	skills, skips, err := DiscoverSkills(root)
	if err != nil || len(skills) != 0 || len(skips) != 0 {
		t.Errorf("missing skills/ = %d skills, %v skips, %v; want zero and no error", len(skills), skips, err)
	}

	fileRoot := t.TempDir()
	writePlugin(t, fileRoot, map[string]string{
		"plugin.json": manifestJSON(t, baseFields()),
		"skills":      "I am a file, not a directory",
	})
	skills, skips, err = DiscoverSkills(fileRoot)
	if err != nil || len(skills) != 0 {
		t.Fatalf("file skills/ = %v skills, err %v; want zero skills", len(skills), err)
	}
	if len(skips) != 1 || !strings.Contains(skips[0].Reason, "not a directory") {
		t.Errorf("file skills/ skips = %v, want component-type-invalid report", skips)
	}
}

func TestDiscoverSkillsSkipsInvalid(t *testing.T) {
	root := t.TempDir()
	writePlugin(t, root, map[string]string{
		"plugin.json": manifestJSON(t, baseFields()),
		// Conforming skill — must survive the walk.
		"skills/good/SKILL.md": "---\nname: good\ndescription: A good skill.\n---\n\nBody.\n",
		// Bare skill, no frontmatter.
		"skills/bare/SKILL.md": "Just instructions, no frontmatter.\n",
		// Missing description.
		"skills/nodesc/SKILL.md": "---\nname: nodesc\n---\n\nBody.\n",
		// Invalid name.
		"skills/badname/SKILL.md": "---\nname: Bad-Name\ndescription: d\n---\n\nBody.\n",
		// Name mismatch with directory.
		"skills/renamed/SKILL.md": "---\nname: different\ndescription: d\n---\n\nBody.\n",
		// SKILL.md is a directory, not a file.
		"skills/dirskill/SKILL.md/": "",
		// A non-directory entry in skills/ is not a skill at all.
		"skills/loose.txt": "not a skill",
	})

	skills, skips, err := DiscoverSkills(root)
	if err != nil {
		t.Fatalf("DiscoverSkills: %v", err)
	}
	if len(skills) != 1 || skills[0].Name != "good" {
		t.Errorf("skills = %+v, want only good", skills)
	}
	if len(skips) != 5 {
		t.Fatalf("skips = %d, want 5 (bare, nodesc, badname, renamed, dirskill): %+v", len(skips), skips)
	}
	reasons := make(map[string]string)
	for _, s := range skips {
		reasons[s.Dir] = s.Reason
	}
	for _, want := range []struct{ dir, sub string }{
		{"skills/bare", "frontmatter"},
		{"skills/nodesc", "description"},
		{"skills/badname", "name"},
		{"skills/renamed", "directory"},
		{"skills/dirskill", "not a regular file"},
	} {
		reason, ok := reasons[want.dir]
		if !ok {
			t.Errorf("no skip report for %s", want.dir)
			continue
		}
		if !strings.Contains(reason, want.sub) {
			t.Errorf("skip for %s = %q, want naming %q", want.dir, reason, want.sub)
		}
	}
}

func TestDiscoverSkillsContainment(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writePlugin(t, root, map[string]string{
		"plugin.json":            manifestJSON(t, baseFields()),
		"skills/inside/SKILL.md": "---\nname: inside\ndescription: d\n---\n\nBody.\n",
	})
	// A skill directory that is a symlink resolving outside the root is
	// skipped, not an error (§4.1). The skill it points at is conforming.
	outsideSkill := filepath.Join(outside, "real-skill")
	writePlugin(t, outsideSkill, map[string]string{"SKILL.md": "---\nname: real-skill\ndescription: outside\n---\n\nBody.\n"})
	if err := os.Symlink(outsideSkill, filepath.Join(root, "skills", "escaping")); err != nil {
		t.Fatal(err)
	}
	// A symlinked SKILL.md resolving inside the root is allowed (§4.1): the
	// link target lives at the root, and its frontmatter name matches the
	// skills/<entry> directory name.
	if err := os.MkdirAll(filepath.Join(root, "skills", "linked"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "linked-content.md"),
		[]byte("---\nname: linked\ndescription: linked via symlink\n---\n\nBody.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "linked-content.md"),
		filepath.Join(root, "skills", "linked", "SKILL.md")); err != nil {
		t.Fatal(err)
	}

	skills, skips, err := DiscoverSkills(root)
	if err != nil {
		t.Fatalf("DiscoverSkills: %v", err)
	}
	if len(skills) != 2 {
		t.Fatalf("skills = %+v, want inside and linked", skills)
	}
	names := map[string]string{}
	for _, s := range skills {
		names[s.Name] = s.Path
	}
	if _, ok := names["inside"]; !ok {
		t.Errorf("inside missing: %+v", skills)
	}
	if names["linked"] != filepath.Join(resolved(t, root), "linked-content.md") {
		t.Errorf("linked resolved path = %q, want the in-root target", names["linked"])
	}
	foundEscape := false
	for _, s := range skips {
		if s.Dir == "skills/escaping" && strings.Contains(s.Reason, "outside") {
			foundEscape = true
		}
	}
	if !foundEscape {
		t.Errorf("skips = %+v, want skills/escaping reported", skips)
	}
}

func TestDiscoverSkillsResolvesRootSymlink(t *testing.T) {
	real := t.TempDir()
	writePlugin(t, real, map[string]string{
		"plugin.json":        manifestJSON(t, baseFields()),
		"skills/ok/SKILL.md": "---\nname: ok\ndescription: d\n---\n\nBody.\n",
	})
	link := filepath.Join(t.TempDir(), "root")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	skills, _, err := DiscoverSkills(link)
	if err != nil {
		t.Fatalf("DiscoverSkills through symlinked root: %v", err)
	}
	if len(skills) != 1 || skills[0].Path != filepath.Join(resolved(t, real), "skills", "ok", "SKILL.md") {
		t.Errorf("skills = %+v", skills)
	}
}
