package dashboard

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/matt-riley/waffle/internal/config"
)

// Setup is Desk's bootstrap surface (#192). Desk otherwise presupposes a
// configured Waffle: without a provider the chat runtime refuses outright, so a
// new owner who reaches Desk first has no way to learn what is missing.
//
// This surface reports each prerequisite `waffle setup` walks, in the same
// order, and points at the in-Desk control that satisfies it. Two properties
// are load-bearing:
//
//   - It projects the configuration this process loaded, exactly like posture
//     (#193) and the profile editor (#194). Every Desk mutation is
//     restart-deferred, so a checklist reading a newer config.toml would
//     disagree with the profile list rendered beside it.
//   - It never becomes a second credential channel (#192 AC4). Provider
//     credentials keep travelling only through the capabilities mutation
//     boundary, and the one action mounted here — creating the secret-store
//     identity — returns no key material at all.
const (
	// SetupStateConfigured means the prerequisite is satisfied.
	SetupStateConfigured = "configured"
	// SetupStateMissing means it was never configured.
	SetupStateMissing = "missing"
	// SetupStateMisconfigured means it was configured but does not resolve —
	// a default alias naming an unknown provider, an unreadable keyring.
	SetupStateMisconfigured = "misconfigured"
)

// Setup step identifiers. These are stable contracts: the browser keys its
// inline actions off them.
const (
	SetupStepIdentity  = "identity"
	SetupStepProvider  = "provider"
	SetupStepModels    = "models"
	SetupStepProfile   = "profile"
	SetupStepDashboard = "dashboard"
)

// Setup action identifiers, naming an in-Desk control rather than a command.
const (
	// SetupActionCreateIdentity is served by this surface itself.
	SetupActionCreateIdentity = "create-identity"
	// SetupActionEnrollProvider focuses the guided enrollment form (#175).
	SetupActionEnrollProvider = "enroll-provider"
	// SetupActionSetDefaultModel focuses the Waffle-wide role controls.
	SetupActionSetDefaultModel = "set-default-model"
	// SetupActionCreateProfile opens the profile editor on a starter profile (#194).
	SetupActionCreateProfile = "create-profile"
)

const setupMutationMaxBodyBytes = 4 << 10

var (
	// ErrSetupUnavailable is returned when the identity probe is not wired.
	ErrSetupUnavailable = errors.New("setup surface unavailable")
	// ErrSetupIdentityExists is returned when an identity is already present.
	// Creating a second one would orphan every secret encrypted to the first.
	ErrSetupIdentityExists = errors.New("secret-store identity already exists")
	// ErrSetupIdentityUnavailable is returned when the identity could not be
	// created — typically a headless host with no usable OS keyring.
	ErrSetupIdentityUnavailable = errors.New("secret-store identity could not be created")
)

// SetupStepView is one prerequisite, its state, and how to satisfy it. Exactly
// one of Action and Command is set: Desk offers the action when it can do the
// work safely, and otherwise states the command that can (#192 AC2).
type SetupStepView struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	State string `json:"state"`
	// Detail is fixed operator-facing prose. It never quotes config values,
	// paths, or upstream error text.
	Detail      string `json:"detail"`
	Action      string `json:"action,omitempty"`
	ActionLabel string `json:"action_label,omitempty"`
	Command     string `json:"command,omitempty"`
}

// SetupView is the complete prerequisite projection.
type SetupView struct {
	// Complete is true only when every step is configured, so the browser can
	// collapse the checklist without re-deriving the rule.
	Complete bool            `json:"complete"`
	Steps    []SetupStepView `json:"steps"`
}

// SetupMutationResponse reports a committed setup action. Identity creation
// changes no config file, but the running process resolved its secret-store
// identity at start, so it still takes a restart to take effect.
type SetupMutationResponse struct {
	RestartRequired bool                    `json:"restart_required"`
	Restart         *RestartScheduleOutcome `json:"restart,omitempty"`
}

// SetupIdentityProbe reports whether the secret store has an identity. It is
// implemented in cmd/waffle over secret.LoadIdentity, which keeps the OS
// keyring dependency out of this package and makes the projection testable.
type SetupIdentityProbe interface {
	// IdentityConfigured returns (true, nil) when an identity resolves,
	// (false, nil) when none is configured, and (false, err) when the store
	// could not be consulted at all.
	IdentityConfigured() (bool, error)
}

// SetupIdentityCreator creates the secret-store identity. Implementations must
// never return the identity itself: the value stays in the OS keyring, and the
// owner backs it up with `waffle secret export-identity` (#192 AC4).
type SetupIdentityCreator interface {
	CreateIdentity() error
}

// SetupService projects setup prerequisites from a config snapshot.
type SetupService struct {
	cfg      config.Config
	identity SetupIdentityProbe
	creator  SetupIdentityCreator
}

// NewSetupService builds the projection. creator may be nil, in which case the
// identity step reports the CLI command instead of offering an action.
func NewSetupService(cfg config.Config, identity SetupIdentityProbe, creator SetupIdentityCreator) *SetupService {
	return &SetupService{cfg: cfg, identity: identity, creator: creator}
}

func (s *SetupService) ready() bool { return s != nil && s.identity != nil }

// Read projects every prerequisite. It reports what it could not determine
// rather than guessing: a keyring that cannot be read is "misconfigured", not
// "missing", because the two call for opposite actions.
func (s *SetupService) Read() (SetupView, error) {
	if !s.ready() {
		return SetupView{}, ErrSetupUnavailable
	}
	steps := []SetupStepView{
		s.identityStep(),
		s.providerStep(),
		s.modelStep(),
		s.profileStep(),
		s.dashboardStep(),
	}
	view := SetupView{Complete: true, Steps: steps}
	for _, step := range steps {
		if step.State != SetupStateConfigured {
			view.Complete = false
			break
		}
	}
	return view, nil
}

func (s *SetupService) identityStep() SetupStepView {
	step := SetupStepView{
		ID:    SetupStepIdentity,
		Title: "Secret-store identity",
	}
	configured, err := s.identity.IdentityConfigured()
	switch {
	case err != nil:
		step.State = SetupStateMisconfigured
		step.Detail = "The secret store could not be consulted. On a headless host, create an identity with " +
			"`waffle secret init --print` and supply it as WAFFLE_AGE_IDENTITY."
		step.Command = "waffle secret init --print"
	case configured:
		step.State = SetupStateConfigured
		step.Detail = "An identity unlocks the encrypted secret store. Back it up with `waffle secret export-identity`."
	case s.creator != nil:
		step.State = SetupStateMissing
		step.Detail = "Provider credentials cannot be stored until an identity exists. " +
			"Creating it here keeps the key in the OS keyring; it is never shown in the browser. " +
			"Back it up afterwards with `waffle secret export-identity`."
		step.Action = SetupActionCreateIdentity
		step.ActionLabel = "Create identity"
	default:
		step.State = SetupStateMissing
		step.Detail = "Provider credentials cannot be stored until an identity exists."
		step.Command = "waffle secret init"
	}
	return step
}

func (s *SetupService) providerStep() SetupStepView {
	step := SetupStepView{
		ID:    SetupStepProvider,
		Title: "Provider connection",
	}
	count := len(s.cfg.Providers)
	if count == 0 {
		step.State = SetupStateMissing
		step.Detail = "No provider connection is configured, so chat refuses to start. " +
			"Enrolling one here reads the credential straight into the secret store."
		step.Action = SetupActionEnrollProvider
		step.ActionLabel = "Enroll a provider"
		return step
	}
	step.State = SetupStateConfigured
	step.Detail = strconv.Itoa(count) + " provider " + plural(count, "connection", "connections") +
		" configured. Connection health is reported under Tools & connections."
	return step
}

// modelStep folds the default and utility roles into one prerequisite, because
// that is how they fail: a run picks the default alias, and only summarization
// picks the utility alias. An unset utility model is legitimate; an unset
// default one is not, once a registry exists.
func (s *SetupService) modelStep() SetupStepView {
	step := SetupStepView{
		ID:    SetupStepModels,
		Title: "Default and utility model",
	}
	if len(s.cfg.Models) == 0 {
		step.State = SetupStateMissing
		step.Detail = "No model alias is enrolled, so no run can resolve a model."
		step.Action = SetupActionEnrollProvider
		step.ActionLabel = "Enroll a provider"
		return step
	}
	defaultAlias := strings.TrimSpace(s.cfg.Agent.DefaultModel)
	if defaultAlias == "" {
		step.State = SetupStateMissing
		step.Detail = "Model aliases are enrolled but none is the Waffle-wide default, so a run has nothing to select."
		step.Action = SetupActionSetDefaultModel
		step.ActionLabel = "Set the default model"
		return step
	}
	if _, err := s.cfg.ResolveModel(defaultAlias); err != nil {
		step.State = SetupStateMisconfigured
		step.Detail = "The default model alias does not resolve to a configured provider connection."
		step.Action = SetupActionSetDefaultModel
		step.ActionLabel = "Choose another default"
		return step
	}
	if utility := strings.TrimSpace(s.cfg.Agent.UtilityModel); utility != "" {
		if _, err := s.cfg.ResolveModel(utility); err != nil {
			step.State = SetupStateMisconfigured
			step.Detail = "The utility model alias does not resolve to a configured provider connection."
			step.Action = SetupActionSetDefaultModel
			step.ActionLabel = "Choose another utility model"
			return step
		}
		step.State = SetupStateConfigured
		step.Detail = "A default model and a utility model both resolve."
		return step
	}
	step.State = SetupStateConfigured
	step.Detail = "The default model resolves. No separate utility model is set, so summarization uses the default."
	return step
}

// profileStep reports the owner's interactive profile. An implicit main is
// usable, but it is not what setup produces, and it leaves the owner with no
// system prompt of their own — so it is reported as missing rather than
// configured, with the same starter profile Desk can write.
func (s *SetupService) profileStep() SetupStepView {
	step := SetupStepView{
		ID:    SetupStepProfile,
		Title: "Owner profile (agent.profile.main)",
	}
	profile, explicit := s.cfg.Agent.Profiles[config.GroupMain]
	if !explicit {
		step.State = SetupStateMissing
		step.Detail = "No [agent.profile.main] is configured. Waffle falls back to an implicit default with no system prompt of its own."
		step.Action = SetupActionCreateProfile
		step.ActionLabel = "Create a starter profile"
		return step
	}
	if _, err := s.cfg.ResolveProfileModelAlias(profile); err != nil {
		step.State = SetupStateMisconfigured
		step.Detail = "The main profile names a model that does not resolve, so runs on it will fail to start."
		step.Action = SetupActionCreateProfile
		step.ActionLabel = "Edit the main profile"
		return step
	}
	step.State = SetupStateConfigured
	step.Detail = "The owner's interactive profile is configured. Its system prompt and tool policy are shown under Agent profiles."
	return step
}

// dashboardStep is always configured: a disabled dashboard cannot render this
// checklist. It is listed anyway so the prerequisite set matches `waffle
// setup`, and so the command that enables it is discoverable from the one
// place a reader might look for it (#192).
func (s *SetupService) dashboardStep() SetupStepView {
	return SetupStepView{
		ID:      SetupStepDashboard,
		Title:   "Desk enabled",
		State:   SetupStateConfigured,
		Detail:  "Desk is serving this page, so [dashboard] enabled is already true. Only the CLI can turn it back off.",
		Command: "waffle setup",
	}
}

// CreateIdentity generates the secret-store identity and discards it. The
// value is never returned, logged, or published; it lives in the OS keyring,
// and `waffle secret export-identity` is the sanctioned way to read it back.
func (s *SetupService) CreateIdentity() error {
	if !s.ready() {
		return ErrSetupUnavailable
	}
	if s.creator == nil {
		return ErrSetupUnavailable
	}
	// Re-probe under the same call rather than trusting the last projection:
	// overwriting an existing identity would orphan every stored secret.
	configured, err := s.identity.IdentityConfigured()
	if err != nil {
		return ErrSetupIdentityUnavailable
	}
	if configured {
		return ErrSetupIdentityExists
	}
	if err := s.creator.CreateIdentity(); err != nil {
		// The underlying error can name the keyring backend and the host, so
		// it stays server-side.
		return ErrSetupIdentityUnavailable
	}
	return nil
}

func plural(count int, one, many string) string {
	if count == 1 {
		return one
	}
	return many
}

// SetupRouteConfig is the additive integration seam for the setup surface.
type SetupRouteConfig struct {
	Service *SetupService
	// Mutation wraps the identity action in the shared Desk mutation guard
	// (token, idempotency key, policy_audit row). Without it the read-only
	// projection is still mounted.
	Mutation CapabilityMutationFactory
	Restart  RestartScheduler
}

// RegisterSetupRoutes mounts the bootstrap projection and its one action.
func RegisterSetupRoutes(mux *http.ServeMux, routeConfig SetupRouteConfig) {
	if mux == nil || routeConfig.Service == nil {
		return
	}
	mux.Handle("GET /api/v1/desk/setup", newSetupHandler(routeConfig.Service))
	if routeConfig.Mutation == nil {
		return
	}
	mux.Handle("POST /api/v1/desk/setup/identity", routeConfig.Mutation(
		setupMutationMaxBodyBytes,
		newSetupIdentityHandler(routeConfig),
	))
}

func newSetupHandler(service *SetupService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		view, err := service.Read()
		if err != nil {
			writeSetupError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, view)
	})
}

func newSetupIdentityHandler(routeConfig SetupRouteConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var request struct{}
		if !decodeSetupRequest(w, r, &request) {
			return
		}
		if err := routeConfig.Service.CreateIdentity(); err != nil {
			writeSetupError(w, err)
			return
		}
		response := SetupMutationResponse{RestartRequired: true}
		// The identity is resolved once at process start, so the provider
		// manager this process holds keeps refusing credential writes until it
		// restarts. Schedule that the same way every other Desk mutation does.
		if after, ok := w.(AfterResponseWriter); ok && routeConfig.Restart != nil {
			deferRestart(after, routeConfig.Restart, "")
			outcome := plannedRestartOutcome(routeConfig.Restart)
			response.Restart = &outcome
		}
		writeJSON(w, http.StatusAccepted, response)
	}
}

func decodeSetupRequest(w http.ResponseWriter, r *http.Request, target any) bool {
	return decodeStrictJSON(w, r, target, func(w http.ResponseWriter) {
		writeJSON(w, http.StatusBadRequest, errorResponse{
			Code:    "invalid_request",
			Message: "setup request is invalid",
		})
	})
}

// writeSetupError maps setup failures onto fixed, redacted responses. Keyring
// and filesystem error text never reaches the browser.
func writeSetupError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrSetupIdentityExists):
		writeJSON(w, http.StatusConflict, errorResponse{
			Code:    "identity_exists",
			Message: "a secret-store identity already exists",
		})
	case errors.Is(err, ErrSetupIdentityUnavailable):
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{
			Code: "identity_unavailable",
			Message: "the secret-store identity could not be created; " +
				"on a headless host run `waffle secret init --print` and set WAFFLE_AGE_IDENTITY",
		})
	default:
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{
			Code:    "setup_unavailable",
			Message: "setup state is unavailable",
		})
	}
}
