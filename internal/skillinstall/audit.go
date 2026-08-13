package skillinstall

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/matt-riley/waffle/internal/skill"
	"github.com/matt-riley/waffle/internal/skill/spec"
)

var frontmatterKeyPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)

func readReviewedTree(root string) (reviewedTree, error) {
	return readReviewedTreeBound(root, maxReviewBytes)
}

func readReviewedTreeBound(root string, byteLimit int64) (reviewedTree, error) {
	confined, err := openVerifiedPathRoot(root, nil)
	if err != nil {
		return reviewedTree{}, err
	}
	defer func() { _ = confined.Close() }()
	return readReviewedRoot(confined, byteLimit, nil)
}

type reviewWalk struct {
	byteLimit   int64
	totalBytes  int64
	entriesSeen int
	files       []reviewedFile
	beforeEntry func(string)
}

func readReviewedRoot(root *os.Root, byteLimit int64, beforeEntry func(string)) (reviewedTree, error) {
	if root == nil {
		return reviewedTree{}, fmt.Errorf("%w: source root required", ErrUnsafeTree)
	}
	walk := &reviewWalk{
		byteLimit:   byteLimit,
		files:       make([]reviewedFile, 0),
		beforeEntry: beforeEntry,
	}
	if err := walk.directory(root, ""); err != nil {
		return reviewedTree{}, err
	}
	return reviewedTreeFromFiles(walk.files)
}

func (w *reviewWalk) directory(root *os.Root, prefix string) error {
	directory, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("%w: open source directory", ErrUnsafeTree)
	}
	defer func() { _ = directory.Close() }()
	for {
		entries, readErr := directory.ReadDir(32)
		for _, entry := range entries {
			name := entry.Name()
			slashPath := name
			if prefix != "" {
				slashPath = prefix + "/" + name
			}
			if !safeArchivePath(slashPath) || len(slashPath) > maxReviewPathBytes || !utf8.ValidString(slashPath) {
				return fmt.Errorf("%w: invalid relative path", ErrUnsafeTree)
			}
			w.entriesSeen++
			if w.entriesSeen > maxReviewEntries {
				return fmt.Errorf("%w: more than %d filesystem entries", ErrTreeTooLarge, maxReviewEntries)
			}
			if hasVCSComponent(slashPath) {
				return fmt.Errorf("%w: hidden VCS state %q", ErrUnsafeTree, slashPath)
			}
			if w.beforeEntry != nil {
				w.beforeEntry(slashPath)
			}
			before, err := root.Lstat(name)
			if err != nil || before.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("%w: inspect %q", ErrUnsafeTree, slashPath)
			}
			if before.IsDir() {
				child, err := openVerifiedChildRoot(root, name, before)
				if err != nil {
					return fmt.Errorf("%w: directory %q changed while reading", ErrUnsafeTree, slashPath)
				}
				childErr := w.directory(child, slashPath)
				closeErr := child.Close()
				if childErr != nil {
					return childErr
				}
				if closeErr != nil {
					return fmt.Errorf("%w: close source directory %q", ErrUnsafeTree, slashPath)
				}
				continue
			}
			if err := w.file(root, name, slashPath, before); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return fmt.Errorf("%w: enumerate source directory", ErrUnsafeTree)
		}
	}
}

func (w *reviewWalk) file(root *os.Root, name, slashPath string, before os.FileInfo) error {
	if !before.Mode().IsRegular() {
		return fmt.Errorf("%w: special file %q", ErrUnsafeTree, slashPath)
	}
	if before.Mode().Perm()&0o111 != 0 {
		return fmt.Errorf("%w: executable file %q", ErrUnsafeTree, slashPath)
	}
	if len(w.files)+1 > maxReviewFiles {
		return fmt.Errorf("%w: more than %d files", ErrTreeTooLarge, maxReviewFiles)
	}
	remaining := w.byteLimit - w.totalBytes
	if before.Size() < 0 || before.Size() > remaining {
		return fmt.Errorf("%w: more than %d bytes", ErrTreeTooLarge, w.byteLimit)
	}
	file, err := root.Open(name)
	if err != nil {
		return fmt.Errorf("%w: open %q", ErrUnsafeTree, slashPath)
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || opened.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(before, opened) {
		return fmt.Errorf("%w: %q changed before reading", ErrUnsafeTree, slashPath)
	}
	afterOpen, err := root.Lstat(name)
	if err != nil || afterOpen.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, afterOpen) {
		return fmt.Errorf("%w: %q changed before reading", ErrUnsafeTree, slashPath)
	}
	if opened.Mode().Perm()&0o111 != 0 {
		return fmt.Errorf("%w: executable file %q", ErrUnsafeTree, slashPath)
	}
	if opened.Size() < 0 || opened.Size() > remaining {
		return fmt.Errorf("%w: more than %d bytes", ErrTreeTooLarge, w.byteLimit)
	}
	data, err := io.ReadAll(io.LimitReader(file, remaining+1))
	if err != nil {
		return fmt.Errorf("%w: read %q", ErrUnsafeTree, slashPath)
	}
	if int64(len(data)) > remaining {
		return fmt.Errorf("%w: more than %d bytes", ErrTreeTooLarge, w.byteLimit)
	}
	final, err := file.Stat()
	if err != nil || !os.SameFile(opened, final) || final.Size() != int64(len(data)) {
		return fmt.Errorf("%w: %q changed while reading", ErrUnsafeTree, slashPath)
	}
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return fmt.Errorf("%w: binary or NUL content in %q", ErrUnsafeTree, slashPath)
	}
	w.totalBytes += int64(len(data))
	w.files = append(w.files, newReviewedFile(slashPath, data))
	return nil
}

func openLocalSource(source localSourceSpec) (*os.Root, error) {
	importRoot, err := openVerifiedPathRoot(source.rootPath, source.rootInfo)
	if err != nil {
		return nil, err
	}
	if source.relative == "." {
		opened, err := importRoot.Stat(".")
		if err != nil || !os.SameFile(opened, source.sourceInfo) {
			_ = importRoot.Close()
			return nil, fmt.Errorf("%w: local source changed", ErrUnsafeTree)
		}
		return importRoot, nil
	}
	sourceRoot, err := openRelativeRoot(importRoot, source.relative)
	_ = importRoot.Close()
	if err != nil {
		return nil, err
	}
	opened, err := sourceRoot.Stat(".")
	if err != nil || !os.SameFile(opened, source.sourceInfo) {
		_ = sourceRoot.Close()
		return nil, fmt.Errorf("%w: local source changed", ErrUnsafeTree)
	}
	return sourceRoot, nil
}

func openVerifiedPathRoot(path string, expected os.FileInfo) (*os.Root, error) {
	before, err := os.Lstat(path)
	if err != nil || !before.IsDir() || before.Mode()&os.ModeSymlink != 0 ||
		(expected != nil && !os.SameFile(before, expected)) {
		return nil, fmt.Errorf("%w: source root is not a stable real directory", ErrUnsafeTree)
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, fmt.Errorf("%w: open source root", ErrUnsafeTree)
	}
	opened, openErr := root.Stat(".")
	after, afterErr := os.Lstat(path)
	if openErr != nil || afterErr != nil || after.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(before, opened) || !os.SameFile(opened, after) {
		_ = root.Close()
		return nil, fmt.Errorf("%w: source root changed while opening", ErrUnsafeTree)
	}
	return root, nil
}

func openRelativeRoot(root *os.Root, relative string) (*os.Root, error) {
	current := root
	for _, component := range strings.Split(filepath.ToSlash(relative), "/") {
		before, err := current.Lstat(component)
		if err != nil || !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
			if current != root {
				_ = current.Close()
			}
			return nil, fmt.Errorf("%w: local source contains an unsafe path component", ErrUnsafeTree)
		}
		next, err := openVerifiedChildRoot(current, component, before)
		if err != nil {
			if current != root {
				_ = current.Close()
			}
			return nil, err
		}
		if current != root {
			_ = current.Close()
		}
		current = next
	}
	return current, nil
}

func openVerifiedChildRoot(parent *os.Root, name string, before os.FileInfo) (*os.Root, error) {
	child, err := parent.OpenRoot(name)
	if err != nil {
		return nil, fmt.Errorf("%w: open source directory", ErrUnsafeTree)
	}
	opened, openErr := child.Stat(".")
	after, afterErr := parent.Lstat(name)
	if openErr != nil || afterErr != nil || after.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(before, opened) || !os.SameFile(opened, after) {
		_ = child.Close()
		return nil, fmt.Errorf("%w: source directory changed while opening", ErrUnsafeTree)
	}
	return child, nil
}

func verifyLocalSourcePath(source localSourceSpec, opened *os.Root) error {
	current, err := os.Lstat(source.sourcePath)
	openedInfo, openedErr := opened.Stat(".")
	if err != nil || openedErr != nil || current.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(current, source.sourceInfo) || !os.SameFile(openedInfo, source.sourceInfo) {
		return fmt.Errorf("%w: local source changed after opening", ErrUnsafeTree)
	}
	return nil
}

func newReviewedFile(path string, data []byte) reviewedFile {
	sum := sha256.Sum256(data)
	return reviewedFile{
		entry: FileEntry{
			Path:    path,
			Size:    int64(len(data)),
			SHA256:  hex.EncodeToString(sum[:]),
			Preview: string(data),
		},
		data: append([]byte(nil), data...),
	}
}

func reviewedTreeFromFiles(files []reviewedFile) (reviewedTree, error) {
	sort.Slice(files, func(left, right int) bool {
		return files[left].entry.Path < files[right].entry.Path
	})
	skillFiles := 0
	for index := range files {
		if files[index].entry.Path == "SKILL.md" || filepath.Base(filepath.FromSlash(files[index].entry.Path)) == "SKILL.md" {
			skillFiles++
		}
	}
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
		switch strings.ToLower(component) {
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
	if !spec.ValidName(name) {
		return "", "", fmt.Errorf("%w: skill name %q is not an Agent Skills name", ErrAuditFailed, name)
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
		if strings.ContainsAny(value[:1], "-?:,[]{}#&*!|>'\"%@`") ||
			strings.Contains(value, ": ") || strings.Contains(value, " #") ||
			strings.HasSuffix(value, `"`) || strings.HasSuffix(value, `'`) {
			return "", errors.New("invalid plain frontmatter scalar")
		}
		return value, nil
	}
	if len(value) < 2 || value[len(value)-1] != first {
		return "", errors.New("unmatched frontmatter quote")
	}
	if first == '"' {
		parsed, err := strconv.Unquote(value)
		if err != nil {
			return "", errors.New("invalid double-quoted frontmatter scalar")
		}
		return parsed, nil
	}
	var parsed strings.Builder
	for index := 1; index < len(value)-1; index++ {
		if value[index] != '\'' {
			parsed.WriteByte(value[index])
			continue
		}
		if index+1 >= len(value)-1 || value[index+1] != '\'' {
			return "", errors.New("invalid single-quoted frontmatter scalar")
		}
		parsed.WriteByte('\'')
		index++
	}
	return parsed.String(), nil
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
