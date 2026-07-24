package skillinstall

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/matt-riley/waffle/internal/skill"
)

const stageRecordName = "manifest.json"

var stageIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

type Installer struct {
	SkillsRoot  string
	StageRoot   string
	ImportRoots []string
	GitHosts    []string
	Fetcher     GitFetcher
	Now         func() time.Time
	Random      io.Reader

	mu     sync.Mutex
	rename func(string, string) error
}

func New(skillsRoot, stageRoot string, importRoots, gitHosts []string) *Installer {
	return &Installer{
		SkillsRoot:  skillsRoot,
		StageRoot:   stageRoot,
		ImportRoots: append([]string(nil), importRoots...),
		GitHosts:    append([]string(nil), gitHosts...),
		Fetcher:     commandGitFetcher{},
		Now:         func() time.Time { return time.Now().UTC() },
		Random:      rand.Reader,
		rename:      os.Rename,
	}
}

func (i *Installer) Stage(ctx context.Context, request StageRequest) (manifest Manifest, retErr error) {
	if i == nil {
		return Manifest{}, errors.New("skill installer required")
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Manifest{}, err
	}

	source, err := validateStageRequest(request, i.ImportRoots, i.GitHosts)
	if err != nil {
		return Manifest{}, err
	}

	var (
		tree      reviewedTree
		stageID   string
		stagePath string
		allocated bool
		persisted bool
	)
	defer func() {
		if allocated && !persisted {
			retErr = errors.Join(retErr, removeOwnedStage(i.StageRoot, stageID))
		}
	}()

	if source.localPath != "" {
		tree, err = readReviewedTree(source.localPath)
		if err != nil {
			return Manifest{}, err
		}
	} else {
		if err := ensureStageRoot(i.StageRoot); err != nil {
			return Manifest{}, err
		}
		stageID, stagePath, err = i.allocateStage()
		if err != nil {
			return Manifest{}, err
		}
		allocated = true
		inputPath := filepath.Join(stagePath, "input")
		fetcher := i.Fetcher
		if fetcher == nil {
			fetcher = commandGitFetcher{}
		}
		if err := fetcher.Fetch(ctx, source.gitURL, source.commit, inputPath); err != nil {
			return Manifest{}, fmt.Errorf("fetch reviewed skill source: %w", err)
		}
		tree, err = readReviewedTree(inputPath)
		if err != nil {
			return Manifest{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return Manifest{}, err
	}
	if err := ensureSkillAbsent(i.SkillsRoot, tree.name); err != nil {
		return Manifest{}, err
	}

	if !allocated {
		if err := ensureStageRoot(i.StageRoot); err != nil {
			return Manifest{}, err
		}
		stageID, stagePath, err = i.allocateStage()
		if err != nil {
			return Manifest{}, err
		}
		allocated = true
	}
	contentPath := filepath.Join(stagePath, "content")
	if err := writeReviewedTree(contentPath, tree.files); err != nil {
		return Manifest{}, fmt.Errorf("write private skill stage: %w", err)
	}
	if err := os.RemoveAll(filepath.Join(stagePath, "input")); err != nil {
		return Manifest{}, fmt.Errorf("remove private fetch input: %w", err)
	}

	entries := tree.entries()
	manifest = Manifest{
		Name:          tree.name,
		Description:   tree.description,
		SourceRef:     source.sourceRef,
		ContentDigest: contentDigest(entries),
		Files:         entries,
		Audit:         tree.audit,
		StageID:       stageID,
		ExpiresAt:     i.clock().Add(stageLifetime),
	}
	if err := writeStageRecord(stagePath, persistedStage{Version: 1, Manifest: manifest}); err != nil {
		return Manifest{}, err
	}
	persisted = true
	return manifest, nil
}

func (i *Installer) Install(ctx context.Context, stageID, digest string) (installed skill.Skill, retErr error) {
	if i == nil {
		return skill.Skill{}, errors.New("skill installer required")
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if !stageIDPattern.MatchString(stageID) {
		return skill.Skill{}, ErrStageNotFound
	}
	if err := validateExistingRoot(i.StageRoot); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return skill.Skill{}, ErrStageNotFound
		}
		return skill.Skill{}, err
	}
	stagePath := filepath.Join(i.StageRoot, stageID)
	defer func() {
		retErr = errors.Join(retErr, removeOwnedStage(i.StageRoot, stageID))
	}()
	stageInfo, err := os.Lstat(stagePath)
	if errors.Is(err, os.ErrNotExist) {
		return skill.Skill{}, ErrStageNotFound
	}
	if err != nil || !stageInfo.IsDir() || stageInfo.Mode()&os.ModeSymlink != 0 {
		return skill.Skill{}, ErrStageNotFound
	}
	if err := ctx.Err(); err != nil {
		return skill.Skill{}, err
	}

	record, err := readStageRecord(filepath.Join(stagePath, stageRecordName))
	if err != nil {
		return skill.Skill{}, err
	}
	if record.Version != 1 || record.Manifest.StageID != stageID {
		return skill.Skill{}, ErrStageChanged
	}
	if !i.clock().Before(record.Manifest.ExpiresAt) {
		return skill.Skill{}, ErrStageExpired
	}
	if digest == "" || digest != record.Manifest.ContentDigest {
		return skill.Skill{}, ErrDigestMismatch
	}
	tree, err := readReviewedTree(filepath.Join(stagePath, "content"))
	if err != nil {
		return skill.Skill{}, fmt.Errorf("%w: %v", ErrStageChanged, err)
	}
	if !treeMatchesManifest(tree, record.Manifest) {
		return skill.Skill{}, ErrStageChanged
	}
	if err := ensureSkillsRoot(i.SkillsRoot); err != nil {
		return skill.Skill{}, err
	}
	if err := ensureSkillAbsent(i.SkillsRoot, tree.name); err != nil {
		return skill.Skill{}, err
	}

	finalFiles, err := filesWithInactiveStatus(tree.files)
	if err != nil {
		return skill.Skill{}, err
	}
	temporaryPath := filepath.Join(i.SkillsRoot, ".waffle-install-"+stageID)
	if _, err := os.Lstat(temporaryPath); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return skill.Skill{}, errors.New("private install staging path already exists")
		}
		return skill.Skill{}, fmt.Errorf("inspect private install staging path: %w", err)
	}
	temporaryOwned := false
	committed := false
	defer func() {
		if temporaryOwned && !committed {
			retErr = errors.Join(retErr, removeOwnedPath(i.SkillsRoot, temporaryPath))
		}
	}()
	if err := writeReviewedTree(temporaryPath, finalFiles); err != nil {
		return skill.Skill{}, fmt.Errorf("write atomic skill install stage: %w", err)
	}
	temporaryOwned = true
	finalTree, err := readReviewedTreeBound(temporaryPath, maxReviewBytes+maxInactiveGrowth)
	if err != nil || !reviewedFilesEqual(finalTree.files, finalFiles) {
		return skill.Skill{}, fmt.Errorf("verify atomic skill install stage: %w", ErrStageChanged)
	}
	if err := ensureSkillAbsent(i.SkillsRoot, tree.name); err != nil {
		return skill.Skill{}, err
	}

	targetPath := filepath.Join(i.SkillsRoot, tree.name)
	rename := i.rename
	if rename == nil {
		rename = os.Rename
	}
	if err := rename(temporaryPath, targetPath); err != nil {
		return skill.Skill{}, fmt.Errorf("atomically install reviewed skill: %w", err)
	}
	committed = true
	if err := syncDirectory(i.SkillsRoot); err != nil {
		return skill.Skill{}, fmt.Errorf("sync installed skill directory: %w", err)
	}

	discovered, err := skill.Discover(i.SkillsRoot)
	if err != nil {
		return skill.Skill{}, fmt.Errorf("discover installed skill: %w", err)
	}
	installed, found := skill.Find(discovered, tree.name)
	if !found {
		return skill.Skill{}, errors.New("installed skill was not discoverable")
	}
	return installed, nil
}

func (i *Installer) clock() time.Time {
	if i.Now != nil {
		return i.Now().UTC()
	}
	return time.Now().UTC()
}

func (i *Installer) allocateStage() (string, string, error) {
	random := i.Random
	if random == nil {
		random = rand.Reader
	}
	for range 8 {
		raw := make([]byte, stageIDBytes)
		if _, err := io.ReadFull(random, raw); err != nil {
			return "", "", fmt.Errorf("generate skill stage ID: %w", err)
		}
		stageID := hex.EncodeToString(raw)
		stagePath := filepath.Join(i.StageRoot, stageID)
		if err := os.Mkdir(stagePath, 0o700); err == nil {
			return stageID, stagePath, nil
		} else if !errors.Is(err, os.ErrExist) {
			return "", "", fmt.Errorf("create private skill stage: %w", err)
		}
	}
	return "", "", errors.New("allocate unique skill stage ID")
}

func ensureStageRoot(root string) error {
	if !cleanAbsolutePath(root) {
		return errors.New("skill stage root must be an absolute clean path")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("create private skill stage root: %w", err)
	}
	info, err := os.Lstat(root)
	if err != nil {
		return fmt.Errorf("inspect private skill stage root: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("skill stage root must be a real directory")
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return fmt.Errorf("secure private skill stage root: %w", err)
	}
	return nil
}

func validateExistingRoot(root string) error {
	if !cleanAbsolutePath(root) {
		return errors.New("skill stage root must be an absolute clean path")
	}
	info, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return errors.New("skill stage root is not a private real directory")
	}
	return nil
}

func ensureSkillsRoot(root string) error {
	if !cleanAbsolutePath(root) {
		return errors.New("skills root must be an absolute clean path")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("create skills root: %w", err)
	}
	info, err := os.Lstat(root)
	if err != nil {
		return fmt.Errorf("inspect skills root: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("skills root must be a real directory")
	}
	return nil
}

func ensureSkillAbsent(root, name string) error {
	if !skillNamePattern.MatchString(name) {
		return fmt.Errorf("%w: invalid target name", ErrAuditFailed)
	}
	if !cleanAbsolutePath(root) {
		return errors.New("skills root must be an absolute clean path")
	}
	rootInfo, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect skills root: %w", err)
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("skills root must be a real directory")
	}
	_, err = os.Lstat(filepath.Join(root, name))
	if err == nil {
		return fmt.Errorf("%w: %s", ErrSkillExists, name)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect installed skill target: %w", err)
	}
	return nil
}

func writeReviewedTree(destination string, files []reviewedFile) (retErr error) {
	if err := os.Mkdir(destination, 0o700); err != nil {
		return err
	}
	complete := false
	defer func() {
		if !complete {
			retErr = errors.Join(retErr, os.RemoveAll(destination), syncDirectory(filepath.Dir(destination)))
		}
	}()
	directories := map[string]struct{}{destination: {}}
	for _, file := range files {
		relative := filepath.FromSlash(file.entry.Path)
		if !safeRelativePath(relative) {
			return ErrUnsafeTree
		}
		path := filepath.Join(destination, relative)
		parent := filepath.Dir(path)
		if err := os.MkdirAll(parent, 0o700); err != nil {
			return err
		}
		for current := parent; strings.HasPrefix(current, destination); current = filepath.Dir(current) {
			directories[current] = struct{}{}
			if current == destination {
				break
			}
		}
		if err := os.Chmod(parent, 0o700); err != nil {
			return err
		}
		output, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		if err := writeSyncClose(output, file.data); err != nil {
			return err
		}
	}
	ordered := make([]string, 0, len(directories))
	for directory := range directories {
		ordered = append(ordered, directory)
	}
	sort.Slice(ordered, func(left, right int) bool {
		return len(ordered[left]) > len(ordered[right])
	})
	for _, directory := range ordered {
		if err := syncDirectory(directory); err != nil {
			return err
		}
	}
	complete = true
	return nil
}

func writeSyncClose(file *os.File, data []byte) error {
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	return directory.Sync()
}

func writeStageRecord(stagePath string, record persistedStage) error {
	path := filepath.Join(stagePath, stageRecordName)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create private skill stage record: %w", err)
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		_ = file.Close()
		return fmt.Errorf("encode private skill stage record: %w", err)
	}
	encoded = append(encoded, '\n')
	if err := writeSyncClose(file, encoded); err != nil {
		return fmt.Errorf("write private skill stage record: %w", err)
	}
	if err := syncDirectory(stagePath); err != nil {
		return fmt.Errorf("sync private skill stage: %w", err)
	}
	return nil
}

func readStageRecord(path string) (persistedStage, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 ||
		info.Size() < 0 || info.Size() > maxStageRecord {
		return persistedStage{}, ErrStageChanged
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return persistedStage{}, ErrStageChanged
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var record persistedStage
	if err := decoder.Decode(&record); err != nil {
		return persistedStage{}, ErrStageChanged
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return persistedStage{}, ErrStageChanged
	}
	return record, nil
}

func treeMatchesManifest(tree reviewedTree, manifest Manifest) bool {
	return tree.name == manifest.Name &&
		tree.description == manifest.Description &&
		tree.audit.Passed == manifest.Audit.Passed &&
		slices.Equal(tree.audit.Flags, manifest.Audit.Flags) &&
		slices.Equal(tree.entries(), manifest.Files) &&
		contentDigest(tree.entries()) == manifest.ContentDigest
}

func filesWithInactiveStatus(files []reviewedFile) ([]reviewedFile, error) {
	out := make([]reviewedFile, len(files))
	for index := range files {
		out[index] = reviewedFile{
			entry: files[index].entry,
			data:  append([]byte(nil), files[index].data...),
		}
		if files[index].entry.Path != "SKILL.md" {
			continue
		}
		updated, err := setInactiveStatus(string(files[index].data))
		if err != nil {
			return nil, err
		}
		out[index].data = []byte(updated)
		out[index].entry.Size = int64(len(updated))
		sum := sha256Sum(out[index].data)
		out[index].entry.SHA256 = sum
		out[index].entry.Preview = updated
	}
	return out, nil
}

func setInactiveStatus(raw string) (string, error) {
	if _, _, err := parseSkillFrontmatter(raw); err != nil {
		return "", err
	}
	rest := strings.TrimPrefix(raw, "---\n")
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "", ErrAuditFailed
	}
	lines := strings.Split(rest[:end], "\n")
	found := false
	for index, line := range lines {
		key, _, ok := strings.Cut(line, ":")
		if ok && strings.TrimSpace(key) == "status" {
			if found {
				return "", fmt.Errorf("%w: duplicate status", ErrAuditFailed)
			}
			lines[index] = "status: inactive"
			found = true
		}
	}
	if !found {
		lines = append(lines, "status: inactive")
	}
	return "---\n" + strings.Join(lines, "\n") + rest[end:], nil
}

func sha256Sum(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func reviewedFilesEqual(left, right []reviewedFile) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].entry != right[index].entry || !bytes.Equal(left[index].data, right[index].data) {
			return false
		}
	}
	return true
}

func removeOwnedStage(root, stageID string) error {
	if !stageIDPattern.MatchString(stageID) || !cleanAbsolutePath(root) {
		return nil
	}
	return removeOwnedPath(root, filepath.Join(root, stageID))
}

func removeOwnedPath(root, ownedPath string) error {
	relative, err := filepath.Rel(root, ownedPath)
	if err != nil || !safeRelativePath(relative) {
		return errors.New("refuse unsafe installer cleanup path")
	}
	if err := os.RemoveAll(ownedPath); err != nil {
		return fmt.Errorf("clean private installer path: %w", err)
	}
	if _, err := os.Lstat(root); err == nil {
		return syncDirectory(root)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
