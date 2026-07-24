package dashboard

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/memory"
	"github.com/matt-riley/waffle/internal/session"
)

func TestMemoryRouteSearchRequiresExactlyOneQueryAndReturnsSafeHits(t *testing.T) {
	harness := newMemoryRouteHarness(t)
	for _, target := range []string{
		"/api/v1/desk/memory",
		"/api/v1/desk/memory?query=",
		"/api/v1/desk/memory?query=waffle&query=again",
		"/api/v1/desk/memory?query=waffle&provider=logs",
	} {
		response := harness.request(http.MethodGet, target, "", "", false)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d, want 400: %s", target, response.Code, response.Body.String())
		}
		assertMemoryError(t, response.Body.Bytes(), "invalid_query")
	}

	response := harness.request(http.MethodGet, "/api/v1/desk/memory?query=waffle", "", "", false)
	if response.Code != http.StatusOK {
		t.Fatalf("search status = %d: %s", response.Code, response.Body.String())
	}
	var payload struct {
		Hits []MemoryHit `json:"hits"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Hits) != 1 || payload.Hits[0].SourceID != "abc123" {
		t.Fatalf("search payload = %#v", payload)
	}
	for _, secret := range []string{"workspace=/secret", "provider logs", "raw_line"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatalf("search leaked %q: %s", secret, response.Body.String())
		}
	}
}

func TestMemoryAttachRouteIsProtectedIdempotentAndPublishesCanonicalEvent(t *testing.T) {
	harness := newMemoryRouteHarness(t)
	body := `{"session_id":"session-live","query":"waffle","source":"note","source_id":"abc123"}`

	missingToken := harness.request(http.MethodPost, "/api/v1/desk/memory/attach", body, "attach-once", false)
	if missingToken.Code != http.StatusForbidden {
		t.Fatalf("missing token status = %d, want 403", missingToken.Code)
	}
	first := harness.request(http.MethodPost, "/api/v1/desk/memory/attach", body, "attach-once", true)
	second := harness.request(http.MethodPost, "/api/v1/desk/memory/attach", body, "attach-once", true)
	if first.Code != http.StatusOK || second.Code != http.StatusOK {
		t.Fatalf("attach status = %d/%d: %s / %s", first.Code, second.Code, first.Body.String(), second.Body.String())
	}
	if !bytes.Equal(first.Body.Bytes(), second.Body.Bytes()) {
		t.Fatalf("idempotent response differs: %q / %q", first.Body.Bytes(), second.Body.Bytes())
	}
	if harness.workset.calls != 1 {
		t.Fatalf("workset calls = %d, want 1", harness.workset.calls)
	}
	if harness.events.Cursor() != 1 {
		t.Fatalf("event cursor = %d, want 1", harness.events.Cursor())
	}
	subscription, resync := harness.events.Subscribe(0)
	if resync {
		t.Fatal("unexpected event resync")
	}
	t.Cleanup(func() { harness.events.Unsubscribe(subscription) })
	event := <-subscription
	if event.Type != MemoryAttachedEvent || event.Resource != MemorySourceNote || event.ResourceID != "abc123" {
		t.Fatalf("event = %#v", event)
	}
	for _, secret := range []string{"waffle note", "workspace=/secret", "attach-once", "preview_token"} {
		if bytes.Contains(event.Data, []byte(secret)) {
			t.Fatalf("event leaked %q: %s", secret, event.Data)
		}
	}

	conflict := harness.request(http.MethodPost, "/api/v1/desk/memory/attach",
		`{"session_id":"session-live","query":"waffle","source":"note","source_id":"changed"}`,
		"attach-once", true)
	if conflict.Code != http.StatusConflict || harness.workset.calls != 1 || harness.events.Cursor() != 1 {
		t.Fatalf("idempotency conflict = %d calls=%d events=%d: %s",
			conflict.Code, harness.workset.calls, harness.events.Cursor(), conflict.Body.String())
	}
}

func TestMemoryForgetRoutePreviewsExactScopeAndHasNoFakeUndo(t *testing.T) {
	harness := newMemoryRouteHarness(t)
	preview := harness.request(
		http.MethodPost,
		"/api/v1/desk/memory/abc123/forget-preview",
		`{"query":"waffle"}`,
		"preview-once",
		true,
	)
	if preview.Code != http.StatusOK {
		t.Fatalf("preview status = %d: %s", preview.Code, preview.Body.String())
	}
	var previewPayload MemoryForgetPreview
	if err := json.Unmarshal(preview.Body.Bytes(), &previewPayload); err != nil {
		t.Fatal(err)
	}
	if previewPayload.PreviewToken == "" || previewPayload.Scope != MemoryForgetScope {
		t.Fatalf("preview = %#v", previewPayload)
	}
	for _, word := range []string{"provider logs", "delivered messages", "backups"} {
		if !strings.Contains(preview.Body.String(), word) {
			t.Fatalf("preview missing exclusion %q: %s", word, preview.Body.String())
		}
	}
	if strings.Contains(strings.ToLower(preview.Body.String()), "undo") {
		t.Fatalf("preview offers fake undo: %s", preview.Body.String())
	}

	confirmBody, err := json.Marshal(struct {
		PreviewToken string `json:"preview_token"`
	}{PreviewToken: previewPayload.PreviewToken})
	if err != nil {
		t.Fatal(err)
	}
	confirm := harness.request(
		http.MethodPost,
		"/api/v1/desk/memory/abc123/forget",
		string(confirmBody),
		"forget-once",
		true,
	)
	if confirm.Code != http.StatusOK {
		t.Fatalf("confirm status = %d: %s", confirm.Code, confirm.Body.String())
	}
	if strings.Contains(strings.ToLower(confirm.Body.String()), "undo") {
		t.Fatalf("confirm offers fake undo: %s", confirm.Body.String())
	}
	live, err := os.ReadFile(harness.workspace.MemoryPath())
	if err != nil {
		t.Fatal(err)
	}
	archive, err := os.ReadFile(harness.workspace.ArchivePath())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(live, []byte("abc123")) || !bytes.Contains(archive, []byte("abc123")) {
		t.Fatalf("canonical files live=%q archive=%q", live, archive)
	}
	if harness.events.Cursor() != 1 {
		t.Fatalf("forget event cursor = %d", harness.events.Cursor())
	}
}

func TestMemoryMutationRoutesRejectStrictJSONAndOversizedBodies(t *testing.T) {
	harness := newMemoryRouteHarness(t)
	for _, body := range []string{
		`{"session_id":"session-live","query":"waffle","source":"note","source_id":"abc123","path":"/secret"}`,
		`{"session_id":"session-live","query":"waffle","source":"note","source_id":"abc123"} {}`,
	} {
		response := harness.request(http.MethodPost, "/api/v1/desk/memory/attach", body, "strict-"+body, true)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("strict JSON status = %d: %s", response.Code, response.Body.String())
		}
		assertMemoryError(t, response.Body.Bytes(), "invalid_request")
	}
	oversized := strings.Repeat("x", int(memoryMutationMaxBodyBytes)+1)
	response := harness.request(http.MethodPost, "/api/v1/desk/memory/attach", oversized, "oversized", true)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized status = %d: %s", response.Code, response.Body.String())
	}
	if harness.workset.calls != 0 || harness.events.Cursor() != 0 {
		t.Fatalf("rejected bodies mutated state: calls=%d events=%d", harness.workset.calls, harness.events.Cursor())
	}
}

type memoryRouteHarness struct {
	mux       *http.ServeMux
	security  *Security
	events    *EventHub
	workset   *recordingMemoryWorkset
	workspace memory.Workspace
}

func newMemoryRouteHarness(t *testing.T) *memoryRouteHarness {
	t.Helper()
	workspace := memory.Workspace{Dir: t.TempDir()}
	line := "- [id=abc123] 2026-07-24 [trust=owner_stated source=owner session=session-live channel=desk untrusted=false]: waffle note\n"
	if err := os.WriteFile(workspace.MemoryPath(), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	security := mustSecurity(t, "127.0.0.1:8422")
	events := NewEventHub(8)
	worksets := &recordingMemoryWorkset{}
	operations := &Operations{
		Sessions: &memorySessionStore{
			get: map[string]*session.Session{"session-live": {ID: "session-live"}},
		},
		Notes: &memoryNotesStore{hits: []memory.NoteHit{{
			ID:      "abc123",
			Snippet: "waffle note",
			RawLine: line,
		}}},
		Workset:  worksets,
		Previews: NewPreviewStore(nil, nil),
		Events:   events,
	}
	mux := http.NewServeMux()
	RegisterMemoryRoutes(mux, MemoryRouteConfig{
		Operations:  operations,
		Workspace:   workspace,
		Security:    security,
		Idempotency: NewIdempotencyStore(nil, 32, time.Minute),
		Events:      events,
	})
	return &memoryRouteHarness{
		mux: mux, security: security, events: events, workset: worksets, workspace: workspace,
	}
}

func (h *memoryRouteHarness) request(method, target, body, key string, token bool) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, "http://127.0.0.1:8422"+target, strings.NewReader(body))
	request.Host = "127.0.0.1:8422"
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	if token {
		request.Header.Set("X-Waffle-Desk-Token", h.security.Token())
	}
	recorder := httptest.NewRecorder()
	h.mux.ServeHTTP(recorder, request)
	return recorder
}

func assertMemoryError(t *testing.T, body []byte, code string) {
	t.Helper()
	var payload struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode error response %q: %v", body, err)
	}
	if payload.Code != code || payload.Message == "" {
		t.Fatalf("error response = %#v, want code %q", payload, code)
	}
	for _, leaked := range []string{"/secret", "sqlite", "token=", "workspace="} {
		if strings.Contains(string(body), leaked) {
			t.Fatalf("error leaked %q: %s", leaked, body)
		}
	}
}
