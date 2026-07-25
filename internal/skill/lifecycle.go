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
	guard.Lock()
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
	guard.Lock()
	defer guard.Unlock()
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
	backups := make(map[string]bool)
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
			journals[journal.Name] = journal
		case strings.HasPrefix(name, ".waffle-uninstall-"):
			backups[strings.TrimPrefix(name, ".waffle-uninstall-")] = true
		}
	}
	for name := range backups {
		if _, ok := journals[name]; !ok {
			return fmt.Errorf("%w: orphaned staged skill %q has no journal", ErrUninstallRecovery, name)
		}
	}
	for name, journal := range journals {
		if err := recoverUninstallJournal(ctx, db, journal, uninstallJournalPath(parent, name)); err != nil {
			return err
		}
	}
	return nil
}

func validateUninstallJournal(parent, journalPath string, journal uninstallJournal) error {
	if journal.Version != 1 || !lifecycleSkillNamePattern.MatchString(journal.Name) {
		return fmt.Errorf("%w: invalid journal identity", ErrUninstallRecovery)
	}
	wantSkillDir := filepath.Join(parent, journal.Name)
	wantBackup := filepath.Join(parent, ".waffle-uninstall-"+journal.Name)
	if filepath.Clean(journalPath) != uninstallJournalPath(parent, journal.Name) || filepath.Clean(journal.Parent) != parent || filepath.Clean(journal.SkillDir) != wantSkillDir || filepath.Clean(journal.Backup) != wantBackup {
		return fmt.Errorf("%w: journal paths do not match skill root", ErrUninstallRecovery)
	}
	if journal.Phase != "prepared" && journal.Phase != "committed" {
		return fmt.Errorf("%w: unknown journal phase %q", ErrUninstallRecovery, journal.Phase)
	}
	return nil
}

func recoverUninstallJournal(ctx context.Context, db *sql.DB, journal uninstallJournal, journalPath string) error {
	if journal.Phase == "committed" {
		if _, err := os.Stat(journal.SkillDir); err == nil {
			return fmt.Errorf("%w: committed uninstall still has visible skill %q", ErrUninstallRecovery, journal.Name)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect committed skill uninstall %q: %w", journal.Name, err)
		}
		if err := os.RemoveAll(journal.Backup); err != nil {
			return fmt.Errorf("recover committed skill uninstall %q: %w", journal.Name, err)
		}
		if err := syncDirectory(journal.Parent); err != nil {
			return fmt.Errorf("sync recovered skill uninstall %q: %w", journal.Name, err)
		}
		return removeUninstallJournal(journalPath, journal.Parent)
	}

	backupInfo, backupErr := os.Lstat(journal.Backup)
	if backupErr == nil {
		if _, err := os.Lstat(journal.SkillDir); err == nil {
			return fmt.Errorf("%w: both visible and staged skill %q exist", ErrUninstallRecovery, journal.Name)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect visible skill during recovery %q: %w", journal.Name, err)
		}
		if backupInfo.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: staged skill %q is a symlink", ErrUninstallRecovery, journal.Name)
		}
		if err := os.Rename(journal.Backup, journal.SkillDir); err != nil {
			return fmt.Errorf("restore interrupted skill uninstall %q: %w", journal.Name, err)
		}
	} else if !errors.Is(backupErr, os.ErrNotExist) {
		return fmt.Errorf("inspect staged skill during recovery %q: %w", journal.Name, backupErr)
	} else if _, err := os.Lstat(journal.SkillDir); errors.Is(err, os.ErrNotExist) {
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
	return &StatusRecord{
		Name:          name,
		Status:        status,
		Source:        source,
		SourceRef:     sourceRef,
		ContentDigest: digest,
		CreatedAt:     parseStatusTime(created),
		ActivatedAt:   parseStatusTime(activated),
	}, nil
}

func parseStatusTime(value string) time.Time {
	parsed, _ := time.Parse(time.RFC3339Nano, value)
	return parsed
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	return directory.Sync()
}
