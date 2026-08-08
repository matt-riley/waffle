package skillinstall

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/matt-riley/waffle/internal/policy"
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
	// AuditDB, when set, records Stage/InstallReviewed mutations in policy_audit
	// so skillinstall shares the host policy audit trail with agent tools.
	AuditDB *sql.DB
	// Log reports failed policy_audit writes. Nil falls back to slog.Default():
	// an unaudited skill installation must never be silent (#297).
	Log *slog.Logger

	mu            sync.Mutex
	rename        func(string, string) error
	syncDirectory func(string) error

	afterLocalSourceOpen func()
	beforeLocalEntry     func(string)
}

func New(skillsRoot, stageRoot string, importRoots, gitHosts []string) *Installer {
	return &Installer{
		SkillsRoot:    skillsRoot,
		StageRoot:     stageRoot,
		ImportRoots:   append([]string(nil), importRoots...),
		GitHosts:      append([]string(nil), gitHosts...),
		Fetcher:       commandGitFetcher{},
		Now:           func() time.Time { return time.Now().UTC() },
		Random:        rand.Reader,
		rename:        atomicRenameNoReplace,
		syncDirectory: syncDirectory,
	}
}

type InstallResult struct {
	Skill     skill.Skill
	Committed bool
	Warnings  []error
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

	if source.local != nil {
		localRoot, openErr := openLocalSource(*source.local)
		if openErr != nil {
			return Manifest{}, openErr
		}
		defer func() { _ = localRoot.Close() }()
		if i.afterLocalSourceOpen != nil {
			i.afterLocalSourceOpen()
		}
		if err := verifyLocalSourcePath(*source.local, localRoot); err != nil {
			return Manifest{}, err
		}
		tree, err = readReviewedRoot(localRoot, maxReviewBytes, i.beforeLocalEntry)
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
	i.noteAuditMutation(ctx, "skillinstall.stage", manifest.Name, "stage_id="+manifest.StageID)
	return manifest, nil
}

// Install is the narrow wrapper kept for callers that only need the installed
// skill. It discards InstallResult.Warnings, including a lost audit row (#297);
// callers that report install outcomes must use InstallReviewed.
func (i *Installer) Install(ctx context.Context, stageID, digest string) (skill.Skill, error) {
	result, err := i.InstallReviewed(ctx, stageID, digest)
	if result.Committed {
		return result.Skill, nil
	}
	return result.Skill, err
}

func (i *Installer) InstallReviewed(ctx context.Context, stageID, digest string) (result InstallResult, retErr error) {
	if i == nil {
		return InstallResult{}, errors.New("skill installer required")
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if !stageIDPattern.MatchString(stageID) {
		return InstallResult{}, ErrStageNotFound
	}
	if err := validateExistingRoot(i.StageRoot); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return InstallResult{}, ErrStageNotFound
		}
		return InstallResult{}, err
	}
	stagePath := filepath.Join(i.StageRoot, stageID)
	defer func() {
		cleanupErr := removeOwnedStage(i.StageRoot, stageID)
		if cleanupErr == nil {
			return
		}
		if result.Committed {
			result.Warnings = append(result.Warnings, cleanupErr)
			return
		}
		retErr = errors.Join(retErr, cleanupErr)
	}()
	stageInfo, err := os.Lstat(stagePath)
	if errors.Is(err, os.ErrNotExist) {
		return InstallResult{}, ErrStageNotFound
	}
	if err != nil || !stageInfo.IsDir() || stageInfo.Mode()&os.ModeSymlink != 0 {
		return InstallResult{}, ErrStageNotFound
	}
	if err := ctx.Err(); err != nil {
		return InstallResult{}, err
	}

	record, err := readStageRecord(filepath.Join(stagePath, stageRecordName))
	if err != nil {
		return InstallResult{}, err
	}
	if record.Version != 1 || record.Manifest.StageID != stageID {
		return InstallResult{}, ErrStageChanged
	}
	if !i.clock().Before(record.Manifest.ExpiresAt) {
		return InstallResult{}, ErrStageExpired
	}
	if digest == "" || digest != record.Manifest.ContentDigest {
		return InstallResult{}, ErrDigestMismatch
	}
	tree, err := readReviewedTree(filepath.Join(stagePath, "content"))
	if err != nil {
		return InstallResult{}, fmt.Errorf("%w: %v", ErrStageChanged, err)
	}
	if !treeMatchesManifest(tree, record.Manifest) {
		return InstallResult{}, ErrStageChanged
	}
	if err := ensureSkillsRoot(i.SkillsRoot); err != nil {
		return InstallResult{}, err
	}
	if err := ensureSkillAbsent(i.SkillsRoot, tree.name); err != nil {
		return InstallResult{}, err
	}

	finalFiles, err := filesWithInactiveStatus(tree.files)
	if err != nil {
		return InstallResult{}, err
	}
	temporaryPath := filepath.Join(i.SkillsRoot, ".waffle-install-"+stageID)
	if _, err := os.Lstat(temporaryPath); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return InstallResult{}, errors.New("private install staging path already exists")
		}
		return InstallResult{}, fmt.Errorf("inspect private install staging path: %w", err)
	}
	temporaryOwned := false
	defer func() {
		if temporaryOwned && !result.Committed {
			retErr = errors.Join(retErr, removeOwnedPath(i.SkillsRoot, temporaryPath))
		}
	}()
	if err := writeReviewedTree(temporaryPath, finalFiles); err != nil {
		return InstallResult{}, fmt.Errorf("write atomic skill install stage: %w", err)
	}
	temporaryOwned = true
	finalTree, err := readReviewedTreeBound(temporaryPath, maxReviewBytes+maxInactiveGrowth)
	if err != nil || !reviewedFilesEqual(finalTree.files, finalFiles) {
		return InstallResult{}, fmt.Errorf("verify atomic skill install stage: %w", ErrStageChanged)
	}
	if err := ensureSkillAbsent(i.SkillsRoot, tree.name); err != nil {
		return InstallResult{}, err
	}

	targetPath := filepath.Join(i.SkillsRoot, tree.name)
	installed := skill.Skill{
		Name:        finalTree.name,
		Description: finalTree.description,
		Path:        filepath.Join(targetPath, "SKILL.md"),
	}
	rename := i.rename
	if rename == nil {
		rename = atomicRenameNoReplace
	}
	if err := rename(temporaryPath, targetPath); err != nil {
		return InstallResult{}, fmt.Errorf("atomically install reviewed skill: %w", err)
	}
	temporaryOwned = false
	result = InstallResult{Skill: installed, Committed: true}
	syncParent := i.syncDirectory
	if syncParent == nil {
		syncParent = syncDirectory
	}
	if syncErr := syncParent(i.SkillsRoot); syncErr != nil {
		rollbackErr := rename(targetPath, temporaryPath)
		if rollbackErr == nil {
			result = InstallResult{}
			temporaryOwned = true
			return InstallResult{}, fmt.Errorf("sync installed skill directory: %w", syncErr)
		}
		result.Warnings = append(result.Warnings,
			fmt.Errorf("sync installed skill directory: %w", syncErr),
			fmt.Errorf("roll back installed skill after sync failure: %w", rollbackErr),
		)
		if auditErr := i.auditMutation(ctx, "skillinstall.install", installed.Name, "stage_id="+stageID+",committed=true,sync_warning=1"); auditErr != nil {
			result.Warnings = append(result.Warnings, auditErr)
		}
		return result, nil
	}
	if auditErr := i.auditMutation(ctx, "skillinstall.install", installed.Name, "stage_id="+stageID+",committed=true"); auditErr != nil {
		result.Warnings = append(result.Warnings, auditErr)
	}
	return result, nil
}

// auditMutation records a skillinstall mutation into policy_audit when AuditDB
// is set. The write failure is always logged and is returned so a committed
// install can report that its audit row is missing rather than drop it (#297).
func (i *Installer) auditMutation(ctx context.Context, tool, command, detail string) error {
	if i == nil || i.AuditDB == nil {
		return nil
	}
	err := policy.LogMutation(ctx, i.AuditDB, "", tool, command, detail)
	// detail is built here from the stage id and commit state; the command is
	// the skill name, which is caller-supplied audited content and stays out
	// of the failure log.
	policy.ReportAuditFailure(i.Log, err, "", tool, detail)
	if err != nil {
		return fmt.Errorf("%w: %s: %w", policy.ErrAuditNotRecorded, tool, err)
	}
	return nil
}

// noteAuditMutation records a mutation whose audit failure is reported but not
// returned: a stage installs nothing into SkillsRoot, is retryable, and the
// install that may follow is audited in its own right.
func (i *Installer) noteAuditMutation(ctx context.Context, tool, command, detail string) {
	_ = i.auditMutation(ctx, tool, command, detail)
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
	record, present, err := readBoundedJSONFile[persistedStage](path, maxStageRecord)
	if err != nil {
		return persistedStage{}, err
	}
	if !present {
		return persistedStage{}, ErrStageChanged
	}
	return record, nil
}

// readBoundedJSONFile strictly decodes a single JSON object from path,
// rejecting anything but a regular, non-symlink file no larger than maxSize
// plus any trailing data after the object. present is false only when the
// file does not exist; any other problem is reported as ErrStageChanged so
// callers treat a tampered-with or oversized record the same as a missing one.
func readBoundedJSONFile[T any](path string, maxSize int64) (value T, present bool, err error) {
	info, statErr := os.Lstat(path)
	if errors.Is(statErr, os.ErrNotExist) {
		return value, false, nil
	}
	if statErr != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 ||
		info.Size() < 0 || info.Size() > maxSize {
		return value, false, ErrStageChanged
	}
	body, readErr := os.ReadFile(path)
	if readErr != nil {
		return value, false, ErrStageChanged
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return value, false, ErrStageChanged
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return value, false, ErrStageChanged
	}
	return value, true, nil
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
