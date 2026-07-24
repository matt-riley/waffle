package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/observability"
	"github.com/matt-riley/waffle/internal/store"
)

func TestBootstrapSerializesStableContract(t *testing.T) {
	now := time.Date(2026, time.July, 23, 20, 0, 0, 0, time.FixedZone("BST", 3600))
	obs := observability.New(nil, func() time.Time { return now })
	hub := NewEventHub(2)
	hub.Publish(Event{Type: "status.changed"})
	security := mustSecurity(t, "127.0.0.1:8422")
	mux := http.NewServeMux()
	RegisterRoutes(mux, APIConfig{
		Observability:     obs,
		Security:          security,
		Hub:               hub,
		Version:           "test",
		ProcessGeneration: "process-generation-test",
		Now:               func() time.Time { return now },
	})

	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/desk/bootstrap", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	raw := append([]byte(nil), recorder.Body.Bytes()...)
	if bytes.Contains(raw, []byte(`:null`)) {
		t.Fatalf("bootstrap JSON contains null array: %s", raw)
	}
	var bootstrap Bootstrap
	if err := json.NewDecoder(bytes.NewReader(raw)).Decode(&bootstrap); err != nil {
		t.Fatalf("decode bootstrap: %v", err)
	}
	if bootstrap.Version != "test" ||
		bootstrap.ProcessGeneration != "process-generation-test" ||
		!bootstrap.ServerTime.Equal(now.UTC()) ||
		bootstrap.RequestToken != security.Token() ||
		bootstrap.EventCursor != 1 {
		t.Fatalf("bootstrap = %+v", bootstrap)
	}
	if bootstrap.Status.Active == nil || bootstrap.Status.Recent == nil || bootstrap.Status.RetryQueue == nil {
		t.Fatalf("bootstrap status has null arrays: %+v", bootstrap.Status)
	}
}

func TestDashboardRoutesOnlyClaimExactAPIMethods(t *testing.T) {
	mux := http.NewServeMux()
	RegisterRoutes(mux, testAPIConfig(t))
	tests := []struct {
		name   string
		method string
		path   string
		want   int
	}{
		{
			name:   "known path with unsupported method",
			method: http.MethodPost,
			path:   "/api/v1/desk/bootstrap",
			want:   http.StatusMethodNotAllowed,
		},
		{
			name:   "unknown API path",
			method: http.MethodGet,
			path:   "/api/v1/desk/chat/runs",
			want:   http.StatusNotFound,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, nil)
			recorder := httptest.NewRecorder()
			mux.ServeHTTP(recorder, request)
			if recorder.Code != test.want {
				t.Errorf("%s %s status = %d, want %d", test.method, test.path, recorder.Code, test.want)
			}
		})
	}
}

func TestBootstrapSanitizesObservabilityFailures(t *testing.T) {
	closedStore, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := closedStore.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	obs := observability.New(closedStore, time.Now)
	mux := http.NewServeMux()
	config := testAPIConfig(t)
	config.Observability = obs
	RegisterRoutes(mux, config)

	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/desk/bootstrap", nil))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", recorder.Code)
	}
	if got, want := recorder.Body.String(), "bootstrap_unavailable\n"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

func TestBootstrapRejectsMissingProcessGeneration(t *testing.T) {
	mux := http.NewServeMux()
	config := testAPIConfig(t)
	config.ProcessGeneration = ""
	RegisterRoutes(mux, config)

	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/desk/bootstrap", nil))

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", recorder.Code)
	}
	if got := recorder.Body.String(); got != "bootstrap_unavailable\n" {
		t.Fatalf("body = %q", got)
	}
}

func testAPIConfig(t *testing.T) APIConfig {
	t.Helper()
	return APIConfig{
		Observability:     observability.New(nil, time.Now),
		Security:          mustSecurity(t, "127.0.0.1:8422"),
		Hub:               NewEventHub(256),
		Version:           "test",
		ProcessGeneration: "test-process-generation",
	}
}
