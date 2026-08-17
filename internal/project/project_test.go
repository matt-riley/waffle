package project

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/matt-riley/waffle/internal/store"
)

func newTestStore(t *testing.T, files map[string]string) *Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	// Seed the workspace + session rows the FKs require.
	// Seed the session + workspace rows the FKs require (sessions first: the
	// workspaces table references a session).
	if _, err := st.DB.Exec(`INSERT INTO sessions (id, created_at, updated_at) VALUES ('session-1', '', ''), ('session-2', '', '')`); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	if _, err := st.DB.Exec(`INSERT INTO workspaces (id, repo, url, image, container, volume, session_id, status, created_at, updated_at) VALUES ('ws-1', 'a/b', 'https://example.com/a/b', 'img', 'c', 'v', 'session-1', 'open', '', '')`); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	s := New(st.DB)
	s.ReadFile = func(_ context.Context, workspaceID, path string) ([]byte, error) {
		if workspaceID != "ws-1" {
			return nil, ErrNotOwned
		}
		content, ok := files[path]
		if !ok {
			return nil, errors.New("no such file")
		}
		return []byte(content), nil
	}
	return s
}

func TestValidPath(t *testing.T) {
	valid := []string{"README.md", "docs/plan.md", "src/main.go", "a b/c.txt"}
	for _, p := range valid {
		if !ValidPath(p) {
			t.Errorf("ValidPath(%q) = false, want true", p)
		}
	}
	invalid := []string{"", "/etc/passwd", "../escape", "a/../b", "a\\b", "a;rm -rf", "dir/../../x", `..`}
	for _, p := range invalid {
		if ValidPath(p) {
			t.Errorf("ValidPath(%q) = true, want false", p)
		}
	}
}

func TestPinFileAndRefresh(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, map[string]string{"README.md": "# Readme", "docs/plan.md": "plan"})
	r, err := s.PinFile(ctx, "ws-1", "README.md")
	if err != nil {
		t.Fatalf("PinFile: %v", err)
	}
	if r.Kind != KindFile || r.Name != "README.md" || r.Size != 8 || r.Digest == "" || r.State != StateAvailable {
		t.Fatalf("resource = %+v", r)
	}
	// Refresh with unchanged content keeps it available.
	if err := s.Refresh(ctx, "ws-1"); err != nil {
		t.Fatal(err)
	}
	list, err := s.List(ctx, "ws-1")
	if err != nil || len(list) != 1 || list[0].State != StateAvailable {
		t.Fatalf("list = %+v, %v", list, err)
	}
	// A changed file becomes stale.
	s.ReadFile = func(_ context.Context, _, _ string) ([]byte, error) { return []byte("# CHANGED"), nil }
	if err := s.Refresh(ctx, "ws-1"); err != nil {
		t.Fatal(err)
	}
	list, _ = s.List(ctx, "ws-1")
	if list[0].State != StateStale {
		t.Fatalf("state = %q, want stale", list[0].State)
	}
	// A missing file becomes missing.
	s.ReadFile = func(_ context.Context, _, _ string) ([]byte, error) { return nil, errors.New("no such file") }
	if err := s.Refresh(ctx, "ws-1"); err != nil {
		t.Fatal(err)
	}
	list, _ = s.List(ctx, "ws-1")
	if list[0].State != StateMissing {
		t.Fatalf("state = %q, want missing", list[0].State)
	}
}

func TestPinFileRejectsUnsafePathsAndExclusions(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, map[string]string{"ok.md": "x"})
	for _, p := range []string{"../escape.md", "/etc/passwd", "a\\b.md", "dir/../../x.md"} {
		if _, err := s.PinFile(ctx, "ws-1", p); !errors.Is(err, ErrUnsupportedFile) {
			t.Errorf("PinFile(%q) = %v, want ErrUnsupportedFile", p, err)
		}
	}
	// Secret-like names are excluded by default.
	for _, name := range []string{".env", "secrets.md", "id_rsa.pub", "credentials.txt", "api_key.json"} {
		if _, err := s.PinFile(ctx, "ws-1", name); !errors.Is(err, ErrUnsupportedFile) {
			t.Errorf("PinFile(%q) = %v, want ErrUnsupportedFile", name, err)
		}
	}
	// Unsupported extensions and binary payloads are excluded.
	if _, err := s.PinFile(ctx, "ws-1", "archive.zip"); !errors.Is(err, ErrUnsupportedFile) {
		t.Errorf("zip = %v", err)
	}
	s.ReadFile = func(_ context.Context, _, _ string) ([]byte, error) { return []byte("a\x00b"), nil }
	if _, err := s.PinFile(ctx, "ws-1", "ok.md"); !errors.Is(err, ErrUnsupportedFile) {
		t.Errorf("binary = %v, want ErrUnsupportedFile", err)
	}
	// Missing files fail closed with ErrMissingFile.
	s.ReadFile = func(_ context.Context, _, _ string) ([]byte, error) { return nil, errors.New("gone") }
	if _, err := s.PinFile(ctx, "ws-1", "ok.md"); !errors.Is(err, ErrMissingFile) {
		t.Errorf("missing = %v, want ErrMissingFile", err)
	}
}

func TestAddNoteAndRemove(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, nil)
	r, err := s.AddNote(ctx, "ws-1", "Guidance", "Follow the release checklist.")
	if err != nil {
		t.Fatalf("AddNote: %v", err)
	}
	if r.Kind != KindNote || r.Note != "Follow the release checklist." || r.Provenance != "owner note" {
		t.Fatalf("note = %+v", r)
	}
	if err := s.Remove(ctx, "ws-1", r.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := s.Get(ctx, "ws-1", r.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after remove = %v, want ErrNotFound", err)
	}
	// Cross-workspace remove fails closed.
	note, _ := s.AddNote(ctx, "ws-1", "N", "x")
	if err := s.Remove(ctx, "ws-2", note.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-workspace remove = %v", err)
	}
}

func TestAttachEntersWorkingSetWithProvenance(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, map[string]string{"README.md": "# Readme\n\nline two"})
	r, err := s.PinFile(ctx, "ws-1", "README.md")
	if err != nil {
		t.Fatal(err)
	}
	entry, err := s.Attach(ctx, "session-1", r.ID)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if !contains(entry.Body, "[project context] README.md") || !contains(entry.Body, "untrusted data") {
		t.Fatalf("entry body = %q", entry.Body)
	}
	attached, err := s.ListAttached(ctx, "session-1")
	if err != nil || len(attached) != 1 || attached[0].Resource.ID != r.ID {
		t.Fatalf("attached = %+v, %v", attached, err)
	}
	// Detach removes both the binding and the working-set entry it created
	// (the deterministic attachment entry ID must match, #478 review).
	var workingSetEntries int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM working_set_entries WHERE session_id = 'session-1'`).Scan(&workingSetEntries); err != nil {
		t.Fatal(err)
	}
	if workingSetEntries != 1 {
		t.Fatalf("working set entries after attach = %d, want 1", workingSetEntries)
	}
	if err := s.Detach(ctx, "session-1", r.ID); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	attached, _ = s.ListAttached(ctx, "session-1")
	if len(attached) != 0 {
		t.Fatalf("attached after detach = %+v", attached)
	}
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM working_set_entries WHERE session_id = 'session-1'`).Scan(&workingSetEntries); err != nil {
		t.Fatal(err)
	}
	if workingSetEntries != 0 {
		t.Fatalf("working set entries after detach = %d, want 0", workingSetEntries)
	}
}

func TestAttachFailsClosedForStaleOrMissing(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, map[string]string{"a.md": "one"})
	r, _ := s.PinFile(ctx, "ws-1", "a.md")
	// The file changes behind the pin's back.
	s.ReadFile = func(_ context.Context, _, _ string) ([]byte, error) { return []byte("two"), nil }
	if _, err := s.Attach(ctx, "session-1", r.ID); err == nil {
		t.Fatal("attach of a changed file must fail closed")
	}
	// Missing file fails closed.
	s.ReadFile = func(_ context.Context, _, _ string) ([]byte, error) { return nil, errors.New("gone") }
	if _, err := s.Attach(ctx, "session-1", r.ID); err == nil {
		t.Fatal("attach of a missing file must fail closed")
	}
	// A stale-pinned resource (from Refresh) fails closed without reading.
	s.ReadFile = func(_ context.Context, _, _ string) ([]byte, error) { return []byte("one"), nil }
	if err := s.Refresh(ctx, "ws-1"); err != nil {
		t.Fatal(err)
	}
	// Mark it stale directly.
	if _, err := s.DB.ExecContext(ctx, `UPDATE project_resources SET state = 'stale' WHERE id = ?`, r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Attach(ctx, "session-1", r.ID); err == nil {
		t.Fatal("attach of a stale resource must fail closed")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestRefreshStalenessIsNotTransient pins that a changed file stays stale:
// Refresh must not adopt the new digest, or a later refresh would flip the
// resource back to available and allow attaching changed content (#478).
func TestRefreshStalenessIsNotTransient(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, map[string]string{"a.md": "one"})
	r, err := s.PinFile(ctx, "ws-1", "a.md")
	if err != nil {
		t.Fatal(err)
	}
	// File changes; the first refresh marks it stale.
	s.ReadFile = func(_ context.Context, _, _ string) ([]byte, error) { return []byte("two"), nil }
	if err := s.Refresh(ctx, "ws-1"); err != nil {
		t.Fatal(err)
	}
	// A second refresh with the same changed content must NOT flip back.
	if err := s.Refresh(ctx, "ws-1"); err != nil {
		t.Fatal(err)
	}
	list, err := s.List(ctx, "ws-1")
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if list[0].State != StateStale {
		t.Fatalf("state after second refresh = %q, want stale", list[0].State)
	}
	// The pinned baseline digest is preserved.
	got, err := s.Get(ctx, "ws-1", r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Digest != r.Digest || got.Size != r.Size {
		t.Fatalf("pinned digest/size changed: %+v", got)
	}
	// Restoring the original content re-adopts available.
	s.ReadFile = func(_ context.Context, _, _ string) ([]byte, error) { return []byte("one"), nil }
	if err := s.Refresh(ctx, "ws-1"); err != nil {
		t.Fatal(err)
	}
	list, _ = s.List(ctx, "ws-1")
	if list[0].State != StateAvailable {
		t.Fatalf("state after restore = %q, want available", list[0].State)
	}
}

func TestPinFileEnforcesWorkspaceCap(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, nil)
	s.MaxPerWorkspace = 2
	for i := 0; i < 2; i++ {
		if _, err := s.AddNote(ctx, "ws-1", "N", "body"); err != nil {
			t.Fatalf("note %d: %v", i, err)
		}
	}
	if _, err := s.AddNote(ctx, "ws-1", "Too many", "body"); err == nil {
		t.Fatal("third resource should exceed the workspace cap")
	}
}
