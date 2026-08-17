package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/artifact"
	"github.com/matt-riley/waffle/internal/chat"
	"github.com/matt-riley/waffle/internal/store"
)

func newArtifactFixture(t *testing.T) (*ChatClients, *artifact.Store, string) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.DB.Exec(`INSERT INTO sessions (id, created_at, updated_at) VALUES ('session-art', '', '')`); err != nil {
		t.Fatal(err)
	}
	backend := &fakeChatBackend{openState: chat.State{SessionID: "session-art"}}
	clients := NewChatClients(func(context.Context) (chat.Backend, error) { return backend, nil }, bytes.NewReader(bytes.Repeat([]byte{4}, 32)))
	clients.SetRedactor(func(s string) string { return s })
	artifacts := artifact.New(st.DB)
	if _, err := artifacts.Write(context.Background(), "session-art", "write_artifact", "report.md", "text/markdown", []byte("# Report\n\nFindings.")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := clients.Open(context.Background(), chat.OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	return clients, artifacts, firstClientKey(clients)
}

func firstClientKey(clients *ChatClients) string {
	clients.mu.Lock()
	defer clients.mu.Unlock()
	for id := range clients.clients {
		return id
	}
	return ""
}

func TestArtifactPreviewServesInlineContentToOwner(t *testing.T) {
	clients, artifacts, clientID := newArtifactFixture(t)
	service := NewArtifactsService(clients, artifacts, nil)
	list, err := artifacts.List(context.Background(), "session-art")
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	view, err := service.Preview(context.Background(), list[0].ID, clientID)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if view.Mode != "inline" || !strings.Contains(view.Content, "Findings") {
		t.Fatalf("view = %+v", view)
	}
	if view.ID == "" || strings.ContainsAny(view.ID, `/\`) {
		t.Fatalf("view id = %q", view.ID)
	}
}

func TestArtifactPreviewDeniesCrossSessionClient(t *testing.T) {
	clients, artifacts, _ := newArtifactFixture(t)
	service := NewArtifactsService(clients, artifacts, nil)
	list, err := artifacts.List(context.Background(), "session-art")
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if _, err := service.Preview(context.Background(), list[0].ID, "not-a-client"); err == nil {
		t.Fatal("preview without an owner lease should fail")
	}
}

func TestArtifactDownloadStreamsVerifiedPayload(t *testing.T) {
	clients, artifacts, clientID := newArtifactFixture(t)
	service := NewArtifactsService(clients, artifacts, nil)
	list, err := artifacts.List(context.Background(), "session-art")
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	rec := httptest.NewRecorder()
	if err := service.Download(context.Background(), rec, list[0].ID, clientID); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Fatalf("content type = %q", ct)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("missing nosniff")
	}
	if !strings.Contains(rec.Header().Get("Content-Disposition"), "attachment") ||
		!strings.Contains(rec.Header().Get("Content-Disposition"), "report.md") {
		t.Fatalf("content disposition = %q", rec.Header().Get("Content-Disposition"))
	}
	if !strings.Contains(rec.Body.String(), "Findings") {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

func TestArtifactContentRouteConsumesOneTimeToken(t *testing.T) {
	clients, artifacts, clientID := newArtifactFixture(t)
	service := NewArtifactsService(clients, artifacts, NewPreviewStore(nil, nil))
	list, err := artifacts.List(context.Background(), "session-art")
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	view, err := service.Preview(context.Background(), list[0].ID, clientID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Mode != "inline" {
		t.Fatalf("text artifact mode = %q, want inline", view.Mode)
	}
	// Reissue as a content route by writing an image artifact.
	img, err := artifacts.Write(context.Background(), "session-art", "write_artifact", "shot.png", "image/png", []byte("png-bytes"))
	if err != nil {
		t.Fatal(err)
	}
	view, err = service.Preview(context.Background(), img.ID, clientID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Mode != "content" || view.ContentURL == "" {
		t.Fatalf("image view = %+v", view)
	}
	token := strings.Split(view.ContentURL, "token=")[1]
	rec := httptest.NewRecorder()
	if err := service.Content(context.Background(), rec, img.ID, token); err != nil {
		t.Fatalf("Content: %v", err)
	}
	if rec.Body.String() != "png-bytes" {
		t.Fatalf("content body = %q", rec.Body.String())
	}
	// The token is one-time: a second consume fails.
	if err := service.Content(context.Background(), httptest.NewRecorder(), img.ID, token); err == nil {
		t.Fatal("one-time token should not serve twice")
	}
}

func TestArtifactStalePayloadIsRefused(t *testing.T) {
	clients, artifacts, clientID := newArtifactFixture(t)
	list, err := artifacts.List(context.Background(), "session-art")
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if _, err := artifacts.DB.Exec(`UPDATE artifacts SET payload = ?, digest = '' WHERE id = ?`, []byte("tampered"), list[0].ID); err != nil {
		t.Fatal(err)
	}
	service := NewArtifactsService(clients, artifacts, nil)
	if _, err := service.Preview(context.Background(), list[0].ID, clientID); err == nil {
		t.Fatal("stale payload must not be served")
	}
}

func TestArtifactPreviewEndpointJSONShape(t *testing.T) {
	clients, artifacts, clientID := newArtifactFixture(t)
	list, err := artifacts.List(context.Background(), "session-art")
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	security, err := NewSecurity("127.0.0.1:8422", TailnetOptions{}, bytes.NewReader(bytes.Repeat([]byte{2}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	RegisterArtifactRoutes(mux, ArtifactRouteConfig{
		Clients:     clients,
		Artifacts:   artifacts,
		Previews:    nil,
		Security:    security,
		Idempotency: NewIdempotencyStore(time.Now, 64, time.Minute),
	})
	body, _ := json.Marshal(map[string]string{"client_id": clientID})
	req := httptest.NewRequest("POST", "/api/v1/desk/artifacts/"+list[0].ID+"/preview", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Waffle-Desk-Token", security.token)
	req.Header.Set("Idempotency-Key", "test-key")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var view ArtifactView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Mode != "inline" || !strings.Contains(view.Content, "Findings") {
		t.Fatalf("view = %+v", view)
	}
	if view.Reason != "" {
		t.Fatalf("reason = %q", view.Reason)
	}
}

var _ = time.Second // keep time import when assertions change
