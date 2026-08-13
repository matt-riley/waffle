package plugin

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/matt-riley/waffle/internal/skill/spec"
)

// maxSkillBytes bounds one discovered SKILL.md, mirroring the skill
// installer's content bound (internal/skillinstall maxReviewBytes).
const maxSkillBytes = 1 << 20

// Skill is one skill discovered under a plugin's skills/ directory (spec
// §6.1, §7.1).
type Skill struct {
	Name        string
	Description string
	Path        string // filesystem-resolved SKILL.md path, contained in the root
}

// SkillSkip reports a discovered-but-invalid skill entry. Per §7.1 and §4.1
// such entries are skipped — never fatal — and SHOULD be reported.
type SkillSkip struct {
	Dir    string // the skills/<name> directory (as supplied by the package)
	Reason string
}

// DiscoverSkills discovers skills from <root>/skills per §6.1, §7.1, and
// §4.1: one skill per immediate child directory holding a regular-file
// SKILL.md, frontmatter validated against the Agent Skills spec, each
// SKILL.md path resolved within the filesystem-resolved plugin root.
//
// Missing skills/ yields no skills and no error (§6.2). A skills entry that
// is not a directory, a SKILL.md that is not a regular file, a SKILL.md
// that resolves outside the root (§4.1), or a non-conforming skill (§7.1)
// invalidates only that entry or component type: it is skipped and reported
// in the returned skips, and every other skill still loads. Non-conforming
// means per the spec — frontmatter required, name matching the directory
// name, non-empty description — not waffle policy (a missing status field
// is fine).
func DiscoverSkills(root string) (skills []Skill, skips []SkillSkip, err error) {
	resolvedRoot, err := resolvedDir(root)
	if err != nil {
		return nil, nil, err
	}
	skillsDir, err := ResolveWithin(resolvedRoot, "./skills")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, nil
		}
		// §6.2: the fixed location exists but does not resolve to a real
		// directory inside the root; the component type is invalid.
		return nil, []SkillSkip{{Dir: "skills", Reason: "skills is not a directory within the plugin root: " + err.Error()}}, nil
	}
	info, err := os.Lstat(skillsDir)
	if err != nil {
		return nil, nil, fmt.Errorf("inspect plugin skills dir: %w", err)
	}
	if !info.IsDir() {
		return nil, []SkillSkip{{Dir: "skills", Reason: "skills is not a directory"}}, nil
	}
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		return nil, nil, fmt.Errorf("read plugin skills dir: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		// Resolve the entry itself first so a symlinked skill directory is
		// handled by §4.1 (in-root symlinks are fine, escapes are reported)
		// instead of being silently dropped by DirEntry.IsDir, which reports
		// the entry type, not the target type.
		entryPath, err := ResolveWithin(resolvedRoot, "./skills/"+name)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue // vanished between listing and resolution
			}
			skips = append(skips, SkillSkip{Dir: filepath.Join("skills", name), Reason: err.Error()})
			continue
		}
		info, err := os.Lstat(entryPath)
		if err != nil || !info.IsDir() {
			continue // a plain file in skills/ is not a skill (§7.1)
		}
		skill, skip := discoverOneSkill(resolvedRoot, skillsDir, name)
		if skip != nil {
			skips = append(skips, *skip)
			continue
		}
		skills = append(skills, *skill)
	}
	sort.Slice(skills, func(i, j int) bool { return skills[i].Name < skills[j].Name })
	return skills, skips, nil
}

// discoverOneSkill resolves and validates one skills/<name> entry. A nil
// return with a non-nil skip means the entry is skipped per §7.1/§4.1.
func discoverOneSkill(resolvedRoot, skillsDir, name string) (*Skill, *SkillSkip) {
	skip := func(reason string) (*Skill, *SkillSkip) {
		return nil, &SkillSkip{Dir: filepath.Join("skills", name), Reason: reason}
	}
	// §4.1: the SKILL.md path must resolve within the plugin root. This
	// also covers a symlinked skill directory or SKILL.md: in-root symlinks
	// resolve fine; anything escaping is skipped.
	path, err := ResolveWithin(resolvedRoot, "./skills/"+name+"/SKILL.md")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return skip("no SKILL.md")
		}
		return skip(err.Error())
	}
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return skip("no SKILL.md")
		}
		return skip("cannot inspect SKILL.md: " + err.Error())
	}
	if !info.Mode().IsRegular() {
		return skip("SKILL.md is not a regular file")
	}
	raw, err := readBoundedFile(path, maxSkillBytes)
	if err != nil {
		return skip("cannot read SKILL.md: " + err.Error())
	}
	// §7.1: conform to the Agent Skills spec or be skipped.
	fields, body, err := spec.ParseFrontmatter(string(raw))
	if err != nil {
		return skip(err.Error())
	}
	desc := strings.TrimSpace(fields["description"])
	if err := spec.Validate(fields["name"], desc, fields, body, name); err != nil {
		return skip(err.Error())
	}
	return &Skill{Name: fields["name"], Description: desc, Path: path}, nil
}

// readBoundedFile reads path with the same bounded discipline as
// readManifest: a regular, non-symlink file no larger than maxSize, read
// from the opened handle through a limit so post-stat growth cannot force
// an unbounded allocation.
func readBoundedFile(path string, maxSize int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !openedInfo.Mode().IsRegular() || openedInfo.Size() > maxSize {
		return nil, errors.New("not a regular file within size bounds")
	}
	body, err := io.ReadAll(io.LimitReader(file, maxSize+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxSize {
		return nil, errors.New("exceeds size bound")
	}
	return body, nil
}
