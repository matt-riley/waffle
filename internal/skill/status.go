package skill

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/matt-riley/waffle/internal/filecommit"
	"github.com/matt-riley/waffle/internal/memory"
	"github.com/matt-riley/waffle/internal/skill/spec"
)

// Skill status values.
const (
	StatusActive   = "active"
	StatusInactive = "inactive"
)

// StatusRecord is the persisted state and install provenance for one skill.
type StatusRecord struct {
	Name          string
	Status        string
	Source        string
	SourceRef     string
	ContentDigest string
	CreatedAt     time.Time
	ActivatedAt   time.Time
}

// SetSkillStatusRecord upserts skill status without erasing existing
// provenance or historical activation time when those values are omitted.
func SetSkillStatusRecord(ctx context.Context, db *sql.DB, record StatusRecord) error {
	if record.Name == "" {
		return errors.New("skill name required")
	}
	if record.Status != StatusActive && record.Status != StatusInactive {
		return fmt.Errorf("skill status must be active or inactive, got %q", record.Status)
	}
	if db != nil {
		createdAt := record.CreatedAt.UTC()
		if createdAt.IsZero() {
			createdAt = time.Now().UTC()
		}
		activated := ""
		if record.Status == StatusActive {
			activatedAt := record.ActivatedAt.UTC()
			if activatedAt.IsZero() {
				activatedAt = createdAt
			}
			activated = activatedAt.Format(time.RFC3339Nano)
		}
		_, err := db.ExecContext(ctx, `
			INSERT INTO skill_status (
				name, status, source, source_ref, content_digest, created_at, activated_at
			)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(name) DO UPDATE SET
				status = excluded.status,
				source = CASE WHEN excluded.source != '' THEN excluded.source ELSE skill_status.source END,
				source_ref = CASE WHEN excluded.source_ref != '' THEN excluded.source_ref ELSE skill_status.source_ref END,
				content_digest = CASE WHEN excluded.content_digest != '' THEN excluded.content_digest ELSE skill_status.content_digest END,
				activated_at = CASE WHEN excluded.status = 'active' THEN excluded.activated_at ELSE skill_status.activated_at END`,
			record.Name, record.Status, record.Source, record.SourceRef,
			record.ContentDigest, createdAt.Format(time.RFC3339Nano), activated)
		if err != nil {
			return fmt.Errorf("set skill status: %w", err)
		}
	}
	return nil
}

// SetSkillStatus is the compatibility wrapper for status updates without
// install provenance.
func SetSkillStatus(ctx context.Context, db *sql.DB, name, status, source string) error {
	return SetSkillStatusRecord(ctx, db, StatusRecord{
		Name:   name,
		Status: status,
		Source: source,
	})
}

// ActivateSkill marks a skill active in DB and frontmatter.
func ActivateSkill(ctx context.Context, db *sql.DB, ws memory.Workspace, name string) error {
	path := filepath.Join(ws.SkillsDir(), name, "SKILL.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	updated, err := setFrontmatterStatus(string(raw), StatusActive)
	if err != nil {
		return err
	}
	if err := filecommit.Write(path, []byte(updated), info.Mode().Perm()); err != nil {
		return err
	}
	if err := SetSkillStatusRecord(ctx, db, StatusRecord{
		Name:   name,
		Status: StatusActive,
		Source: "activate",
	}); err != nil {
		restoreErr := filecommit.Write(path, raw, info.Mode().Perm())
		if restoreErr != nil {
			restoreErr = fmt.Errorf("restore inactive skill after status failure: %w", restoreErr)
		}
		return errors.Join(err, restoreErr)
	}
	return nil
}

// setFrontmatterStatus records activation state in a skill's frontmatter
// under the waffle metadata key (metadata.waffle/status, #396), migrating
// the legacy top-level status field away on write. Frontmatter-less and
// non-conforming legacy files are left untouched — activation state is
// authoritative in the skill_status table, and inventing a status-only
// block would produce a non-conforming SKILL.md.
func setFrontmatterStatus(raw, status string) (string, error) {
	fields, body, err := spec.ParseFrontmatter(raw)
	if err != nil {
		return raw, nil
	}
	if fields["name"] == "" || fields["description"] == "" {
		return raw, nil
	}
	fields[spec.WaffleStatusKey] = status
	delete(fields, "status")
	return string(spec.MarshalSKILL(fields, body)), nil
}

// isActiveFrontmatter reports whether a skill's frontmatter status is active
// or missing (pre-#65 skills default to active). Reads the waffle metadata
// key first, then the legacy top-level status.
func isActiveFrontmatter(raw string) bool {
	fields, _, err := spec.ParseFrontmatter(raw)
	if err != nil {
		return true
	}
	status := spec.StatusField(fields)
	// Missing status is treated as active (pre-#65 skills).
	return status == "" || status == StatusActive
}

// DiscoverActive returns skills that are active (frontmatter status active or
// missing; inactive filtered out). When db is non-nil, skill_status overrides
// frontmatter for known names. If skill_status cannot be read, DiscoverActive
// returns an error (fail closed) rather than silently treating inactive skills
// as active.
func DiscoverActive(dir string, db *sql.DB) ([]Skill, error) {
	all, err := Discover(dir)
	if err != nil {
		return nil, err
	}
	return FilterActive(all, db)
}

// FilterActive returns the active subset of an already-discovered skill list.
// Callers that also need the full list should Discover once and filter, rather
// than calling Discover and DiscoverActive (which walks the directory twice).
//
// When db is non-nil, skill_status rows override frontmatter. A missing row is
// not an error: legacy skills without a status row fall back to frontmatter
// (missing status defaults to active, preserving #65). A failed query, Scan, or
// rows.Err is an error — no skill is reported active when the override table
// cannot be read (deny-by-default).
func FilterActive(all []Skill, db *sql.DB) ([]Skill, error) {
	return FilterActiveWithExtension(all, db, nil)
}

// FilterActiveWithExtension is FilterActive with an additional plugin
// extension status override layer (#394): for plugin-discovered skills, the
// waffle client extension may carry per-skill activation state. Precedence
// is skill_status table (operator, wins) > extension (plugin-authored) >
// frontmatter status (missing defaults to active, #65).
func FilterActiveWithExtension(all []Skill, db *sql.DB, extension map[string]string) ([]Skill, error) {
	statusOverride, err := loadSkillStatusOverrides(db)
	if err != nil {
		return nil, err
	}
	var out []Skill
	for _, s := range all {
		st := statusOverride[s.Name]
		if st == "" {
			st = extension[s.Name]
		}
		if st == "" {
			if isActiveFrontmatter(s.raw) {
				st = StatusActive
			} else {
				st = StatusInactive
			}
		}
		if st == StatusActive {
			out = append(out, s)
		}
	}
	return out, nil
}

// loadSkillStatusOverrides reads name→status from skill_status. A nil db means
// no overrides (frontmatter-only). Query/scan/iteration failures return an
// error so callers fail closed instead of activating inactive skills.
func loadSkillStatusOverrides(db *sql.DB) (map[string]string, error) {
	if db == nil {
		return nil, nil
	}
	rows, err := db.Query(`SELECT name, status FROM skill_status`)
	if err != nil {
		return nil, fmt.Errorf("skill_status: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanSkillStatusRows(rows)
}

// skillStatusRows is the *sql.Rows subset used when loading overrides.
type skillStatusRows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}

func scanSkillStatusRows(rows skillStatusRows) (map[string]string, error) {
	out := map[string]string{}
	for rows.Next() {
		var n, s string
		if err := rows.Scan(&n, &s); err != nil {
			return nil, fmt.Errorf("skill_status scan: %w", err)
		}
		out[n] = s
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("skill_status iterate: %w", err)
	}
	return out, nil
}

// ActiveNames returns the set of active skill names for an already-discovered
// list. Prefer this when only membership is needed after a single Discover.
func ActiveNames(all []Skill, db *sql.DB) (map[string]struct{}, error) {
	active, err := FilterActive(all, db)
	if err != nil {
		return nil, err
	}
	out := make(map[string]struct{}, len(active))
	for _, item := range active {
		out[item.Name] = struct{}{}
	}
	return out, nil
}
