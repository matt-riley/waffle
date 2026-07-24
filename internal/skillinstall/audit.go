package skillinstall

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/matt-riley/waffle/internal/skill"
)

var (
	frontmatterKeyPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)
	skillNamePattern      = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
)

func readReviewedTree(root string) (reviewedTree, error) {
	return readReviewedTreeBound(root, maxReviewBytes)
}

func readReviewedTreeBound(root string, byteLimit int64) (reviewedTree, error) {
	info, err := os.Lstat(root)
	if err != nil {
		return reviewedTree{}, fmt.Errorf("%w: inspect source root: %v", ErrUnsafeTree, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return reviewedTree{}, fmt.Errorf("%w: source root is not a real directory", ErrUnsafeTree)
	}
	confined, err := os.OpenRoot(root)
	if err != nil {
		return reviewedTree{}, fmt.Errorf("%w: open source root: %v", ErrUnsafeTree, err)
	}
	defer func() { _ = confined.Close() }()

	files := make([]reviewedFile, 0)
	entriesSeen := 0
	var totalBytes int64
	skillFiles := 0
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("%w: walk source: %v", ErrUnsafeTree, walkErr)
		}
		if path == root {
			return nil
		}
		entriesSeen++
		if entriesSeen > maxReviewEntries {
			return fmt.Errorf("%w: more than %d filesystem entries", ErrTreeTooLarge, maxReviewEntries)
		}

		relative, err := filepath.Rel(root, path)
		if err != nil || !safeRelativePath(relative) {
			return fmt.Errorf("%w: invalid relative path", ErrUnsafeTree)
		}
		slashPath := filepath.ToSlash(relative)
		if hasVCSComponent(slashPath) {
			return fmt.Errorf("%w: hidden VCS state %q", ErrUnsafeTree, slashPath)
		}

		current, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("%w: inspect %q: %v", ErrUnsafeTree, slashPath, err)
		}
		if current.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: symlink %q", ErrUnsafeTree, slashPath)
		}
		if current.IsDir() {
			return nil
		}
		if !current.Mode().IsRegular() {
			return fmt.Errorf("%w: special file %q", ErrUnsafeTree, slashPath)
		}
		if current.Mode().Perm()&0o111 != 0 {
			return fmt.Errorf("%w: executable file %q", ErrUnsafeTree, slashPath)
		}
		if len(files)+1 > maxReviewFiles {
			return fmt.Errorf("%w: more than %d files", ErrTreeTooLarge, maxReviewFiles)
		}
		if current.Size() < 0 || current.Size() > byteLimit-totalBytes {
			return fmt.Errorf("%w: more than %d bytes", ErrTreeTooLarge, byteLimit)
		}

		data, err := confined.ReadFile(relative)
		if err != nil {
			return fmt.Errorf("%w: read %q: %v", ErrUnsafeTree, slashPath, err)
		}
		after, err := confined.Lstat(relative)
		if err != nil || !after.Mode().IsRegular() || after.Mode()&os.ModeSymlink != 0 || after.Size() != int64(len(data)) {
			return fmt.Errorf("%w: %q changed while reading", ErrUnsafeTree, slashPath)
		}
		if int64(len(data)) > byteLimit-totalBytes {
			return fmt.Errorf("%w: more than %d bytes", ErrTreeTooLarge, byteLimit)
		}
		totalBytes += int64(len(data))
		if !utf8.Valid(data) || strings.IndexByte(string(data), 0) >= 0 {
			return fmt.Errorf("%w: binary or NUL content in %q", ErrUnsafeTree, slashPath)
		}

		sum := sha256.Sum256(data)
		files = append(files, reviewedFile{
			entry: FileEntry{
				Path:    slashPath,
				Size:    int64(len(data)),
				SHA256:  hex.EncodeToString(sum[:]),
				Preview: string(data),
			},
			data: append([]byte(nil), data...),
		})
		if filepath.Base(relative) == "SKILL.md" {
			skillFiles++
		}
		return nil
	})
	if err != nil {
		return reviewedTree{}, err
	}
	sort.Slice(files, func(left, right int) bool {
		return files[left].entry.Path < files[right].entry.Path
	})
	if skillFiles != 1 {
		return reviewedTree{}, fmt.Errorf("%w: require exactly one SKILL.md, found %d", ErrAuditFailed, skillFiles)
	}
	skillIndex := -1
	for index := range files {
		if files[index].entry.Path == "SKILL.md" {
			skillIndex = index
			break
		}
	}
	if skillIndex < 0 {
		return reviewedTree{}, fmt.Errorf("%w: SKILL.md must be at source root", ErrAuditFailed)
	}
	name, description, err := parseSkillFrontmatter(string(files[skillIndex].data))
	if err != nil {
		return reviewedTree{}, err
	}
	flags := auditFlags(files)
	return reviewedTree{
		name:        name,
		description: description,
		files:       files,
		audit: AuditView{
			Passed: true,
			Flags:  flags,
		},
	}, nil
}

func safeRelativePath(relative string) bool {
	if relative == "" || relative == "." || filepath.IsAbs(relative) || filepath.Clean(relative) != relative {
		return false
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func hasVCSComponent(path string) bool {
	for _, component := range strings.Split(path, "/") {
		switch component {
		case ".git", ".hg", ".svn":
			return true
		}
	}
	return false
}

func parseSkillFrontmatter(raw string) (string, string, error) {
	if !strings.HasPrefix(raw, "---\n") {
		return "", "", fmt.Errorf("%w: SKILL.md requires leading frontmatter", ErrAuditFailed)
	}
	rest := strings.TrimPrefix(raw, "---\n")
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "", "", fmt.Errorf("%w: SKILL.md frontmatter is not closed", ErrAuditFailed)
	}
	after := rest[end+len("\n---"):]
	if after != "" && !strings.HasPrefix(after, "\n") {
		return "", "", fmt.Errorf("%w: invalid SKILL.md frontmatter delimiter", ErrAuditFailed)
	}

	values := make(map[string]string)
	for _, line := range strings.Split(rest[:end], "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if trimmed != line {
			return "", "", fmt.Errorf("%w: multiline or nested frontmatter is not allowed", ErrAuditFailed)
		}
		key, value, found := strings.Cut(line, ":")
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if !found || !frontmatterKeyPattern.MatchString(key) {
			return "", "", fmt.Errorf("%w: invalid frontmatter line %q", ErrAuditFailed, line)
		}
		if _, duplicate := values[key]; duplicate {
			return "", "", fmt.Errorf("%w: duplicate frontmatter key %q", ErrAuditFailed, key)
		}
		parsed, err := unquoteFrontmatterValue(value)
		if err != nil {
			return "", "", fmt.Errorf("%w: %v", ErrAuditFailed, err)
		}
		values[key] = parsed
	}
	name := values["name"]
	if !skillNamePattern.MatchString(name) {
		return "", "", fmt.Errorf("%w: skill name %q is not a slug", ErrAuditFailed, name)
	}
	description := strings.TrimSpace(values["description"])
	if description == "" || description == "|" || description == ">" ||
		strings.ContainsAny(description, "\r\n") {
		return "", "", fmt.Errorf("%w: description must be one non-empty line", ErrAuditFailed)
	}
	if status, present := values["status"]; present && status != skill.StatusActive && status != skill.StatusInactive {
		return "", "", fmt.Errorf("%w: invalid skill status %q", ErrAuditFailed, status)
	}
	return name, description, nil
}

func unquoteFrontmatterValue(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	first := value[0]
	if first != '"' && first != '\'' {
		if strings.HasSuffix(value, `"`) || strings.HasSuffix(value, `'`) {
			return "", errors.New("unmatched frontmatter quote")
		}
		return value, nil
	}
	if len(value) < 2 || value[len(value)-1] != first {
		return "", errors.New("unmatched frontmatter quote")
	}
	return value[1 : len(value)-1], nil
}

func auditFlags(files []reviewedFile) []string {
	flags := make(map[string]struct{})
	for _, file := range files {
		lower := strings.ToLower(file.entry.Path)
		extension := strings.ToLower(filepath.Ext(lower))
		switch extension {
		case ".sh", ".bash", ".zsh", ".fish":
			flags["shell:"+file.entry.Path] = struct{}{}
		case ".c", ".cc", ".cpp", ".css", ".go", ".html", ".java", ".js", ".jsx",
			".lua", ".php", ".pl", ".py", ".rb", ".rs", ".swift", ".ts", ".tsx":
			flags["code:"+file.entry.Path] = struct{}{}
		}
		if strings.Contains(string(file.data), "http://") || strings.Contains(string(file.data), "https://") {
			flags["network-reference:"+file.entry.Path] = struct{}{}
		}
	}
	out := make([]string, 0, len(flags))
	for flag := range flags {
		out = append(out, flag)
	}
	sort.Strings(out)
	return out
}
