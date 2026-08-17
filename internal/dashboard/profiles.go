package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/providerconfig"
	"github.com/matt-riley/waffle/internal/workspace"
)

// The profile editor is a browser UI over a trust boundary, so it is
// deliberately constrained (#194):
//
//   - Structured only. Every field is typed; no raw TOML is accepted or
//     returned for editing.
//   - Narrowing enforced server-side by config.ValidateProfileNarrows, which
//     validates the resolved outcome of the same ApplyProfilePolicy the chat
//     runtime enforces — not a second implementation living in this handler.
//   - Preview then commit, with the same short-lived token discipline as the
//     workspace close flow.
//   - Restart-deferred and audited: mutations run through the shared Desk
//     mutation wrapper (one policy_audit row each) and commit for restart.
//   - Deletion refuses while anything still references the profile.
//
// Agent groups are intentionally not editable here. They are the outer bound a
// profile narrows within, and editing them would remove the fixed point the
// narrowing check is measured against.

const (
	profileMutationMaxBodyBytes = 64 << 10
	profilePreviewTTL           = 120 * time.Second
	profileSaveOperation        = "profile-save"
	profileDeleteOperation      = "profile-delete"

	ProfileSavedEvent   = "profile.saved"
	ProfileDeletedEvent = "profile.deleted"
)

var (
	// ErrProfileEditorUnavailable is returned when no config manager is wired.
	ErrProfileEditorUnavailable = errors.New("profile editor unavailable")
	// ErrProfileInvalid marks a request that failed field validation.
	ErrProfileInvalid = errors.New("profile request is invalid")
)

// ProfileFieldsView is the editable shape of one profile. It is the only
// profile representation the browser ever sees: there is no TOML in it.
type ProfileFieldsView struct {
	Name            string   `json:"name"`
	System          string   `json:"system"`
	Model           string   `json:"model"`
	Sandbox         string   `json:"sandbox"`
	Allow           []string `json:"allow"`
	Deny            []string `json:"deny"`
	DenyPrefixes    []string `json:"deny_prefixes"`
	Guidance        string   `json:"guidance"`
	MaxTokens       int      `json:"max_tokens"`
	MaxIterations   int      `json:"max_iterations"`
	AllowedChildren []string `json:"allowed_children"`
	// FileRoots confines the profile's file tools (#269).
	FileRoots []string `json:"file_roots"`
}

type ProfileListView struct {
	Profiles []ProfileFieldsView `json:"profiles"`
	// Groups names the agent groups a profile can be measured against, so the
	// editor can explain which ceiling applies without inventing one.
	Groups []string `json:"groups"`
}

// ProfilePreview is the before/after posture of a pending edit plus the token
// that authorises committing exactly it (#194 AC3).
type ProfilePreview struct {
	Profile          string      `json:"profile"`
	Before           PostureView `json:"before"`
	After            PostureView `json:"after"`
	Exists           bool        `json:"exists"`
	PreviewToken     string      `json:"preview_token"`
	ExpiresInSeconds int         `json:"expires_in_seconds"`
}

// ProfileDeletePreview reports whether a profile can be deleted and what still
// references it (#194 AC4).
type ProfileDeletePreview struct {
	Profile          string      `json:"profile"`
	Posture          PostureView `json:"posture"`
	Eligible         bool        `json:"eligible"`
	References       []string    `json:"references"`
	PreviewToken     string      `json:"preview_token,omitempty"`
	ExpiresInSeconds int         `json:"expires_in_seconds,omitempty"`
}

// ProfileMutationResponse reports a committed change and whether the running
// process must restart before it takes effect.
type ProfileMutationResponse struct {
	Profile         string `json:"profile"`
	RestartRequired bool   `json:"restart_required"`
}

// ProfileWideningResponse names the field that widened, so the editor can
// point at it rather than showing a generic refusal (#194 AC2).
type ProfileWideningResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Field   string `json:"field"`
}

// ProfileConfigStore is the transactional config seam. It is satisfied by
// *providerconfig.Manager, which owns the lock, staging, and journal.
type ProfileConfigStore interface {
	PutProfile(context.Context, providerconfig.ProfileRequest, providerconfig.CommitMode) (providerconfig.MutationResult, error)
	RemoveProfile(context.Context, string, []string, providerconfig.CommitMode) (providerconfig.MutationResult, error)
}

// ProfileReferenceSource reports runtime references that live outside
// config.toml — scheduled jobs and open workspaces bound to a profile.
type ProfileReferenceSource interface {
	ProfileReferences(ctx context.Context, name string) ([]string, error)
}

// operationsProfileReferences finds the runtime bindings that config.toml
// cannot see: scheduled jobs and workspaces pinned to a profile.
type operationsProfileReferences struct{ operations *Operations }

// NewOperationsProfileReferences adapts the shared operations dependencies to
// the delete guard. A nil Operations yields no references.
func NewOperationsProfileReferences(operations *Operations) ProfileReferenceSource {
	if operations == nil {
		return nil
	}
	return operationsProfileReferences{operations: operations}
}

func (s operationsProfileReferences) ProfileReferences(ctx context.Context, name string) ([]string, error) {
	var refs []string
	if s.operations.Jobs != nil {
		jobs, err := s.operations.Jobs.List(ctx)
		if err != nil {
			return nil, err
		}
		for _, job := range jobs {
			if strings.TrimSpace(job.Profile) == name {
				refs = append(refs, "scheduled job "+job.Name)
			}
		}
	}
	if s.operations.Workspaces != nil {
		spaces, err := s.operations.Workspaces.List(ctx)
		if err != nil {
			return nil, err
		}
		for _, space := range spaces {
			// A closed workspace is history, not a live binding.
			if space.Status == workspace.StatusClosed {
				continue
			}
			if strings.TrimSpace(space.Profile) == name {
				refs = append(refs, "workspace "+space.Repo)
			}
		}
	}
	return refs, nil
}

// ProfileEditor owns the guarded profile lifecycle.
type ProfileEditor struct {
	store      ProfileConfigStore
	posture    *PostureService
	previews   *PreviewStore
	references ProfileReferenceSource
	events     *EventHub
}

func NewProfileEditor(
	store ProfileConfigStore,
	posture *PostureService,
	previews *PreviewStore,
	references ProfileReferenceSource,
	events *EventHub,
) *ProfileEditor {
	return &ProfileEditor{
		store: store, posture: posture, previews: previews,
		references: references, events: events,
	}
}

func (e *ProfileEditor) ready() bool {
	return e != nil && e.store != nil && e.posture != nil && e.previews != nil
}

// List returns every configured profile in its editable form.
func (e *ProfileEditor) List() (ProfileListView, error) {
	if e == nil || e.posture == nil {
		return ProfileListView{}, ErrProfileEditorUnavailable
	}
	cfg := e.posture.Config()
	view := ProfileListView{
		Profiles: make([]ProfileFieldsView, 0, len(cfg.Agent.Profiles)),
		Groups:   make([]string, 0, len(cfg.Agent.Groups)),
	}
	for _, name := range providerconfig.SortedKeys(cfg.Agent.Profiles) {
		view.Profiles = append(view.Profiles, e.fields(name, cfg.Agent.Profiles[name]))
	}
	// SortedKeys already orders these, and sanitizing cannot reorder them:
	// it only rewrites secret-shaped substrings.
	for _, name := range providerconfig.SortedKeys(cfg.Agent.Groups) {
		view.Groups = append(view.Groups, sanitizeDashboardString(name))
	}
	return view, nil
}

// fields projects a stored profile back into editable form. The system prompt
// is returned as its *configured* value (which may be an @path), not the
// resolved body: editing must round-trip what config.toml actually holds.
func (e *ProfileEditor) fields(name string, profile config.AgentProfile) ProfileFieldsView {
	redact := func(value string) string {
		if e.posture != nil {
			return e.posture.redact(value)
		}
		return sanitizeDashboardString(value)
	}
	redactAll := func(values []string) []string {
		out := make([]string, 0, len(values))
		for _, value := range values {
			out = append(out, redact(value))
		}
		return out
	}
	return ProfileFieldsView{
		Name:            redact(name),
		System:          redact(profile.System),
		Model:           redact(profile.Model),
		Sandbox:         redact(profile.Sandbox),
		Allow:           redactAll(profile.Tools.Allow),
		Deny:            redactAll(profile.Tools.Deny),
		DenyPrefixes:    redactAll(profile.DenyPrefixes),
		Guidance:        redact(profile.Guidance),
		MaxTokens:       profile.MaxTokens,
		MaxIterations:   profile.MaxIterations,
		AllowedChildren: redactAll(profile.AllowedChildren),
		FileRoots:       redactAll(profile.Tools.FileRoots),
	}
}

// Preview resolves the before/after posture of a candidate edit and, only when
// it narrows, issues the token that authorises committing it.
func (e *ProfileEditor) Preview(request providerconfig.ProfileRequest) (ProfilePreview, error) {
	if !e.ready() {
		return ProfilePreview{}, ErrProfileEditorUnavailable
	}
	name := strings.TrimSpace(request.Name)
	if !config.ValidProfileName(name) {
		return ProfilePreview{}, ErrProfileInvalid
	}
	candidate := request.AgentProfile()

	cfg := e.posture.Config()
	group := providerconfig.ProfileGroup(*cfg, name)
	// The refusal happens here, before a token exists, so a widening edit can
	// never be confirmed (#194 AC2).
	if err := config.ValidateProfileNarrows(cfg.AgentPolicy(group), candidate); err != nil {
		return ProfilePreview{}, err
	}

	_, exists := cfg.Agent.Profiles[name]
	return ProfilePreview{
		Profile:          sanitizeDashboardString(name),
		Before:           e.posture.Read(name),
		After:            e.posture.ReadCandidate(name, candidate),
		Exists:           exists,
		PreviewToken:     e.previews.Issue(profileSaveOperation, profileTokenKey(name, candidate), profilePreviewTTL),
		ExpiresInSeconds: int(profilePreviewTTL / time.Second),
	}, nil
}

// Save commits a previously previewed edit. The token is bound to the exact
// candidate, so an edit cannot be swapped between preview and confirm.
func (e *ProfileEditor) Save(ctx context.Context, request providerconfig.ProfileRequest, token string) (ProfileMutationResponse, error) {
	if !e.ready() {
		return ProfileMutationResponse{}, ErrProfileEditorUnavailable
	}
	name := strings.TrimSpace(request.Name)
	if !config.ValidProfileName(name) {
		return ProfileMutationResponse{}, ErrProfileInvalid
	}
	candidate := request.AgentProfile()
	if err := e.previews.Consume(token, profileSaveOperation, profileTokenKey(name, candidate)); err != nil {
		return ProfileMutationResponse{}, err
	}
	// The manager re-validates narrowing under its own lock, against the
	// config it is about to write. This check is not the boundary; it is a
	// fast refusal before taking the lock.
	cfg := e.posture.Config()
	if err := config.ValidateProfileNarrows(
		cfg.AgentPolicy(providerconfig.ProfileGroup(*cfg, name)), candidate); err != nil {
		return ProfileMutationResponse{}, err
	}

	result, err := e.store.PutProfile(ctx, request, providerconfig.CommitForRestart)
	if err != nil {
		return ProfileMutationResponse{}, err
	}
	response := ProfileMutationResponse{
		Profile:         sanitizeDashboardString(name),
		RestartRequired: result.RestartRequired,
	}
	e.publish(ProfileSavedEvent, response)
	return response, nil
}

// PreviewDelete reports every reference that would block a delete.
func (e *ProfileEditor) PreviewDelete(ctx context.Context, name string) (ProfileDeletePreview, error) {
	if !e.ready() {
		return ProfileDeletePreview{}, ErrProfileEditorUnavailable
	}
	name = strings.TrimSpace(name)
	if !config.ValidProfileName(name) {
		return ProfileDeletePreview{}, ErrProfileInvalid
	}
	cfg := e.posture.Config()
	if _, exists := cfg.Agent.Profiles[name]; !exists {
		return ProfileDeletePreview{}, providerconfig.ErrProfileNotFound
	}
	refs, err := e.allReferences(ctx, cfg, name)
	if err != nil {
		return ProfileDeletePreview{}, err
	}

	preview := ProfileDeletePreview{
		Profile:    sanitizeDashboardString(name),
		Posture:    e.posture.Read(name),
		Eligible:   len(refs) == 0,
		References: refs,
	}
	if preview.Eligible {
		preview.PreviewToken = e.previews.Issue(profileDeleteOperation, name, profilePreviewTTL)
		preview.ExpiresInSeconds = int(profilePreviewTTL / time.Second)
	}
	return preview, nil
}

// Delete removes a previewed profile. References are re-read here and inside
// the manager, so a job created between preview and confirm still blocks it.
func (e *ProfileEditor) Delete(ctx context.Context, name, token string) (ProfileMutationResponse, error) {
	if !e.ready() {
		return ProfileMutationResponse{}, ErrProfileEditorUnavailable
	}
	name = strings.TrimSpace(name)
	if !config.ValidProfileName(name) {
		return ProfileMutationResponse{}, ErrProfileInvalid
	}
	if err := e.previews.Consume(token, profileDeleteOperation, name); err != nil {
		return ProfileMutationResponse{}, err
	}
	refs, err := e.allReferences(ctx, e.posture.Config(), name)
	if err != nil {
		return ProfileMutationResponse{}, err
	}
	result, err := e.store.RemoveProfile(ctx, name, refs, providerconfig.CommitForRestart)
	if err != nil {
		return ProfileMutationResponse{}, err
	}
	response := ProfileMutationResponse{
		Profile:         sanitizeDashboardString(name),
		RestartRequired: result.RestartRequired,
	}
	e.publish(ProfileDeletedEvent, response)
	return response, nil
}

// allReferences merges config-level references with runtime ones. Both are
// projected as stable labels, never as raw rows.
func (e *ProfileEditor) allReferences(ctx context.Context, cfg *config.Config, name string) ([]string, error) {
	refs := providerconfig.ProfileReferences(*cfg, name)
	if e.references != nil {
		runtime, err := e.references.ProfileReferences(ctx, name)
		if err != nil {
			return nil, err
		}
		refs = append(refs, runtime...)
	}
	out := make([]string, 0, len(refs))
	seen := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		clean := sanitizeDashboardString(strings.TrimSpace(ref))
		if clean == "" {
			continue
		}
		if _, duplicate := seen[clean]; duplicate {
			continue
		}
		seen[clean] = struct{}{}
		out = append(out, clean)
	}
	sort.Strings(out)
	return out, nil
}

// profileTokenKey binds a preview token to the exact candidate it previewed,
// so a confirmed save cannot carry different fields than the ones reviewed.
func profileTokenKey(name string, profile config.AgentProfile) string {
	encoded, err := json.Marshal(profile)
	if err != nil {
		return name
	}
	return name + ":" + string(encoded)
}

func (e *ProfileEditor) publish(eventType string, response ProfileMutationResponse) {
	if e == nil || e.events == nil {
		return
	}
	data, err := json.Marshal(response)
	if err != nil {
		return
	}
	e.events.Publish(Event{
		Type: eventType, Resource: "profile", ResourceID: response.Profile, Data: data,
	})
}

// ProfileRouteConfig is the additive integration seam for the profile editor.
type ProfileRouteConfig struct {
	Editor   *ProfileEditor
	Mutation func(limit int64, next http.Handler) http.Handler
}

// RegisterProfileRoutes mounts the structured profile editor. Every mutation
// goes through the caller's shared mutation wrapper, which is what supplies
// the policy_audit row and idempotency (#194 AC5).
func RegisterProfileRoutes(mux *http.ServeMux, routeConfig ProfileRouteConfig) {
	if mux == nil || routeConfig.Editor == nil {
		return
	}
	mux.Handle("GET /api/v1/desk/profiles", newProfileListHandler(routeConfig.Editor))
	if routeConfig.Mutation == nil {
		return
	}
	mutation := func(next http.Handler) http.Handler {
		return preserveResponseType(routeConfig.Mutation(profileMutationMaxBodyBytes, next))
	}
	mux.Handle("POST /api/v1/desk/profiles/preview", mutation(newProfilePreviewHandler(routeConfig.Editor)))
	mux.Handle("POST /api/v1/desk/profiles", mutation(newProfileSaveHandler(routeConfig.Editor)))
	mux.Handle("POST /api/v1/desk/profiles/{name}/delete-preview", mutation(newProfileDeletePreviewHandler(routeConfig.Editor)))
	mux.Handle("POST /api/v1/desk/profiles/{name}/delete", mutation(newProfileDeleteHandler(routeConfig.Editor)))
}

// profileRequestBody is the wire shape of the structured editor. Unknown
// fields are rejected, so a client cannot smuggle config keys the editor does
// not model.
type profileRequestBody struct {
	Name            string   `json:"name"`
	System          string   `json:"system"`
	Model           string   `json:"model"`
	Sandbox         string   `json:"sandbox"`
	Allow           []string `json:"allow"`
	Deny            []string `json:"deny"`
	DenyPrefixes    []string `json:"deny_prefixes"`
	Guidance        string   `json:"guidance"`
	MaxTokens       int      `json:"max_tokens"`
	MaxIterations   int      `json:"max_iterations"`
	AllowedChildren []string `json:"allowed_children"`
	FileRoots       []string `json:"file_roots"`
	PreviewToken    string   `json:"preview_token"`
}

func (b profileRequestBody) request() providerconfig.ProfileRequest {
	return providerconfig.ProfileRequest{
		Name: b.Name, System: b.System, Model: b.Model, Sandbox: b.Sandbox,
		Allow: b.Allow, Deny: b.Deny, DenyPrefixes: b.DenyPrefixes,
		Guidance: b.Guidance, MaxTokens: b.MaxTokens,
		MaxIterations: b.MaxIterations, AllowedChildren: b.AllowedChildren,
		FileRoots: b.FileRoots,
	}
}

func newProfileListHandler(editor *ProfileEditor) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		view, err := editor.List()
		if err != nil {
			writeProfileError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, view)
	})
}

func newProfilePreviewHandler(editor *ProfileEditor) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body profileRequestBody
		if !decodeProfileRequest(w, r, &body) {
			return
		}
		preview, err := editor.Preview(body.request())
		if err != nil {
			writeProfileError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, preview)
	})
}

func newProfileSaveHandler(editor *ProfileEditor) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body profileRequestBody
		if !decodeProfileRequest(w, r, &body) {
			return
		}
		if body.PreviewToken == "" {
			writeJSON(w, http.StatusBadRequest, errorResponse{
				Code: "invalid_request", Message: "a reviewed change is required before saving",
			})
			return
		}
		response, err := editor.Save(r.Context(), body.request(), body.PreviewToken)
		if err != nil {
			writeProfileError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, response)
	})
}

func newProfileDeletePreviewHandler(editor *ProfileEditor) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{}
		if !decodeProfileRequest(w, r, &body) {
			return
		}
		preview, err := editor.PreviewDelete(r.Context(), r.PathValue("name"))
		if err != nil {
			writeProfileError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, preview)
	})
}

func newProfileDeleteHandler(editor *ProfileEditor) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			PreviewToken string `json:"preview_token"`
		}
		if !decodeProfileRequest(w, r, &body) {
			return
		}
		if body.PreviewToken == "" {
			writeJSON(w, http.StatusBadRequest, errorResponse{
				Code: "invalid_request", Message: "a reviewed deletion is required before deleting",
			})
			return
		}
		response, err := editor.Delete(r.Context(), r.PathValue("name"), body.PreviewToken)
		if err != nil {
			writeProfileError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, response)
	})
}

func decodeProfileRequest(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		writeJSON(w, http.StatusBadRequest, errorResponse{
			Code: "invalid_request", Message: "profile request is invalid",
		})
		return false
	}
	return true
}

func writeProfileError(w http.ResponseWriter, err error) {
	// A widening refusal names its field so the editor can point at it.
	var widening *config.ProfileWideningError
	if errors.As(err, &widening) {
		writeJSON(w, http.StatusUnprocessableEntity, ProfileWideningResponse{
			Code:    "profile_widens_group",
			Message: "This change would widen the agent group's policy: " + widening.Detail + ".",
			Field:   widening.Field,
		})
		return
	}
	switch {
	case errors.Is(err, providerconfig.ErrReferenced):
		writeJSON(w, http.StatusConflict, errorResponse{
			Code: "profile_referenced", Message: "the profile is still in use",
		})
	case errors.Is(err, providerconfig.ErrProfileNotFound):
		writeJSON(w, http.StatusNotFound, errorResponse{
			Code: "profile_not_found", Message: "the profile was not found",
		})
	case errors.Is(err, ErrProfileInvalid):
		writeJSON(w, http.StatusUnprocessableEntity, errorResponse{
			Code: "invalid_profile", Message: "the profile fields are invalid",
		})
	case errors.Is(err, ErrPreviewExpired), errors.Is(err, ErrPreviewEvicted),
		errors.Is(err, ErrPreviewMismatch), errors.Is(err, ErrPreviewUnknown),
		errors.Is(err, ErrPreviewUsed):
		writeJSON(w, http.StatusConflict, errorResponse{
			Code: "preview_invalid", Message: "review the change again before saving",
		})
	case errors.Is(err, providerconfig.ErrLocked):
		writeJSON(w, http.StatusConflict, errorResponse{
			Code: "config_busy", Message: "another configuration change is in progress",
		})
	case errors.Is(err, ErrProfileEditorUnavailable):
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{
			Code: "profile_editor_unavailable", Message: "the profile editor is unavailable",
		})
	default:
		// Upstream text can carry paths and config bytes; it never ships.
		writeJSON(w, http.StatusUnprocessableEntity, errorResponse{
			Code: "profile_rejected", Message: "the profile change was refused",
		})
	}
}
