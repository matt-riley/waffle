package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/matt-riley/waffle/internal/observability"
)

const eventHeartbeatInterval = 15 * time.Second

// APIConfig supplies the process-scoped dependencies for the Desk routes.
// The serve command owns these dependencies so the routes never create a
// second security token, mux, listener, or event hub.
type APIConfig struct {
	Observability *observability.Service
	Security      *Security
	Hub           *EventHub
	Version       string
	Now           func() time.Time
	Heartbeat     func(time.Duration) (<-chan time.Time, func())
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
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(bootstrap); err != nil {
			return
		}
	})
}

func newEventsHandler(config APIConfig) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming_unsupported", http.StatusInternalServerError)
			return
		}
		after, err := parseLastEventID(r.Header.Get("Last-Event-ID"))
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
