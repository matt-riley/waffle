package dashboard

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/observability"
	"github.com/matt-riley/waffle/internal/providerconfig"
)

const connectionHealthStaleAfter = 2 * time.Minute

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
}

// ConnectionSource supplies already-sanitized connection records.
type ConnectionSource interface {
	Connections(context.Context) ([]ConnectionView, error)
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
	health        ConnectionHealthSource
}

// NewConnectionSource snapshots only allowlisted labels and closed policy
// summaries from cfg. Credentials, endpoints, commands, environment names,
// tool policies, guidance, paths, allowlists, and hooks are never retained.
func NewConnectionSource(cfg config.Config, health ConnectionHealthSource) ConnectionSource {
	records := make([]ConnectionView, 0, len(cfg.Providers)+len(cfg.MCP)+len(cfg.Agent.Groups)+len(cfg.Agent.Profiles)+1)

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
		mode := cfg.AgentPolicy(name).Mode
		if profile, exists := cfg.Agent.Profiles[name]; exists {
			mode = cfg.AgentPolicy(config.GroupMain).Mode
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

	return &configuredConnectionSource{
		records:       records,
		telegramIndex: telegramIndex,
		health:        health,
	}
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
