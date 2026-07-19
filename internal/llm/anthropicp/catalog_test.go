package anthropicp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/modelcatalog"
)

func TestCatalogListsAllAnthropicPages(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		request := requests.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" {
			t.Errorf("request = %s %s, want GET /v1/models", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("x-api-key"); got != "test-key" {
			t.Errorf("x-api-key = %q, want %q", got, "test-key")
		}
		if got := r.Header.Get("anthropic-version"); got == "" {
			t.Error("anthropic-version header is absent")
		}
		if got := r.URL.Query().Get("limit"); got != "1000" {
			t.Errorf("limit = %q, want %q", got, "1000")
		}

		switch request {
		case 1:
			if got := r.URL.Query().Get("after_id"); got != "" {
				t.Errorf("first after_id = %q, want absent", got)
			}
			_, _ = fmt.Fprintf(w, `{"data":[%s],"has_more":true,"first_id":"model-1","last_id":"model-1"}`, anthropicModelJSON("model-1", "Model One", 200000))
		case 2:
			if got := r.URL.Query().Get("after_id"); got != "model-1" {
				t.Errorf("second after_id = %q, want %q", got, "model-1")
			}
			_, _ = fmt.Fprintf(w, `{"data":[%s],"has_more":false,"first_id":"model-2","last_id":"model-2"}`, anthropicModelJSON("model-2", "Model Two", 100000))
		default:
			t.Errorf("unexpected request %d", request)
			http.Error(w, "unexpected request", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(server.Close)

	if DefaultBaseURL != "https://api.anthropic.com" {
		t.Fatalf("DefaultBaseURL = %q, want %q", DefaultBaseURL, "https://api.anthropic.com")
	}
	models, err := NewCatalog("test-key", server.URL).ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels() error = %v", err)
	}
	want := []modelcatalog.Model{
		{ID: "model-1", DisplayName: "Model One", ContextWindow: 200000},
		{ID: "model-2", DisplayName: "Model Two", ContextWindow: 100000},
	}
	if !reflect.DeepEqual(models, want) {
		t.Fatalf("ListModels() = %#v, want %#v", models, want)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("requests = %d, want 2", got)
	}
}

func TestCatalogStopsAtAnthropicPageLimit(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		request := requests.Add(1)
		if request > 100 {
			t.Errorf("received request %d; request 101 must never be made", request)
			http.Error(w, "too many requests", http.StatusInternalServerError)
			return
		}
		id := "model-" + strconv.Itoa(int(request))
		_, _ = fmt.Fprintf(w, `{"data":[%s],"has_more":true,"first_id":%q,"last_id":%q}`, anthropicModelJSON(id, id, 1000), id, id)
	}))
	t.Cleanup(server.Close)

	_, err := NewCatalog("test-key", server.URL).ListModels(context.Background())
	if err == nil || !strings.Contains(err.Error(), "exceeds 100 pages") {
		t.Fatalf("ListModels() error = %v, want Anthropic page limit error", err)
	}
	if got := requests.Load(); got != 100 {
		t.Fatalf("requests = %d, want 100", got)
	}
}

func TestCatalogRejectsOversizedMalformedAndNonSuccessResponses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		apiKey    string
		handler   http.Handler
		wantError string
		check     func(*testing.T, error)
	}{
		{
			name: "response larger than 16 MiB",
			handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"data":[{"id":"%s"}],"has_more":false}`, strings.Repeat("x", 16*1024*1024))
			}),
			wantError: "exceeds 16777216 bytes",
			check: func(t *testing.T, err error) {
				t.Helper()
				if !errors.Is(err, errAnthropicCatalogResponseTooLarge) {
					t.Fatalf("errors.Is(error, response too large) = false; error = %v", err)
				}
			},
		},
		{
			name: "malformed JSON",
			handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":[`))
			}),
			wantError: "anthropic catalogue",
		},
		{
			name:   "non-success response is safe and redacted",
			apiKey: "secret-key",
			handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte("credential secret-key\nwas rejected"))
			}),
			wantError: "400 Bad Request",
			check: func(t *testing.T, err error) {
				t.Helper()
				if strings.Contains(err.Error(), "secret-key") {
					t.Fatalf("error contains active API key: %q", err)
				}
				if strings.Contains(err.Error(), "rejected\n") {
					t.Fatalf("error contains unescaped control character: %q", err)
				}
			},
		},
		{
			name:   "API key leaked in descriptor",
			apiKey: "secret-key",
			handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"data":[%s],"has_more":false}`, anthropicModelJSON("model-1", "Model secret-key", 1000))
			}),
			wantError: "contains the active API key",
		},
		{
			name: "more than 10000 descriptors",
			handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":[`))
				for i := 0; i <= modelcatalog.MaxModels; i++ {
					if i > 0 {
						_, _ = w.Write([]byte(","))
					}
					_, _ = fmt.Fprintf(w, `{"id":"model-%d"}`, i)
				}
				_, _ = w.Write([]byte(`],"has_more":false}`))
			}),
			wantError: "maximum is 10000",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(tt.handler)
			t.Cleanup(server.Close)

			apiKey := tt.apiKey
			if apiKey == "" {
				apiKey = "test-key"
			}
			_, err := NewCatalog(apiKey, server.URL).ListModels(context.Background())
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("ListModels() error = %v, want containing %q", err, tt.wantError)
			}
			if tt.check != nil {
				tt.check(t, err)
			}
		})
	}

	t.Run("redirects are not followed", func(t *testing.T) {
		var destinationCalled atomic.Bool
		destination := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
			destinationCalled.Store(true)
		}))
		t.Cleanup(destination.Close)
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, destination.URL+"/v1/models", http.StatusTemporaryRedirect)
		}))
		t.Cleanup(origin.Close)

		_, err := NewCatalog("secret-key", origin.URL).ListModels(context.Background())
		if err == nil {
			t.Fatal("ListModels() error = nil, want redirect response error")
		}
		if destinationCalled.Load() {
			t.Fatal("catalogue followed a redirect")
		}
	})
}

func TestCatalogSanitizesEntireProviderErrorChain(t *testing.T) {
	t.Parallel()

	const apiKey = "secret-key"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"credential secret-key was rejected"}}`))
	}))
	t.Cleanup(server.Close)

	_, err := NewCatalog(apiKey, server.URL).ListModels(context.Background())
	if err == nil {
		t.Fatal("ListModels() error = nil, want provider error")
	}
	assertErrorTreeOmits(t, err, apiKey)

	var sdkAPIError interface {
		DumpRequest(bool) []byte
		RawJSON() string
	}
	if errors.As(err, &sdkAPIError) {
		t.Fatalf("errors.As recovered unsafe SDK API error: request = %q response = %q", sdkAPIError.DumpRequest(false), sdkAPIError.RawJSON())
	}
}

func TestCatalogHonorsCancellation(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	deadlineCtx, cancelDeadline := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	t.Cleanup(cancelDeadline)

	for _, tt := range []struct {
		name string
		ctx  context.Context
		want error
	}{
		{name: "canceled", ctx: canceledCtx, want: context.Canceled},
		{name: "deadline exceeded", ctx: deadlineCtx, want: context.DeadlineExceeded},
	} {
		t.Run(tt.name, func(t *testing.T) {
			start := time.Now()
			_, err := NewCatalog("test-key", server.URL).ListModels(tt.ctx)
			if err == nil || !strings.Contains(err.Error(), tt.want.Error()) {
				t.Fatalf("ListModels() error = %v, want %v", err, tt.want)
			}
			if !errors.Is(err, tt.want) {
				t.Fatalf("errors.Is(ListModels() error, %v) = false; error = %v", tt.want, err)
			}
			if elapsed := time.Since(start); elapsed > time.Second {
				t.Fatalf("ListModels() took %v after cancellation", elapsed)
			}
		})
	}
}

func assertErrorTreeOmits(t *testing.T, err error, forbidden string) {
	t.Helper()
	if err == nil {
		return
	}
	if strings.Contains(err.Error(), forbidden) {
		t.Fatalf("error chain contains active API key: %q", err)
	}
	if many, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range many.Unwrap() {
			assertErrorTreeOmits(t, child, forbidden)
		}
		return
	}
	assertErrorTreeOmits(t, errors.Unwrap(err), forbidden)
}

func anthropicModelJSON(id, displayName string, maxInputTokens int64) string {
	return fmt.Sprintf(`{"id":%q,"display_name":%q,"max_input_tokens":%d,"max_tokens":8192,"created_at":"2025-01-01T00:00:00Z","type":"model","capabilities":{}}`, id, displayName, maxInputTokens)
}
