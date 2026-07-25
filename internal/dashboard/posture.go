package dashboard

import (
	"context"
	"database/sql"
	"net/http"
	"sort"
	"strings"

	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/policy"
)

// Posture is Desk's read-only window onto what the agent was actually told and
// what it is actually allowed to do (#193). Nothing on this surface mutates:
// editing a profile is #194 and lives behind the mutation guard.

const postureDenialLimit = 20

// PostureSystemView is a profile's system prompt and where it came from.
type PostureSystemView struct {
	// Source is "default", "inline", or "file".
	Source string `json:"source"`
	// Path is the source file relative to WAFFLE_HOME, set only for "file".
	// It is the one filesystem path this surface exposes, and it is
	// deliberately relative so the home location never leaks (#193 AC4).
	Path string `json:"path,omitempty"`
	Text string `json:"text,omitempty"`
	// Error explains a prompt that could not be resolved, in redacted text.
	Error string `json:"error,omitempty"`
}

// PostureLayerView is one tier's own contribution to the effective policy,
// kept separate from the running total so a reader can see which tier imposed
// a restriction (#193 AC2).
type PostureLayerView struct {
	Name         string   `json:"name"`
	Applied      bool     `json:"applied"`
	SandboxMode  string   `json:"sandbox_mode,omitempty"`
	Allow        []string `json:"allow"`
	Deny         []string `json:"deny"`
	DenyPrefixes []string `json:"deny_prefixes"`
	Guidance     string   `json:"guidance,omitempty"`
}

// PostureLimitsView is the profile's bounded resources.
type PostureLimitsView struct {
	Model           string   `json:"model,omitempty"`
	MaxTokens       int      `json:"max_tokens,omitempty"`
	MaxIterations   int      `json:"max_iterations,omitempty"`
	AllowedChildren []string `json:"allowed_children"`
}

// PostureView is the complete read-only posture projection for one profile.
type PostureView struct {
	Profile   string             `json:"profile"`
	Group     string             `json:"group"`
	Known     bool               `json:"known"`
	System    PostureSystemView  `json:"system"`
	Layers    []PostureLayerView `json:"layers"`
	Effective PostureLayerView   `json:"effective"`
	Limits    PostureLimitsView  `json:"limits"`
}

// PostureDenial is one recorded refusal, projected so a denied tool call can
// be traced to the rule that denied it (#193 AC3).
type PostureDenial struct {
	At      string `json:"at"`
	Tool    string `json:"tool"`
	Command string `json:"command,omitempty"`
	Rule    string `json:"rule,omitempty"`
	Verdict string `json:"verdict"`
	Detail  string `json:"detail,omitempty"`
}

type PostureDenialSnapshot struct {
	Denials []PostureDenial `json:"denials"`
}

// PostureRedactor removes known secret values from a projected string. It is
// satisfied by *ChatClients so posture shares Today's exact-value redaction
// rather than standing up a second, drifting boundary (#193 AC4).
type PostureRedactor interface {
	RedactExact(string) string
}

// PostureAuditSource supplies the policy_audit rows behind the denial trace.
type PostureAuditSource interface {
	RecentDenials(ctx context.Context, session string, limit int) ([]policy.AuditEntry, error)
}

// PostureService projects agent posture. It holds a config snapshot and never
// a runtime client.
type PostureService struct {
	cfg      config.Config
	redactor PostureRedactor
	audit    PostureAuditSource
}

func NewPostureService(cfg config.Config, redactor PostureRedactor, audit PostureAuditSource) *PostureService {
	return &PostureService{cfg: cfg, redactor: redactor, audit: audit}
}

// Profiles lists the profile names posture can be read for, so the UI can
// offer them without guessing at config shape.
func (s *PostureService) Profiles() []string {
	if s == nil {
		return []string{}
	}
	names := make([]string, 0, len(s.cfg.Agent.Profiles)+1)
	for name := range s.cfg.Agent.Profiles {
		names = append(names, name)
	}
	if _, exists := s.cfg.Agent.Profiles[config.GroupMain]; !exists {
		names = append(names, config.GroupMain)
	}
	sort.Strings(names)
	return names
}

// Read projects one profile's posture. An unknown name still returns the
// group-level posture it would inherit, flagged with Known=false, rather than
// an error: "this profile does not exist" is itself useful posture.
func (s *PostureService) Read(profileName string) PostureView {
	if s == nil {
		return PostureView{Layers: []PostureLayerView{}}
	}
	profileName = strings.TrimSpace(profileName)
	profile, known := s.cfg.Profile(profileName)
	resolved := profileName
	if resolved == "" {
		resolved = s.cfg.Agent.DefaultProfile
	}
	if resolved == "" {
		resolved = config.GroupMain
	}

	// A profile's own group is its name when a group of that name exists;
	// otherwise it runs inside the main interactive group, matching the chat
	// runtime and the connections projection (#155).
	group := config.GroupMain
	if _, isGroup := s.cfg.Agent.Groups[resolved]; isGroup {
		group = resolved
	}

	view := PostureView{
		Profile: s.projectLabel(resolved),
		Group:   s.projectLabel(group),
		Known:   known,
		System:  s.projectSystem(profile),
		Limits: PostureLimitsView{
			Model:           s.projectLabel(profile.Model),
			MaxTokens:       max(profile.MaxTokens, 0),
			MaxIterations:   max(profile.MaxIterations, 0),
			AllowedChildren: s.projectIdentifiers(profile.AllowedChildren),
		},
	}

	// Repo policy is nil here: a profile read from Desk is not scoped to a
	// checkout, so claiming a WAFFLE.md layer applies would be a lie.
	layered := s.cfg.LayeredAgentPolicy(group, resolved, nil)
	view.Layers = make([]PostureLayerView, 0, len(layered.Layers)+1)
	for _, layer := range layered.Layers {
		view.Layers = append(view.Layers, PostureLayerView{
			Name:         layer.Name,
			Applied:      layer.Applied,
			SandboxMode:  s.projectLabel(layer.Sandbox),
			Allow:        s.projectIdentifiers(layer.Allow),
			Deny:         s.projectIdentifiers(layer.Deny),
			DenyPrefixes: s.projectPrefixes(layer.DenyPrefixes),
			Guidance:     s.projectText(layer.Guidance),
		})
	}
	// The repo tier is always named so its absence reads as "not in scope"
	// rather than the tier being missing from the model (#193 AC2).
	view.Layers = append(view.Layers, PostureLayerView{
		Name: "repo", Applied: false,
		Allow: []string{}, Deny: []string{}, DenyPrefixes: []string{},
		Guidance: "WAFFLE.md applies inside a repo workspace and can only tighten this further.",
	})

	view.Effective = PostureLayerView{
		Name:         "effective",
		Applied:      true,
		SandboxMode:  s.projectLabel(layered.Effective.Mode),
		Allow:        s.projectIdentifiers(layered.Effective.Allow),
		Deny:         s.projectIdentifiers(layered.Effective.Deny),
		DenyPrefixes: s.projectPrefixes(layered.Effective.DenyPrefixes),
		Guidance:     s.projectText(layered.Effective.Guidance),
	}
	return view
}

// Denials projects recent refusals for a session.
func (s *PostureService) Denials(ctx context.Context, session string) (PostureDenialSnapshot, error) {
	snapshot := PostureDenialSnapshot{Denials: []PostureDenial{}}
	if s == nil || s.audit == nil {
		return snapshot, nil
	}
	entries, err := s.audit.RecentDenials(ctx, strings.TrimSpace(session), postureDenialLimit)
	if err != nil {
		return PostureDenialSnapshot{}, err
	}
	for _, entry := range entries {
		snapshot.Denials = append(snapshot.Denials, PostureDenial{
			At:      s.projectLabel(entry.At),
			Tool:    s.projectIdentifier(entry.Tool),
			Command: s.projectText(entry.Command),
			Rule:    s.projectIdentifier(entry.Rule),
			Verdict: s.projectIdentifier(entry.Verdict),
			Detail:  s.projectText(entry.Detail),
		})
	}
	return snapshot, nil
}

// projectSystem resolves and projects the profile's system prompt. Resolution
// failures are reported as posture too — a prompt that cannot be read is
// exactly the thing an operator needs to see — but the message is redacted,
// since it can quote a path.
func (s *PostureService) projectSystem(profile config.AgentProfile) PostureSystemView {
	prompt, err := config.ResolveProfileSystem(profile.System)
	if err != nil {
		return PostureSystemView{
			Source: config.SystemPromptFile,
			Error:  "The system prompt file could not be read.",
		}
	}
	return PostureSystemView{
		Source: prompt.Kind,
		Path:   s.projectPromptPath(prompt.Path),
		Text:   s.projectText(prompt.Text),
	}
}

// projectPromptPath is the single sanctioned path on this surface. It is
// already relative to WAFFLE_HOME; anything that escaped that root, or that
// carries a parent traversal, is withheld rather than shown.
func (s *PostureService) projectPromptPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if strings.HasPrefix(path, "/") || strings.HasPrefix(path, "..") {
		return "[redacted]"
	}
	return s.redact(path)
}

func (s *PostureService) redact(value string) string {
	if s != nil && s.redactor != nil {
		value = s.redactor.RedactExact(value)
	}
	return sanitizeDashboardString(value)
}

// projectLabel admits short configuration labels: model aliases, modes, and
// profile names.
func (s *PostureService) projectLabel(value string) string {
	return s.redact(strings.TrimSpace(value))
}

// projectText admits owner-authored prose (guidance, audit detail) after
// redaction. It is the only place free-form config text reaches the browser.
func (s *PostureService) projectText(value string) string {
	return s.redact(strings.TrimSpace(value))
}

// projectIdentifier admits tool identifiers only. Anything carrying a path
// separator, scheme, or whitespace is replaced wholesale, so host paths can
// never arrive dressed as a tool name (#193 AC4).
func (s *PostureService) projectIdentifier(value string) string {
	value = s.redact(strings.TrimSpace(value))
	if value == "" {
		return ""
	}
	if !postureIdentifier(value) {
		return "[redacted]"
	}
	return value
}

func (s *PostureService) projectIdentifiers(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if projected := s.projectIdentifier(value); projected != "" {
			out = append(out, projected)
		}
	}
	return out
}

// projectPrefixes admits bash command prefixes, which legitimately contain
// spaces ("git push"). Path separators stay barred, so a prefix naming a host
// path is redacted rather than shown.
func (s *PostureService) projectPrefixes(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		projected := s.redact(strings.TrimSpace(value))
		if projected == "" {
			continue
		}
		if !posturePrefix(projected) {
			projected = "[redacted]"
		}
		out = append(out, projected)
	}
	return out
}

func postureIdentifier(value string) bool {
	return postureAllowedRunes(value, false)
}

func posturePrefix(value string) bool {
	return postureAllowedRunes(value, true)
}

func postureAllowedRunes(value string, allowSpace bool) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_' || r == '-' || r == '.' || r == '*' || r == ':':
		case allowSpace && r == ' ':
		default:
			return false
		}
	}
	return true
}

// PostureRouteConfig is the additive integration seam for the posture surface.
type PostureRouteConfig struct {
	Service *PostureService
}

// RegisterPostureRoutes mounts the read-only posture endpoints. There is no
// mutation handler here by design: this surface cannot change state (#193).
func RegisterPostureRoutes(mux *http.ServeMux, routeConfig PostureRouteConfig) {
	if mux == nil || routeConfig.Service == nil {
		return
	}
	mux.Handle("GET /api/v1/desk/posture", newPostureHandler(routeConfig.Service))
	mux.Handle("GET /api/v1/desk/posture/denials", newPostureDenialsHandler(routeConfig.Service))
}

func newPostureHandler(service *PostureService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested := r.URL.Query().Get("profile")
		if len(requested) > config.ProfileNameMax {
			writeJSON(w, http.StatusUnprocessableEntity, errorResponse{
				Code: "invalid_profile", Message: "profile name is invalid",
			})
			return
		}
		writeJSON(w, http.StatusOK, struct {
			PostureView
			Profiles []string `json:"profiles"`
		}{PostureView: service.Read(requested), Profiles: service.Profiles()})
	})
}

func newPostureDenialsHandler(service *PostureService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		snapshot, err := service.Denials(r.Context(), r.URL.Query().Get("session"))
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, errorResponse{
				Code: "posture_unavailable", Message: "policy history is unavailable",
			})
			return
		}
		writeJSON(w, http.StatusOK, snapshot)
	})
}

// sqlPostureAudit reads denials straight from the shared policy_audit table.
type sqlPostureAudit struct{ db *sql.DB }

// NewPostureAuditSource adapts the shared state database to the denial trace.
// A nil db yields no denials rather than an error: the trace is additive.
func NewPostureAuditSource(db *sql.DB) PostureAuditSource {
	if db == nil {
		return nil
	}
	return sqlPostureAudit{db: db}
}

func (a sqlPostureAudit) RecentDenials(ctx context.Context, session string, limit int) ([]policy.AuditEntry, error) {
	return policy.RecentDenials(ctx, a.db, session, limit)
}
