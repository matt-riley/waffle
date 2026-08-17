package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/matt-riley/waffle/internal/artifact"
	"github.com/matt-riley/waffle/internal/memory"
	"github.com/matt-riley/waffle/internal/observability"
	"github.com/matt-riley/waffle/internal/project"
)

const eventHeartbeatInterval = 15 * time.Second

// APIConfig supplies the process-scoped dependencies for the Desk routes.
// The serve command owns these dependencies so the routes never create a
// second security token, mux, listener, or event hub.
type APIConfig struct {
	Observability *observability.Service
	Security      *Security
	Hub           *EventHub
	ChatClients   *ChatClients
	Idempotency   *IdempotencyStore
	// Projects is the workspace-scoped project context store (#478); nil
	// disables the project surface.
	Projects *project.Store
	// Artifacts is the session artifact registry (#480); nil disables the
	// artifact surface. Previews is the shared one-time token store.
	Artifacts *artifact.Store
	Previews  *PreviewStore

	Operations      *Operations
	Schedules       TaskScheduleStore
	Memory          memory.Workspace
	WorkspaceEgress string
	Capabilities    *Capabilities
	// Posture is the read-only agent-posture projection (#193). Optional: when
	// nil the endpoints are simply not mounted.
	Posture *PostureService
	// Profiles is the structured agent-profile editor (#194). Optional: when
	// nil the editor is not mounted at all.
	Profiles *ProfileEditor
	// Setup is the bootstrap prerequisite projection (#192). Optional: when
	// nil the checklist endpoints are not mounted.
	Setup          *SetupService
	Restart        RestartScheduler
	RestartOutcome MutationOutcomeObserver
	Version        string
	// ProcessGeneration is a coordinator-created opaque identity that changes
	// for every serving process. Restart-aware clients use it to reject the
	// still-running process while waiting for deferred activation.
	ProcessGeneration string
	Now               func() time.Time
	Heartbeat         func(time.Duration) (<-chan time.Time, func())
}

func (c APIConfig) now() func() time.Time {
	if c.Now != nil {
		return c.Now
	}
	return time.Now
}

func (c APIConfig) heartbeat() func(time.Duration) (<-chan time.Time, func()) {
	if c.Heartbeat != nil {
		return c.Heartbeat
	}
	return func(interval time.Duration) (<-chan time.Time, func()) {
		ticker := time.NewTicker(interval)
		return ticker.C, ticker.Stop
	}
}

func newBootstrapHandler(config APIConfig) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bootstrap, err := buildBootstrap(r.Context(), config)
		if err != nil {
			http.Error(w, "bootstrap_unavailable", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, bootstrap)
	})
}

func newEventsHandler(config APIConfig) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming_unsupported", http.StatusInternalServerError)
			return
		}
		after, err := parseEventCursor(r)
		if err != nil {
			http.Error(w, "invalid_last_event_id", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		subscription, resync := config.Hub.Subscribe(after)
		if resync {
			if writeSSE(w, "resync_required", 0, []byte(`{}`)) == nil {
				flusher.Flush()
			}
			return
		}
		defer config.Hub.Unsubscribe(subscription)

		ticks, stop := config.heartbeat()(eventHeartbeatInterval)
		defer stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case event, open := <-subscription:
				if !open {
					return
				}
				data, err := json.Marshal(event)
				if err != nil || writeSSE(w, event.Type, event.Cursor, data) != nil {
					return
				}
				flusher.Flush()
			case <-ticks:
				if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
					return
				}
				flusher.Flush()
			}
		}
	})
}

func parseLastEventID(value string) (uint64, error) {
	if value == "" {
		return 0, nil
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return 0, fmt.Errorf("not an unsigned decimal")
		}
	}
	return strconv.ParseUint(value, 10, 64)
}

func parseEventCursor(r *http.Request) (uint64, error) {
	if lastEventID := r.Header.Get("Last-Event-ID"); lastEventID != "" {
		return parseLastEventID(lastEventID)
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return 0, err
	}
	after, present := query["after"]
	if !present {
		return 0, nil
	}
	if len(after) != 1 || after[0] == "" {
		return 0, fmt.Errorf("after must be one unsigned decimal")
	}
	return parseLastEventID(after[0])
}

func writeSSE(w http.ResponseWriter, eventType string, cursor uint64, data []byte) error {
	if cursor != 0 {
		if _, err := fmt.Fprintf(w, "id: %d\n", cursor); err != nil {
			return err
		}
	}
	if eventType != "" {
		if _, err := fmt.Fprintf(w, "event: %s\n", eventType); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(w, "data: %s\n\n", data)
	return err
}
