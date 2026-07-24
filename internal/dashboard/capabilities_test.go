package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/memory"
	"github.com/matt-riley/waffle/internal/modelcatalog"
	"github.com/matt-riley/waffle/internal/providerconfig"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/skill"
	"github.com/matt-riley/waffle/internal/skillinstall"
	"github.com/matt-riley/waffle/internal/store"
)

const capabilityCredentialCanary = "sk-super-private"

func TestCapabilitiesSessionModelIsolatedFromGlobalRoles(t *testing.T) {
	providers := &fakeCapabilityProviders{snapshot: providerconfig.Listing{
		DefaultModel: "default",
		UtilityModel: "utility",
		Providers:    map[string]providerconfig.ProviderSummary{},
		Models: map[string]providerconfig.ModelSummary{
			"default": {Provider: "openai", Model: "gpt-default"},
			"session": {Provider: "local", Model: "local-session"},
			"utility": {Provider: "openai", Model: "gpt-utility"},
		},
	}}
	sessions := &fakeCapabilitySessions{
		sessions: map[string]*session.Session{
			"sess-1": {ID: "sess-1", ModelAlias: "default"},
		},
	}
	capabilities := &Capabilities{Providers: providers, Sessions: sessions}

	if err := capabilities.SetSessionModel(t.Context(), "sess-1", "session"); err != nil {
		t.Fatal(err)
	}
	if sessions.setSession != "sess-1" || sessions.setAlias != "session" {
		t.Fatalf("session mutation = %q %q", sessions.setSession, sessions.setAlias)
	}
	if providers.mutations != 0 {
		t.Fatalf("provider mutations = %d, want zero", providers.mutations)
	}
	if err := capabilities.SetSessionModel(t.Context(), "sess-1", "missing"); !errors.Is(err, ErrCapabilityModelNotFound) {
		t.Fatalf("unknown alias error = %v", err)
	}
	if err := capabilities.SetSessionModel(t.Context(), "missing", "session"); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("missing session error = %v", err)
	}
}

func TestCapabilitiesSnapshotCombinesModelsAndSessionSkills(t *testing.T) {
	providers := &fakeCapabilityProviders{snapshot: providerconfig.Listing{
		State:        "ready",
		DefaultModel: "default",
		UtilityModel: "utility",
		Providers: map[string]providerconfig.ProviderSummary{
			"local": {Type: "openai", BaseURL: "http://127.0.0.1:11434/v1"},
		},
		Models: map[string]providerconfig.ModelSummary{
			"default": {Provider: "local", Model: "llama"},
		},
	}}
	sessions := &fakeCapabilitySessions{sessions: map[string]*session.Session{
		"sess-1": {ID: "sess-1", ModelAlias: "default"},
	}}
	skills := &fakeCapabilitySkills{listed: []CapabilitySkill{
		{Name: "review", Description: "Review changes", Active: true, Attached: true},
	}}
	capabilities := &Capabilities{Providers: providers, Sessions: sessions, Skills: skills}

	snapshot, err := capabilities.Snapshot(t.Context(), "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Session == nil || snapshot.Session.ID != "sess-1" || snapshot.Session.ModelAlias != "default" {
		t.Fatalf("session snapshot = %#v", snapshot.Session)
	}
	if snapshot.Providers.UtilityModel != "utility" || len(snapshot.Skills) != 1 || !snapshot.Skills[0].Attached {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if skills.listSession != "sess-1" {
		t.Fatalf("skills listed for %q", skills.listSession)
	}
}

func TestCapabilitiesSkillsStageInstallInactiveThenExplicitActivate(t *testing.T) {
	skills := &fakeCapabilitySkills{
		manifest: skillinstall.Manifest{
			Name:          "review",
			StageID:       "stage-1",
			ContentDigest: "sha256:digest",
		},
		installed: CapabilitySkill{Name: "review", Active: false},
	}
	capabilities := &Capabilities{Skills: skills}

	manifest, err := capabilities.StageSkill(t.Context(), skillinstall.StageRequest{LocalPath: "/imports/review"})
	if err != nil {
		t.Fatal(err)
	}
	installed, err := capabilities.InstallSkill(t.Context(), manifest.StageID, manifest.ContentDigest)
	if err != nil {
		t.Fatal(err)
	}
	if installed.Active {
		t.Fatal("reviewed install was activated implicitly")
	}
	result, err := capabilities.ActivateSkill(t.Context(), "review")
	if err != nil {
		t.Fatal(err)
	}
	if !skills.activated || !result.RestartRequired || result.TransactionID == "" {
		t.Fatalf("activation result=%#v activated=%v", result, skills.activated)
	}
}

func TestCapabilitiesSessionSkillMutationRequiresExistingSession(t *testing.T) {
	sessions := &fakeCapabilitySessions{sessions: map[string]*session.Session{
		"sess-1": {ID: "sess-1"},
	}}
	skills := &fakeCapabilitySkills{}
	capabilities := &Capabilities{Sessions: sessions, Skills: skills}

	if err := capabilities.AttachSkill(t.Context(), "missing", "review"); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("missing session attach error = %v", err)
	}
	if err := capabilities.DetachSkill(t.Context(), "missing", "review"); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("missing session detach error = %v", err)
	}
	if skills.attachCalls != 0 || skills.detachCalls != 0 {
		t.Fatalf("skill mutations attach=%d detach=%d", skills.attachCalls, skills.detachCalls)
	}
	if err := capabilities.AttachSkill(t.Context(), "sess-1", "review"); err != nil {
		t.Fatal(err)
	}
	if err := capabilities.DetachSkill(t.Context(), "sess-1", "review"); err != nil {
		t.Fatal(err)
	}
	if skills.attachCalls != 1 || skills.detachCalls != 1 {
		t.Fatalf("skill mutations attach=%d detach=%d", skills.attachCalls, skills.detachCalls)
	}
}

func TestCapabilitiesCatalogueRedactsPrivateFetchValuesAcrossEveryPublicString(t *testing.T) {
	const (
		apiKey  = "sk-catalogue-canary"
		scopeID = "scope-catalogue-canary"
	)
	capabilities := &Capabilities{
		Catalogue: fakeCapabilityCatalogue{result: CapabilityCatalogueResult{
			Result: modelcatalog.Result{
				Record: modelcatalog.Record{
					Connection: modelcatalog.Connection{Name: "primary-" + apiKey},
					FetchedAt:  time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC),
					Models: []modelcatalog.Model{{
						ID:            "model-" + apiKey,
						DisplayName:   "display-" + scopeID,
						Owner:         "owner-" + apiKey,
						ContextWindow: 128_000,
						Capabilities:  []string{"text-" + apiKey, "tools-" + scopeID},
					}},
				},
				Warning: "warning-" + scopeID,
			},
			PrivateValues: []string{apiKey, scopeID},
		}},
	}

	view, err := capabilities.RefreshCatalogue(t.Context(), "primary")
	if err != nil {
		t.Fatal(err)
	}
	public, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{apiKey, scopeID} {
		if bytes.Contains(public, []byte(private)) {
			t.Fatalf("catalogue response leaked %q: %s", private, public)
		}
	}
	if view.Connection != "primary-[REDACTED]" ||
		view.Warning != "warning-[REDACTED]" ||
		len(view.Models) != 1 ||
		view.Models[0].ID != "model-[REDACTED]" ||
		view.Models[0].DisplayName != "display-[REDACTED]" ||
		view.Models[0].Owner != "owner-[REDACTED]" ||
		!reflect.DeepEqual(view.Models[0].Capabilities, []string{"text-[REDACTED]", "tools-[REDACTED]"}) {
		t.Fatalf("redacted catalogue = %#v", view)
	}
}

func TestWorkspaceCapabilitySkillsPreservesReviewedProvenanceAndInactiveInstall(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(t.Context(), filepath.Join(root, "state", "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sessions := session.New(st)
	current, err := sessions.Create(t.Context(), "capability review")
	if err != nil {
		t.Fatal(err)
	}
	workspace := memory.Workspace{Dir: filepath.Join(root, "workspace"), Agent: "main"}
	imports := filepath.Join(root, "imports")
	source := filepath.Join(imports, "reviewed-skill")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	sourceBody := "---\nname: reviewed-skill\ndescription: Reviews changes carefully.\n---\n\n# Reviewed skill\n\nInspect every change.\n"
	if err := os.WriteFile(filepath.Join(source, "SKILL.md"), []byte(sourceBody), 0o600); err != nil {
		t.Fatal(err)
	}
	installer := skillinstall.New(
		workspace.SkillsDir(),
		filepath.Join(root, "private-stages"),
		[]string{imports},
		[]string{"github.com"},
	)
	skills := &WorkspaceCapabilitySkills{
		DB:          st.DB,
		Workspace:   workspace,
		Attachments: &skill.Attachments{DB: st.DB},
		Installer:   installer,
	}

	manifest, err := skills.Stage(t.Context(), skillinstall.StageRequest{LocalPath: source})
	if err != nil {
		t.Fatal(err)
	}
	installed, err := skills.Install(t.Context(), manifest.StageID, manifest.ContentDigest)
	if err != nil {
		t.Fatal(err)
	}
	if installed.Name != "reviewed-skill" || installed.Active {
		t.Fatalf("installed = %#v", installed)
	}
	active, err := skill.DiscoverActive(workspace.SkillsDir(), st.DB)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 {
		t.Fatalf("inactive install discovered active: %#v", active)
	}
	var status, sourceRef, digest string
	if err := st.DB.QueryRowContext(t.Context(), `
		SELECT status, source_ref, content_digest
		FROM skill_status WHERE name = ?`, "reviewed-skill").Scan(&status, &sourceRef, &digest); err != nil {
		t.Fatal(err)
	}
	if status != skill.StatusInactive || sourceRef != manifest.SourceRef || digest != manifest.ContentDigest {
		t.Fatalf("provenance status=%q source=%q digest=%q", status, sourceRef, digest)
	}
	if err := skills.Activate(t.Context(), "reviewed-skill"); err != nil {
		t.Fatal(err)
	}
	if err := skills.Attach(t.Context(), current.ID, "reviewed-skill"); err != nil {
		t.Fatal(err)
	}
	listed, err := skills.List(t.Context(), current.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || !listed[0].Active || !listed[0].Attached {
		t.Fatalf("listed skills = %#v", listed)
	}
}

func TestRegisterCapabilitiesRoutesProviderCredentialNeverLeaks(t *testing.T) {
	for _, tc := range []struct {
		name        string
		providerErr bool
		wantStatus  int
	}{
		{name: "success", wantStatus: http.StatusAccepted},
		{name: "failure", providerErr: true, wantStatus: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			providers := &fakeCapabilityProviders{}
			if tc.providerErr {
				providers.mutationErr = errors.New("provider rejected " + capabilityCredentialCanary)
			}
			var mutationLimits []int64
			mux := http.NewServeMux()
			RegisterCapabilitiesRoutes(mux, CapabilitiesRouteConfig{
				Service: &Capabilities{Providers: providers},
				Mutation: func(limit int64, next http.Handler) http.Handler {
					mutationLimits = append(mutationLimits, limit)
					return next
				},
				Restart: fakeRestartScheduler{},
			})
			body := `{"connection_name":"openai","type":"openai","base_url":"https://api.example/v1","api_key":"` +
				capabilityCredentialCanary +
				`","models":{"gpt":{"model":"gpt-test"}},"default_model":"gpt"}`
			request := httptest.NewRequest(http.MethodPost, "/api/v1/desk/providers", strings.NewReader(body))
			response := newAfterResponseRecorder()

			mux.ServeHTTP(response, request)

			if response.Code != tc.wantStatus {
				t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
			}
			foundProviderLimit := false
			for _, limit := range mutationLimits {
				if limit == CapabilityProviderMaxBodyBytes {
					foundProviderLimit = true
				}
			}
			if !foundProviderLimit {
				t.Fatalf("provider body limit missing from %v", mutationLimits)
			}
			if strings.Contains(response.Body.String(), capabilityCredentialCanary) {
				t.Fatalf("response leaked credential: %s", response.Body.String())
			}
			if tc.providerErr {
				if providers.lastAPIKey != capabilityCredentialCanary {
					t.Fatal("credential was not passed to provider transaction")
				}
				return
			}
			if len(response.after) != 1 || providers.lastAPIKey != capabilityCredentialCanary {
				t.Fatalf("callbacks=%d key=%q", len(response.after), providers.lastAPIKey)
			}
		})
	}
}

func TestCapabilitiesRestartSchedulesOnlyAfterResponseAndNeverOnReplayData(t *testing.T) {
	providers := &fakeCapabilityProviders{
		result: providerconfig.MutationResult{RestartRequired: true, TransactionID: "txn-1"},
	}
	scheduler := &recordingRestartScheduler{}
	mux := http.NewServeMux()
	RegisterCapabilitiesRoutes(mux, CapabilitiesRouteConfig{
		Service:  &Capabilities{Providers: providers},
		Mutation: func(_ int64, next http.Handler) http.Handler { return next },
		Restart:  scheduler,
	})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/desk/models/default", strings.NewReader(`{"alias":"gpt"}`))
	response := newAfterResponseRecorder()

	mux.ServeHTTP(response, request)

	if response.Code != http.StatusAccepted || scheduler.calls != 0 {
		t.Fatalf("before write completion status=%d scheduler calls=%d body=%s", response.Code, scheduler.calls, response.Body.String())
	}
	response.RunAfterResponse()
	if scheduler.calls != 1 || scheduler.transactionID != "txn-1" {
		t.Fatalf("after response scheduler calls=%d transaction=%q", scheduler.calls, scheduler.transactionID)
	}
	response.RunAfterResponse()
	if scheduler.calls != 1 {
		t.Fatalf("after-response callback repeated: %d", scheduler.calls)
	}
	var result providerconfig.MutationResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result, providers.result) {
		t.Fatalf("response result=%#v want=%#v", result, providers.result)
	}
}

func TestCapabilitiesRestartOutcomeIsSanitizedAndOperatorActionable(t *testing.T) {
	tests := []struct {
		name          string
		scheduler     RestartScheduler
		wantScheduled bool
		wantCode      string
		wantMessage   string
	}{
		{
			name:          "managed schedule succeeds",
			scheduler:     fakeRestartScheduler{},
			wantScheduled: true,
			wantCode:      "restart_scheduled",
			wantMessage:   "Waffle restart was scheduled.",
		},
		{
			name:          "standalone requires operator restart",
			scheduler:     StandaloneRestartScheduler{},
			wantScheduled: false,
			wantCode:      "manual_restart_required",
			wantMessage:   "restart waffle serve to apply the change",
		},
		{
			name:          "scheduler failure is sanitized",
			scheduler:     failingRestartScheduler{err: errors.New("systemd exposed " + capabilityCredentialCanary)},
			wantScheduled: false,
			wantCode:      "restart_schedule_failed",
			wantMessage:   "restart could not be scheduled; restart waffle serve to apply the change",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			providers := &fakeCapabilityProviders{
				result: providerconfig.MutationResult{RestartRequired: true, TransactionID: "txn-private-diagnostic"},
			}
			mux := http.NewServeMux()
			RegisterCapabilitiesRoutes(mux, CapabilitiesRouteConfig{
				Service:  &Capabilities{Providers: providers},
				Mutation: func(_ int64, next http.Handler) http.Handler { return next },
				Restart:  tt.scheduler,
			})
			response := newAfterResponseRecorder()

			mux.ServeHTTP(response, httptest.NewRequest(
				http.MethodPost,
				"/api/v1/desk/models/default",
				strings.NewReader(`{"alias":"gpt"}`),
			))

			if response.Code != http.StatusAccepted || len(response.after) != 1 || len(response.outcomes) != 0 {
				t.Fatalf("before flush status=%d callbacks=%d outcomes=%d body=%s",
					response.Code, len(response.after), len(response.outcomes), response.Body.String())
			}
			response.RunAfterResponse()
			if len(response.outcomes) != 1 {
				t.Fatalf("outcomes = %#v, want one observable result", response.outcomes)
			}
			outcome := response.outcomes[0]
			if outcome.Scheduled != tt.wantScheduled ||
				outcome.Code != tt.wantCode ||
				outcome.Message != tt.wantMessage {
				t.Fatalf("outcome = %#v", outcome)
			}
			public, err := json.Marshal(outcome)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(public, []byte(capabilityCredentialCanary)) ||
				bytes.Contains(public, []byte("txn-private-diagnostic")) {
				t.Fatalf("outcome leaked private diagnostics: %s", public)
			}
		})
	}
}

func TestRegisterCapabilitiesRoutesHasNoRemovalEndpoints(t *testing.T) {
	mux := http.NewServeMux()
	RegisterCapabilitiesRoutes(mux, CapabilitiesRouteConfig{
		Service:  &Capabilities{},
		Mutation: func(_ int64, next http.Handler) http.Handler { return next },
	})
	for _, path := range []string{
		"/api/v1/desk/models/remove",
		"/api/v1/desk/providers/remove",
		"/api/v1/desk/skills/review/remove",
	} {
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`)))
		if response.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d, want 404", path, response.Code)
		}
	}
}

type fakeCapabilityProviders struct {
	snapshot    providerconfig.Listing
	result      providerconfig.MutationResult
	mutationErr error
	mutations   int
	lastAPIKey  string
}

func (f *fakeCapabilityProviders) Snapshot(context.Context) (providerconfig.Listing, error) {
	return f.snapshot, nil
}

func (f *fakeCapabilityProviders) AddWithMode(_ context.Context, request providerconfig.AddRequest, _ providerconfig.CommitMode) (providerconfig.MutationResult, error) {
	f.mutations++
	f.lastAPIKey = request.APIKey
	return f.mutationResult()
}

func (f *fakeCapabilityProviders) AddModelWithMode(context.Context, providerconfig.AddModelRequest, providerconfig.CommitMode) (providerconfig.MutationResult, error) {
	f.mutations++
	return f.mutationResult()
}

func (f *fakeCapabilityProviders) ActivateModelWithMode(context.Context, string, providerconfig.CommitMode) (providerconfig.MutationResult, error) {
	f.mutations++
	return f.mutationResult()
}

func (f *fakeCapabilityProviders) ActivateUtilityModelWithMode(context.Context, string, providerconfig.CommitMode) (providerconfig.MutationResult, error) {
	f.mutations++
	return f.mutationResult()
}

func (f *fakeCapabilityProviders) mutationResult() (providerconfig.MutationResult, error) {
	if f.mutationErr != nil {
		return providerconfig.MutationResult{}, f.mutationErr
	}
	if f.result.TransactionID == "" {
		f.result = providerconfig.MutationResult{RestartRequired: true, TransactionID: "txn-provider"}
	}
	return f.result, nil
}

type fakeCapabilitySessions struct {
	sessions   map[string]*session.Session
	setSession string
	setAlias   string
}

func (f *fakeCapabilitySessions) Get(_ context.Context, id string) (*session.Session, error) {
	value, ok := f.sessions[id]
	if !ok {
		return nil, session.ErrNotFound
	}
	copy := *value
	return &copy, nil
}

func (f *fakeCapabilitySessions) SetModelAlias(_ context.Context, id, alias string) error {
	if _, ok := f.sessions[id]; !ok {
		return session.ErrNotFound
	}
	f.setSession, f.setAlias = id, alias
	f.sessions[id].ModelAlias = alias
	return nil
}

type fakeCapabilitySkills struct {
	listed      []CapabilitySkill
	listSession string
	manifest    skillinstall.Manifest
	installed   CapabilitySkill
	activated   bool
	attachCalls int
	detachCalls int
}

func (f *fakeCapabilitySkills) List(_ context.Context, sessionID string) ([]CapabilitySkill, error) {
	f.listSession = sessionID
	return append([]CapabilitySkill(nil), f.listed...), nil
}

func (f *fakeCapabilitySkills) Attach(context.Context, string, string) error {
	f.attachCalls++
	return nil
}
func (f *fakeCapabilitySkills) Detach(context.Context, string, string) error {
	f.detachCalls++
	return nil
}
func (f *fakeCapabilitySkills) Stage(context.Context, skillinstall.StageRequest) (skillinstall.Manifest, error) {
	return f.manifest, nil
}
func (f *fakeCapabilitySkills) Install(context.Context, string, string) (CapabilitySkill, error) {
	return f.installed, nil
}
func (f *fakeCapabilitySkills) Activate(context.Context, string) error {
	f.activated = true
	return nil
}

type fakeCapabilityCatalogue struct {
	result CapabilityCatalogueResult
	err    error
}

func (f fakeCapabilityCatalogue) Refresh(context.Context, string) (CapabilityCatalogueResult, error) {
	return f.result, f.err
}

type fakeRestartScheduler struct{}

func (fakeRestartScheduler) Schedule(context.Context, string) error { return nil }

type failingRestartScheduler struct{ err error }

func (s failingRestartScheduler) Schedule(context.Context, string) error { return s.err }

type recordingRestartScheduler struct {
	calls         int
	transactionID string
}

func (s *recordingRestartScheduler) Schedule(_ context.Context, transactionID string) error {
	s.calls++
	s.transactionID = transactionID
	return nil
}

type afterResponseRecorder struct {
	*httptest.ResponseRecorder
	after    []func() RestartScheduleOutcome
	outcomes []RestartScheduleOutcome
	ran      bool
}

func newAfterResponseRecorder() *afterResponseRecorder {
	return &afterResponseRecorder{ResponseRecorder: httptest.NewRecorder()}
}

func (r *afterResponseRecorder) AfterResponse(callback func() RestartScheduleOutcome) {
	r.after = append(r.after, callback)
}

func (r *afterResponseRecorder) RunAfterResponse() {
	if r.ran {
		return
	}
	r.ran = true
	for _, callback := range r.after {
		r.outcomes = append(r.outcomes, callback())
	}
}

var (
	_ CapabilityProviders = (*fakeCapabilityProviders)(nil)
	_ CapabilitySessions  = (*fakeCapabilitySessions)(nil)
	_ CapabilitySkills    = (*fakeCapabilitySkills)(nil)
	_ CapabilityCatalogue = fakeCapabilityCatalogue{}
	_ RestartScheduler    = fakeRestartScheduler{}
	_                     = fmt.Sprintf
	_                     = config.Config{}
)
