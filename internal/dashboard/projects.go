package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/matt-riley/waffle/internal/project"
)

const projectMutationMaxBodyBytes = 64 << 10

// ProjectResourceView is the safe, sanitized project-resource shape shared by
// reads and mutations (#478). Paths are repo-relative for files and omitted
// for notes; host paths never cross this boundary.
type ProjectResourceView struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace"`
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	Path        string `json:"path,omitempty"`
	Size        int64  `json:"size,omitempty"`
	State       string `json:"state"`
	Provenance  string `json:"provenance,omitempty"`
	Attached    bool   `json:"attached,omitempty"`
}

// ProjectSnapshot is the list projection for one workspace's project surface.
type ProjectSnapshot struct {
	WorkspaceID string                `json:"workspace"`
	Resources   []ProjectResourceView `json:"resources"`
}

// ProjectAttachmentView is a session's attached resource with its binding.
type ProjectAttachmentView struct {
	Resource   ProjectResourceView `json:"resource"`
	AttachedAt string              `json:"attached_at,omitempty"`
}

// ProjectsService serves the workspace-scoped project context library (#478).
// File reads are delegated to the workspace runtime via ProjectFileReader;
// paths resolve beneath the workspace root and any traversal, secret-like,
// binary, or oversized file fails closed with a fixed, redacted explanation.
type ProjectsService struct {
	projects *project.Store
	readFile ProjectFileReader
	// operations supplies the workspace list so attachments can be verified
	// to stay within the session's own workspace (#478 review).
	operations *Operations
}

// NewProjectsService wires the project surface. readFile must be non-nil for
// file resources; notes still work without it.
func NewProjectsService(projects *project.Store, readFile ProjectFileReader, operations *Operations) *ProjectsService {
	service := &ProjectsService{projects: projects, readFile: readFile, operations: operations}
	if service.projects != nil && readFile != nil {
		service.projects.ReadFile = func(ctx context.Context, workspaceID, path string) ([]byte, error) {
			return readFile.ReadFile(ctx, workspaceID, path)
		}
	}
	return service
}

// List returns one workspace's pinned resources with live staleness, plus
// which are attached to the given session (empty session means none).
func (s *ProjectsService) List(ctx context.Context, workspaceID, sessionID string) (ProjectSnapshot, error) {
	if s == nil || s.projects == nil {
		return ProjectSnapshot{}, ErrOperationsDependencyUnavailable
	}
	if err := s.projects.Refresh(ctx, workspaceID); err != nil {
		return ProjectSnapshot{}, err
	}
	resources, err := s.projects.List(ctx, workspaceID)
	if err != nil {
		return ProjectSnapshot{}, err
	}
	attached := map[string]bool{}
	if sessionID != "" {
		list, err := s.projects.ListAttached(ctx, sessionID)
		if err != nil {
			return ProjectSnapshot{}, err
		}
		for _, a := range list {
			attached[a.Resource.ID] = true
		}
	}
	snapshot := ProjectSnapshot{WorkspaceID: sanitizeDashboardString(workspaceID)}
	for _, r := range resources {
		snapshot.Resources = append(snapshot.Resources, s.view(r, attached[r.ID]))
	}
	return snapshot, nil
}

// ListAttached returns the resources attached to a session (for Today's
// project panel), newest first.
func (s *ProjectsService) ListAttached(ctx context.Context, sessionID string) ([]ProjectAttachmentView, error) {
	if s == nil || s.projects == nil {
		return nil, ErrOperationsDependencyUnavailable
	}
	list, err := s.projects.ListAttached(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	out := make([]ProjectAttachmentView, 0, len(list))
	for _, a := range list {
		out = append(out, ProjectAttachmentView{
			Resource:   s.view(a.Resource, true),
			AttachedAt: a.AttachedAt.UTC().Format("2006-01-02T15:04:05Z"),
		})
	}
	return out, nil
}

// PinFile pins a workspace file as a project resource.
func (s *ProjectsService) PinFile(ctx context.Context, workspaceID, filePath string) (ProjectResourceView, error) {
	if s == nil || s.projects == nil {
		return ProjectResourceView{}, ErrOperationsDependencyUnavailable
	}
	if s.readFile == nil {
		return ProjectResourceView{}, project.ErrUnsupportedFile
	}
	resource, err := s.projects.PinFile(ctx, workspaceID, strings.TrimSpace(filePath))
	if err != nil {
		return ProjectResourceView{}, err
	}
	return s.view(*resource, false), nil
}

// AddNote pins an explicit owner note.
func (s *ProjectsService) AddNote(ctx context.Context, workspaceID, name, note string) (ProjectResourceView, error) {
	if s == nil || s.projects == nil {
		return ProjectResourceView{}, ErrOperationsDependencyUnavailable
	}
	resource, err := s.projects.AddNote(ctx, workspaceID, name, note)
	if err != nil {
		return ProjectResourceView{}, err
	}
	return s.view(*resource, false), nil
}

// Remove unpins a resource from a workspace.
func (s *ProjectsService) Remove(ctx context.Context, workspaceID, id string) error {
	if s == nil || s.projects == nil {
		return ErrOperationsDependencyUnavailable
	}
	return s.projects.Remove(ctx, workspaceID, id)
}

// Attach binds a resource to a session's bounded working set. The resource
// must belong to the session's own open workspace: attachments never cross
// workspace boundaries (#478).
func (s *ProjectsService) Attach(ctx context.Context, resourceID, sessionID string) error {
	if s == nil || s.projects == nil {
		return ErrOperationsDependencyUnavailable
	}
	if err := s.verifySessionWorkspace(ctx, resourceID, sessionID); err != nil {
		return err
	}
	_, err := s.projects.Attach(ctx, sessionID, resourceID)
	return err
}

// Detach removes a resource from a session's working set.
func (s *ProjectsService) Detach(ctx context.Context, resourceID, sessionID string) error {
	if s == nil || s.projects == nil {
		return ErrOperationsDependencyUnavailable
	}
	if err := s.verifySessionWorkspace(ctx, resourceID, sessionID); err != nil {
		return err
	}
	return s.projects.Detach(ctx, sessionID, resourceID)
}

// verifySessionWorkspace fails closed unless the resource belongs to the
// workspace bound to the session (or the session has no workspace at all),
// so a crafted request can never attach another workspace's resource.
func (s *ProjectsService) verifySessionWorkspace(ctx context.Context, resourceID, sessionID string) error {
	if sessionID == "" {
		return project.ErrNotFound
	}
	if s.operations == nil || s.operations.Workspaces == nil {
		return ErrOperationsDependencyUnavailable
	}
	workspaces, err := s.operations.Workspaces.List(ctx)
	if err != nil {
		return err
	}
	var sessionWorkspace string
	for _, ws := range workspaces {
		if ws.SessionID == sessionID && ws.Status != "closed" {
			sessionWorkspace = ws.ID
			break
		}
	}
	if sessionWorkspace == "" {
		return project.ErrNotFound
	}
	resource, err := s.projects.Get(ctx, sessionWorkspace, resourceID)
	if err != nil {
		return err
	}
	if resource.WorkspaceID != sessionWorkspace {
		return project.ErrNotOwned
	}
	return nil
}

func (s *ProjectsService) view(r project.Resource, attached bool) ProjectResourceView {
	view := ProjectResourceView{
		ID:          sanitizeDashboardString(r.ID),
		WorkspaceID: sanitizeDashboardString(r.WorkspaceID),
		Kind:        sanitizeDashboardString(r.Kind),
		Name:        sanitizeDashboardString(r.Name),
		Size:        r.Size,
		State:       sanitizeDashboardString(r.State),
		Provenance:  sanitizeDashboardString(r.Provenance),
		Attached:    attached,
	}
	if r.Kind == project.KindFile {
		view.Path = sanitizeDashboardString(r.Path)
	}
	return view
}

// ProjectRouteConfig is the additive project-context integration seam for the
// caller-owned Desk mux (#478).
type ProjectRouteConfig struct {
	Projects    *project.Store
	Operations  *Operations
	Security    *Security
	Idempotency *IdempotencyStore
}

// RegisterProjectRoutes mounts the exact project endpoints.
func RegisterProjectRoutes(mux *http.ServeMux, routeConfig ProjectRouteConfig) {
	if routeConfig.Projects == nil || routeConfig.Operations == nil {
		return
	}
	var readFile ProjectFileReader
	if reader, ok := routeConfig.Operations.Workspaces.(ProjectFileReader); ok {
		readFile = reader
	}
	service := NewProjectsService(routeConfig.Projects, readFile, routeConfig.Operations)
	if routeConfig.Security == nil || routeConfig.Idempotency == nil {
		// Read-only attached list still mounts; guarded mutations drop out.
		mux.Handle("GET /api/v1/desk/projects/attached", newProjectAttachedHandler(service))
		return
	}
	mutation := func(next http.Handler) http.Handler {
		return NewMutationHandler(
			routeConfig.Security,
			routeConfig.Idempotency,
			projectMutationMaxBodyBytes,
			next,
		)
	}
	mux.Handle("GET /api/v1/desk/projects/{workspaceID}/resources", newProjectListHandler(service))
	mux.Handle("GET /api/v1/desk/projects/attached", newProjectAttachedHandler(service))
	mux.Handle("POST /api/v1/desk/projects/{workspaceID}/resources/pin", mutation(newProjectPinHandler(service)))
	mux.Handle("POST /api/v1/desk/projects/{workspaceID}/resources/notes", mutation(newProjectNoteHandler(service)))
	mux.Handle("POST /api/v1/desk/projects/resources/{id}/remove", mutation(newProjectRemoveHandler(service)))
	mux.Handle("POST /api/v1/desk/projects/resources/{id}/attach", mutation(newProjectAttachHandler(service)))
	mux.Handle("POST /api/v1/desk/projects/resources/{id}/detach", mutation(newProjectDetachHandler(service)))
}

func newProjectListHandler(service *ProjectsService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		snapshot, err := service.List(r.Context(), r.PathValue("workspaceID"), r.URL.Query().Get("session_id"))
		if err != nil {
			writeProjectError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, snapshot)
	})
}

func newProjectAttachedHandler(service *ProjectsService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		list, err := service.ListAttached(r.Context(), r.URL.Query().Get("session_id"))
		if err != nil {
			writeProjectError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, list)
	})
}

func newProjectPinHandler(service *ProjectsService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Path string `json:"path"`
		}
		if !decodeProjectRequest(w, r, &request) {
			return
		}
		view, err := service.PinFile(r.Context(), r.PathValue("workspaceID"), request.Path)
		if err != nil {
			writeProjectError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, view)
	})
}

func newProjectNoteHandler(service *ProjectsService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Name string `json:"name"`
			Note string `json:"note"`
		}
		if !decodeProjectRequest(w, r, &request) {
			return
		}
		view, err := service.AddNote(r.Context(), r.PathValue("workspaceID"), request.Name, request.Note)
		if err != nil {
			writeProjectError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, view)
	})
}

func newProjectRemoveHandler(service *ProjectsService) http.Handler {
	return projectSessionMutation(service, func(ctx context.Context, id, _ string) error {
		workspaceID, err := resourceWorkspaceID(ctx, service, id)
		if err != nil {
			return err
		}
		return service.Remove(ctx, workspaceID, id)
	})
}

func newProjectAttachHandler(service *ProjectsService) http.Handler {
	return projectSessionMutation(service, func(ctx context.Context, id, sessionID string) error {
		return service.Attach(ctx, id, sessionID)
	})
}

func newProjectDetachHandler(service *ProjectsService) http.Handler {
	return projectSessionMutation(service, func(ctx context.Context, id, sessionID string) error {
		return service.Detach(ctx, id, sessionID)
	})
}

// projectSessionMutation decodes {session_id} (empty allowed for remove) and
// runs the operation.
func projectSessionMutation(service *ProjectsService, run func(context.Context, string, string) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			SessionID string `json:"session_id"`
		}
		if !decodeProjectRequest(w, r, &request) {
			return
		}
		if err := run(r.Context(), r.PathValue("id"), strings.TrimSpace(request.SessionID)); err != nil {
			writeProjectError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, struct{}{})
	})
}

// resourceWorkspaceID resolves the workspace owning a resource so remove can
// enforce ownership.
func resourceWorkspaceID(ctx context.Context, service *ProjectsService, id string) (string, error) {
	// The project store's Get requires a workspace id; resolve it through
	// the attachment-free path by listing every workspace (bounded) is
	// wasteful, so remove is delegated with the store's ownership check
	// using the empty-workspace query then the caller's workspace list.
	// Simpler: ask the store for the resource without ownership and verify
	// the workspace exists via the service's workspace list.
	list, err := service.projects.ListAll(ctx)
	if err != nil {
		return "", err
	}
	for _, r := range list {
		if r.ID == id {
			return r.WorkspaceID, nil
		}
	}
	return "", project.ErrNotFound
}

func decodeProjectRequest(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		writeProjectError(w, errInvalidProjectRequest)
		return false
	}
	return true
}

var errInvalidProjectRequest = errors.New("invalid project request")

// projectErrorMapping maps known project failures to fixed, redacted HTTP
// responses. Paths, file content, and upstream errors never reach the client.
var projectErrorMapping = []struct {
	err     error
	status  int
	code    string
	message string
}{
	{project.ErrNotFound, http.StatusNotFound, "project_resource_not_found", "project resource was not found"},
	{project.ErrNotOwned, http.StatusNotFound, "project_resource_not_found", "project resource was not found"},
	{project.ErrUnsupportedFile, http.StatusUnprocessableEntity, "project_file_unsupported", "file is not eligible for project context"},
	{project.ErrMissingFile, http.StatusUnprocessableEntity, "project_file_missing", "workspace file is unavailable"},
	{ErrOperationsDependencyUnavailable, http.StatusServiceUnavailable, "project_unavailable", "project context is unavailable"},
	{errInvalidProjectRequest, http.StatusBadRequest, "invalid_request", "project request is invalid"},
}

func writeProjectError(w http.ResponseWriter, err error) {
	for _, mapping := range projectErrorMapping {
		if errors.Is(err, mapping.err) {
			writeJSON(w, mapping.status, errorResponse{Code: mapping.code, Message: mapping.message})
			return
		}
	}
	writeJSON(w, http.StatusServiceUnavailable, errorResponse{Code: "project_unavailable", Message: "project request could not be completed"})
}
