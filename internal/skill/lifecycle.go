package skill

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/matt-riley/waffle/internal/filecommit"
	"github.com/matt-riley/waffle/internal/lifecycle"
	"github.com/matt-riley/waffle/internal/memory"
)

var (
	ErrSkillNotFound          = errors.New("skill not found")
	ErrSkillActive            = errors.New("skill is active")
	ErrSkillAttached          = errors.New("skill is attached to sessions")
	ErrUninstallRecovery      = errors.New("skill uninstall recovery required")
	lifecycleSkillNamePattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

	uninstallDeleteStatus = deleteSkillStatus
	uninstallRemoveBackup = os.RemoveAll
	uninstallAfterPhase   func(string) error
)

// AttachmentConflictError keeps exact session references behind the stable
// ErrSkillAttached sentinel used by dashboard error mapping.
type AttachmentConflictError struct {
	References []AttachmentReference
}

func (e *AttachmentConflictError) Error() string { return ErrSkillAttached.Error() }
func (e *AttachmentConflictError) Unwrap() error { return ErrSkillAttached }

// DeactivateSkill is the inverse of ActivateSkill. It updates frontmatter and
// the persisted status together, restoring the file if the status write fails.
func DeactivateSkill(ctx context.Context, db *sql.DB, ws memory.Workspace, name string) error {
	target, err := findSkill(ws, name)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(target.Path)
	if err != nil {
		return err
	}
	info, err := os.Stat(target.Path)
	if err != nil {
		return err
	}
	updated := setFrontmatterStatus(string(raw), StatusInactive)
	if err := os.WriteFile(target.Path, []byte(updated), info.Mode().Perm()); err != nil {
		return err
	}
	if err := SetSkillStatusRecord(ctx, db, StatusRecord{Name: target.Name, Status: StatusInactive}); err != nil {
		restoreErr := os.WriteFile(target.Path, raw, info.Mode().Perm())
		if restoreErr != nil {
			restoreErr = fmt.Errorf("restore active skill after status failure: %w", restoreErr)
		}
		return errors.Join(err, restoreErr)
	}
	return nil
}

type uninstallJournal struct {
	Version        int           `json:"version"`
	Name           string        `json:"name"`
	SkillDir       string        `json:"skill_dir"`
	Backup         string        `json:"backup"`
	Parent         string        `json:"parent"`
	PreviousStatus *StatusRecord `json:"previous_status,omitempty"`
	Phase          string        `json:"phase"`
}

// UninstallSkill removes an inactive skill and its status/provenance record.
// The shared guard covers the attachment check, filesystem move, and status
// transition. A durable journal makes pre-commit failures roll back and lets
// startup finish or roll back a transaction after interruption.
func UninstallSkill(ctx context.Context, db *sql.DB, ws memory.Workspace, name string, attachments *Attachments, guard *lifecycle.Guard) error {
	if guard == nil && attachments != nil {
		guard = attachments.Lifecycle
	}
	if guard == nil {
		guard = lifecycle.NewGuard()
	}
	if err := guard.Lock(ctx); err != nil {
		return fmt.Errorf("lock skill lifecycle for uninstall: %w", err)
	}
	defer guard.Unlock()
	if err := recoverPendingSkillUninstallsLocked(ctx, db, ws); err != nil {
		return err
	}

	target, err := findSkill(ws, name)
	if err != nil {
		return err
	}
	active, err := FilterActive([]Skill{target}, db)
	if err != nil {
		return err
	}
	if len(active) != 0 {
		return ErrSkillActive
	}
	if attachments == nil && db != nil {
		attachments = &Attachments{DB: db, Workspace: ws, Lifecycle: guard}
	}
	if attachments != nil {
		if attachments.Lifecycle == nil {
			attachments.Lifecycle = guard
		}
		references, err := attachments.References(ctx, target.Name)
		if err != nil {
			return err
		}
		if len(references) != 0 {
			return &AttachmentConflictError{References: references}
		}
	}

	skillDir := filepath.Dir(target.Path)
	if filepath.Base(skillDir) != target.Name {
		// Journals key recovery on skill name; a mismatched directory would let
		// prepared-phase rollback rename into the wrong sibling skill path.
		return fmt.Errorf("skill directory name %q does not match skill name %q", filepath.Base(skillDir), target.Name)
	}
	parent := filepath.Dir(skillDir)
	backup := filepath.Join(parent, ".waffle-uninstall-"+target.Name)
	journalPath := uninstallJournalPath(parent, target.Name)
	previous, err := loadStatusRecord(ctx, db, target.Name)
	if err != nil {
		return err
	}
	journal := uninstallJournal{
		Version: 1, Name: target.Name, SkillDir: skillDir, Backup: backup, Parent: parent,
		PreviousStatus: previous, Phase: "prepared",
	}
	if err := writeUninstallJournal(journalPath, journal); err != nil {
		return err
	}
	if err := os.Rename(skillDir, backup); err != nil {
		return errors.Join(fmt.Errorf("stage skill uninstall: %w", err), removeUninstallJournal(journalPath, parent))
	}
	if err := syncDirectory(parent); err != nil {
		return errors.Join(fmt.Errorf("sync staged skill uninstall: %w", err), rollbackUninstall(ctx, db, journal, journalPath))
	}
	if err := uninstallDeleteStatus(ctx, db, target.Name); err != nil {
		return errors.Join(fmt.Errorf("remove skill status: %w", err), rollbackUninstall(ctx, db, journal, journalPath))
	}
	if uninstallAfterPhase != nil {
		if err := uninstallAfterPhase("status_removed"); err != nil {
			return err
		}
	}
	journal.Phase = "committed"
	if err := writeUninstallJournal(journalPath, journal); err != nil {
		prepared := journal
		prepared.Phase = "prepared"
		return errors.Join(fmt.Errorf("commit skill uninstall journal: %w", err), rollbackUninstall(ctx, db, prepared, journalPath))
	}
	if err := uninstallRemoveBackup(backup); err != nil {
		return fmt.Errorf("remove uninstalled skill: %w (recovery journal retained)", err)
	}
	if err := syncDirectory(parent); err != nil {
		return fmt.Errorf("sync removed skill: %w (recovery journal retained)", err)
	}
	if err := removeUninstallJournal(journalPath, parent); err != nil {
		return fmt.Errorf("finalize skill uninstall journal: %w", err)
	}
	return nil
}

func deleteSkillStatus(ctx context.Context, db *sql.DB, name string) error {
	if db == nil {
		return nil
	}
	_, err := db.ExecContext(ctx, `DELETE FROM skill_status WHERE name = ?`, name)
	return err
}

// RecoverPendingSkillUninstalls repairs or finalizes durable uninstall
// journals. It is called during workspace startup and is safe to call again.
func RecoverPendingSkillUninstalls(ctx context.Context, db *sql.DB, ws memory.Workspace, guard *lifecycle.Guard) error {
	if guard == nil {
		guard = lifecycle.NewGuard()
	}
	if err := guard.Lock(ctx); err != nil {
		return fmt.Errorf("lock skill lifecycle for recovery: %w", err)
	}
	defer guard.Unlock()
	return recoverPendingSkillUninstallsLocked(ctx, db, ws)
}

// RecoverPendingSkillUninstallsLocked repairs or finalizes uninstall journals
// while the caller already holds the lifecycle guard. It avoids deadlocking
// when recovery must remain atomic with a follow-up skill operation.
func RecoverPendingSkillUninstallsLocked(ctx context.Context, db *sql.DB, ws memory.Workspace) error {
	return recoverPendingSkillUninstallsLocked(ctx, db, ws)
}

func recoverPendingSkillUninstallsLocked(ctx context.Context, db *sql.DB, ws memory.Workspace) error {
	parent := ws.SkillsDir()
	entries, err := os.ReadDir(parent)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read skill uninstall recovery directory: %w", err)
	}
	journals := make(map[string]uninstallJournal)
	journalPaths := make(map[string]string)
	backups := make(map[string]string)
	for _, entry := range entries {
		name := entry.Name()
		switch {
		case strings.HasPrefix(name, ".waffle-uninstall-") && strings.HasSuffix(name, ".json"):
			journalPath := filepath.Join(parent, name)
			bytes, readErr := os.ReadFile(journalPath)
			if readErr != nil {
				return fmt.Errorf("read skill uninstall journal %q: %w", name, readErr)
			}
			var journal uninstallJournal
			if err := json.Unmarshal(bytes, &journal); err != nil {
				return fmt.Errorf("parse skill uninstall journal %q: %w", name, err)
			}
			if err := validateUninstallJournal(parent, journalPath, journal); err != nil {
				return err
			}
			key := filepath.Clean(journal.SkillDir)
			if _, exists := journals[key]; exists {
				return fmt.Errorf("%w: multiple journals reference skill directory %q", ErrUninstallRecovery, key)
			}
			journals[key] = journal
			journalPaths[key] = journalPath
		case strings.HasPrefix(name, ".waffle-uninstall-"):
			backups[name] = filepath.Join(parent, name)
		}
	}
	for backupName, backupPath := range backups {
		matched := false
		for _, journal := range journals {
			if filepath.Clean(journal.Backup) == filepath.Clean(backupPath) {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("%w: orphaned staged skill %q has no journal", ErrUninstallRecovery, backupName)
		}
	}
	for skillDir, journal := range journals {
		if err := recoverUninstallJournal(ctx, db, journal, journalPaths[skillDir]); err != nil {
			return err
		}
	}
	return nil
}

func validateUninstallJournal(parent, journalPath string, journal uninstallJournal) error {
	if journal.Version != 1 || !lifecycleSkillNamePattern.MatchString(journal.Name) {
		return fmt.Errorf("%w: invalid journal identity", ErrUninstallRecovery)
	}
	skillDir := filepath.Clean(journal.SkillDir)
	wantBackup := filepath.Join(parent, ".waffle-uninstall-"+journal.Name)
	skillDirName := filepath.Base(skillDir)
	if filepath.Clean(journalPath) != uninstallJournalPath(parent, journal.Name) || filepath.Clean(journal.Parent) != parent || filepath.Dir(skillDir) != parent || skillDirName == "" || skillDirName == "." || skillDirName == ".." || skillDirName != journal.Name || strings.HasPrefix(skillDirName, ".waffle-uninstall-") || filepath.Clean(journal.Backup) != wantBackup {
		return fmt.Errorf("%w: journal paths do not match skill root", ErrUninstallRecovery)
	}
	if journal.Phase != "prepared" && journal.Phase != "committed" {
		return fmt.Errorf("%w: unknown journal phase %q", ErrUninstallRecovery, journal.Phase)
	}
	return nil
}

func recoverUninstallJournal(ctx context.Context, db *sql.DB, journal uninstallJournal, journalPath string) error {
	if journal.Phase == "committed" {
		visibleInfo, err := requireRecoveryDirectory(journal.SkillDir, "visible skill")
		if err != nil {
			return err
		}
		if visibleInfo != nil {
			return fmt.Errorf("%w: committed uninstall still has visible skill %q", ErrUninstallRecovery, journal.Name)
		}
		backupInfo, err := requireRecoveryDirectory(journal.Backup, "staged skill")
		if err != nil {
			return err
		}
		if backupInfo == nil {
			return removeUninstallJournal(journalPath, journal.Parent)
		}
		if err := os.RemoveAll(journal.Backup); err != nil {
			return fmt.Errorf("recover committed skill uninstall %q: %w", journal.Name, err)
		}
		if err := syncDirectory(journal.Parent); err != nil {
			return fmt.Errorf("sync recovered skill uninstall %q: %w", journal.Name, err)
		}
		return removeUninstallJournal(journalPath, journal.Parent)
	}

	backupInfo, err := requireRecoveryDirectory(journal.Backup, "staged skill")
	if err != nil {
		return err
	}
	visibleInfo, err := requireRecoveryDirectory(journal.SkillDir, "visible skill")
	if err != nil {
		return err
	}
	if backupInfo != nil {
		if visibleInfo != nil {
			return fmt.Errorf("%w: both visible and staged skill %q exist", ErrUninstallRecovery, journal.Name)
		}
		if err := os.Rename(journal.Backup, journal.SkillDir); err != nil {
			return fmt.Errorf("restore interrupted skill uninstall %q: %w", journal.Name, err)
		}
	} else if visibleInfo == nil {
		return fmt.Errorf("%w: interrupted uninstall lost both copies of skill %q", ErrUninstallRecovery, journal.Name)
	}
	if journal.PreviousStatus != nil {
		if err := SetSkillStatusRecord(ctx, db, *journal.PreviousStatus); err != nil {
			return fmt.Errorf("restore skill status %q: %w", journal.Name, err)
		}
	}
	if err := syncDirectory(journal.Parent); err != nil {
		return fmt.Errorf("sync rolled back skill uninstall %q: %w", journal.Name, err)
	}
	return removeUninstallJournal(journalPath, journal.Parent)
}

func requireRecoveryDirectory(path, label string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: inspect %s %q: %v", ErrUninstallRecovery, label, path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("%w: %s %q is not a real directory", ErrUninstallRecovery, label, path)
	}
	return info, nil
}

func rollbackUninstall(ctx context.Context, db *sql.DB, journal uninstallJournal, journalPath string) error {
	err := recoverUninstallJournal(ctx, db, journal, journalPath)
	if err != nil {
		return err
	}
	return nil
}

func uninstallJournalPath(parent, name string) string {
	return filepath.Join(parent, ".waffle-uninstall-"+name+".json")
}

func writeUninstallJournal(path string, journal uninstallJournal) error {
	bytes, err := json.Marshal(journal)
	if err != nil {
		return fmt.Errorf("encode skill uninstall journal: %w", err)
	}
	if err := filecommit.Write(path, append(bytes, '\n'), 0o600); err != nil {
		return fmt.Errorf("write skill uninstall journal: %w", err)
	}
	return nil
}

func removeUninstallJournal(path, parent string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDirectory(parent)
}

func findSkill(ws memory.Workspace, name string) (Skill, error) {
	name = strings.TrimSpace(name)
	if !lifecycleSkillNamePattern.MatchString(name) {
		return Skill{}, ErrSkillNotFound
	}
	all, err := Discover(ws.SkillsDir())
	if err != nil {
		return Skill{}, err
	}
	target, ok := Find(all, name)
	if !ok {
		return Skill{}, ErrSkillNotFound
	}
	return target, nil
}

func loadStatusRecord(ctx context.Context, db *sql.DB, name string) (*StatusRecord, error) {
	if db == nil {
		return nil, nil
	}
	var status, source, sourceRef, digest, created, activated string
	err := db.QueryRowContext(ctx, `
		SELECT status, source, source_ref, content_digest, created_at, activated_at
		FROM skill_status WHERE name = ?`, name).Scan(
		&status, &source, &sourceRef, &digest, &created, &activated,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load skill status: %w", err)
	}
	createdAt, err := parseStatusTime(created)
	if err != nil {
		return nil, fmt.Errorf("load skill status %q created_at: %w", name, err)
	}
	activatedAt, err := parseStatusTime(activated)
	if err != nil {
		return nil, fmt.Errorf("load skill status %q activated_at: %w", name, err)
	}
	return &StatusRecord{
		Name:          name,
		Status:        status,
		Source:        source,
		SourceRef:     sourceRef,
		ContentDigest: digest,
		CreatedAt:     createdAt,
		ActivatedAt:   activatedAt,
	}, nil
}

// parseStatusTime distinguishes missing timestamps (empty → zero time) from
// corrupt values that must fail closed before they are journaled as PreviousStatus.
func parseStatusTime(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid skill status timestamp %q: %w", value, err)
	}
	return parsed, nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	return directory.Sync()
}
