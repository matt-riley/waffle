package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/sandbox"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/workspace"
)

func TestWorkspaceRoutesListOpenSelectIdleAndResume(t *testing.T) {
	harness := newWorkspaceRouteHarness(t)

	list := harness.request(http.MethodGet, "/api/v1/desk/workspaces", "", "", true)
	if list.Code != http.StatusOK {
		t.Fatalf("list status = %d: %s", list.Code, list.Body.String())
	}
	var initial WorkspaceSnapshot
	if err := json.Unmarshal(list.Body.Bytes(), &initial); err != nil {
		t.Fatal(err)
	}
	if initial.Workspaces == nil || len(initial.Workspaces) != 0 {
		t.Fatalf("initial workspaces = %#v, want canonical empty slice", initial.Workspaces)
	}

	for i, body := range []string{
		`{"repository":"https://evil.example/owner/repo","profile":"reviewer"}`,
		`{"repository":"../repo","profile":"reviewer"}`,
		`{"repository":"owner/repo/extra","profile":"reviewer"}`,
		`{"repository":"owner/repo","profile":"../../root"}`,
	} {
		rec := harness.request(http.MethodPost, "/api/v1/desk/workspaces/open", body, "invalid-"+string(rune('a'+i)), true)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("invalid request %d status = %d, want 422: %s", i, rec.Code, rec.Body.String())
		}
		assertWorkspaceError(t, rec.Body.Bytes(), "invalid_workspace")
	}
	if harness.manager.openCalls != 0 {
		t.Fatalf("invalid requests reached manager %d times", harness.manager.openCalls)
	}

	opened := harness.request(
		http.MethodPost,
		"/api/v1/desk/workspaces/open",
		`{"repository":"matt-riley/waffle","profile":"reviewer"}`,
		"open-workspace",
		true,
	)
	if opened.Code != http.StatusCreated {
		t.Fatalf("open status = %d: %s", opened.Code, opened.Body.String())
	}
	var openResponse WorkspaceMutationResponse
	if err := json.Unmarshal(opened.Body.Bytes(), &openResponse); err != nil {
		t.Fatal(err)
	}
	if harness.manager.openRepo != "matt-riley/waffle" || harness.manager.openProfile != "reviewer" {
		t.Fatalf("open forwarding = repo %q profile %q", harness.manager.openRepo, harness.manager.openProfile)
	}
	if openResponse.Workspace.Repository != "matt-riley/waffle" ||
		openResponse.Workspace.SessionID != "session-workspace" ||
		openResponse.Workspace.Profile != "reviewer" ||
		openResponse.Workspace.Egress != "No network egress" {
		t.Fatalf("open response = %+v", openResponse)
	}
	assertSandboxClientClosed(t, harness.manager.openClient)

	selected := harness.request(
		http.MethodPost,
		"/api/v1/desk/workspaces/ws-1/select",
		`{}`,
		"select-workspace",
		true,
	)
	if selected.Code != http.StatusOK {
		t.Fatalf("select status = %d: %s", selected.Code, selected.Body.String())
	}
	var selectResponse WorkspaceMutationResponse
	if err := json.Unmarshal(selected.Body.Bytes(), &selectResponse); err != nil {
		t.Fatal(err)
	}
	wantToday := "/desk/?section=today&session_id=" + url.QueryEscape("session-workspace")
	if selectResponse.TodayURL != wantToday || harness.sessions.gotID != "session-workspace" {
		t.Fatalf("select response = %+v, session lookup = %q", selectResponse, harness.sessions.gotID)
	}

	idled := harness.request(
		http.MethodPost,
		"/api/v1/desk/workspaces/ws-1/idle",
		`{}`,
		"idle-workspace",
		true,
	)
	if idled.Code != http.StatusOK {
		t.Fatalf("idle status = %d: %s", idled.Code, idled.Body.String())
	}
	var idleResponse WorkspaceMutationResponse
	if err := json.Unmarshal(idled.Body.Bytes(), &idleResponse); err != nil {
		t.Fatal(err)
	}
	if idleResponse.Workspace.Status != workspace.StatusIdle || idleResponse.Workspace.SessionID != "session-workspace" {
		t.Fatalf("idle response = %+v", idleResponse)
	}

	resumed := harness.request(
		http.MethodPost,
		"/api/v1/desk/workspaces/ws-1/resume",
		`{}`,
		"resume-workspace",
		true,
	)
	if resumed.Code != http.StatusOK {
		t.Fatalf("resume status = %d: %s", resumed.Code, resumed.Body.String())
	}
	var resumeResponse WorkspaceMutationResponse
	if err := json.Unmarshal(resumed.Body.Bytes(), &resumeResponse); err != nil {
		t.Fatal(err)
	}
	if resumeResponse.Workspace.Status != workspace.StatusOpen || resumeResponse.Workspace.SessionID != "session-workspace" {
		t.Fatalf("resume response = %+v", resumeResponse)
	}
	assertSandboxClientClosed(t, harness.manager.resumeClient)

	events := readWorkspaceEvents(t, harness.events, 0, 4)
	wantTypes := []string{
		WorkspaceOpenedEvent,
		WorkspaceSelectedEvent,
		WorkspaceIdledEvent,
		WorkspaceResumedEvent,
	}
	for i, event := range events {
		if event.Type != wantTypes[i] || event.Resource != "workspace" || event.ResourceID != "ws-1" {
			t.Fatalf("event %d = %+v", i, event)
		}
		if strings.Contains(string(event.Data), "container-secret") {
			t.Fatalf("event leaked internal workspace fields: %s", event.Data)
		}
		var view WorkspaceView
		if err := json.Unmarshal(event.Data, &view); err != nil {
			t.Fatal(err)
		}
		if view.ID != "ws-1" || view.SessionID != "session-workspace" {
			t.Fatalf("event view = %+v", view)
		}
	}
}

func TestWorkspaceClosePreviewIsInspectOnlyExactAndResourceBound(t *testing.T) {
	harness := newWorkspaceRouteHarness(t)
	harness.manager.addWorkspace(workspace.Workspace{
		ID: "ws-1", Repo: "matt-riley/waffle", SessionID: "session-workspace",
		Status: workspace.StatusOpen, Image: "bookworm", Profile: "reviewer",
	})
	harness.manager.addWorkspace(workspace.Workspace{
		ID: "ws-2", Repo: "matt-riley/other", SessionID: "session-other",
		Status: workspace.StatusOpen, Image: "bookworm",
	})
	harness.manager.inspectReport = &workspace.CloseReport{
		Dirty:    " M secret-token.go",
		Unpushed: "abc123 local commit",
	}

	previewRec := harness.request(
		http.MethodPost,
		"/api/v1/desk/workspaces/ws-1/close-preview",
		`{}`,
		"preview-dirty",
		true,
	)
	if previewRec.Code != http.StatusOK {
		t.Fatalf("preview status = %d: %s", previewRec.Code, previewRec.Body.String())
	}
	var preview WorkspaceClosePreview
	if err := json.Unmarshal(previewRec.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if harness.manager.inspectCalls != 1 || harness.manager.closeCalls != 0 {
		t.Fatalf("preview calls inspect=%d close=%d", harness.manager.inspectCalls, harness.manager.closeCalls)
	}
	if preview.PreviewToken == "" || preview.ExpiresInSeconds != 60 || preview.Eligible {
		t.Fatalf("preview metadata = %+v", preview)
	}
	if preview.Dirty != "M secret-token.go" || preview.Unpushed != "abc123 local commit" {
		t.Fatalf("preview evidence = %+v", preview)
	}

	wrongResource := harness.request(
		http.MethodPost,
		"/api/v1/desk/workspaces/ws-2/close",
		`{"preview_token":"`+preview.PreviewToken+`"}`,
		"confirm-wrong-resource",
		true,
	)
	if wrongResource.Code != http.StatusConflict {
		t.Fatalf("wrong-resource status = %d, want 409: %s", wrongResource.Code, wrongResource.Body.String())
	}
	if harness.manager.closeCalls != 0 {
		t.Fatal("mismatched preview token reached Close")
	}
	replay := harness.request(
		http.MethodPost,
		"/api/v1/desk/workspaces/ws-1/close",
		`{"preview_token":"`+preview.PreviewToken+`"}`,
		"confirm-burned-token",
		true,
	)
	if replay.Code != http.StatusConflict || harness.manager.closeCalls != 0 {
		t.Fatalf("burned token status=%d close calls=%d", replay.Code, harness.manager.closeCalls)
	}

	expiring := issueWorkspacePreview(t, harness, "ws-1", "preview-expiring")
	harness.now = harness.now.Add(60 * time.Second)
	expired := harness.request(
		http.MethodPost,
		"/api/v1/desk/workspaces/ws-1/close",
		`{"preview_token":"`+expiring.PreviewToken+`"}`,
		"confirm-expired",
		true,
	)
	if expired.Code != http.StatusConflict || harness.manager.closeCalls != 0 {
		t.Fatalf("exact-expiry status=%d close calls=%d", expired.Code, harness.manager.closeCalls)
	}

	wrongOperationToken := harness.previews.Issue("memory-forget", "ws-1", time.Minute)
	wrongOperation := harness.request(
		http.MethodPost,
		"/api/v1/desk/workspaces/ws-1/close",
		`{"preview_token":"`+wrongOperationToken+`"}`,
		"confirm-wrong-operation",
		true,
	)
	if wrongOperation.Code != http.StatusConflict || harness.manager.closeCalls != 0 {
		t.Fatalf("wrong-operation status=%d close calls=%d", wrongOperation.Code, harness.manager.closeCalls)
	}
}

func TestWorkspaceCloseNeverForcesAndReinspectionCanRefuse(t *testing.T) {
	harness := newWorkspaceRouteHarness(t)
	harness.manager.addWorkspace(workspace.Workspace{
		ID: "ws-1", Repo: "matt-riley/waffle", SessionID: "session-workspace",
		Status: workspace.StatusOpen, Image: "bookworm",
	})
	harness.manager.inspectReport = &workspace.CloseReport{}
	preview := issueWorkspacePreview(t, harness, "ws-1", "preview-clean")
	harness.manager.closeReport = &workspace.CloseReport{Dirty: " M changed-after-preview.go"}
	harness.manager.closeErr = errors.New("workspace has unsaved work")

	refused := harness.request(
		http.MethodPost,
		"/api/v1/desk/workspaces/ws-1/close?force=true",
		`{"preview_token":"`+preview.PreviewToken+`"}`,
		"confirm-refused",
		true,
	)
	if refused.Code != http.StatusConflict {
		t.Fatalf("refused close status = %d: %s", refused.Code, refused.Body.String())
	}
	var conflict WorkspaceCloseConflict
	if err := json.Unmarshal(refused.Body.Bytes(), &conflict); err != nil {
		t.Fatal(err)
	}
	if conflict.Dirty != "M changed-after-preview.go" {
		t.Fatalf("close conflict = %+v", conflict)
	}
	if harness.manager.closeCalls != 1 || harness.manager.lastForce {
		t.Fatalf("close calls=%d force=%t", harness.manager.closeCalls, harness.manager.lastForce)
	}
	if harness.events.Cursor() != 0 {
		t.Fatal("refused close published an event")
	}

	unknownForce := issueWorkspacePreview(t, harness, "ws-1", "preview-unknown-force")
	badBody := harness.request(
		http.MethodPost,
		"/api/v1/desk/workspaces/ws-1/close",
		`{"preview_token":"`+unknownForce.PreviewToken+`","force":true}`,
		"confirm-unknown-force",
		true,
	)
	if badBody.Code != http.StatusBadRequest {
		t.Fatalf("force body status = %d, want 400: %s", badBody.Code, badBody.Body.String())
	}
	if harness.manager.closeCalls != 1 {
		t.Fatal("unknown force body reached Close")
	}
}

func TestWorkspaceClosePublishesOnlyCanonicalSuccessfulDeletion(t *testing.T) {
	harness := newWorkspaceRouteHarness(t)
	harness.manager.addWorkspace(workspace.Workspace{
		ID: "ws-1", Repo: "matt-riley/waffle", SessionID: "session-workspace",
		Status: workspace.StatusOpen, Image: "bookworm", Profile: "reviewer",
	})
	harness.manager.inspectReport = &workspace.CloseReport{}
	harness.manager.closeReport = &workspace.CloseReport{}
	preview := issueWorkspacePreview(t, harness, "ws-1", "preview-success")

	first := harness.request(
		http.MethodPost,
		"/api/v1/desk/workspaces/ws-1/close",
		`{"preview_token":"`+preview.PreviewToken+`"}`,
		"confirm-success",
		true,
	)
	second := harness.request(
		http.MethodPost,
		"/api/v1/desk/workspaces/ws-1/close",
		`{"preview_token":"`+preview.PreviewToken+`"}`,
		"confirm-success",
		true,
	)
	if first.Code != http.StatusOK || second.Code != http.StatusOK || first.Body.String() != second.Body.String() {
		t.Fatalf("idempotent close = %d/%d %q/%q", first.Code, second.Code, first.Body.String(), second.Body.String())
	}
	if harness.manager.closeCalls != 1 || harness.manager.lastForce {
		t.Fatalf("close calls=%d force=%t", harness.manager.closeCalls, harness.manager.lastForce)
	}
	if harness.events.Cursor() != 1 {
		t.Fatalf("event cursor = %d, want one", harness.events.Cursor())
	}
	events := readWorkspaceEvents(t, harness.events, 0, 1)
	if events[0].Type != WorkspaceClosedEvent || events[0].ResourceID != "ws-1" {
		t.Fatalf("close event = %+v", events[0])
	}
	var response WorkspaceMutationResponse
	if err := json.Unmarshal(first.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Workspace.Status != workspace.StatusClosed || response.Workspace.ID != "ws-1" {
		t.Fatalf("close response = %+v", response)
	}

	replayedWithFreshKey := harness.request(
		http.MethodPost,
		"/api/v1/desk/workspaces/ws-1/close",
		`{"preview_token":"`+preview.PreviewToken+`"}`,
		"confirm-success-again",
		true,
	)
	if replayedWithFreshKey.Code != http.StatusConflict || harness.manager.closeCalls != 1 {
		t.Fatalf("spent preview status=%d close calls=%d", replayedWithFreshKey.Code, harness.manager.closeCalls)
	}
}

func TestWorkspacesServiceRejectsClosedRowBeforePreviewOrConfirm(t *testing.T) {
	harness := newWorkspaceRouteHarness(t)
	harness.manager.addWorkspace(workspace.Workspace{
		ID: "ws-closed", Repo: "matt-riley/waffle", SessionID: "session-workspace",
		Status: workspace.StatusClosed, Image: "bookworm",
	})
	service := NewWorkspacesService(&Operations{
		Workspaces: harness.manager,
		Sessions:   harness.sessions,
		Previews:   harness.previews,
		Events:     harness.events,
	}, "none")

	if _, err := service.PreviewClose(context.Background(), "ws-closed"); !errors.Is(err, ErrWorkspaceStateConflict) {
		t.Fatalf("PreviewClose(closed) error = %v, want ErrWorkspaceStateConflict", err)
	}
	token := harness.previews.Issue(workspaceCloseOperation, "ws-closed", time.Minute)
	if _, err := service.ConfirmClose(context.Background(), "ws-closed", token); !errors.Is(err, ErrWorkspaceStateConflict) {
		t.Fatalf("ConfirmClose(closed) error = %v, want ErrWorkspaceStateConflict", err)
	}
	if harness.manager.inspectCalls != 0 || harness.manager.closeCalls != 0 {
		t.Fatalf("closed row reached inspect=%d close=%d", harness.manager.inspectCalls, harness.manager.closeCalls)
	}
	if harness.events.Cursor() != 0 {
		t.Fatalf("closed row published %d events", harness.events.Cursor())
	}

	harness.manager.workspaces["ws-closed"] = workspace.Workspace{
		ID: "ws-closed", Repo: "matt-riley/waffle", SessionID: "session-workspace",
		Status: workspace.StatusOpen, Image: "bookworm",
	}
	if _, err := service.ConfirmClose(context.Background(), "ws-closed", token); err != nil {
		t.Fatalf("closed-state rejection consumed preview token: %v", err)
	}
	if harness.manager.closeCalls != 1 || harness.events.Cursor() != 1 {
		t.Fatalf("eligible transition close=%d events=%d", harness.manager.closeCalls, harness.events.Cursor())
	}
}

func TestWorkspaceClosedRowHTTPConflictIsIdempotentAndPublishesNothing(t *testing.T) {
	harness := newWorkspaceRouteHarness(t)
	harness.manager.addWorkspace(workspace.Workspace{
		ID: "ws-closed", Repo: "matt-riley/waffle", SessionID: "session-workspace",
		Status: workspace.StatusClosed, Image: "bookworm",
	})

	firstPreview := harness.request(
		http.MethodPost,
		"/api/v1/desk/workspaces/ws-closed/close-preview",
		`{}`,
		"closed-preview",
		true,
	)
	replayedPreview := harness.request(
		http.MethodPost,
		"/api/v1/desk/workspaces/ws-closed/close-preview",
		`{}`,
		"closed-preview",
		true,
	)
	if firstPreview.Code != http.StatusConflict || replayedPreview.Code != http.StatusConflict {
		t.Fatalf("closed preview status = %d/%d: %s / %s",
			firstPreview.Code, replayedPreview.Code, firstPreview.Body.String(), replayedPreview.Body.String())
	}
	if firstPreview.Body.String() != replayedPreview.Body.String() {
		t.Fatalf("closed preview replay changed body: %q / %q", firstPreview.Body.String(), replayedPreview.Body.String())
	}
	assertWorkspaceError(t, firstPreview.Body.Bytes(), "workspace_state_conflict")

	token := harness.previews.Issue(workspaceCloseOperation, "ws-closed", time.Minute)
	firstConfirm := harness.request(
		http.MethodPost,
		"/api/v1/desk/workspaces/ws-closed/close",
		`{"preview_token":"`+token+`"}`,
		"closed-confirm",
		true,
	)
	replayedConfirm := harness.request(
		http.MethodPost,
		"/api/v1/desk/workspaces/ws-closed/close",
		`{"preview_token":"`+token+`"}`,
		"closed-confirm",
		true,
	)
	freshConfirm := harness.request(
		http.MethodPost,
		"/api/v1/desk/workspaces/ws-closed/close",
		`{"preview_token":"`+token+`"}`,
		"closed-confirm-fresh",
		true,
	)
	for _, rec := range []*httptest.ResponseRecorder{firstConfirm, replayedConfirm, freshConfirm} {
		if rec.Code != http.StatusConflict {
			t.Fatalf("closed confirm status = %d: %s", rec.Code, rec.Body.String())
		}
		assertWorkspaceError(t, rec.Body.Bytes(), "workspace_state_conflict")
	}
	if firstConfirm.Body.String() != replayedConfirm.Body.String() {
		t.Fatalf("closed confirm replay changed body: %q / %q", firstConfirm.Body.String(), replayedConfirm.Body.String())
	}
	if harness.manager.inspectCalls != 0 || harness.manager.closeCalls != 0 {
		t.Fatalf("closed HTTP row reached inspect=%d close=%d", harness.manager.inspectCalls, harness.manager.closeCalls)
	}
	if harness.events.Cursor() != 0 {
		t.Fatalf("closed HTTP row published %d events", harness.events.Cursor())
	}
}

func TestWorkspacesServicePublishesCloseOnlyAfterCanonicalTransition(t *testing.T) {
	harness := newWorkspaceRouteHarness(t)
	harness.manager.addWorkspace(workspace.Workspace{
		ID: "ws-1", Repo: "matt-riley/waffle", SessionID: "session-workspace",
		Status: workspace.StatusOpen, Image: "bookworm",
	})
	harness.manager.closeNoTransition = true
	token := harness.previews.Issue(workspaceCloseOperation, "ws-1", time.Minute)
	service := NewWorkspacesService(&Operations{
		Workspaces: harness.manager,
		Sessions:   harness.sessions,
		Previews:   harness.previews,
		Events:     harness.events,
	}, "none")

	if _, err := service.ConfirmClose(context.Background(), "ws-1", token); !errors.Is(err, ErrWorkspaceStateConflict) {
		t.Fatalf("ConfirmClose(no transition) error = %v, want ErrWorkspaceStateConflict", err)
	}
	if harness.manager.closeCalls != 1 {
		t.Fatalf("Close calls = %d, want one guarded attempt", harness.manager.closeCalls)
	}
	if harness.events.Cursor() != 0 {
		t.Fatalf("no-op close published %d events", harness.events.Cursor())
	}
}

func issueWorkspacePreview(t *testing.T, harness *workspaceRouteHarness, id, key string) WorkspaceClosePreview {
	t.Helper()
	rec := harness.request(
		http.MethodPost,
		"/api/v1/desk/workspaces/"+id+"/close-preview",
		`{}`,
		key,
		true,
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("preview %s status = %d: %s", id, rec.Code, rec.Body.String())
	}
	var preview WorkspaceClosePreview
	if err := json.Unmarshal(rec.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	return preview
}

func assertWorkspaceError(t *testing.T, body []byte, code string) {
	t.Helper()
	var response struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("decode error response %q: %v", body, err)
	}
	if response.Code != code || response.Message == "" {
		t.Fatalf("error = %+v, want code %q", response, code)
	}
}

func assertSandboxClientClosed(t *testing.T, client *sandbox.Client) {
	t.Helper()
	if client == nil {
		t.Fatal("manager did not return a sandbox client")
	}
	_, _, err := client.Exec(context.Background(), "bash", json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("sandbox client remained usable after handler cleanup: %v", err)
	}
}

func readWorkspaceEvents(t *testing.T, hub *EventHub, after uint64, count int) []Event {
	t.Helper()
	subscription, resync := hub.Subscribe(after)
	if resync {
		t.Fatal("unexpected event resync")
	}
	defer hub.Unsubscribe(subscription)
	events := make([]Event, 0, count)
	for range count {
		select {
		case event := <-subscription:
			events = append(events, event)
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for event %d", len(events))
		}
	}
	return events
}

type workspaceRouteHarness struct {
	t        *testing.T
	now      time.Time
	manager  *recordingWorkspaceManager
	sessions *recordingWorkspaceSessions
	security *Security
	events   *EventHub
	previews *PreviewStore
	handler  http.Handler
}

func newWorkspaceRouteHarness(t *testing.T) *workspaceRouteHarness {
	t.Helper()
	now := time.Unix(10_000, 0).UTC()
	manager := &recordingWorkspaceManager{
		t:          t,
		workspaces: make(map[string]workspace.Workspace),
	}
	sessions := &recordingWorkspaceSessions{
		sessions: map[string]session.Session{
			"session-workspace": {ID: "session-workspace"},
			"session-other":     {ID: "session-other"},
		},
	}
	security := mustSecurity(t, "127.0.0.1:8422")
	events := NewEventHub(32)
	harness := &workspaceRouteHarness{
		t: t, now: now, manager: manager, sessions: sessions,
		security: security, events: events,
	}
	previews := NewPreviewStore(func() time.Time { return harness.now }, previewEntropy(32))
	harness.previews = previews
	operations := &Operations{
		Workspaces: manager,
		Sessions:   sessions,
		Previews:   previews,
		Events:     events,
		Now:        func() time.Time { return harness.now },
	}
	mux := http.NewServeMux()
	RegisterWorkspaceRoutes(mux, WorkspaceRouteConfig{
		Operations:  operations,
		Security:    security,
		Idempotency: NewIdempotencyStore(nil, 64, time.Minute),
		Events:      events,
		Egress:      "none",
	})
	harness.handler = mux
	return harness
}

func (h *workspaceRouteHarness) request(method, path, body, key string, token bool) *httptest.ResponseRecorder {
	h.t.Helper()
	req := httptest.NewRequest(method, "http://127.0.0.1:8422"+path, strings.NewReader(body))
	req.Host = "127.0.0.1:8422"
	if token {
		req.Header.Set("X-Waffle-Desk-Token", h.security.Token())
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	req.Header.Set("X-Waffle-Force", "true")
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

type recordingWorkspaceManager struct {
	t                 *testing.T
	workspaces        map[string]workspace.Workspace
	openCalls         int
	openRepo          string
	openProfile       string
	openClient        *sandbox.Client
	resumeClient      *sandbox.Client
	inspectReport     *workspace.CloseReport
	inspectErr        error
	inspectCalls      int
	closeReport       *workspace.CloseReport
	closeErr          error
	closeCalls        int
	lastForce         bool
	closeNoTransition bool
}

func (m *recordingWorkspaceManager) addWorkspace(ws workspace.Workspace) {
	m.workspaces[ws.ID] = ws
}

func (m *recordingWorkspaceManager) List(context.Context) ([]workspace.Workspace, error) {
	out := make([]workspace.Workspace, 0, len(m.workspaces))
	for _, ws := range m.workspaces {
		out = append(out, ws)
	}
	return out, nil
}

func (m *recordingWorkspaceManager) Get(_ context.Context, id string) (*workspace.Workspace, error) {
	ws, ok := m.workspaces[id]
	if !ok {
		return nil, workspace.ErrWorkspaceNotFound
	}
	copy := ws
	return &copy, nil
}

func (m *recordingWorkspaceManager) OpenWithProfile(_ context.Context, repo, profile string) (*workspace.Workspace, *sandbox.Client, error) {
	m.openCalls++
	m.openRepo = repo
	m.openProfile = profile
	client, err := sandbox.NewClient(m.t.TempDir())
	if err != nil {
		return nil, nil, err
	}
	m.openClient = client
	ws := workspace.Workspace{
		ID: "ws-1", Repo: repo, SessionID: "session-workspace", Status: workspace.StatusOpen,
		Image: "bookworm", Container: "container-secret", Profile: profile,
	}
	m.addWorkspace(ws)
	copy := ws
	return &copy, client, nil
}

func (m *recordingWorkspaceManager) Idle(_ context.Context, id string) error {
	ws, ok := m.workspaces[id]
	if !ok {
		return workspace.ErrWorkspaceNotFound
	}
	if ws.Status != workspace.StatusOpen {
		return errors.New("workspace is not open")
	}
	ws.Status = workspace.StatusIdle
	m.workspaces[id] = ws
	return nil
}

func (m *recordingWorkspaceManager) Resume(_ context.Context, id string) (*workspace.Workspace, *sandbox.Client, error) {
	ws, ok := m.workspaces[id]
	if !ok {
		return nil, nil, workspace.ErrWorkspaceNotFound
	}
	client, err := sandbox.NewClient(m.t.TempDir())
	if err != nil {
		return nil, nil, err
	}
	m.resumeClient = client
	ws.Status = workspace.StatusOpen
	m.workspaces[id] = ws
	copy := ws
	return &copy, client, nil
}

func (m *recordingWorkspaceManager) InspectClose(context.Context, string) (*workspace.CloseReport, error) {
	m.inspectCalls++
	if m.inspectReport == nil {
		return &workspace.CloseReport{}, m.inspectErr
	}
	copy := *m.inspectReport
	return &copy, m.inspectErr
}

func (m *recordingWorkspaceManager) Close(_ context.Context, id string, force bool) (*workspace.CloseReport, error) {
	m.closeCalls++
	m.lastForce = force
	report := &workspace.CloseReport{}
	if m.closeReport != nil {
		copy := *m.closeReport
		report = &copy
	}
	if m.closeErr != nil {
		return report, m.closeErr
	}
	if m.closeNoTransition {
		return report, nil
	}
	ws, ok := m.workspaces[id]
	if !ok {
		return report, workspace.ErrWorkspaceNotFound
	}
	ws.Status = workspace.StatusClosed
	m.workspaces[id] = ws
	return report, nil
}

type recordingWorkspaceSessions struct {
	sessions map[string]session.Session
	gotID    string
}

func (s *recordingWorkspaceSessions) Get(_ context.Context, id string) (*session.Session, error) {
	s.gotID = id
	value, ok := s.sessions[id]
	if !ok {
		return nil, session.ErrNotFound
	}
	copy := value
	return &copy, nil
}

func (*recordingWorkspaceSessions) Search(context.Context, string, int) ([]session.Hit, error) {
	return nil, nil
}

func (*recordingWorkspaceSessions) SearchSummaries(context.Context, string, int) ([]session.Hit, error) {
	return nil, nil
}
