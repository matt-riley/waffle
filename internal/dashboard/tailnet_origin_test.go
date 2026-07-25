package dashboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestDeskShellIsOriginAgnosticOnTailnetHost proves the shell and its asset
// references work unchanged on the tailnet origin. Nothing the client requests
// may hard-code the loopback origin, because the same document is served on both
// profiles. A browser cannot easily be pointed at a fabricated Host, so this
// asserts the property the browser fixture relies on rather than driving one.
func TestDeskShellIsOriginAgnosticOnTailnetHost(t *testing.T) {
	security := mustTailnetSecurity(t, "127.0.0.1:8422", tailnetTestOptions())
	mux := http.NewServeMux()
	mux.Handle("/desk/", ShellHandler(security))

	rec := httptest.NewRecorder()
	security.Wrap(mux).ServeHTTP(rec, newTailnetRequest(http.MethodGet, "/desk/"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	for _, absolute := range []string{"http://127.0.0.1", "http://localhost", "//127.0.0.1"} {
		if strings.Contains(body, absolute) {
			t.Errorf("shell references the absolute loopback origin %q; it cannot be served on another origin", absolute)
		}
	}
	if !strings.Contains(body, `href="/desk/assets/app.css`) {
		t.Error("stylesheet reference is not a root-relative Desk asset URL")
	}
	if !strings.Contains(body, `src="/desk/assets/app.js`) {
		t.Error("script reference is not a root-relative Desk asset URL")
	}
}

// TestDeskAssetsAndEventStreamServeOnTailnetHost covers the two requests the
// shell makes immediately after loading: its assets, and the event stream that
// has to survive Serve's reverse proxy.
func TestDeskAssetsAndEventStreamServeOnTailnetHost(t *testing.T) {
	security := mustTailnetSecurity(t, "127.0.0.1:8422", tailnetTestOptions())
	hub := NewEventHub(8)
	ticks := make(chan time.Time)
	mux := http.NewServeMux()
	mux.Handle("/desk/", ShellHandler(security))
	mux.Handle("GET /api/v1/desk/events", newEventsHandler(APIConfig{
		Hub: hub,
		Heartbeat: func(time.Duration) (<-chan time.Time, func()) {
			return ticks, func() {}
		},
	}))
	handler := security.Wrap(mux)

	t.Run("asset", func(t *testing.T) {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, newTailnetRequest(http.MethodGet, "/desk/assets/app.js"))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		if rec.Body.Len() == 0 {
			t.Error("asset body is empty")
		}
	})

	t.Run("event stream", func(t *testing.T) {
		event := hub.Publish(Event{Type: "state", Resource: "session"})
		req := newTailnetRequest(http.MethodGet, "/api/v1/desk/events?after="+stringCursor(event.Cursor-1))
		ctx, cancel := context.WithCancel(req.Context())
		req = req.WithContext(ctx)
		writer := newStreamRecorder()
		done := make(chan struct{})
		go func() {
			defer close(done)
			handler.ServeHTTP(writer, req)
		}()
		writer.waitForFlush(t)
		cancel()
		<-done

		if got := writer.String(); !strings.Contains(got, "event: state") {
			t.Errorf("SSE body = %q, want the replayed state event", got)
		}
		// text/event-stream is what makes Go's reverse proxy flush each event
		// immediately instead of buffering the stream behind Serve.
		if got := writer.Header().Get("Content-Type"); got != "text/event-stream" {
			t.Errorf("Content-Type = %q, want text/event-stream", got)
		}
		if got := writer.Header().Get("Strict-Transport-Security"); got == "" {
			t.Error("Strict-Transport-Security header is missing on the tailnet event stream")
		}
	})
}
