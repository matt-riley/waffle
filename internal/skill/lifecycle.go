package skill

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/matt-riley/waffle/internal/memory"
)

var (
	ErrSkillNotFound          = errors.New("skill not found")
	ErrSkillActive            = errors.New("skill is active")
	ErrSkillAttached          = errors.New("skill is attached to sessions")
	lifecycleSkillNamePattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
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

// UninstallSkill removes an inactive skill and its status/provenance record.
// The installed directory is moved aside while the status row is removed so
// failures can restore the visible skill before the staged files are deleted.
func UninstallSkill(ctx context.Context, db *sql.DB, ws memory.Workspace, name string, attachments *Attachments) error {
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
		attachments = &Attachments{DB: db}
	}
	if attachments != nil {
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
	if _, err := os.Lstat(backup); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return errors.New("skill uninstall staging path already exists")
		}
		return fmt.Errorf("inspect skill uninstall staging path: %w", err)
	}
	if err := os.Rename(skillDir, backup); err != nil {
		return fmt.Errorf("stage skill uninstall: %w", err)
	}
	if err := syncDirectory(parent); err != nil {
		return errors.Join(
			fmt.Errorf("sync staged skill uninstall: %w", err),
			restoreUninstalledSkill(ctx, db, nil, backup, skillDir),
		)
	}

	var previous *StatusRecord
	if db != nil {
		previous, err = loadStatusRecord(ctx, db, target.Name)
		if err != nil {
			return errors.Join(err, restoreUninstalledSkill(ctx, db, nil, backup, skillDir))
		}
		if _, err := db.ExecContext(ctx, `DELETE FROM skill_status WHERE name = ?`, target.Name); err != nil {
			return errors.Join(
				fmt.Errorf("remove skill status: %w", err),
				restoreUninstalledSkill(ctx, db, previous, backup, skillDir),
			)
		}
	}
	if err := os.RemoveAll(backup); err != nil {
		return errors.Join(
			fmt.Errorf("remove uninstalled skill: %w", err),
			restoreUninstalledSkill(ctx, db, previous, backup, skillDir),
		)
	}
	if err := syncDirectory(parent); err != nil {
		return fmt.Errorf("sync removed skill: %w", err)
	}
	return nil
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

func restoreUninstalledSkill(ctx context.Context, db *sql.DB, previous *StatusRecord, backup, skillDir string) error {
	var errs []error
	if err := os.Rename(backup, skillDir); err != nil {
		errs = append(errs, err)
	}
	if previous != nil && db != nil {
		if err := SetSkillStatusRecord(ctx, db, *previous); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	return directory.Sync()
}
