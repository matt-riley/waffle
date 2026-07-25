package skill

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/store"
)

func openAttachmentTestStore(t *testing.T) (*store.Store, *session.Store) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, session.New(st)
}

func TestAttachmentsAreUniqueSortedAndIdempotent(t *testing.T) {
	ctx := context.Background()
	st, sessions := openAttachmentTestStore(t)
	sess, err := sessions.Create(ctx, "dashboard")
	if err != nil {
		t.Fatal(err)
	}
	attachments := &Attachments{DB: st.DB}

	for _, name := range []string{"reviewer", "github-review", "reviewer"} {
		if err := attachments.Attach(ctx, sess.ID, name); err != nil {
			t.Fatal(err)
		}
	}
	got, err := attachments.List(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"github-review", "reviewer"}; !slices.Equal(got, want) {
		t.Fatalf("attachments = %v, want %v", got, want)
	}

	var attachedAt string
	if err := st.DB.QueryRowContext(ctx, `
		SELECT attached_at FROM session_skills
		WHERE session_id = ? AND skill_name = ?`, sess.ID, "reviewer").Scan(&attachedAt); err != nil {
		t.Fatal(err)
	}
	if attachedAt == "" {
		t.Fatal("attached_at is empty")
	}

	if err := attachments.Detach(ctx, sess.ID, "github-review"); err != nil {
		t.Fatal(err)
	}
	if err := attachments.Detach(ctx, sess.ID, "github-review"); err != nil {
		t.Fatal(err)
	}
	got, err = attachments.List(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"reviewer"}; !slices.Equal(got, want) {
		t.Fatalf("attachments after detach = %v, want %v", got, want)
	}
}

func TestAttachmentsNormalizeSkillNamesBeforePersisting(t *testing.T) {
	ctx := context.Background()
	st, sessions := openAttachmentTestStore(t)
	sess, err := sessions.Create(ctx, "normalized")
	if err != nil {
		t.Fatal(err)
	}
	attachments := &Attachments{DB: st.DB}

	if err := attachments.Attach(ctx, sess.ID, " reviewer \t"); err != nil {
		t.Fatal(err)
	}
	got, err := attachments.List(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"reviewer"}; !slices.Equal(got, want) {
		t.Fatalf("attachments = %v, want %v", got, want)
	}
	references, err := attachments.References(ctx, " reviewer ")
	if err != nil {
		t.Fatal(err)
	}
	if len(references) != 1 || references[0].SessionID != sess.ID {
		t.Fatalf("references = %#v, want session %q", references, sess.ID)
	}
	if err := attachments.Detach(ctx, sess.ID, " reviewer "); err != nil {
		t.Fatal(err)
	}
	got, err = attachments.List(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("attachments after normalized detach = %v, want empty", got)
	}
}

func TestAttachmentsNormalizeLegacyWhitespaceRowsAtSQLBoundary(t *testing.T) {
	ctx := context.Background()
	st, sessions := openAttachmentTestStore(t)
	sess, err := sessions.Create(ctx, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	attachments := &Attachments{DB: st.DB}

	for _, name := range []string{" reviewer ", "reviewer"} {
		if _, err := st.DB.ExecContext(ctx, `
			INSERT INTO session_skills (session_id, skill_name, attached_at)
			VALUES (?, ?, ?)`, sess.ID, name, "2026-07-25T00:00:00Z"); err != nil {
			t.Fatal(err)
		}
	}

	got, err := attachments.List(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"reviewer"}; !slices.Equal(got, want) {
		t.Fatalf("legacy attachments = %v, want %v", got, want)
	}
	references, err := attachments.References(ctx, " reviewer ")
	if err != nil {
		t.Fatal(err)
	}
	if want := []AttachmentReference{{SessionID: sess.ID, Title: "legacy"}}; !slices.Equal(references, want) {
		t.Fatalf("legacy references = %#v, want %#v", references, want)
	}

	if err := attachments.Attach(ctx, sess.ID, " reviewer "); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := st.DB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM session_skills
		WHERE session_id = ?`, sess.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("attachment rows after idempotent legacy attach = %d, want 2", count)
	}

	if err := attachments.Detach(ctx, sess.ID, " reviewer "); err != nil {
		t.Fatal(err)
	}
	got, err = attachments.List(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("legacy attachments after detach = %v, want empty", got)
	}
	references, err = attachments.References(ctx, "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if len(references) != 0 {
		t.Fatalf("legacy references after detach = %#v, want empty", references)
	}
}

func TestAttachmentsReferencesReturnStableSessionLabels(t *testing.T) {
	ctx := context.Background()
	st, sessions := openAttachmentTestStore(t)
	first, err := sessions.Create(ctx, "first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := sessions.Create(ctx, "second")
	if err != nil {
		t.Fatal(err)
	}
	attachments := &Attachments{DB: st.DB}
	if err := attachments.Attach(ctx, second.ID, "reviewer"); err != nil {
		t.Fatal(err)
	}
	if err := attachments.Attach(ctx, first.ID, "reviewer"); err != nil {
		t.Fatal(err)
	}

	references, err := attachments.References(ctx, "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if want := []AttachmentReference{
		{SessionID: first.ID, Title: "first"},
		{SessionID: second.ID, Title: "second"},
	}; !slices.Equal(references, want) {
		t.Fatalf("references = %#v, want %#v", references, want)
	}
}

func TestAttachmentsCascadeWithSession(t *testing.T) {
	ctx := context.Background()
	st, sessions := openAttachmentTestStore(t)
	first, err := sessions.Create(ctx, "first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := sessions.Create(ctx, "second")
	if err != nil {
		t.Fatal(err)
	}
	attachments := &Attachments{DB: st.DB}
	if err := attachments.Attach(ctx, first.ID, "reviewer"); err != nil {
		t.Fatal(err)
	}
	if err := attachments.Attach(ctx, second.ID, "reviewer"); err != nil {
		t.Fatal(err)
	}

	if err := sessions.Delete(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	got, err := attachments.List(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("deleted session attachments = %#v, want non-nil empty", got)
	}
	got, err = attachments.List(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"reviewer"}; !slices.Equal(got, want) {
		t.Fatalf("second session attachments = %v, want %v", got, want)
	}
}

func TestAttachmentsRejectInvalidInputsAndMissingSession(t *testing.T) {
	ctx := context.Background()
	st, _ := openAttachmentTestStore(t)

	for _, tc := range []struct {
		name        string
		attachments *Attachments
		sessionID   string
		skillName   string
	}{
		{name: "nil receiver"},
		{name: "nil database", attachments: &Attachments{}},
		{name: "blank session", attachments: &Attachments{DB: st.DB}, sessionID: " \t", skillName: "reviewer"},
		{name: "blank skill", attachments: &Attachments{DB: st.DB}, sessionID: "missing", skillName: "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.attachments.Attach(ctx, tc.sessionID, tc.skillName); err == nil {
				t.Fatal("Attach succeeded, want error")
			}
		})
	}

	attachments := &Attachments{DB: st.DB}
	if err := attachments.Attach(ctx, "missing", "reviewer"); err == nil {
		t.Fatal("Attach to missing session succeeded")
	} else if !strings.Contains(strings.ToLower(err.Error()), "attach") {
		t.Fatalf("missing-session error = %q, want attachment context", err)
	}
	if _, err := (&Attachments{}).List(ctx, "session"); err == nil {
		t.Fatal("List with nil database succeeded")
	}
	if err := (&Attachments{}).Detach(ctx, "session", "reviewer"); err == nil {
		t.Fatal("Detach with nil database succeeded")
	}
}
