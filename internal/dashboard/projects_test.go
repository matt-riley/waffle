package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/project"
	"github.com/matt-riley/waffle/internal/sandbox"
	"github.com/matt-riley/waffle/internal/store"
	"github.com/matt-riley/waffle/internal/workspace"
)

// fakeProjectReader serves a fixed set of workspace files.
type fakeProjectReader struct {
	files map[string]string
}

func (f *fakeProjectReader) ReadFile(_ context.Context, _ string, path string) ([]byte, error) {
	content, ok := f.files[path]
	if !ok {
		return nil, errors.New("no such file")
	}
	return []byte(content), nil
}

// fakeProjectWorkspaces is a minimal WorkspaceManager + ProjectFileReader.
type fakeProjectWorkspaces struct {
	reader ProjectFileReader
}

func (f *fakeProjectWorkspaces) ReadFile(ctx context.Context, id, path string) ([]byte, error) {
	return f.reader.ReadFile(ctx, id, path)
}

func (f *fakeProjectWorkspaces) List(context.Context) ([]workspace.Workspace, error) { return nil, nil }
func (f *fakeProjectWorkspaces) Get(context.Context, string) (*workspace.Workspace, error) {
	return nil, nil
}
func (f *fakeProjectWorkspaces) OpenWithProfile(context.Context, string, string) (*workspace.Workspace, *sandbox.Client, error) {
	return nil, nil, nil
}
func (f *fakeProjectWorkspaces) Idle(context.Context, string) error { return nil }
func (f *fakeProjectWorkspaces) Resume(context.Context, string) (*workspace.Workspace, *sandbox.Client, error) {
	return nil, nil, nil
}
func (f *fakeProjectWorkspaces) InspectClose(context.Context, string) (*workspace.CloseReport, error) {
	return nil, nil
}
func (f *fakeProjectWorkspaces) Close(context.Context, string, bool) (*workspace.CloseReport, error) {
	return nil, nil
}

func newProjectFixture(t *testing.T, files map[string]string) (*ProjectsService, *project.Store) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.DB.Exec(`INSERT INTO sessions (id, created_at, updated_at) VALUES ('session-p', '', '')`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.Exec(`INSERT INTO workspaces (id, repo, url, image, container, volume, session_id, status, created_at, updated_at) VALUES ('ws-p', 'a/b', 'https://example.com/a/b', 'img', 'c', 'v', 'session-p', 'open', '', '')`); err != nil {
		t.Fatal(err)
	}
	projects := project.New(st.DB)
	service := NewProjectsService(projects, &fakeProjectReader{files: files})
	return service, projects
}

func TestProjectsPinListAttachDetach(t *testing.T) {
	ctx := context.Background()
	service, _ := newProjectFixture(t, map[string]string{"README.md": "# Readme"})

	view, err := service.PinFile(ctx, "ws-p", "README.md")
	if err != nil {
		t.Fatalf("PinFile: %v", err)
	}
	if view.Kind != "file" || view.Name != "README.md" || view.State != "available" {
		t.Fatalf("view = %+v", view)
	}
	if strings.Contains(view.Path, "/work/repo") {
		t.Fatalf("view leaks workspace path: %+v", view)
	}
	note, err := service.AddNote(ctx, "ws-p", "Guidance", "Follow the checklist.")
	if err != nil {
		t.Fatalf("AddNote: %v", err)
	}

	snapshot, err := service.List(ctx, "ws-p", "session-p")
	if err != nil || len(snapshot.Resources) != 2 {
		t.Fatalf("list = %+v, %v", snapshot, err)
	}
	if snapshot.Resources[0].Attached || snapshot.Resources[1].Attached {
		t.Fatal("nothing attached yet")
	}

	if err := service.Attach(ctx, note.ID, "session-p"); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	attached, err := service.ListAttached(ctx, "session-p")
	if err != nil || len(attached) != 1 || attached[0].Resource.ID != note.ID {
		t.Fatalf("attached = %+v, %v", attached, err)
	}
	snapshot, _ = service.List(ctx, "ws-p", "session-p")
	for _, r := range snapshot.Resources {
		if r.ID == note.ID && !r.Attached {
			t.Fatal("attached flag not reflected in list")
		}
	}
	if err := service.Detach(ctx, note.ID, "session-p"); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	attached, _ = service.ListAttached(ctx, "session-p")
	if len(attached) != 0 {
		t.Fatalf("attached after detach = %+v", attached)
	}
}

func TestProjectsPinRejectsTraversal(t *testing.T) {
	ctx := context.Background()
	service, _ := newProjectFixture(t, nil)
	for _, bad := range []string{"../etc/passwd", "/etc/passwd", ".env", "a\\b.md"} {
		if _, err := service.PinFile(ctx, "ws-p", bad); err == nil {
			t.Errorf("PinFile(%q) should fail", bad)
		}
	}
}

func TestProjectsEndpointsAreGuardedAndSanitized(t *testing.T) {
	ctx := context.Background()
	_, projects := newProjectFixture(t, map[string]string{"README.md": "# Readme"})
	security, err := NewSecurity("127.0.0.1:8422", TailnetOptions{}, bytes.NewReader(bytes.Repeat([]byte{2}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	RegisterProjectRoutes(mux, ProjectRouteConfig{
		Projects:    projects,
		Operations:  &Operations{Workspaces: &fakeProjectWorkspaces{reader: &fakeProjectReader{files: map[string]string{"README.md": "# Readme"}}}},
		Security:    security,
		Idempotency: NewIdempotencyStore(time.Now, 64, time.Minute),
	})

	// Missing token is rejected before the handler runs.
	body, _ := json.Marshal(map[string]string{"path": "README.md"})
	req := httptest.NewRequest("POST", "/api/v1/desk/projects/ws-p/resources/pin", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "k1")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unauthenticated pin status = %d", rec.Code)
	}

	// With token, pin succeeds and the response is sanitized.
	req = httptest.NewRequest("POST", "/api/v1/desk/projects/ws-p/resources/pin", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Waffle-Desk-Token", security.token)
	req.Header.Set("Idempotency-Key", "k2")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("pin status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var view ProjectResourceView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Name != "README.md" || strings.Contains(rec.Body.String(), "/work/repo") {
		t.Fatalf("view = %+v, body = %s", view, rec.Body.String())
	}

	// Attach binds to a session and detach is idempotent-safe.
	body2, _ := json.Marshal(map[string]string{"session_id": "session-p"})
	req = httptest.NewRequest("POST", "/api/v1/desk/projects/resources/"+view.ID+"/attach", strings.NewReader(string(body2)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Waffle-Desk-Token", security.token)
	req.Header.Set("Idempotency-Key", "k3")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("attach status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// List for the workspace reflects the attachment.
	listReq := httptest.NewRequest("GET", "/api/v1/desk/projects/ws-p/resources?session_id=session-p", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, listReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"attached":true`) {
		t.Fatalf("list = %s", rec.Body.String())
	}
	_ = ctx
}
