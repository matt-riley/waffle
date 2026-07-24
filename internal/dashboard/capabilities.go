package dashboard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/modelcatalog"
	"github.com/matt-riley/waffle/internal/providerconfig"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/skillinstall"
)

const (
	CapabilityProviderMaxBodyBytes = 64 << 10
	capabilityMutationMaxBodyBytes = 16 << 10
	restartScheduleTimeout         = 5 * time.Second
)

var (
	ErrCapabilityModelNotFound  = errors.New("capability model not found")
	ErrCapabilitySkillNotFound  = errors.New("capability skill not found")
	ErrAfterResponseUnavailable = errors.New("after-response scheduling unavailable")
	ErrCapabilitiesUnavailable  = errors.New("capabilities dependency unavailable")
)

type CapabilityProviders interface {
	Snapshot(context.Context) (providerconfig.Listing, error)
	AddWithMode(context.Context, providerconfig.AddRequest, providerconfig.CommitMode) (providerconfig.MutationResult, error)
	AddModelWithMode(context.Context, providerconfig.AddModelRequest, providerconfig.CommitMode) (providerconfig.MutationResult, error)
	ActivateModelWithMode(context.Context, string, providerconfig.CommitMode) (providerconfig.MutationResult, error)
	ActivateUtilityModelWithMode(context.Context, string, providerconfig.CommitMode) (providerconfig.MutationResult, error)
}

type CapabilitySessions interface {
	Get(context.Context, string) (*session.Session, error)
	SetModelAlias(context.Context, string, string) error
}

type CapabilitySkills interface {
	List(context.Context, string) ([]CapabilitySkill, error)
	Attach(context.Context, string, string) error
	Detach(context.Context, string, string) error
	Stage(context.Context, skillinstall.StageRequest) (skillinstall.Manifest, error)
	Install(context.Context, string, string) (CapabilitySkill, error)
	Activate(context.Context, string) error
}

type CapabilityCatalogue interface {
	Refresh(context.Context, string) (CapabilityCatalogueResult, error)
}

// CapabilityCatalogueResult keeps the private values supplied to a catalogue
// fetch beside its untrusted result so the public boundary can redact them.
type CapabilityCatalogueResult struct {
	Result        modelcatalog.Result
	PrivateValues []string
}

type CapabilitySkill struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Active      bool   `json:"active"`
	Attached    bool   `json:"attached"`
	Missing     bool   `json:"missing,omitempty"`
}

type CapabilitySession struct {
	ID         string `json:"id"`
	ModelAlias string `json:"model_alias,omitempty"`
}

type CapabilitiesSnapshot struct {
	Providers providerconfig.Listing `json:"providers"`
	Session   *CapabilitySession     `json:"session,omitempty"`
	Skills    []CapabilitySkill      `json:"skills"`
}

type CapabilityCatalogueView struct {
	Connection string               `json:"connection"`
	FetchedAt  time.Time            `json:"fetched_at"`
	Stale      bool                 `json:"stale"`
	Warning    string               `json:"warning,omitempty"`
	Models     []modelcatalog.Model `json:"models"`
}

// Capabilities owns the narrow application operations used by the additive
// Desk routes. It never receives or emits raw secret-store state.
type Capabilities struct {
	Providers CapabilityProviders
	Sessions  CapabilitySessions
	Skills    CapabilitySkills
	Catalogue CapabilityCatalogue
}

func (c *Capabilities) Snapshot(ctx context.Context, sessionID string) (CapabilitiesSnapshot, error) {
	if c == nil || c.Providers == nil {
		return CapabilitiesSnapshot{}, ErrCapabilitiesUnavailable
	}
	providers, err := c.Providers.Snapshot(ctx)
	if err != nil {
		return CapabilitiesSnapshot{}, err
	}
	snapshot := CapabilitiesSnapshot{
		Providers: providers,
		Skills:    make([]CapabilitySkill, 0),
	}
	if sessionID != "" {
		if c.Sessions == nil {
			return CapabilitiesSnapshot{}, ErrCapabilitiesUnavailable
		}
		current, err := c.Sessions.Get(ctx, sessionID)
		if err != nil {
			return CapabilitiesSnapshot{}, err
		}
		snapshot.Session = &CapabilitySession{ID: current.ID, ModelAlias: current.ModelAlias}
	}
	if c.Skills != nil {
		skills, err := c.Skills.List(ctx, sessionID)
		if err != nil {
			return CapabilitiesSnapshot{}, err
		}
		if skills != nil {
			snapshot.Skills = skills
		}
	}
	return snapshot, nil
}

func (c *Capabilities) SetSessionModel(ctx context.Context, sessionID, alias string) error {
	if c == nil || c.Providers == nil || c.Sessions == nil {
		return ErrCapabilitiesUnavailable
	}
	snapshot, err := c.Providers.Snapshot(ctx)
	if err != nil {
		return err
	}
	if _, ok := snapshot.Models[strings.TrimSpace(alias)]; !ok {
		return ErrCapabilityModelNotFound
	}
	if _, err := c.Sessions.Get(ctx, sessionID); err != nil {
		return err
	}
	return c.Sessions.SetModelAlias(ctx, sessionID, alias)
}

func (c *Capabilities) SetDefaultModel(ctx context.Context, alias string) (providerconfig.MutationResult, error) {
	if c == nil || c.Providers == nil {
		return providerconfig.MutationResult{}, ErrCapabilitiesUnavailable
	}
	return c.Providers.ActivateModelWithMode(ctx, alias, providerconfig.CommitForRestart)
}

func (c *Capabilities) SetUtilityModel(ctx context.Context, alias string) (providerconfig.MutationResult, error) {
	if c == nil || c.Providers == nil {
		return providerconfig.MutationResult{}, ErrCapabilitiesUnavailable
	}
	return c.Providers.ActivateUtilityModelWithMode(ctx, alias, providerconfig.CommitForRestart)
}

func (c *Capabilities) AddModel(ctx context.Context, request providerconfig.AddModelRequest) (providerconfig.MutationResult, error) {
	if c == nil || c.Providers == nil {
		return providerconfig.MutationResult{}, ErrCapabilitiesUnavailable
	}
	return c.Providers.AddModelWithMode(ctx, request, providerconfig.CommitForRestart)
}

func (c *Capabilities) EnrollProvider(ctx context.Context, request providerconfig.AddRequest) (providerconfig.MutationResult, error) {
	if c == nil || c.Providers == nil {
		return providerconfig.MutationResult{}, ErrCapabilitiesUnavailable
	}
	return c.Providers.AddWithMode(ctx, request, providerconfig.CommitForRestart)
}

func (c *Capabilities) RefreshCatalogue(ctx context.Context, connection string) (CapabilityCatalogueView, error) {
	if c == nil || c.Catalogue == nil {
		return CapabilityCatalogueView{}, ErrCapabilitiesUnavailable
	}
	result, err := c.Catalogue.Refresh(ctx, connection)
	if err != nil {
		return CapabilityCatalogueView{}, err
	}
	models := redactCapabilityCatalogueModels(result.Result.Models, result.PrivateValues...)
	if models == nil {
		models = make([]modelcatalog.Model, 0)
	}
	return CapabilityCatalogueView{
		Connection: redactCapabilityCatalogueText(result.Result.Connection.Name, result.PrivateValues...),
		FetchedAt:  result.Result.FetchedAt,
		Stale:      result.Result.Stale,
		Warning:    redactCapabilityCatalogueText(result.Result.Warning, result.PrivateValues...),
		Models:     models,
	}, nil
}

func redactCapabilityCatalogueModels(models []modelcatalog.Model, private ...string) []modelcatalog.Model {
	redacted := make([]modelcatalog.Model, len(models))
	for index, model := range models {
		model.ID = redactCapabilityCatalogueText(model.ID, private...)
		model.DisplayName = redactCapabilityCatalogueText(model.DisplayName, private...)
		model.Owner = redactCapabilityCatalogueText(model.Owner, private...)
		model.Capabilities = append([]string(nil), model.Capabilities...)
		for capabilityIndex := range model.Capabilities {
			model.Capabilities[capabilityIndex] = redactCapabilityCatalogueText(model.Capabilities[capabilityIndex], private...)
		}
		redacted[index] = model
	}
	return redacted
}

func redactCapabilityCatalogueText(value string, private ...string) string {
	for _, privateValue := range private {
		if privateValue != "" {
			value = strings.ReplaceAll(value, privateValue, "[REDACTED]")
		}
	}
	return value
}

func (c *Capabilities) AttachSkill(ctx context.Context, sessionID, name string) error {
	if c == nil || c.Skills == nil || c.Sessions == nil {
		return ErrCapabilitiesUnavailable
	}
	if _, err := c.Sessions.Get(ctx, sessionID); err != nil {
		return err
	}
	return c.Skills.Attach(ctx, sessionID, name)
}

func (c *Capabilities) DetachSkill(ctx context.Context, sessionID, name string) error {
	if c == nil || c.Skills == nil || c.Sessions == nil {
		return ErrCapabilitiesUnavailable
	}
	if _, err := c.Sessions.Get(ctx, sessionID); err != nil {
		return err
	}
	return c.Skills.Detach(ctx, sessionID, name)
}

func (c *Capabilities) StageSkill(ctx context.Context, request skillinstall.StageRequest) (skillinstall.Manifest, error) {
	if c == nil || c.Skills == nil {
		return skillinstall.Manifest{}, ErrCapabilitiesUnavailable
	}
	return c.Skills.Stage(ctx, request)
}

func (c *Capabilities) InstallSkill(ctx context.Context, stageID, digest string) (CapabilitySkill, error) {
	if c == nil || c.Skills == nil {
		return CapabilitySkill{}, ErrCapabilitiesUnavailable
	}
	return c.Skills.Install(ctx, stageID, digest)
}

func (c *Capabilities) ActivateSkill(ctx context.Context, name string) (providerconfig.MutationResult, error) {
	if c == nil || c.Skills == nil {
		return providerconfig.MutationResult{}, ErrCapabilitiesUnavailable
	}
	name = strings.TrimSpace(name)
	if err := c.Skills.Activate(ctx, name); err != nil {
		return providerconfig.MutationResult{}, err
	}
	sum := sha256.Sum256([]byte("activate-skill\x00" + name))
	return providerconfig.MutationResult{
		RestartRequired: true,
		TransactionID:   hex.EncodeToString(sum[:16]),
	}, nil
}

type CapabilityMutationFactory func(int64, http.Handler) http.Handler

type CapabilitiesRouteConfig struct {
	Service  *Capabilities
	Mutation CapabilityMutationFactory
	Restart  RestartScheduler
}

// RegisterCapabilitiesRoutes mounts only the additive Task 4 API endpoints.
// The caller owns security, idempotency, the shared router, and server wiring.
func RegisterCapabilitiesRoutes(mux *http.ServeMux, routeConfig CapabilitiesRouteConfig) {
	if mux == nil || routeConfig.Service == nil {
		return
	}
	mux.Handle("GET /api/v1/desk/capabilities", capabilitiesSnapshotHandler(routeConfig.Service))
	if routeConfig.Mutation == nil {
		return
	}
	mutation := func(limit int64, handler http.HandlerFunc) http.Handler {
		return routeConfig.Mutation(limit, handler)
	}
	mux.Handle("POST /api/v1/desk/models/session", mutation(capabilityMutationMaxBodyBytes, sessionModelHandler(routeConfig.Service)))
	mux.Handle("POST /api/v1/desk/models/default", mutation(capabilityMutationMaxBodyBytes, globalAliasHandler(routeConfig, false)))
	mux.Handle("POST /api/v1/desk/models/utility", mutation(capabilityMutationMaxBodyBytes, globalAliasHandler(routeConfig, true)))
	mux.Handle("POST /api/v1/desk/models/catalogue/refresh", mutation(capabilityMutationMaxBodyBytes, catalogueRefreshHandler(routeConfig.Service)))
	mux.Handle("POST /api/v1/desk/models", mutation(capabilityMutationMaxBodyBytes, addModelHandler(routeConfig)))
	mux.Handle("POST /api/v1/desk/providers", mutation(CapabilityProviderMaxBodyBytes, providerEnrollmentHandler(routeConfig)))
	mux.Handle("POST /api/v1/desk/skills/session/attach", mutation(capabilityMutationMaxBodyBytes, sessionSkillHandler(routeConfig.Service, true)))
	mux.Handle("POST /api/v1/desk/skills/session/detach", mutation(capabilityMutationMaxBodyBytes, sessionSkillHandler(routeConfig.Service, false)))
	mux.Handle("POST /api/v1/desk/skills/stage", mutation(capabilityMutationMaxBodyBytes, stageSkillHandler(routeConfig.Service)))
	mux.Handle("POST /api/v1/desk/skills/install", mutation(capabilityMutationMaxBodyBytes, installSkillHandler(routeConfig.Service)))
	mux.Handle("POST /api/v1/desk/skills/{name}/activate", mutation(capabilityMutationMaxBodyBytes, activateSkillHandler(routeConfig)))
}

func capabilitiesSnapshotHandler(service *Capabilities) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		snapshot, err := service.Snapshot(r.Context(), r.URL.Query().Get("session_id"))
		if err != nil {
			writeCapabilityError(w, err)
			return
		}
		writeCapabilityJSON(w, http.StatusOK, snapshot)
	})
}

func sessionModelHandler(service *Capabilities) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			SessionID string `json:"session_id"`
			Alias     string `json:"alias"`
		}
		if !decodeCapabilityRequest(w, r, &request) {
			return
		}
		if err := service.SetSessionModel(r.Context(), request.SessionID, request.Alias); err != nil {
			writeCapabilityError(w, err)
			return
		}
		writeCapabilityJSON(w, http.StatusOK, struct{}{})
	}
}

func globalAliasHandler(routeConfig CapabilitiesRouteConfig, utility bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		after, ok := w.(AfterResponseWriter)
		if !ok || routeConfig.Restart == nil {
			writeCapabilityError(w, ErrAfterResponseUnavailable)
			return
		}
		var request struct {
			Alias string `json:"alias"`
		}
		if !decodeCapabilityRequest(w, r, &request) {
			return
		}
		var (
			result providerconfig.MutationResult
			err    error
		)
		if utility {
			result, err = routeConfig.Service.SetUtilityModel(r.Context(), request.Alias)
		} else {
			result, err = routeConfig.Service.SetDefaultModel(r.Context(), request.Alias)
		}
		if err != nil {
			writeCapabilityError(w, err)
			return
		}
		deferRestart(after, routeConfig.Restart, result.TransactionID)
		writeCapabilityJSON(w, http.StatusAccepted, result)
	}
}

func catalogueRefreshHandler(service *Capabilities) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Connection string `json:"connection"`
		}
		if !decodeCapabilityRequest(w, r, &request) {
			return
		}
		result, err := service.RefreshCatalogue(r.Context(), request.Connection)
		if err != nil {
			writeCapabilityError(w, err)
			return
		}
		writeCapabilityJSON(w, http.StatusOK, result)
	}
}

func addModelHandler(routeConfig CapabilitiesRouteConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		after, ok := w.(AfterResponseWriter)
		if !ok || routeConfig.Restart == nil {
			writeCapabilityError(w, ErrAfterResponseUnavailable)
			return
		}
		var request struct {
			ConnectionName string `json:"connection_name"`
			Alias          string `json:"alias"`
			UpstreamModel  string `json:"upstream_model"`
			Default        bool   `json:"default"`
			Utility        bool   `json:"utility"`
		}
		if !decodeCapabilityRequest(w, r, &request) {
			return
		}
		result, err := routeConfig.Service.AddModel(r.Context(), providerconfig.AddModelRequest{
			ConnectionName: request.ConnectionName,
			Alias:          request.Alias,
			UpstreamModel:  request.UpstreamModel,
			Default:        request.Default,
			Utility:        request.Utility,
		})
		if err != nil {
			writeCapabilityError(w, err)
			return
		}
		deferRestart(after, routeConfig.Restart, result.TransactionID)
		writeCapabilityJSON(w, http.StatusAccepted, result)
	}
}

func providerEnrollmentHandler(routeConfig CapabilitiesRouteConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		after, ok := w.(AfterResponseWriter)
		if !ok || routeConfig.Restart == nil {
			writeCapabilityError(w, ErrAfterResponseUnavailable)
			return
		}
		var request struct {
			ConnectionName string                        `json:"connection_name"`
			Type           string                        `json:"type"`
			BaseURL        string                        `json:"base_url"`
			MaxTokens      int                           `json:"max_tokens"`
			APIKey         string                        `json:"api_key"`
			Models         map[string]config.ModelTarget `json:"models"`
			DefaultModel   string                        `json:"default_model"`
			UtilityModel   string                        `json:"utility_model"`
		}
		if !decodeCapabilityRequest(w, r, &request) {
			return
		}
		credential := []byte(request.APIKey)
		defer func() {
			clear(credential)
			request.APIKey = ""
		}()
		result, err := routeConfig.Service.EnrollProvider(r.Context(), providerconfig.AddRequest{
			ConnectionName: request.ConnectionName,
			Connection: config.ProviderConnection{
				Type:      request.Type,
				BaseURL:   request.BaseURL,
				MaxTokens: request.MaxTokens,
			},
			Models:       request.Models,
			DefaultModel: request.DefaultModel,
			UtilityModel: request.UtilityModel,
			APIKey:       string(credential),
		})
		if err != nil {
			writeCapabilityError(w, err)
			return
		}
		deferRestart(after, routeConfig.Restart, result.TransactionID)
		writeCapabilityJSON(w, http.StatusAccepted, result)
	}
}

func sessionSkillHandler(service *Capabilities, attach bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			SessionID string `json:"session_id"`
			Name      string `json:"name"`
		}
		if !decodeCapabilityRequest(w, r, &request) {
			return
		}
		var err error
		if attach {
			err = service.AttachSkill(r.Context(), request.SessionID, request.Name)
		} else {
			err = service.DetachSkill(r.Context(), request.SessionID, request.Name)
		}
		if err != nil {
			writeCapabilityError(w, err)
			return
		}
		writeCapabilityJSON(w, http.StatusOK, struct{}{})
	}
}

func stageSkillHandler(service *Capabilities) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			LocalPath string `json:"local_path"`
			GitURL    string `json:"git_url"`
			Commit    string `json:"commit"`
		}
		if !decodeCapabilityRequest(w, r, &request) {
			return
		}
		manifest, err := service.StageSkill(r.Context(), skillinstall.StageRequest{
			LocalPath: request.LocalPath,
			GitURL:    request.GitURL,
			Commit:    request.Commit,
		})
		if err != nil {
			writeCapabilityError(w, err)
			return
		}
		writeCapabilityJSON(w, http.StatusOK, manifest)
	}
}

func installSkillHandler(service *Capabilities) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			StageID string `json:"stage_id"`
			Digest  string `json:"digest"`
		}
		if !decodeCapabilityRequest(w, r, &request) {
			return
		}
		installed, err := service.InstallSkill(r.Context(), request.StageID, request.Digest)
		if err != nil {
			writeCapabilityError(w, err)
			return
		}
		writeCapabilityJSON(w, http.StatusOK, installed)
	}
}

func activateSkillHandler(routeConfig CapabilitiesRouteConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		after, ok := w.(AfterResponseWriter)
		if !ok || routeConfig.Restart == nil {
			writeCapabilityError(w, ErrAfterResponseUnavailable)
			return
		}
		result, err := routeConfig.Service.ActivateSkill(r.Context(), r.PathValue("name"))
		if err != nil {
			writeCapabilityError(w, err)
			return
		}
		deferRestart(after, routeConfig.Restart, result.TransactionID)
		writeCapabilityJSON(w, http.StatusAccepted, result)
	}
}

func deferRestart(after AfterResponseWriter, scheduler RestartScheduler, transactionID string) {
	after.AfterResponse(func() RestartScheduleOutcome {
		ctx, cancel := context.WithTimeout(context.Background(), restartScheduleTimeout)
		defer cancel()
		err := scheduler.Schedule(ctx, transactionID)
		switch {
		case err == nil:
			return RestartScheduleOutcome{
				Scheduled: true,
				Code:      "restart_scheduled",
				Message:   "Waffle restart was scheduled.",
			}
		case errors.Is(err, ErrManualRestartRequired):
			return RestartScheduleOutcome{
				Code:    "manual_restart_required",
				Message: ErrManualRestartRequired.Error(),
			}
		default:
			return RestartScheduleOutcome{
				Code:    "restart_schedule_failed",
				Message: "restart could not be scheduled; restart waffle serve to apply the change",
			}
		}
	})
}

func decodeCapabilityRequest(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		writeCapabilityJSON(w, http.StatusBadRequest, capabilityError{
			Code:    "invalid_request",
			Message: "capability request is invalid",
		})
		return false
	}
	return true
}

type capabilityError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeCapabilityError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	response := capabilityError{
		Code:    "capability_failed",
		Message: "capability request could not be completed",
	}
	switch {
	case errors.Is(err, session.ErrNotFound):
		status = http.StatusNotFound
		response.Code, response.Message = "session_not_found", "session was not found"
	case errors.Is(err, ErrCapabilityModelNotFound):
		status = http.StatusNotFound
		response.Code, response.Message = "model_not_found", "model alias was not found"
	case errors.Is(err, ErrCapabilitySkillNotFound):
		status = http.StatusNotFound
		response.Code, response.Message = "skill_not_found", "skill was not found"
	case errors.Is(err, ErrAfterResponseUnavailable), errors.Is(err, ErrCapabilitiesUnavailable):
		status = http.StatusServiceUnavailable
		response.Code, response.Message = "capabilities_unavailable", "capabilities are unavailable"
	}
	writeCapabilityJSON(w, status, response)
}

func writeCapabilityJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
