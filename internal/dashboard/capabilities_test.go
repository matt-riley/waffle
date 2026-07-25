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

func TestCapabilitiesSnapshotIncludesCredentialFreeProviderPresets(t *testing.T) {
	capabilities := &Capabilities{Providers: &fakeCapabilityProviders{snapshot: providerconfig.Listing{
		Providers: map[string]providerconfig.ProviderSummary{},
		Models:    map[string]providerconfig.ModelSummary{},
	}}}

	snapshot, err := capabilities.Snapshot(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.ProviderPresets) != len(providerconfig.Presets()) {
		t.Fatalf("provider presets = %#v, want %#v", snapshot.ProviderPresets, providerconfig.Presets())
	}
	public, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(public, []byte(capabilityCredentialCanary)) {
		t.Fatalf("snapshot leaked credential material: %s", public)
	}
}

func TestCapabilitiesSnapshotRetriesTransientProviderLock(t *testing.T) {
	providers := &fakeCapabilityProviders{
		snapshot: providerconfig.Listing{
			State:     "ready",
			Providers: map[string]providerconfig.ProviderSummary{},
			Models:    map[string]providerconfig.ModelSummary{},
		},
		snapshotErrs: []error{providerconfig.ErrLocked},
	}
	mux := http.NewServeMux()
	RegisterCapabilitiesRoutes(mux, CapabilitiesRouteConfig{
		Service: &Capabilities{Providers: providers},
	})
	response := httptest.NewRecorder()

	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/desk/capabilities", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", response.Code, response.Body.String())
	}
	if providers.snapshotCalls != 2 {
		t.Fatalf("snapshot calls = %d, want 2", providers.snapshotCalls)
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

func TestCapabilitiesCatalogueEnrolledAliasIgnoresOtherConnectionsAndKeepsLowestAlias(t *testing.T) {
	providers := &fakeCapabilityProviders{snapshot: providerconfig.Listing{
		Providers: map[string]providerconfig.ProviderSummary{
			"router": {Type: "openai"},
			"other":  {Type: "openai"},
		},
		Models: map[string]providerconfig.ModelSummary{
			"zeta":    {Provider: "router", Model: "anthropic/claude-sonnet-4-6"},
			"alpha":   {Provider: "router", Model: "anthropic/claude-sonnet-4-6"},
			"mirrors": {Provider: "other", Model: "openai/gpt-5.4"},
		},
	}}
	capabilities := &Capabilities{
		Providers: providers,
		Catalogue: fakeCapabilityCatalogue{result: CapabilityCatalogueResult{Result: modelcatalog.Result{
			Record: modelcatalog.Record{Connection: modelcatalog.Connection{Name: "router"}, Models: []modelcatalog.Model{
				{ID: "anthropic/claude-sonnet-4-6"},
				{ID: "openai/gpt-5.4"},
			}},
		}}},
	}

	view, err := capabilities.RefreshCatalogue(t.Context(), "router")
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Models) != 2 {
		t.Fatalf("models = %#v", view.Models)
	}
	if view.Models[0].EnrolledAlias != "alpha" {
		t.Fatalf("duplicate aliases must resolve to the lowest alias, got %#v", view.Models[0])
	}
	if view.Models[1].EnrolledAlias != "" {
		t.Fatalf("another connection's alias must not mark this catalogue model, got %#v", view.Models[1])
	}
}

func TestCapabilitiesCatalogueMarksAlreadyEnrolledModelsAndSuggestsAliases(t *testing.T) {
	providers := &fakeCapabilityProviders{snapshot: providerconfig.Listing{
		Providers: map[string]providerconfig.ProviderSummary{"router": {Type: "openai"}},
		Models: map[string]providerconfig.ModelSummary{
			"claude": {Provider: "router", Model: "anthropic/claude-sonnet-4-6"},
		},
	}}
	capabilities := &Capabilities{
		Providers: providers,
		Catalogue: fakeCapabilityCatalogue{result: CapabilityCatalogueResult{Result: modelcatalog.Result{
			Record: modelcatalog.Record{Connection: modelcatalog.Connection{Name: "router"}, Models: []modelcatalog.Model{
				{ID: "anthropic/claude-sonnet-4-6"},
				{ID: "openai/gpt-5.4"},
			}},
		}}},
	}

	view, err := capabilities.RefreshCatalogue(t.Context(), "router")
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Models) != 2 {
		t.Fatalf("models = %#v", view.Models)
	}
	if view.Models[0].EnrolledAlias != "claude" || view.Models[0].AliasSuggestion != "anthropic-claude-sonnet-4-6" {
		t.Fatalf("enrolled model = %#v", view.Models[0])
	}
	if view.Models[1].EnrolledAlias != "" || view.Models[1].AliasSuggestion != "openai-gpt-5-4" {
		t.Fatalf("new model = %#v", view.Models[1])
	}
	public, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(public, []byte("sk-")) {
		t.Fatalf("catalogue JSON leaked a credential: %s", public)
	}
}

func TestCapabilitiesProviderTestClassifiesOnlySafeOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{name: "success", want: "success"},
		{name: "authentication", err: errors.New("upstream 401 Unauthorized for sk-super-private"), want: "authentication_failed"},
		{name: "unreachable", err: context.DeadlineExceeded, want: "unreachable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			providers := &fakeCapabilityProviders{testErr: tc.err}
			result, err := (&Capabilities{Providers: providers}).TestProvider(t.Context(), "primary")
			if err != nil {
				t.Fatal(err)
			}
			if result.Outcome != providerconfig.ProbeOutcome(tc.want) {
				t.Fatalf("outcome = %q, want %q", result.Outcome, tc.want)
			}
			public, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(public, []byte(capabilityCredentialCanary)) || bytes.Contains(public, []byte("401")) {
				t.Fatalf("test result leaked probe detail: %s", public)
			}
		})
	}
}

func TestCapabilitiesProspectiveProviderTestClassifiesOnlySafeOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want providerconfig.ProbeOutcome
	}{
		{name: "success", want: providerconfig.ProbeOutcomeSuccess},
		{name: "authentication", err: errors.New("upstream 401 Unauthorized for " + capabilityCredentialCanary), want: providerconfig.ProbeOutcomeAuthentication},
		{name: "unreachable", err: context.DeadlineExceeded, want: providerconfig.ProbeOutcomeUnreachable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			providers := &fakeCapabilityProviders{prospectiveErr: tc.err}
			result, err := (&Capabilities{Providers: providers}).TestProspectiveProvider(t.Context(), providerconfig.ProspectiveProbeRequest{
				ConnectionName: "primary",
				Connection:     config.ProviderConnection{Type: "openai"},
				Model:          "gpt-test",
				APIKey:         capabilityCredentialCanary,
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.Outcome != tc.want {
				t.Fatalf("outcome = %q, want %q", result.Outcome, tc.want)
			}
			public, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(public, []byte(capabilityCredentialCanary)) || bytes.Contains(public, []byte("401")) {
				t.Fatalf("prospective result leaked probe detail: %s", public)
			}
		})
	}
}

func TestRegisterCapabilitiesRoutesProviderTestKeepsMutationProtectionAndRedactsFailures(t *testing.T) {
	providers := &fakeCapabilityProviders{testErr: errors.New("provider returned 401 for " + capabilityCredentialCanary)}
	mux := http.NewServeMux()
	RegisterCapabilitiesRoutes(mux, CapabilitiesRouteConfig{
		Service:  &Capabilities{Providers: providers},
		Mutation: func(_ int64, next http.Handler) http.Handler { return next },
	})
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/desk/providers/primary/test", strings.NewReader(`{}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), capabilityCredentialCanary) || !strings.Contains(response.Body.String(), "authentication_failed") {
		t.Fatalf("unsafe or unclassified response: %s", response.Body.String())
	}
}

func TestRegisterCapabilitiesRoutesProspectiveProviderTestUsesEnteredInputs(t *testing.T) {
	providers := &fakeCapabilityProviders{}
	mux := http.NewServeMux()
	RegisterCapabilitiesRoutes(mux, CapabilitiesRouteConfig{
		Service:  &Capabilities{Providers: providers},
		Mutation: func(_ int64, next http.Handler) http.Handler { return next },
	})
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(
		http.MethodPost,
		"/api/v1/desk/providers/test",
		strings.NewReader(`{"connection_name":"new-provider","type":"openai-compatible","base_url":"https://gateway.example/v1","max_tokens":321,"model":"vendor/model","api_key":"`+capabilityCredentialCanary+`"}`),
	))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	want := providerconfig.ProspectiveProbeRequest{
		ConnectionName: "new-provider",
		Connection: config.ProviderConnection{
			Type:      "openai",
			BaseURL:   "https://gateway.example/v1",
			MaxTokens: 321,
		},
		Model:  "vendor/model",
		APIKey: capabilityCredentialCanary,
	}
	if providers.lastProspective != want {
		t.Fatalf("prospective request = %#v, want %#v", providers.lastProspective, want)
	}
	if strings.Contains(response.Body.String(), capabilityCredentialCanary) {
		t.Fatalf("response leaked credential: %s", response.Body.String())
	}
}

func TestRegisterCapabilitiesRoutesCatalogueAddPreservesExactModelIDAndConnection(t *testing.T) {
	providers := &fakeCapabilityProviders{}
	mux := http.NewServeMux()
	RegisterCapabilitiesRoutes(mux, CapabilitiesRouteConfig{
		Service:  &Capabilities{Providers: providers},
		Mutation: func(_ int64, next http.Handler) http.Handler { return next },
		Restart:  fakeRestartScheduler{},
	})
	response := newAfterResponseRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(
		http.MethodPost,
		"/api/v1/desk/models",
		strings.NewReader(`{"connection_name":"router","alias":"claude-opus","upstream_model":"accounts/fireworks/models/claude-opus-4-6"}`),
	))
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	if providers.lastAdd.ConnectionName != "router" || providers.lastAdd.UpstreamModel != "accounts/fireworks/models/claude-opus-4-6" {
		t.Fatalf("add request = %#v", providers.lastAdd)
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

func TestWorkspaceCapabilitySkillsRepairsCommittedProvenanceAcrossProcessRestart(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(t.Context(), filepath.Join(root, "state", "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	failedStore, err := store.Open(t.Context(), filepath.Join(root, "failed", "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := failedStore.Close(); err != nil {
		t.Fatal(err)
	}

	workspace := memory.Workspace{Dir: filepath.Join(root, "workspace"), Agent: "main"}
	imports := filepath.Join(root, "imports")
	source := filepath.Join(imports, "durable-review")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "SKILL.md"), []byte(
		"---\nname: durable-review\ndescription: Durable reviewed install.\n---\n\n# Durable review\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}
	stages := filepath.Join(root, "private-stages")
	first := &WorkspaceCapabilitySkills{
		DB:        failedStore.DB,
		Workspace: workspace,
		Installer: skillinstall.New(workspace.SkillsDir(), stages, []string{imports}, []string{"github.com"}),
	}
	manifest, err := first.Stage(t.Context(), skillinstall.StageRequest{LocalPath: source})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := first.Install(t.Context(), manifest.StageID, manifest.ContentDigest); err == nil {
		t.Fatal("first install succeeded despite unavailable provenance database")
	}
	if _, err := os.Stat(filepath.Join(workspace.SkillsDir(), manifest.Name, "SKILL.md")); err != nil {
		t.Fatalf("skill commit was not retained for repair: %v", err)
	}

	restarted := &WorkspaceCapabilitySkills{
		DB:        st.DB,
		Workspace: workspace,
		Installer: skillinstall.New(workspace.SkillsDir(), stages, []string{imports}, []string{"github.com"}),
	}
	repaired, err := restarted.Install(t.Context(), manifest.StageID, manifest.ContentDigest)
	if err != nil {
		t.Fatalf("repair after process restart: %v", err)
	}
	public, err := json.Marshal(repaired)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(public, []byte(`"install_disposition":"committed_with_provenance_repair"`)) {
		t.Fatalf("repair result did not distinguish committed repair: %s", public)
	}
	if repaired.Active {
		t.Fatal("repaired install was activated")
	}
	var status, sourceRef, digest string
	if err := st.DB.QueryRowContext(t.Context(), `
		SELECT status, source_ref, content_digest
		FROM skill_status WHERE name = ?`, manifest.Name).Scan(&status, &sourceRef, &digest); err != nil {
		t.Fatal(err)
	}
	if status != skill.StatusInactive || sourceRef != manifest.SourceRef || digest != manifest.ContentDigest {
		t.Fatalf("repaired provenance status=%q source=%q digest=%q", status, sourceRef, digest)
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
	var body capabilityMutationResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.RestartRequired || body.Restart == nil || body.Restart.Code != RestartCodeScheduled {
		t.Fatalf("response body=%#v", body)
	}
	if bytes.Contains(response.Body.Bytes(), []byte("transaction_id")) ||
		bytes.Contains(response.Body.Bytes(), []byte("txn-1")) {
		t.Fatalf("browser response must omit transaction_id: %s", response.Body.String())
	}
}

func TestCapabilitiesRestartOutcomeIsSanitizedAndOperatorActionable(t *testing.T) {
	tests := []struct {
		name                string
		scheduler           RestartScheduler
		wantScheduled       bool
		wantCode            string
		wantMessage         string
		wantResponseCode    string
		wantResponseSched   bool
		wantResponseMessage string
	}{
		{
			name:                "managed schedule succeeds",
			scheduler:           fakeRestartScheduler{},
			wantScheduled:       true,
			wantCode:            RestartCodeScheduled,
			wantMessage:         restartMessageScheduled,
			wantResponseCode:    RestartCodeScheduled,
			wantResponseSched:   true,
			wantResponseMessage: restartMessageScheduled,
		},
		{
			name:                "standalone requires operator restart",
			scheduler:           StandaloneRestartScheduler{},
			wantScheduled:       false,
			wantCode:            RestartCodeManualRestartRequired,
			wantMessage:         restartMessageManual,
			wantResponseCode:    RestartCodeManualRestartRequired,
			wantResponseSched:   false,
			wantResponseMessage: restartMessageManual,
		},
		{
			name:          "scheduler failure is sanitized",
			scheduler:     failingRestartScheduler{err: errors.New("systemd exposed " + capabilityCredentialCanary)},
			wantScheduled: false,
			wantCode:      RestartCodeScheduleFailed,
			wantMessage:   restartMessageFailed,
			// Failure is only known after Schedule; response is optimistically scheduled.
			wantResponseCode:    RestartCodeScheduled,
			wantResponseSched:   true,
			wantResponseMessage: restartMessageScheduled,
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
			var body capabilityMutationResponse
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if !body.RestartRequired || body.Restart == nil {
				t.Fatalf("response missing restart outcome: %#v", body)
			}
			if body.Restart.Code != tt.wantResponseCode ||
				body.Restart.Scheduled != tt.wantResponseSched ||
				body.Restart.Message != tt.wantResponseMessage {
				t.Fatalf("response restart = %#v", body.Restart)
			}
			if bytes.Contains(response.Body.Bytes(), []byte(capabilityCredentialCanary)) {
				t.Fatalf("response leaked credential: %s", response.Body.String())
			}
			// AC3: transaction IDs and host detail stay off the browser response.
			if bytes.Contains(response.Body.Bytes(), []byte("txn-private-diagnostic")) ||
				bytes.Contains(response.Body.Bytes(), []byte("transaction_id")) {
				t.Fatalf("response leaked transaction_id: %s", response.Body.String())
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

func TestCapabilitiesRestartOutcomeIsDeliveredToEventHub(t *testing.T) {
	tests := []struct {
		name          string
		scheduler     RestartScheduler
		wantScheduled bool
		wantCode      string
		wantMessage   string
	}{
		{
			name:          "scheduled",
			scheduler:     fakeRestartScheduler{},
			wantScheduled: true,
			wantCode:      RestartCodeScheduled,
			wantMessage:   restartMessageScheduled,
		},
		{
			name:          "manual",
			scheduler:     StandaloneRestartScheduler{},
			wantScheduled: false,
			wantCode:      RestartCodeManualRestartRequired,
			wantMessage:   restartMessageManual,
		},
		{
			name:          "failed",
			scheduler:     failingRestartScheduler{err: errors.New("systemctl leaked " + capabilityCredentialCanary)},
			wantScheduled: false,
			wantCode:      RestartCodeScheduleFailed,
			wantMessage:   restartMessageFailed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			security := mustSecurity(t, "127.0.0.1:8422")
			store := NewIdempotencyStore(nil, 8, time.Minute)
			hub := NewEventHub(8)
			providers := &fakeCapabilityProviders{
				result: providerconfig.MutationResult{
					RestartRequired: true,
					TransactionID:   "txn-private-diagnostic",
				},
			}
			mux := http.NewServeMux()
			RegisterCapabilitiesRoutes(mux, CapabilitiesRouteConfig{
				Service: &Capabilities{Providers: providers},
				Mutation: func(limit int64, next http.Handler) http.Handler {
					return NewMutationHandler(
						security,
						store,
						limit,
						next,
						composeRestartOutcomeObservers(hub, nil),
					)
				},
				Restart: tt.scheduler,
			})

			request := httptest.NewRequest(
				http.MethodPost,
				"http://127.0.0.1:8422/api/v1/desk/models/default",
				strings.NewReader(`{"alias":"gpt"}`),
			)
			request.Host = "127.0.0.1:8422"
			request.Header.Set("X-Waffle-Desk-Token", security.Token())
			request.Header.Set("Idempotency-Key", "restart-delivery-"+tt.name)
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, request)

			if response.Code != http.StatusAccepted {
				t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
			}

			events, resync := hub.Subscribe(0)
			if resync {
				t.Fatal("hub required resync with empty history")
			}
			defer hub.Unsubscribe(events)

			select {
			case event := <-events:
				if event.Type != EventTypeCapabilityRestartOutcome || event.Resource != "capability" {
					t.Fatalf("event = %#v", event)
				}
				var outcome RestartScheduleOutcome
				if err := json.Unmarshal(event.Data, &outcome); err != nil {
					t.Fatal(err)
				}
				if outcome.Scheduled != tt.wantScheduled ||
					outcome.Code != tt.wantCode ||
					outcome.Message != tt.wantMessage {
					t.Fatalf("delivered outcome = %#v", outcome)
				}
				if bytes.Contains(event.Data, []byte(capabilityCredentialCanary)) ||
					bytes.Contains(event.Data, []byte("txn-private-diagnostic")) ||
					bytes.Contains(event.Data, []byte("systemctl")) {
					t.Fatalf("event leaked host or private detail: %s", event.Data)
				}
			default:
				t.Fatal("expected capability.restart_outcome on the event hub")
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

func TestWriteCapabilityErrorMapsKnownSentinels(t *testing.T) {
	// Every table entry must map to a distinct stable code with the
	// declared status. Messages are fixed mapper strings (never upstream
	// text). Unmapped errors keep the generic capability_failed fallback.
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"session_not_found", session.ErrNotFound, http.StatusNotFound, "session_not_found"},
		{"model_not_found", ErrCapabilityModelNotFound, http.StatusNotFound, "model_not_found"},
		{"skill_not_found", ErrCapabilitySkillNotFound, http.StatusNotFound, "skill_not_found"},
		{"after_response_unavailable", ErrAfterResponseUnavailable, http.StatusServiceUnavailable, "capabilities_unavailable"},
		{"capabilities_unavailable", ErrCapabilitiesUnavailable, http.StatusServiceUnavailable, "capabilities_unavailable"},

		{"skill_invalid_request", skillinstall.ErrInvalidRequest, http.StatusUnprocessableEntity, "skill_request_invalid"},
		{"skill_source_not_allowed", skillinstall.ErrSourceNotAllowed, http.StatusForbidden, "skill_source_not_allowed"},
		{"skill_git_host_not_allowed", skillinstall.ErrGitHostNotAllowed, http.StatusForbidden, "skill_git_host_not_allowed"},
		{"skill_commit_required", skillinstall.ErrCommitRequired, http.StatusUnprocessableEntity, "skill_commit_required"},
		{"skill_git_archive_unsupported", skillinstall.ErrBoundedGitUnsupported, http.StatusUnprocessableEntity, "skill_git_archive_unsupported"},
		{"skill_commit_mismatch", skillinstall.ErrCommitMismatch, http.StatusUnprocessableEntity, "skill_commit_mismatch"},
		{"skill_tree_unsafe", skillinstall.ErrUnsafeTree, http.StatusUnprocessableEntity, "skill_tree_unsafe"},
		{"skill_tree_too_large", skillinstall.ErrTreeTooLarge, http.StatusUnprocessableEntity, "skill_tree_too_large"},
		{"skill_audit_failed", skillinstall.ErrAuditFailed, http.StatusUnprocessableEntity, "skill_audit_failed"},
		{"skill_already_installed", skillinstall.ErrSkillExists, http.StatusConflict, "skill_already_installed"},
		{"skill_stage_not_found", skillinstall.ErrStageNotFound, http.StatusNotFound, "skill_stage_not_found"},
		{"skill_stage_expired", skillinstall.ErrStageExpired, http.StatusConflict, "skill_stage_expired"},
		{"skill_stage_changed", skillinstall.ErrStageChanged, http.StatusConflict, "skill_stage_changed"},
		{"skill_digest_mismatch", skillinstall.ErrDigestMismatch, http.StatusConflict, "skill_digest_mismatch"},
		{"skill_install_unsupported", skillinstall.ErrAtomicRenameUnsupported, http.StatusServiceUnavailable, "skill_install_unsupported"},

		{"provider_locked", providerconfig.ErrLocked, http.StatusConflict, "provider_locked"},
		{"provider_referenced", providerconfig.ErrReferenced, http.StatusConflict, "provider_referenced"},
		{"provider_restart_pending", providerconfig.ErrDeferredRestartPending, http.StatusConflict, "provider_restart_pending"},
		{"provider_restart_health_failed", providerconfig.ErrDeferredHealth, http.StatusBadGateway, "provider_restart_health_failed"},
		{"provider_restart_integrity_failed", providerconfig.ErrDeferredIntegrity, http.StatusConflict, "provider_restart_integrity_failed"},

		{"wrapped_skill_exists", fmt.Errorf("install: %w", skillinstall.ErrSkillExists), http.StatusConflict, "skill_already_installed"},
		{"unknown_fallback", errors.New("probe failed: secret sk-super-private"), http.StatusBadRequest, "capability_failed"},
	}

	// Codes for mapped sentinels (excluding the intentional shared codes and
	// the generic fallback) must be unique so the UI can branch on them.
	sharedOK := map[string]bool{
		"capabilities_unavailable": true,
		"capability_failed":        true,
	}
	seenCodes := map[string]string{}
	for _, tc := range cases {
		if sharedOK[tc.code] {
			continue
		}
		if prev, ok := seenCodes[tc.code]; ok && prev != tc.name {
			// Allow the same code only when testing wrapped variants of the
			// same sentinel class (name prefix match).
			if !strings.HasPrefix(tc.name, "wrapped_") {
				t.Fatalf("code %q used by both %q and %q", tc.code, prev, tc.name)
			}
		}
		if !strings.HasPrefix(tc.name, "wrapped_") {
			seenCodes[tc.code] = tc.name
		}
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			writeCapabilityError(recorder, tc.err)

			if recorder.Code != tc.status {
				t.Fatalf("status = %d, want %d body=%s", recorder.Code, tc.status, recorder.Body.String())
			}
			var body errorResponse
			if err := json.NewDecoder(recorder.Body).Decode(&body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if body.Code != tc.code {
				t.Fatalf("code = %q, want %q", body.Code, tc.code)
			}
			if body.Message == "" {
				t.Fatal("message is empty")
			}
			// Redaction: never echo upstream material (credentials, secrets).
			if strings.Contains(body.Message, capabilityCredentialCanary) {
				t.Fatalf("message leaked credential material: %q", body.Message)
			}
			if strings.Contains(body.Message, "probe failed") {
				t.Fatalf("message leaked upstream error text: %q", body.Message)
			}
			// Mapped messages must match the table's fixed string, not err.Error().
			if tc.code != "capability_failed" && body.Message == tc.err.Error() {
				t.Fatalf("message echoed raw error: %q", body.Message)
			}
		})
	}
}

func TestWriteCapabilityErrorTableCoversDeclaredSentinels(t *testing.T) {
	// Guard against the mapper table drifting from the issue's required
	// skillinstall and providerconfig sentinels.
	required := []error{
		skillinstall.ErrSourceNotAllowed,
		skillinstall.ErrGitHostNotAllowed,
		skillinstall.ErrCommitRequired,
		skillinstall.ErrCommitMismatch,
		skillinstall.ErrUnsafeTree,
		skillinstall.ErrTreeTooLarge,
		skillinstall.ErrAuditFailed,
		skillinstall.ErrSkillExists,
		skillinstall.ErrStageExpired,
		skillinstall.ErrStageChanged,
		skillinstall.ErrDigestMismatch,
		providerconfig.ErrLocked,
		providerconfig.ErrReferenced,
		providerconfig.ErrDeferredRestartPending,
		providerconfig.ErrDeferredHealth,
		providerconfig.ErrDeferredIntegrity,
	}
	for _, want := range required {
		found := false
		for _, mapping := range capabilityErrorMappings {
			if mapping.err == want {
				found = true
				if mapping.code == "" || mapping.message == "" || mapping.status == 0 {
					t.Fatalf("incomplete mapping for %v", want)
				}
				break
			}
		}
		if !found {
			t.Fatalf("capabilityErrorMappings missing %v", want)
		}
	}
}

type fakeCapabilityProviders struct {
	snapshot        providerconfig.Listing
	snapshotErrs    []error
	snapshotCalls   int
	result          providerconfig.MutationResult
	mutationErr     error
	mutations       int
	lastAPIKey      string
	testErr         error
	lastProspective providerconfig.ProspectiveProbeRequest
	prospectiveErr  error
	lastAdd         providerconfig.AddModelRequest
}

func (f *fakeCapabilityProviders) Snapshot(context.Context) (providerconfig.Listing, error) {
	f.snapshotCalls++
	if len(f.snapshotErrs) > 0 {
		err := f.snapshotErrs[0]
		f.snapshotErrs = f.snapshotErrs[1:]
		return providerconfig.Listing{}, err
	}
	return f.snapshot, nil
}

func (f *fakeCapabilityProviders) AddWithMode(_ context.Context, request providerconfig.AddRequest, _ providerconfig.CommitMode) (providerconfig.MutationResult, error) {
	f.mutations++
	f.lastAPIKey = request.APIKey
	return f.mutationResult()
}

func (f *fakeCapabilityProviders) AddModelWithMode(_ context.Context, request providerconfig.AddModelRequest, _ providerconfig.CommitMode) (providerconfig.MutationResult, error) {
	f.mutations++
	f.lastAdd = request
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

func (f *fakeCapabilityProviders) Test(context.Context, string) error { return f.testErr }

func (f *fakeCapabilityProviders) TestProspective(_ context.Context, request providerconfig.ProspectiveProbeRequest) error {
	f.lastProspective = request
	return f.prospectiveErr
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
