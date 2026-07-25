package skill

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matt-riley/waffle/internal/lifecycle"
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

	err = UninstallSkill(ctx, st.DB, ws, "reviewer", attachments, st.SkillLifecycleGuard())
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

	if err := UninstallSkill(ctx, st.DB, ws, "reviewer", &Attachments{DB: st.DB}, st.SkillLifecycleGuard()); err != nil {
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

func TestAttachmentsRejectAttachAfterConcurrentUninstallCompletes(t *testing.T) {
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
	st, sessions := openAttachmentTestStore(t)
	sess, err := sessions.Create(ctx, "runtime")
	if err != nil {
		t.Fatal(err)
	}
	if err := SetSkillStatusRecord(ctx, st.DB, StatusRecord{
		Name: "reviewer", Status: StatusInactive, Source: "dashboard", SourceRef: "local:reviewer",
	}); err != nil {
		t.Fatal(err)
	}
	guard := st.SkillLifecycleGuard()
	attachments := &Attachments{DB: st.DB, Workspace: ws, Lifecycle: guard}

	uninstallDone := make(chan error, 1)
	go func() {
		uninstallDone <- UninstallSkill(ctx, st.DB, ws, "reviewer", attachments, guard)
	}()
	if err := <-uninstallDone; err != nil {
		t.Fatal(err)
	}

	if err := attachments.Attach(ctx, sess.ID, "reviewer"); !errors.Is(err, ErrSkillNotFound) {
		t.Fatalf("attach after completed uninstall = %v, want ErrSkillNotFound", err)
	}
	var count int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_skills WHERE session_id = ? AND skill_name = ?`, sess.ID, "reviewer").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("removed skill attachment rows = %d, want zero", count)
	}
}

func TestUninstallSkillRollsBackWhenStatusDeleteFails(t *testing.T) {
	ctx, ws, st, path, guard := newInactiveSkillFixture(t)
	attachments := &Attachments{DB: st.DB, Workspace: ws, Lifecycle: guard}
	injected := errors.New("status delete failed")
	previous := uninstallDeleteStatus
	uninstallDeleteStatus = func(context.Context, *sql.DB, string) error { return injected }
	t.Cleanup(func() { uninstallDeleteStatus = previous })

	err := UninstallSkill(ctx, st.DB, ws, "reviewer", attachments, guard)
	if !errors.Is(err, injected) {
		t.Fatalf("uninstall error = %v, want injected status failure", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("skill was not restored after status failure: %v", err)
	}
	assertStatusRecord(t, st.DB, "reviewer", StatusInactive, "local:reviewer")
	assertNoUninstallArtifacts(t, ws, "reviewer")
}

func TestUninstallSkillLeavesCommittedJournalForFilesystemRecovery(t *testing.T) {
	ctx, ws, st, path, guard := newInactiveSkillFixture(t)
	attachments := &Attachments{DB: st.DB, Workspace: ws, Lifecycle: guard}
	injected := errors.New("backup cleanup failed")
	previous := uninstallRemoveBackup
	uninstallRemoveBackup = func(string) error { return injected }
	t.Cleanup(func() { uninstallRemoveBackup = previous })

	err := UninstallSkill(ctx, st.DB, ws, "reviewer", attachments, guard)
	if !errors.Is(err, injected) {
		t.Fatalf("uninstall error = %v, want injected filesystem failure", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("skill after committed cleanup failure = %v, want removed", err)
	}
	assertNoStatusRecord(t, st.DB, "reviewer")
	assertUninstallArtifacts(t, ws, "reviewer")

	uninstallRemoveBackup = os.RemoveAll
	if err := RecoverPendingSkillUninstalls(ctx, st.DB, ws, guard); err != nil {
		t.Fatal(err)
	}
	assertNoUninstallArtifacts(t, ws, "reviewer")
}

func TestRecoverPendingUninstallRestoresVisibleSkillAndStatusAfterRestart(t *testing.T) {
	ctx, ws, st, path, guard := newInactiveSkillFixture(t)
	attachments := &Attachments{DB: st.DB, Workspace: ws, Lifecycle: guard}
	injected := errors.New("simulated interruption")
	previous := uninstallAfterPhase
	uninstallAfterPhase = func(phase string) error {
		if phase == "status_removed" {
			return injected
		}
		return nil
	}
	t.Cleanup(func() { uninstallAfterPhase = previous })

	err := UninstallSkill(ctx, st.DB, ws, "reviewer", attachments, guard)
	if !errors.Is(err, injected) {
		t.Fatalf("uninstall error = %v, want simulated interruption", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("interrupted uninstall skill path = %v, want staged", err)
	}
	assertUninstallArtifacts(t, ws, "reviewer")
	assertNoStatusRecord(t, st.DB, "reviewer")
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.Open(ctx, filepath.Join(filepath.Dir(rootOfWorkspace(ws)), "state", "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err := RecoverPendingSkillUninstalls(ctx, reopened.DB, ws, reopened.SkillLifecycleGuard()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("restart recovery did not restore skill: %v", err)
	}
	assertStatusRecord(t, reopened.DB, "reviewer", StatusInactive, "local:reviewer")
	assertNoUninstallArtifacts(t, ws, "reviewer")
}

func newInactiveSkillFixture(t *testing.T) (context.Context, memory.Workspace, *store.Store, string, *lifecycle.Guard) {
	t.Helper()
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
	if err := SetSkillStatusRecord(ctx, st.DB, StatusRecord{
		Name: "reviewer", Status: StatusInactive, Source: "dashboard", SourceRef: "local:reviewer", ContentDigest: "sha256:reviewer",
	}); err != nil {
		t.Fatal(err)
	}
	return ctx, ws, st, path, st.SkillLifecycleGuard()
}

func rootOfWorkspace(ws memory.Workspace) string { return filepath.Dir(ws.Dir) }

func assertStatusRecord(t *testing.T, db *sql.DB, name, wantStatus, wantSourceRef string) {
	t.Helper()
	var status, sourceRef string
	if err := db.QueryRow(`SELECT status, source_ref FROM skill_status WHERE name = ?`, name).Scan(&status, &sourceRef); err != nil {
		t.Fatal(err)
	}
	if status != wantStatus || sourceRef != wantSourceRef {
		t.Fatalf("status = (%q, %q), want (%q, %q)", status, sourceRef, wantStatus, wantSourceRef)
	}
}

func assertNoStatusRecord(t *testing.T, db *sql.DB, name string) {
	t.Helper()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM skill_status WHERE name = ?`, name).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("status rows = %d, want zero", count)
	}
}

func assertUninstallArtifacts(t *testing.T, ws memory.Workspace, name string) {
	t.Helper()
	parent := filepath.Dir(filepath.Join(ws.SkillsDir(), name))
	for _, suffix := range []string{"", ".json"} {
		if _, err := os.Stat(filepath.Join(parent, ".waffle-uninstall-"+name+suffix)); err != nil {
			t.Fatalf("uninstall artifact %q missing: %v", suffix, err)
		}
	}
}

func assertNoUninstallArtifacts(t *testing.T, ws memory.Workspace, name string) {
	t.Helper()
	parent := filepath.Dir(filepath.Join(ws.SkillsDir(), name))
	for _, suffix := range []string{"", ".json"} {
		if _, err := os.Stat(filepath.Join(parent, ".waffle-uninstall-"+name+suffix)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("uninstall artifact %q = %v, want absent", suffix, err)
		}
	}
}
