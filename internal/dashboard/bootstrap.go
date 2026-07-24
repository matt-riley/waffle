package dashboard

import (
	"context"
	"errors"
	"time"

	"github.com/matt-riley/waffle/internal/observability"
)

// Bootstrap is the initial Desk state returned before a client starts its
// event stream.
type Bootstrap struct {
	Version           string                 `json:"version"`
	ServerTime        time.Time              `json:"server_time"`
	RequestToken      string                 `json:"request_token"`
	ProcessGeneration string                 `json:"process_generation"`
	EventCursor       uint64                 `json:"event_cursor"`
	Health            observability.Health   `json:"health"`
	Status            observability.Snapshot `json:"status"`
}

func buildBootstrap(ctx context.Context, config APIConfig) (Bootstrap, error) {
	if config.ProcessGeneration == "" {
		return Bootstrap{}, errors.New("dashboard process generation is required")
	}
	health, err := config.Observability.HealthSnapshot(ctx, 2*time.Minute)
	if err != nil {
		return Bootstrap{}, err
	}
	status, err := config.Observability.Snapshot(ctx)
	if err != nil {
		return Bootstrap{}, err
	}
	if status.Active == nil {
		status.Active = make([]observability.ActiveRun, 0)
	}
	if status.Recent == nil {
		status.Recent = make([]observability.RecentRun, 0)
	}
	if status.RetryQueue == nil {
		status.RetryQueue = make([]any, 0)
	}
	return Bootstrap{
		Version:           config.Version,
		ServerTime:        config.now()().UTC(),
		RequestToken:      config.Security.Token(),
		ProcessGeneration: config.ProcessGeneration,
		EventCursor:       config.Hub.Cursor(),
		Health:            health,
		Status:            status,
	}, nil
}
