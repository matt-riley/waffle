package dashboard

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/observability"
	"github.com/matt-riley/waffle/internal/providerconfig"
)

const (
	connectionHealthStaleAfter = 2 * time.Minute
	// githubProbeTTL bounds how often a Desk read mints a GitHub installation
	// token. Connections is polled by the browser; the probe is a real
	// outbound API call and must not run once per page render (#182).
	githubProbeTTL = 5 * time.Minute
	// githubProbeTimeout bounds the detached probe. Without a caller context
	// to cancel it, the mint needs its own deadline.
	githubProbeTimeout = 15 * time.Second
)

// ConnectionView is the complete, allowlisted public representation of one
// configured connection or policy posture.
type ConnectionView struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Status      string `json:"status"`
	Profile     string `json:"profile,omitempty"`
	SandboxMode string `json:"sandbox_mode,omitempty"`
	Egress      string `json:"egress,omitempty"`
	Guidance    string `json:"guidance,omitempty"`
	// Label and Concurrency describe a configured intake watcher. Nothing
	// else about a watcher — token, deliver target, poll interval — is
	// projected (#182).
	Label       string `json:"label,omitempty"`
	Concurrency int    `json:"concurrency,omitempty"`
}

// ConnectionSource supplies already-sanitized connection records.
type ConnectionSource interface {
	Connections(context.Context) ([]ConnectionView, error)
}

// GitHubProbe reports whether configured GitHub App credentials can still
// mint an installation token. Implementations must return only an error:
// tokens, identifiers, and endpoints never cross this seam.
type GitHubProbe interface {
	Verify(context.Context) error
}

// ConnectionHealthSource is the narrow observability seam used to project
// configured adapter freshness. Overall process, database, and scheduler
// health are intentionally ignored.
type ConnectionHealthSource interface {
	HealthSnapshot(context.Context, time.Duration) (observability.Health, error)
}

type configuredConnectionSource struct {
	records       []ConnectionView
	telegramIndex int
	githubIndex   int
	health        ConnectionHealthSource
	github        GitHubProbe

	// githubProbeMu serializes the outbound mint; githubMu guards only the
	// cached result. Keeping them separate means the state lock is never held
	// across a network call (#214).
	githubProbeMu   sync.Mutex
	githubMu        sync.Mutex
	githubCheckedAt time.Time
	githubErr       error
	githubProbed    bool
	now             func() time.Time
}

// NewConnectionSource snapshots only allowlisted labels and closed policy
// summaries from cfg. Credentials, endpoints, commands, environment names,
// tool policies, guidance, paths, allowlists, and hooks are never retained.
// github is optional: when nil, a configured GitHub App reports "configured"
// without an outbound probe.
func NewConnectionSource(cfg config.Config, health ConnectionHealthSource, github GitHubProbe) ConnectionSource {
	records := make([]ConnectionView, 0, len(cfg.Providers)+len(cfg.MCP)+len(cfg.Agent.Groups)+len(cfg.Agent.Profiles)+len(cfg.Intake.GitHub)+2)

	providerNames := providerconfig.SortedKeys(cfg.Providers)
	for _, name := range providerNames {
		records = append(records, ConnectionView{Name: name, Kind: "provider", Status: "configured"})
	}

	telegramIndex := -1
	if cfg.Channel.Telegram.Enabled {
		telegramIndex = len(records)
		records = append(records, ConnectionView{Name: "telegram", Kind: "adapter", Status: "configured"})
	}

	mcpNames := make([]string, 0, len(cfg.MCP))
	for _, server := range cfg.MCP {
		mcpNames = append(mcpNames, server.Name)
	}
	sort.Strings(mcpNames)
	for _, name := range mcpNames {
		records = append(records, ConnectionView{Name: name, Kind: "mcp", Status: "configured"})
	}

	profileNames := make(map[string]struct{}, len(cfg.Agent.Groups)+len(cfg.Agent.Profiles))
	for name := range cfg.Agent.Groups {
		profileNames[name] = struct{}{}
	}
	for name := range cfg.Agent.Profiles {
		profileNames[name] = struct{}{}
	}
	for _, name := range providerconfig.SortedKeys(profileNames) {
		// Groups resolve via AgentPolicy(name). Profiles without a same-named
		// group inherit the main interactive group (matches chat runtime), then
		// apply an explicit profile sandbox override. Never force GroupMain over
		// a real group of the same name (#155).
		mode := cfg.AgentPolicy(name).Mode
		if profile, exists := cfg.Agent.Profiles[name]; exists {
			if _, isGroup := cfg.Agent.Groups[name]; !isGroup {
				mode = cfg.AgentPolicy(config.GroupMain).Mode
			}
			if profile.Sandbox != "" {
				mode = profile.Sandbox
			}
		}
		mode = connectionSandboxSummary(mode)
		guidance := "Tool policy is enforced."
		if mode == "docker" {
			guidance = "Runs in a sandbox."
		}
		records = append(records, ConnectionView{
			Name:        name,
			Kind:        "profile",
			Status:      "configured",
			Profile:     name,
			SandboxMode: mode,
			Egress:      connectionEgressSummary(cfg.Workspace.Egress),
			Guidance:    guidance,
		})
	}

	githubIndex := len(records)
	records = append(records, githubConnectionRecord(cfg.GitHub))
	records = append(records, intakeConnectionRecords(cfg.Intake)...)

	return &configuredConnectionSource{
		records:       records,
		telegramIndex: telegramIndex,
		githubIndex:   githubIndex,
		health:        health,
		github:        github,
		now:           time.Now,
	}
}

// githubConnectionRecord reports whether GitHub App git auth is configured.
// It reads only whether the three required fields are present — the app ID,
// installation ID, private key reference, and base URL are never projected.
func githubConnectionRecord(cfg config.GitHub) ConnectionView {
	record := ConnectionView{Name: "github", Kind: "github", Status: "unconfigured"}
	if cfg.App.PrivateKey == "" || cfg.App.AppID <= 0 || cfg.App.InstallationID <= 0 {
		record.Guidance = "Configure [github.app] to give workspaces git access."
		return record
	}
	record.Status = "configured"
	record.Guidance = "Workspace git auth is brokered; containers never hold a credential."
	return record
}

// intakeConnectionRecords projects configured board intake watchers. Only the
// repo, label, and concurrency are exposed; tokens, delivery targets, and
// poll intervals stay in config.
func intakeConnectionRecords(cfg config.Intake) []ConnectionView {
	records := make([]ConnectionView, 0, len(cfg.GitHub))
	for _, watch := range cfg.GitHub {
		repo := strings.TrimSpace(watch.Repo)
		if repo == "" {
			continue
		}
		records = append(records, ConnectionView{
			Name:        sanitizeDashboardString(repo),
			Kind:        "intake",
			Status:      "configured",
			Label:       sanitizeDashboardString(strings.TrimSpace(watch.Label)),
			Concurrency: max(watch.MaxConcurrency, 0),
			Guidance:    "Issues matching this label are picked up by the issue profile.",
		})
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Name < records[j].Name })
	return records
}

func connectionSandboxSummary(mode string) string {
	if mode == "docker" {
		return "docker"
	}
	return "host"
}

func connectionEgressSummary(egress string) string {
	switch egress {
	case "allowlist":
		return "restricted"
	case "full":
		return "enabled"
	default:
		return "disabled"
	}
}

func (s *configuredConnectionSource) Connections(ctx context.Context) ([]ConnectionView, error) {
	records := append([]ConnectionView(nil), s.records...)
	if records == nil {
		records = []ConnectionView{}
	}
	s.applyGitHubHealth(records)
	if s.telegramIndex < 0 || s.health == nil {
		return records, nil
	}
	health, err := s.health.HealthSnapshot(ctx, connectionHealthStaleAfter)
	if err != nil {
		return nil, err
	}
	if adapter, exists := health.Adapters["telegram"]; exists {
		if adapter.Stale {
			records[s.telegramIndex].Status = "stale"
		} else {
			records[s.telegramIndex].Status = "healthy"
		}
	}
	return records, nil
}

// applyGitHubHealth upgrades a configured GitHub record to healthy or stale.
// A probe failure downgrades the record rather than failing the whole read:
// GitHub being unreachable must not blank out providers, MCP, and profiles.
func (s *configuredConnectionSource) applyGitHubHealth(records []ConnectionView) {
	if s.github == nil || s.githubIndex < 0 || s.githubIndex >= len(records) {
		return
	}
	if records[s.githubIndex].Status != "configured" {
		return
	}
	if err := s.probeGitHub(); err != nil {
		records[s.githubIndex].Status = "stale"
		records[s.githubIndex].Guidance = "GitHub App credentials did not mint an installation token."
		return
	}
	records[s.githubIndex].Status = "healthy"
}

// probeGitHub mints an installation token at most once per githubProbeTTL and
// caches only the resulting error, never the token.
//
// It deliberately takes no caller context. The probe describes the
// installation, not one request: a browser that navigates away mid-probe would
// otherwise cache context.Canceled and report healthy credentials as stale for
// the rest of the TTL.
func (s *configuredConnectionSource) probeGitHub() error {
	if err, fresh := s.cachedGitHubProbe(); fresh {
		return err
	}
	// Serialize probes so a burst of Desk polls mints one token, but never
	// hold the state lock across the network call (#214).
	s.githubProbeMu.Lock()
	defer s.githubProbeMu.Unlock()
	if err, fresh := s.cachedGitHubProbe(); fresh {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), githubProbeTimeout)
	defer cancel()
	err := s.github.Verify(ctx)

	s.githubMu.Lock()
	s.githubErr = err
	s.githubCheckedAt = s.nowTime()
	s.githubProbed = true
	s.githubMu.Unlock()
	return err
}

func (s *configuredConnectionSource) cachedGitHubProbe() (error, bool) {
	s.githubMu.Lock()
	defer s.githubMu.Unlock()
	if s.githubProbed && s.nowTime().Sub(s.githubCheckedAt) < githubProbeTTL {
		return s.githubErr, true
	}
	return nil, false
}

func (s *configuredConnectionSource) nowTime() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// RegisterConnectionsRoutes mounts the additive, read-only Task 5 endpoint.
// The caller owns the shared mux and its single outer security wrapper.
func RegisterConnectionsRoutes(mux *http.ServeMux, source ConnectionSource) {
	if mux == nil || source == nil {
		return
	}
	mux.Handle("GET /api/v1/desk/connections", newConnectionsHandler(source))
}

func newConnectionsHandler(source ConnectionSource) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		records, err := source.Connections(r.Context())
		if err != nil {
			http.Error(w, "connections_unavailable", http.StatusServiceUnavailable)
			return
		}
		if records == nil {
			records = []ConnectionView{}
		}
		writeJSON(w, http.StatusOK, records)
	})
}
