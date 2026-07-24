package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/workspace"
)

const (
	workspaceMutationMaxBodyBytes = 64 << 10
	workspaceClosePreviewTTL      = 60 * time.Second
	workspaceCloseOperation       = "workspace-close"

	WorkspaceOpenedEvent   = "workspace.opened"
	WorkspaceSelectedEvent = "workspace.selected"
	WorkspaceIdledEvent    = "workspace.idled"
	WorkspaceResumedEvent  = "workspace.resumed"
	WorkspaceClosedEvent   = "workspace.closed"
)

var workspaceRepositoryPart = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

var ErrWorkspaceStateConflict = errors.New("workspace state conflict")

// WorkspaceView is the stable, sanitized workspace shape shared by reads,
// mutations, events, and the section client.
type WorkspaceView struct {
	ID         string `json:"id"`
	Repository string `json:"repository"`
	SessionID  string `json:"session"`
	Status     string `json:"status"`
	Profile    string `json:"profile,omitempty"`
	Image      string `json:"image"`
	Egress     string `json:"egress"`
}

type WorkspaceSnapshot struct {
	Workspaces []WorkspaceView `json:"workspaces"`
}

type WorkspaceMutationResponse struct {
	Workspace WorkspaceView `json:"workspace"`
	TodayURL  string        `json:"today_url,omitempty"`
}

type WorkspaceClosePreview struct {
	Workspace        WorkspaceView `json:"workspace"`
	PreviewToken     string        `json:"preview_token"`
	ExpiresInSeconds int           `json:"expires_in_seconds"`
	Eligible         bool          `json:"eligible"`
	Dirty            string        `json:"dirty,omitempty"`
	Unpushed         string        `json:"unpushed,omitempty"`
}

type WorkspaceCloseConflict struct {
	Code     string `json:"code"`
	Message  string `json:"message"`
	Dirty    string `json:"dirty,omitempty"`
	Unpushed string `json:"unpushed,omitempty"`
}

type workspaceCloseRefusal struct {
	report *workspace.CloseReport
	cause  error
}

func (e *workspaceCloseRefusal) Error() string {
	return "workspace close was refused"
}

func (e *workspaceCloseRefusal) Unwrap() error {
	return e.cause
}

// WorkspacesService owns the guarded workspace lifecycle and its canonical
// public shapes. Runtime clients are never retained by the dashboard.
type WorkspacesService struct {
	operations *Operations
	egress     string
}

func NewWorkspacesService(operations *Operations, egress string) *WorkspacesService {
	return &WorkspacesService{operations: operations, egress: resolveWorkspaceEgressLabel(egress)}
}

func (s *WorkspacesService) Read(ctx context.Context) (WorkspaceSnapshot, error) {
	if s == nil || s.operations == nil || s.operations.Workspaces == nil {
		return WorkspaceSnapshot{}, ErrOperationsDependencyUnavailable
	}
	loaded, err := s.operations.Workspaces.List(ctx)
	if err != nil {
		return WorkspaceSnapshot{}, err
	}
	snapshot := WorkspaceSnapshot{Workspaces: make([]WorkspaceView, 0, len(loaded))}
	for _, ws := range loaded {
		snapshot.Workspaces = append(snapshot.Workspaces, s.view(ws))
	}
	return snapshot, nil
}

func (s *WorkspacesService) Open(ctx context.Context, repository, profile string) (WorkspaceMutationResponse, error) {
	repository = strings.TrimSpace(repository)
	profile = strings.TrimSpace(profile)
	if !validWorkspaceRepository(repository) || (profile != "" && !config.ValidProfileName(profile)) {
		return WorkspaceMutationResponse{}, fmt.Errorf("invalid workspace input")
	}
	if s == nil || s.operations == nil || s.operations.Workspaces == nil {
		return WorkspaceMutationResponse{}, ErrOperationsDependencyUnavailable
	}
	ws, client, err := s.operations.Workspaces.OpenWithProfile(ctx, repository, profile)
	if client != nil {
		defer func() { _ = client.Close() }()
	}
	if err != nil {
		return WorkspaceMutationResponse{}, err
	}
	if ws == nil {
		return WorkspaceMutationResponse{}, ErrOperationsDependencyUnavailable
	}
	response := WorkspaceMutationResponse{Workspace: s.view(*ws)}
	s.publish(WorkspaceOpenedEvent, response.Workspace)
	return response, nil
}

func (s *WorkspacesService) Select(ctx context.Context, id string) (WorkspaceMutationResponse, error) {
	ws, err := s.get(ctx, id)
	if err != nil {
		return WorkspaceMutationResponse{}, err
	}
	if ws.Status == workspace.StatusClosed {
		return WorkspaceMutationResponse{}, ErrWorkspaceStateConflict
	}
	if s.operations.Sessions == nil {
		return WorkspaceMutationResponse{}, ErrOperationsDependencyUnavailable
	}
	persisted, err := s.operations.Sessions.Get(ctx, ws.SessionID)
	if err != nil {
		return WorkspaceMutationResponse{}, err
	}
	if persisted == nil || persisted.ID != ws.SessionID {
		return WorkspaceMutationResponse{}, session.ErrNotFound
	}
	response := WorkspaceMutationResponse{
		Workspace: s.view(*ws),
		TodayURL:  "/desk/?section=today&session_id=" + url.QueryEscape(ws.SessionID),
	}
	s.publish(WorkspaceSelectedEvent, response.Workspace)
	return response, nil
}

func (s *WorkspacesService) Idle(ctx context.Context, id string) (WorkspaceMutationResponse, error) {
	if s == nil || s.operations == nil || s.operations.Workspaces == nil {
		return WorkspaceMutationResponse{}, ErrOperationsDependencyUnavailable
	}
	if err := s.operations.Workspaces.Idle(ctx, id); err != nil {
		return WorkspaceMutationResponse{}, err
	}
	ws, err := s.get(ctx, id)
	if err != nil {
		return WorkspaceMutationResponse{}, err
	}
	response := WorkspaceMutationResponse{Workspace: s.view(*ws)}
	s.publish(WorkspaceIdledEvent, response.Workspace)
	return response, nil
}

func (s *WorkspacesService) Resume(ctx context.Context, id string) (WorkspaceMutationResponse, error) {
	if s == nil || s.operations == nil || s.operations.Workspaces == nil {
		return WorkspaceMutationResponse{}, ErrOperationsDependencyUnavailable
	}
	ws, client, err := s.operations.Workspaces.Resume(ctx, id)
	if client != nil {
		defer func() { _ = client.Close() }()
	}
	if err != nil {
		return WorkspaceMutationResponse{}, err
	}
	if ws == nil {
		return WorkspaceMutationResponse{}, ErrOperationsDependencyUnavailable
	}
	response := WorkspaceMutationResponse{Workspace: s.view(*ws)}
	s.publish(WorkspaceResumedEvent, response.Workspace)
	return response, nil
}

func (s *WorkspacesService) PreviewClose(ctx context.Context, id string) (WorkspaceClosePreview, error) {
	ws, err := s.get(ctx, id)
	if err != nil {
		return WorkspaceClosePreview{}, err
	}
	if ws.Status == workspace.StatusClosed {
		return WorkspaceClosePreview{}, ErrWorkspaceStateConflict
	}
	if s.operations.Previews == nil {
		return WorkspaceClosePreview{}, ErrOperationsDependencyUnavailable
	}
	report, err := s.operations.Workspaces.InspectClose(ctx, id)
	if err != nil {
		return WorkspaceClosePreview{}, err
	}
	if report == nil {
		report = &workspace.CloseReport{}
	}
	dirty, unpushed := workspaceCloseEvidence(report)
	return WorkspaceClosePreview{
		Workspace:        s.view(*ws),
		PreviewToken:     s.operations.Previews.Issue(workspaceCloseOperation, id, workspaceClosePreviewTTL),
		ExpiresInSeconds: int(workspaceClosePreviewTTL / time.Second),
		Eligible:         dirty == "" && unpushed == "",
		Dirty:            dirty,
		Unpushed:         unpushed,
	}, nil
}

func (s *WorkspacesService) ConfirmClose(ctx context.Context, id, previewToken string) (WorkspaceMutationResponse, error) {
	if s == nil || s.operations == nil || s.operations.Workspaces == nil || s.operations.Previews == nil {
		return WorkspaceMutationResponse{}, ErrOperationsDependencyUnavailable
	}
	ws, err := s.get(ctx, id)
	if err != nil {
		return WorkspaceMutationResponse{}, err
	}
	if ws.Status == workspace.StatusClosed {
		return WorkspaceMutationResponse{}, ErrWorkspaceStateConflict
	}
	if err := s.operations.Previews.Consume(previewToken, workspaceCloseOperation, id); err != nil {
		return WorkspaceMutationResponse{}, err
	}
	report, err := s.operations.Workspaces.Close(ctx, id, false)
	if err != nil {
		dirty, unpushed := workspaceCloseEvidence(report)
		if dirty != "" || unpushed != "" {
			return WorkspaceMutationResponse{}, &workspaceCloseRefusal{report: &workspace.CloseReport{
				Dirty: dirty, Unpushed: unpushed,
			}, cause: err}
		}
		return WorkspaceMutationResponse{}, err
	}
	closed, err := s.get(ctx, id)
	if err != nil {
		return WorkspaceMutationResponse{}, err
	}
	if closed.Status != workspace.StatusClosed {
		return WorkspaceMutationResponse{}, ErrWorkspaceStateConflict
	}
	response := WorkspaceMutationResponse{Workspace: s.view(*closed)}
	s.publish(WorkspaceClosedEvent, response.Workspace)
	return response, nil
}

func (s *WorkspacesService) get(ctx context.Context, id string) (*workspace.Workspace, error) {
	if s == nil || s.operations == nil || s.operations.Workspaces == nil {
		return nil, ErrOperationsDependencyUnavailable
	}
	return s.operations.Workspaces.Get(ctx, id)
}

func (s *WorkspacesService) view(ws workspace.Workspace) WorkspaceView {
	return WorkspaceView{
		ID:         sanitizeDashboardString(ws.ID),
		Repository: sanitizeDashboardString(ws.Repo),
		SessionID:  sanitizeDashboardString(ws.SessionID),
		Status:     sanitizeDashboardString(ws.Status),
		Profile:    sanitizeDashboardString(ws.Profile),
		Image:      sanitizeDashboardString(ws.Image),
		Egress:     s.egress,
	}
}

func (s *WorkspacesService) publish(eventType string, view WorkspaceView) {
	if s == nil || s.operations == nil {
		return
	}
	events := s.operations.Events
	if events == nil {
		return
	}
	data, err := json.Marshal(view)
	if err != nil {
		return
	}
	events.Publish(Event{
		Type: eventType, Resource: "workspace", ResourceID: view.ID, Data: data,
	})
}

func validWorkspaceRepository(repository string) bool {
	if len(repository) == 0 || len(repository) > 255 || strings.TrimSpace(repository) != repository {
		return false
	}
	parts := strings.Split(repository, "/")
	if len(parts) != 2 {
		return false
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || !workspaceRepositoryPart.MatchString(part) {
			return false
		}
	}
	return true
}

func resolveWorkspaceEgressLabel(egress string) string {
	switch strings.TrimSpace(egress) {
	case "full":
		return "Full network egress"
	case "allowlist":
		return "Allowlisted network egress"
	default:
		return "No network egress"
	}
}

func workspaceCloseEvidence(report *workspace.CloseReport) (string, string) {
	if report == nil {
		return "", ""
	}
	return sanitizeDashboardString(strings.TrimSpace(report.Dirty)),
		sanitizeDashboardString(strings.TrimSpace(report.Unpushed))
}

// WorkspaceRouteConfig is the additive workspace integration seam for the
// caller-owned Desk mux.
type WorkspaceRouteConfig struct {
	Operations  *Operations
	Security    *Security
	Idempotency *IdempotencyStore
	Events      *EventHub
	Egress      string
}

// RegisterWorkspaceRoutes mounts only the exact Workspaces endpoints. Shared
// router and serve composition remain caller-owned.
func RegisterWorkspaceRoutes(mux *http.ServeMux, routeConfig WorkspaceRouteConfig) {
	if routeConfig.Operations != nil && routeConfig.Events != nil {
		routeConfig.Operations.Events = routeConfig.Events
	}
	service := NewWorkspacesService(routeConfig.Operations, routeConfig.Egress)
	mux.Handle("GET /api/v1/desk/workspaces", newWorkspaceListHandler(service))
	if routeConfig.Security == nil || routeConfig.Idempotency == nil {
		return
	}
	mutation := func(next http.Handler) http.Handler {
		protected := NewMutationHandler(
			routeConfig.Security,
			routeConfig.Idempotency,
			workspaceMutationMaxBodyBytes,
			next,
		)
		return preserveWorkspaceResponseType(protected)
	}
	mux.Handle("POST /api/v1/desk/workspaces/open", mutation(newWorkspaceOpenHandler(service)))
	mux.Handle("POST /api/v1/desk/workspaces/{id}/select", mutation(newWorkspaceSelectHandler(service)))
	mux.Handle("POST /api/v1/desk/workspaces/{id}/idle", mutation(newWorkspaceIdleHandler(service)))
	mux.Handle("POST /api/v1/desk/workspaces/{id}/resume", mutation(newWorkspaceResumeHandler(service)))
	mux.Handle("POST /api/v1/desk/workspaces/{id}/close-preview", mutation(newWorkspaceClosePreviewHandler(service)))
	mux.Handle("POST /api/v1/desk/workspaces/{id}/close", mutation(newWorkspaceCloseHandler(service)))
}

func newWorkspaceListHandler(service *WorkspacesService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		snapshot, err := service.Read(r.Context())
		if err != nil {
			writeWorkspaceError(w, http.StatusServiceUnavailable, "workspaces_unavailable", "workspaces are temporarily unavailable")
			return
		}
		writeWorkspaceJSON(w, http.StatusOK, snapshot)
	})
}

func newWorkspaceOpenHandler(service *WorkspacesService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Repository string `json:"repository"`
			Profile    string `json:"profile"`
		}
		if !decodeWorkspaceRequest(w, r, &request) {
			return
		}
		if !validWorkspaceRepository(strings.TrimSpace(request.Repository)) ||
			(strings.TrimSpace(request.Profile) != "" && !config.ValidProfileName(strings.TrimSpace(request.Profile))) {
			writeWorkspaceError(w, http.StatusUnprocessableEntity, "invalid_workspace", "repository or profile is invalid")
			return
		}
		response, err := service.Open(r.Context(), request.Repository, request.Profile)
		if err != nil {
			writeWorkspaceServiceError(w, err, false)
			return
		}
		writeWorkspaceJSON(w, http.StatusCreated, response)
	})
}

func newWorkspaceSelectHandler(service *WorkspacesService) http.Handler {
	return workspaceEmptyMutationHandler(service, func(ctx context.Context, id string) (WorkspaceMutationResponse, error) {
		return service.Select(ctx, id)
	})
}

func newWorkspaceIdleHandler(service *WorkspacesService) http.Handler {
	return workspaceEmptyMutationHandler(service, func(ctx context.Context, id string) (WorkspaceMutationResponse, error) {
		return service.Idle(ctx, id)
	})
}

func newWorkspaceResumeHandler(service *WorkspacesService) http.Handler {
	return workspaceEmptyMutationHandler(service, func(ctx context.Context, id string) (WorkspaceMutationResponse, error) {
		return service.Resume(ctx, id)
	})
}

func workspaceEmptyMutationHandler(
	service *WorkspacesService,
	run func(context.Context, string) (WorkspaceMutationResponse, error),
) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct{}
		if !decodeWorkspaceRequest(w, r, &request) {
			return
		}
		response, err := run(r.Context(), r.PathValue("id"))
		if err != nil {
			writeWorkspaceServiceError(w, err, true)
			return
		}
		writeWorkspaceJSON(w, http.StatusOK, response)
	})
}

func newWorkspaceClosePreviewHandler(service *WorkspacesService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct{}
		if !decodeWorkspaceRequest(w, r, &request) {
			return
		}
		preview, err := service.PreviewClose(r.Context(), r.PathValue("id"))
		if err != nil {
			writeWorkspaceServiceError(w, err, false)
			return
		}
		writeWorkspaceJSON(w, http.StatusOK, preview)
	})
}

func newWorkspaceCloseHandler(service *WorkspacesService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			PreviewToken string `json:"preview_token"`
		}
		if !decodeWorkspaceRequest(w, r, &request) {
			return
		}
		if request.PreviewToken == "" {
			writeWorkspaceError(w, http.StatusBadRequest, "invalid_request", "workspace request is invalid")
			return
		}
		response, err := service.ConfirmClose(r.Context(), r.PathValue("id"), request.PreviewToken)
		if err != nil {
			var refusal *workspaceCloseRefusal
			if errors.As(err, &refusal) {
				dirty, unpushed := workspaceCloseEvidence(refusal.report)
				writeWorkspaceJSON(w, http.StatusConflict, WorkspaceCloseConflict{
					Code: "workspace_not_clean", Message: "workspace has unsaved or unpushed work",
					Dirty: dirty, Unpushed: unpushed,
				})
				return
			}
			writeWorkspaceServiceError(w, err, false)
			return
		}
		writeWorkspaceJSON(w, http.StatusOK, response)
	})
}

func decodeWorkspaceRequest(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		writeWorkspaceError(w, http.StatusBadRequest, "invalid_request", "workspace request is invalid")
		return false
	}
	return true
}

func writeWorkspaceServiceError(w http.ResponseWriter, err error, stateConflict bool) {
	switch {
	case errors.Is(err, workspace.ErrWorkspaceNotFound), errors.Is(err, session.ErrNotFound):
		writeWorkspaceError(w, http.StatusNotFound, "workspace_not_found", "workspace or session was not found")
	case errors.Is(err, ErrWorkspaceStateConflict):
		writeWorkspaceError(w, http.StatusConflict, "workspace_state_conflict", "workspace state does not allow that action")
	case errors.Is(err, ErrPreviewExpired),
		errors.Is(err, ErrPreviewEvicted),
		errors.Is(err, ErrPreviewMismatch),
		errors.Is(err, ErrPreviewUnknown),
		errors.Is(err, ErrPreviewUsed):
		writeWorkspaceError(w, http.StatusConflict, "preview_invalid", "workspace confirmation is invalid or expired")
	case stateConflict:
		writeWorkspaceError(w, http.StatusConflict, "workspace_state_conflict", "workspace state does not allow that action")
	default:
		writeWorkspaceError(w, http.StatusServiceUnavailable, "workspace_unavailable", "workspace request could not be completed")
	}
}

func writeWorkspaceError(w http.ResponseWriter, status int, code, message string) {
	writeWorkspaceJSON(w, status, struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{Code: code, Message: message})
}

func writeWorkspaceJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func preserveWorkspaceResponseType(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capture := newResponseCapture()
		next.ServeHTTP(capture, r)
		if json.Valid(capture.body.Bytes()) {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(capture.status)
		_, _ = w.Write(capture.body.Bytes())
	})
}
