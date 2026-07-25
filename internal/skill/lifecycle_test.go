package skill

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matt-riley/waffle/internal/memory"
	"github.com/matt-riley/waffle/internal/store"
)

func TestDeactivateSkillPreservesProvenanceAndMakesSkillInactive(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	ws := memory.Workspace{Dir: filepath.Join(root, "workspace")}
	path := filepath.Join(ws.SkillsDir(), "reviewer", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("---\nname: reviewer\ndescription: Reviews changes.\nstatus: active\n---\n\n# Review\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, filepath.Join(root, "state", "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := SetSkillStatusRecord(ctx, st.DB, StatusRecord{
		Name:          "reviewer",
		Status:        StatusActive,
		Source:        "dashboard",
		SourceRef:     "local:reviewer",
		ContentDigest: "sha256:reviewer",
	}); err != nil {
		t.Fatal(err)
	}

	if err := DeactivateSkill(ctx, st.DB, ws, "reviewer"); err != nil {
		t.Fatal(err)
	}
	active, err := DiscoverActive(ws.SkillsDir(), st.DB)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 {
		t.Fatalf("deactivated skill remains active: %#v", active)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "status: inactive") {
		t.Fatalf("frontmatter did not become inactive: %s", raw)
	}
	var status, sourceRef, digest string
	if err := st.DB.QueryRowContext(ctx, `
		SELECT status, source_ref, content_digest FROM skill_status WHERE name = ?`, "reviewer").Scan(
		&status, &sourceRef, &digest,
	); err != nil {
		t.Fatal(err)
	}
	if status != StatusInactive || sourceRef != "local:reviewer" || digest != "sha256:reviewer" {
		t.Fatalf("status = (%q, %q, %q)", status, sourceRef, digest)
	}
}

func TestUninstallSkillRefusesAttachedSessionsWithReferences(t *testing.T) {
	ctx := context.Background()
	st, sessions := openAttachmentTestStore(t)
	first, err := sessions.Create(ctx, "first review")
	if err != nil {
		t.Fatal(err)
	}
	second, err := sessions.Create(ctx, "second review")
	if err != nil {
		t.Fatal(err)
	}
	attachments := &Attachments{DB: st.DB}
	for _, id := range []string{second.ID, first.ID} {
		if err := attachments.Attach(ctx, id, "reviewer"); err != nil {
			t.Fatal(err)
		}
	}
	root := t.TempDir()
	ws := memory.Workspace{Dir: filepath.Join(root, "workspace")}
	path := filepath.Join(ws.SkillsDir(), "reviewer", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("---\nname: reviewer\nstatus: inactive\n---\n\n# Review\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SetSkillStatus(ctx, st.DB, "reviewer", StatusInactive, "dashboard"); err != nil {
		t.Fatal(err)
	}

	err = UninstallSkill(ctx, st.DB, ws, "reviewer", attachments)
	var conflict *AttachmentConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("error = %v, want attachment conflict", err)
	}
	if len(conflict.References) != 2 || conflict.References[0].SessionID != first.ID || conflict.References[1].SessionID != second.ID {
		t.Fatalf("attachment references = %#v", conflict.References)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("attached skill was removed: %v", statErr)
	}
}

func TestUninstallSkillRemovesInactiveFilesAndStatus(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	ws := memory.Workspace{Dir: filepath.Join(root, "workspace")}
	path := filepath.Join(ws.SkillsDir(), "reviewer", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("---\nname: reviewer\nstatus: inactive\n---\n\n# Review\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, filepath.Join(root, "state", "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := SetSkillStatus(ctx, st.DB, "reviewer", StatusInactive, "dashboard"); err != nil {
		t.Fatal(err)
	}

	if err := UninstallSkill(ctx, st.DB, ws, "reviewer", &Attachments{DB: st.DB}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("skill directory = %v, want removed", err)
	}
	var count int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM skill_status WHERE name = ?`, "reviewer").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("skill status rows = %d, want zero", count)
	}
}
