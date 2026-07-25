package dashboard

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/matt-riley/waffle/internal/config"
)

type stubSetupIdentity struct {
	configured bool
	err        error
	probes     int
}

func (s *stubSetupIdentity) IdentityConfigured() (bool, error) {
	s.probes++
	return s.configured, s.err
}

type stubSetupCreator struct {
	err   error
	calls int
	// created flips the paired probe, mirroring a real keyring write.
	probe *stubSetupIdentity
}

func (s *stubSetupCreator) CreateIdentity() error {
	s.calls++
	if s.err != nil {
		return s.err
	}
	if s.probe != nil {
		s.probe.configured = true
	}
	return nil
}

// fullyConfigured is the config a completed `waffle setup` leaves behind.
func fullyConfigured() config.Config {
	return config.Config{
		Providers: map[string]config.ProviderConnection{
			"anthropic": {Type: "anthropic", APIKey: "secret://provider/anthropic/api-key", MaxTokens: 64000},
		},
		Models: map[string]config.ModelTarget{
			"main":  {Provider: "anthropic", Model: "claude-opus-4-8"},
			"cheap": {Provider: "anthropic", Model: "claude-haiku-4-5"},
		},
		Agent: config.Agent{
			DefaultModel: "main",
			UtilityModel: "cheap",
			Profiles: map[string]config.AgentProfile{
				"main": {System: config.DefaultMainSystemPrompt, Model: "default", Sandbox: "host"},
			},
		},
		Dashboard: config.Dashboard{Enabled: true},
	}
}

func stepByID(t *testing.T, view SetupView, id string) SetupStepView {
	t.Helper()
	for _, step := range view.Steps {
		if step.ID == id {
			return step
		}
	}
	t.Fatalf("setup view has no step %q", id)
	return SetupStepView{}
}

func TestSetupReadProjectsInstallStates(t *testing.T) {
	misconfiguredDefault := fullyConfigured()
	// A default alias naming a model that is not enrolled is not rejected by
	// config.Load when it arrives through a programmatic config, and it is the
	// exact case AC1 calls out.
	misconfiguredDefault.Agent.DefaultModel = "gone"

	misconfiguredProfile := fullyConfigured()
	misconfiguredProfile.Agent.Profiles = map[string]config.AgentProfile{
		"main": {System: config.DefaultMainSystemPrompt, Model: "gone"},
	}

	noUtility := fullyConfigured()
	noUtility.Agent.UtilityModel = ""

	tests := []struct {
		name       string
		cfg        config.Config
		identity   *stubSetupIdentity
		creator    SetupIdentityCreator
		complete   bool
		wantStates map[string]string
		wantAction map[string]string
	}{
		{
			name:     "fresh install reports every prerequisite missing",
			cfg:      config.Config{Dashboard: config.Dashboard{Enabled: true}},
			identity: &stubSetupIdentity{},
			creator:  &stubSetupCreator{},
			wantStates: map[string]string{
				SetupStepIdentity:  SetupStateMissing,
				SetupStepProvider:  SetupStateMissing,
				SetupStepModels:    SetupStateMissing,
				SetupStepProfile:   SetupStateMissing,
				SetupStepDashboard: SetupStateConfigured,
			},
			wantAction: map[string]string{
				SetupStepIdentity: SetupActionCreateIdentity,
				SetupStepProvider: SetupActionEnrollProvider,
				SetupStepModels:   SetupActionEnrollProvider,
				SetupStepProfile:  SetupActionCreateProfile,
			},
		},
		{
			name: "partial install reports the enrolled provider and the missing default",
			cfg: config.Config{
				Providers: map[string]config.ProviderConnection{
					"anthropic": {Type: "anthropic", APIKey: "secret://provider/anthropic/api-key"},
				},
				Models:    map[string]config.ModelTarget{"main": {Provider: "anthropic", Model: "claude-opus-4-8"}},
				Dashboard: config.Dashboard{Enabled: true},
			},
			identity: &stubSetupIdentity{configured: true},
			creator:  &stubSetupCreator{},
			wantStates: map[string]string{
				SetupStepIdentity:  SetupStateConfigured,
				SetupStepProvider:  SetupStateConfigured,
				SetupStepModels:    SetupStateMissing,
				SetupStepProfile:   SetupStateMissing,
				SetupStepDashboard: SetupStateConfigured,
			},
			wantAction: map[string]string{
				SetupStepModels:  SetupActionSetDefaultModel,
				SetupStepProfile: SetupActionCreateProfile,
			},
		},
		{
			name:     "fully configured install is complete",
			cfg:      fullyConfigured(),
			identity: &stubSetupIdentity{configured: true},
			creator:  &stubSetupCreator{},
			complete: true,
			wantStates: map[string]string{
				SetupStepIdentity:  SetupStateConfigured,
				SetupStepProvider:  SetupStateConfigured,
				SetupStepModels:    SetupStateConfigured,
				SetupStepProfile:   SetupStateConfigured,
				SetupStepDashboard: SetupStateConfigured,
			},
		},
		{
			name:     "an unset utility model is not a defect",
			cfg:      noUtility,
			identity: &stubSetupIdentity{configured: true},
			creator:  &stubSetupCreator{},
			complete: true,
			wantStates: map[string]string{
				SetupStepModels: SetupStateConfigured,
			},
		},
		{
			name:     "a default alias with no enrolled model is misconfigured",
			cfg:      misconfiguredDefault,
			identity: &stubSetupIdentity{configured: true},
			creator:  &stubSetupCreator{},
			wantStates: map[string]string{
				SetupStepModels: SetupStateMisconfigured,
			},
			wantAction: map[string]string{
				SetupStepModels: SetupActionSetDefaultModel,
			},
		},
		{
			name:     "a main profile naming an unknown model is misconfigured",
			cfg:      misconfiguredProfile,
			identity: &stubSetupIdentity{configured: true},
			creator:  &stubSetupCreator{},
			wantStates: map[string]string{
				SetupStepProfile: SetupStateMisconfigured,
			},
			wantAction: map[string]string{
				SetupStepProfile: SetupActionCreateProfile,
			},
		},
		{
			name:     "an unreadable secret store is misconfigured, not missing",
			cfg:      fullyConfigured(),
			identity: &stubSetupIdentity{err: errors.New("keyring unavailable: dbus")},
			creator:  &stubSetupCreator{},
			wantStates: map[string]string{
				SetupStepIdentity: SetupStateMisconfigured,
			},
		},
		{
			name:     "without a creator the identity step states the command",
			cfg:      config.Config{Dashboard: config.Dashboard{Enabled: true}},
			identity: &stubSetupIdentity{},
			wantStates: map[string]string{
				SetupStepIdentity: SetupStateMissing,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := NewSetupService(test.cfg, test.identity, test.creator)
			view, err := service.Read()
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if view.Complete != test.complete {
				t.Fatalf("Complete = %v, want %v", view.Complete, test.complete)
			}
			if len(view.Steps) != 5 {
				t.Fatalf("Steps = %d, want 5", len(view.Steps))
			}
			for id, want := range test.wantStates {
				if got := stepByID(t, view, id).State; got != want {
					t.Errorf("step %q state = %q, want %q", id, got, want)
				}
			}
			for id, want := range test.wantAction {
				if got := stepByID(t, view, id).Action; got != want {
					t.Errorf("step %q action = %q, want %q", id, got, want)
				}
			}
			for _, step := range view.Steps {
				if step.Detail == "" || step.Title == "" {
					t.Errorf("step %q is missing operator-facing text", step.ID)
				}
				// AC2: an unsatisfied step must offer either an in-Desk action
				// or the exact command, never neither.
				if step.State != SetupStateConfigured && step.Action == "" && step.Command == "" {
					t.Errorf("step %q offers no action and no command", step.ID)
				}
			}
		})
	}
}

func TestSetupReadRequiresAnIdentityProbe(t *testing.T) {
	service := NewSetupService(fullyConfigured(), nil, nil)
	if _, err := service.Read(); !errors.Is(err, ErrSetupUnavailable) {
		t.Fatalf("Read err = %v, want ErrSetupUnavailable", err)
	}
	var nilService *SetupService
	if _, err := nilService.Read(); !errors.Is(err, ErrSetupUnavailable) {
		t.Fatalf("nil Read err = %v, want ErrSetupUnavailable", err)
	}
}

func TestSetupCreateIdentityRefusesToOverwrite(t *testing.T) {
	probe := &stubSetupIdentity{configured: true}
	creator := &stubSetupCreator{probe: probe}
	service := NewSetupService(config.Config{}, probe, creator)
	if err := service.CreateIdentity(); !errors.Is(err, ErrSetupIdentityExists) {
		t.Fatalf("CreateIdentity err = %v, want ErrSetupIdentityExists", err)
	}
	if creator.calls != 0 {
		t.Fatalf("creator was called %d times, want 0", creator.calls)
	}
}

func TestSetupCreateIdentityRedactsFailures(t *testing.T) {
	probe := &stubSetupIdentity{}
	creator := &stubSetupCreator{err: errors.New("store identity in OS keyring: /home/owner/.dbus failed")}
	service := NewSetupService(config.Config{}, probe, creator)
	err := service.CreateIdentity()
	if !errors.Is(err, ErrSetupIdentityUnavailable) {
		t.Fatalf("CreateIdentity err = %v, want ErrSetupIdentityUnavailable", err)
	}
	if bytes.Contains([]byte(err.Error()), []byte("/home/owner")) {
		t.Fatalf("CreateIdentity error leaked a host path: %v", err)
	}
}

func TestSetupCreateIdentitySucceedsAndFlipsTheStep(t *testing.T) {
	probe := &stubSetupIdentity{}
	creator := &stubSetupCreator{probe: probe}
	service := NewSetupService(config.Config{Dashboard: config.Dashboard{Enabled: true}}, probe, creator)
	if err := service.CreateIdentity(); err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}
	view, err := service.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got := stepByID(t, view, SetupStepIdentity).State; got != SetupStateConfigured {
		t.Fatalf("identity state = %q, want %q", got, SetupStateConfigured)
	}
}

func TestSetupHandlerServesTheChecklist(t *testing.T) {
	mux := http.NewServeMux()
	RegisterSetupRoutes(mux, SetupRouteConfig{
		Service: NewSetupService(fullyConfigured(), &stubSetupIdentity{configured: true}, nil),
	})

	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/desk/setup", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	var view SetupView
	if err := json.Unmarshal(recorder.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !view.Complete {
		t.Fatalf("Complete = false, want true for a configured install")
	}
	// AC4: the projection carries no key material of any kind.
	for _, forbidden := range []string{"AGE-SECRET-KEY", "secret://", "api_key"} {
		if bytes.Contains(recorder.Body.Bytes(), []byte(forbidden)) {
			t.Fatalf("setup response contains %q: %s", forbidden, recorder.Body.String())
		}
	}
}

func TestSetupIdentityRouteIsNotMountedWithoutAMutationGuard(t *testing.T) {
	mux := http.NewServeMux()
	RegisterSetupRoutes(mux, SetupRouteConfig{
		Service: NewSetupService(config.Config{}, &stubSetupIdentity{}, &stubSetupCreator{}),
	})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/desk/setup/identity", bytes.NewBufferString("{}"))
	mux.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusMethodNotAllowed && recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want the identity action to be unmounted", recorder.Code)
	}
}
