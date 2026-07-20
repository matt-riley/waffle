package openaip

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/modelcatalog"
)

func TestCatalogListsAuthenticatedOpenAIModels(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" {
			t.Errorf("request = %s %s, want GET /v1/models", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q, want %q", got, "Bearer test-key")
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5.4","owned_by":"openai"},{"id":"openai/gpt-5","name":"GPT-5","context_length":400000,"supported_parameters":["tools","temperature"],"architecture":{"output_modalities":["text"]}}]}`))
	}))
	t.Cleanup(server.Close)

	models, err := NewCatalog("test-key", server.URL+"/v1/", false).ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels() error = %v", err)
	}
	want := []modelcatalog.Model{
		{ID: "gpt-5.4", Owner: "openai"},
		{
			ID:            "openai/gpt-5",
			DisplayName:   "GPT-5",
			ContextWindow: 400000,
			Capabilities:  []string{"text-output", "tool-calling"},
		},
	}
	if !reflect.DeepEqual(models, want) {
		t.Fatalf("ListModels() = %#v, want %#v", models, want)
	}
}

func TestCatalogAllowsAuthFreeOpenAICompatibleEndpoint(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization = %q, want absent", got)
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"local-model"}]}`))
	}))
	t.Cleanup(server.Close)

	models, err := NewCatalog("", server.URL, false).ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels() error = %v", err)
	}
	if want := []modelcatalog.Model{{ID: "local-model"}}; !reflect.DeepEqual(models, want) {
		t.Fatalf("ListModels() = %#v, want %#v", models, want)
	}
}

func TestCatalogRejectsIncompleteSuccessfulOpenAIEnvelopes(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name string
		body string
		ok   bool
	}{
		{name: "missing data", body: `{}`},
		{name: "null data", body: `{"data":null}`},
		{name: "whitespace null data", body: `{"data":  null }`},
		{name: "wrong data type", body: `{"data":{}}`},
		{name: "explicit empty data", body: `{"data":[]}`, ok: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tt.body))
			}))
			t.Cleanup(server.Close)

			models, err := NewCatalog("test-key", server.URL, false).ListModels(t.Context())
			if tt.ok {
				if err != nil || len(models) != 0 {
					t.Fatalf("ListModels() = %#v, %v; want explicit empty catalogue", models, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "data") {
				t.Fatalf("ListModels() error = %v, want required data envelope error", err)
			}
		})
	}
}

func TestCatalogUsesOpenRouterUserModelsAndFallsBackOn404(t *testing.T) {
	t.Parallel()

	var (
		mu    sync.Mutex
		paths []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/v1/models/user":
			http.NotFound(w, r)
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"openrouter/auto"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	models, err := NewCatalog("test-key", server.URL+"/v1", true).ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels() error = %v", err)
	}
	if want := []modelcatalog.Model{{ID: "openrouter/auto"}}; !reflect.DeepEqual(models, want) {
		t.Fatalf("ListModels() = %#v, want %#v", models, want)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"/v1/models/user", "/v1/models"}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("request paths = %v, want %v", paths, want)
	}
}

func TestCatalogRejectsOversizedMalformedAndNonSuccessResponses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		apiKey       string
		userFiltered bool
		handler      http.Handler
		wantError    string
		check        func(*testing.T, error)
	}{
		{
			name: "non-success response has bounded safe body",
			handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte("\tunsafe\n" + strings.Repeat("x", 5000)))
			}),
			wantError: "500 Internal Server Error: \\tunsafe\\n",
			check: func(t *testing.T, err error) {
				t.Helper()
				if strings.Contains(err.Error(), "unsafe\n") {
					t.Fatalf("error contains unescaped control character: %q", err)
				}
				if len(err.Error()) > 4200 {
					t.Fatalf("error length = %d, want bounded error", len(err.Error()))
				}
			},
		},
		{
			name:   "non-success response redacts API key",
			apiKey: "secret-key",
			handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte("credential secret-key was rejected"))
			}),
			wantError: "401 Unauthorized",
			check: func(t *testing.T, err error) {
				t.Helper()
				if strings.Contains(err.Error(), "secret-key") {
					t.Fatalf("error contains active API key: %q", err)
				}
			},
		},
		{
			name: "malformed JSON",
			handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"data":[`))
			}),
			wantError: "decode model catalogue",
		},
		{
			name: "response larger than 16 MiB",
			handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprintf(w, `{"data":[{"id":"%s"}]}`, strings.Repeat("x", 16*1024*1024))
			}),
			wantError: "exceeds 16777216 bytes",
		},
		{
			name:         "user endpoint only falls back on 404",
			userFiltered: true,
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/models" {
					t.Error("catalogue fell back after a non-404 response")
				}
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
			}),
			wantError: "503 Service Unavailable",
		},
		{
			name:   "API key leaked in descriptor",
			apiKey: "secret-key",
			handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"data":[{"id":"model-secret-key"}]}`))
			}),
			wantError: "contains the active API key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(tt.handler)
			t.Cleanup(server.Close)

			_, err := NewCatalog(tt.apiKey, server.URL, tt.userFiltered).ListModels(context.Background())
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("ListModels() error = %v, want containing %q", err, tt.wantError)
			}
			if tt.check != nil {
				tt.check(t, err)
			}
		})
	}

	t.Run("redirects are not followed", func(t *testing.T) {
		var destinationCalled bool
		destination := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
			destinationCalled = true
		}))
		t.Cleanup(destination.Close)
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, destination.URL+"/models", http.StatusTemporaryRedirect)
		}))
		t.Cleanup(origin.Close)

		_, err := NewCatalog("secret-key", origin.URL, false).ListModels(context.Background())
		if err == nil || !strings.Contains(err.Error(), "307 Temporary Redirect") {
			t.Fatalf("ListModels() error = %v, want redirect response error", err)
		}
		if destinationCalled {
			t.Fatal("catalogue followed a redirect")
		}
	})
}

func TestCatalogHonorsCancellation(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, err := NewCatalog("test-key", server.URL, false).ListModels(ctx)
	if err == nil || !strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Fatalf("ListModels() error = %v, want context canceled", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("ListModels() took %v after cancellation", elapsed)
	}
}
