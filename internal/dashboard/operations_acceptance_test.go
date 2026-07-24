package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/memory"
	"github.com/matt-riley/waffle/internal/observability"
	"github.com/matt-riley/waffle/internal/schedule"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/store"
	"github.com/matt-riley/waffle/internal/workset"
	"github.com/matt-riley/waffle/internal/workspace"
)

func TestOperationsFlowPreservesSessionAndRequiresForgetConfirmation(t *testing.T) {
	const (
		selectedSession = "session-workspace"
		otherSession    = "session-other"
	)
	now := time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC)

	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	for _, id := range []string{selectedSession, otherSession} {
		if _, err := st.DB.ExecContext(
			t.Context(),
			`INSERT INTO sessions (id, title, created_at, updated_at) VALUES (?, ?, ?, ?)`,
			id,
			id,
			now.Format(time.RFC3339Nano),
			now.Format(time.RFC3339Nano),
		); err != nil {
			t.Fatal(err)
		}
	}

	memoryWorkspace := memory.Workspace{
		Dir:   t.TempDir(),
		Agent: memory.DefaultAgent,
	}
	liveNotes := strings.Join([]string{
		"- [id=notea] 2026-07-24 [trust=owner_stated source=owner session=session-workspace channel=desk untrusted=false]: waffle primary fact",
		"- [id=noteb] 2026-07-24 [trust=owner_stated source=owner session=session-other channel=desk untrusted=false]: waffle companion fact",
		"",
	}, "\n")
	if err := os.WriteFile(memoryWorkspace.MemoryPath(), []byte(liveNotes), 0o600); err != nil {
		t.Fatal(err)
	}
	notes := &memory.NotesIndex{DB: st.DB, Now: func() time.Time { return now }}
	memoryWorkspace.Notes = notes
	if err := notes.SyncWorkspace(t.Context(), memory.DefaultAgent, memoryWorkspace); err != nil {
		t.Fatal(err)
	}

	sessionStore := session.New(st)
	workspaceManager := &recordingWorkspaceManager{
		t:          t,
		workspaces: make(map[string]workspace.Workspace),
	}
	worksets := &workset.Store{DB: st.DB, MaxEntries: 1, MaxBytes: workset.MaxEntryBytes}
	events := NewEventHub(32)
	security := mustSecurity(t, "127.0.0.1:8422")
	idempotency := NewIdempotencyStore(func() time.Time { return now }, 64, time.Minute)
	operations := &Operations{
		Runs: taskRunReader{snapshot: observability.Snapshot{
			Recent: []observability.RecentRun{
				{
					ID:        "run-failed",
					SessionID: selectedSession,
					Source:    "cron",
					Phase:     "turn",
					Outcome:   "failed",
				},
				{
					ID:        "run-ok",
					SessionID: otherSession,
					Source:    "cron",
					Phase:     "turn",
					Outcome:   "ok",
				},
			},
		}},
		Jobs:       schedule.NewStore(st),
		Workspaces: workspaceManager,
		Sessions:   sessionStore,
		Notes:      notes,
		Workset:    worksets,
		Usage:      taskUsageReader{},
		Previews:   NewPreviewStore(func() time.Time { return now }, previewEntropy(8)),
		Events:     events,
		Now:        func() time.Time { return now },
	}

	mux := http.NewServeMux()
	RegisterTaskRoutes(mux, TaskRouteConfig{Operations: operations})
	RegisterWorkspaceRoutes(mux, WorkspaceRouteConfig{
		Operations: operations, Security: security, Idempotency: idempotency,
		Events: events, Egress: "none",
	})
	RegisterMemoryRoutes(mux, MemoryRouteConfig{
		Operations: operations, Workspace: memoryWorkspace, Security: security,
		Idempotency: idempotency, Events: events,
	})
	request := func(method, target, body, key string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, "http://127.0.0.1:8422"+target, strings.NewReader(body))
		req.Host = "127.0.0.1:8422"
		if method == http.MethodPost {
			req.Header.Set("X-Waffle-Desk-Token", security.Token())
			req.Header.Set("Idempotency-Key", key)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	attention := request(http.MethodGet, "/api/v1/desk/tasks?filter=attention", "", "")
	if attention.Code != http.StatusOK {
		t.Fatalf("attention status = %d: %s", attention.Code, attention.Body.String())
	}
	var tasks TasksSnapshot
	if err := json.Unmarshal(attention.Body.Bytes(), &tasks); err != nil {
		t.Fatal(err)
	}
	if len(tasks.Tasks) != 1 ||
		tasks.Tasks[0].ID != "run-failed" ||
		tasks.Tasks[0].SessionID != selectedSession ||
		!tasks.Tasks[0].OpenAtDesk {
		t.Fatalf("attention tasks = %+v", tasks.Tasks)
	}

	opened := request(
		http.MethodPost,
		"/api/v1/desk/workspaces/open",
		`{"repository":"owner/repo","profile":"reviewer"}`,
		"workspace-open",
	)
	assertAcceptanceWorkspaceSession(t, opened, http.StatusCreated, selectedSession, "")
	for _, transition := range []struct {
		name       string
		path       string
		wantStatus string
		wantURL    string
	}{
		{
			name: "select", path: "/api/v1/desk/workspaces/ws-1/select",
			wantStatus: "open",
			wantURL:    "/desk/?section=today&session_id=" + selectedSession,
		},
		{name: "idle", path: "/api/v1/desk/workspaces/ws-1/idle", wantStatus: "idle"},
		{name: "resume", path: "/api/v1/desk/workspaces/ws-1/resume", wantStatus: "open"},
	} {
		response := request(http.MethodPost, transition.path, `{}`, "workspace-"+transition.name)
		assertAcceptanceWorkspaceSession(
			t,
			response,
			http.StatusOK,
			selectedSession,
			transition.wantURL,
		)
		var payload WorkspaceMutationResponse
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Workspace.Status != transition.wantStatus {
			t.Fatalf("%s status = %q, want %q", transition.name, payload.Workspace.Status, transition.wantStatus)
		}
	}

	search := request(http.MethodGet, "/api/v1/desk/memory?query=waffle", "", "")
	if search.Code != http.StatusOK {
		t.Fatalf("memory search status = %d: %s", search.Code, search.Body.String())
	}
	var searchPayload struct {
		Hits []MemoryHit `json:"hits"`
	}
	if err := json.Unmarshal(search.Body.Bytes(), &searchPayload); err != nil {
		t.Fatal(err)
	}
	if !acceptanceHitState(searchPayload.Hits, "notea", false) ||
		!acceptanceHitState(searchPayload.Hits, "noteb", false) {
		t.Fatalf("memory hits = %+v", searchPayload.Hits)
	}

	attachA := request(
		http.MethodPost,
		"/api/v1/desk/memory/attach",
		`{"session_id":"session-workspace","query":"waffle","source":"note","source_id":"notea"}`,
		"memory-attach-a",
	)
	if attachA.Code != http.StatusOK {
		t.Fatalf("attach A status = %d: %s", attachA.Code, attachA.Body.String())
	}
	attachB := request(
		http.MethodPost,
		"/api/v1/desk/memory/attach",
		`{"session_id":"session-workspace","query":"waffle","source":"note","source_id":"noteb"}`,
		"memory-attach-b",
	)
	if attachB.Code != http.StatusConflict {
		t.Fatalf("attach B status = %d, want 409: %s", attachB.Code, attachB.Body.String())
	}
	entries, err := worksets.List(t.Context(), selectedSession)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 ||
		entries[0].Kind != workset.KindFact ||
		entries[0].Source != workset.SourceUser ||
		!entries[0].Pinned ||
		!strings.Contains(entries[0].Body, "notea") {
		t.Fatalf("selected-session workset = %+v", entries)
	}
	otherEntries, err := worksets.List(t.Context(), otherSession)
	if err != nil {
		t.Fatal(err)
	}
	if len(otherEntries) != 0 {
		t.Fatalf("other-session workset = %+v", otherEntries)
	}

	cancelledPreview := acceptanceForgetPreview(t, request, "forget-preview-cancel")
	if cancelledPreview.PreviewToken == "" {
		t.Fatal("cancelled preview did not issue a confirmation token")
	}
	liveAfterCancel, err := os.ReadFile(memoryWorkspace.MemoryPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(liveAfterCancel), "notea") || !strings.Contains(string(liveAfterCancel), "noteb") {
		t.Fatalf("cancelled preview changed live notes: %s", liveAfterCancel)
	}

	confirmedPreview := acceptanceForgetPreview(t, request, "forget-preview-confirm")
	confirmBody, err := json.Marshal(struct {
		PreviewToken string `json:"preview_token"`
	}{PreviewToken: confirmedPreview.PreviewToken})
	if err != nil {
		t.Fatal(err)
	}
	confirmed := request(
		http.MethodPost,
		"/api/v1/desk/memory/notea/forget",
		string(confirmBody),
		"forget-confirm",
	)
	if confirmed.Code != http.StatusOK {
		t.Fatalf("forget status = %d: %s", confirmed.Code, confirmed.Body.String())
	}
	liveAfterConfirm, err := os.ReadFile(memoryWorkspace.MemoryPath())
	if err != nil {
		t.Fatal(err)
	}
	archiveAfterConfirm, err := os.ReadFile(memoryWorkspace.ArchivePath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(liveAfterConfirm), "notea") ||
		!strings.Contains(string(liveAfterConfirm), "noteb") ||
		!strings.Contains(string(archiveAfterConfirm), "notea") ||
		strings.Contains(string(archiveAfterConfirm), "noteb") {
		t.Fatalf("forget scope live=%q archive=%q", liveAfterConfirm, archiveAfterConfirm)
	}

	refreshed := request(http.MethodGet, "/api/v1/desk/memory?query=waffle", "", "")
	if refreshed.Code != http.StatusOK {
		t.Fatalf("refreshed memory status = %d: %s", refreshed.Code, refreshed.Body.String())
	}
	searchPayload.Hits = nil
	if err := json.Unmarshal(refreshed.Body.Bytes(), &searchPayload); err != nil {
		t.Fatal(err)
	}
	if !acceptanceHitState(searchPayload.Hits, "notea", true) ||
		!acceptanceHitState(searchPayload.Hits, "noteb", false) {
		t.Fatalf("refreshed memory hits = %+v", searchPayload.Hits)
	}
}

func assertAcceptanceWorkspaceSession(
	t *testing.T,
	response *httptest.ResponseRecorder,
	wantCode int,
	wantSession string,
	wantURL string,
) {
	t.Helper()
	if response.Code != wantCode {
		t.Fatalf("workspace status = %d, want %d: %s", response.Code, wantCode, response.Body.String())
	}
	var payload WorkspaceMutationResponse
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Workspace.SessionID != wantSession || payload.TodayURL != wantURL {
		t.Fatalf("workspace response = %+v, want session %q URL %q", payload, wantSession, wantURL)
	}
}

func acceptanceForgetPreview(
	t *testing.T,
	request func(string, string, string, string) *httptest.ResponseRecorder,
	key string,
) MemoryForgetPreview {
	t.Helper()
	response := request(
		http.MethodPost,
		"/api/v1/desk/memory/notea/forget-preview",
		`{"query":"waffle"}`,
		key,
	)
	if response.Code != http.StatusOK {
		t.Fatalf("forget preview status = %d: %s", response.Code, response.Body.String())
	}
	var preview MemoryForgetPreview
	if err := json.Unmarshal(response.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	return preview
}

func acceptanceHitState(hits []MemoryHit, id string, archived bool) bool {
	for _, hit := range hits {
		if hit.Source == MemorySourceNote && hit.SourceID == id && hit.Archived == archived {
			return true
		}
	}
	return false
}
