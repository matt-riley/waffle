package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/llm/anthropicp"
	"github.com/matt-riley/waffle/internal/llm/openaip"
	"github.com/matt-riley/waffle/internal/modelcatalog"
)

var presetCases = []struct {
	input, runtimeType, storedBase, effectiveBase string
}{
	{"openai", "openai", "", openaip.DefaultBaseURL},
	{"anthropic", "anthropic", "", anthropicp.DefaultBaseURL},
	{"openrouter", "openai", "https://openrouter.ai/api/v1", "https://openrouter.ai/api/v1"},
}

func TestProviderPresetDefaultsAndOverrides(t *testing.T) {
	for _, tt := range presetCases {
		t.Run(tt.input, func(t *testing.T) {
			preset, err := resolveProviderPreset(tt.input, "")
			if err != nil {
				t.Fatalf("resolveProviderPreset() error = %v", err)
			}
			if preset.Name != tt.input || preset.RuntimeType != tt.runtimeType || preset.StoredBaseURL != tt.storedBase {
				t.Fatalf("resolveProviderPreset() = %+v, want name/type/base %q/%q/%q", preset, tt.input, tt.runtimeType, tt.storedBase)
			}
			connection, _, err := effectiveCatalogConnection("primary", config.ProviderConnection{
				Type:    preset.RuntimeType,
				BaseURL: preset.StoredBaseURL,
			}, "scope")
			if err != nil {
				t.Fatalf("effectiveCatalogConnection() error = %v", err)
			}
			if connection.BaseURL != tt.effectiveBase {
				t.Fatalf("effective base URL = %q, want %q", connection.BaseURL, tt.effectiveBase)
			}
		})
	}

	preset, err := resolveProviderPreset("  OPENAI-COMPATIBLE ", "HTTPS://Gateway.Example/v1/")
	if err != nil {
		t.Fatalf("resolveProviderPreset(openai-compatible override) error = %v", err)
	}
	if want := (providerPreset{Name: "openai-compatible", RuntimeType: "openai", StoredBaseURL: "https://gateway.example/v1"}); preset != want {
		t.Fatalf("resolveProviderPreset(openai-compatible override) = %+v, want %+v", preset, want)
	}

	for _, kind := range []string{"openai", "anthropic", "openrouter"} {
		t.Run(kind+" override", func(t *testing.T) {
			preset, err := resolveProviderPreset(kind, "https://gateway.example/v1/")
			if err != nil {
				t.Fatalf("resolveProviderPreset() error = %v", err)
			}
			if preset.StoredBaseURL != "https://gateway.example/v1" {
				t.Fatalf("stored base URL = %q, want normalized override", preset.StoredBaseURL)
			}
		})
	}

	for _, baseURL := range []string{
		"http://[::1]:11434/custom/v1",
		"https://gateway.example:8443/custom/models/v1",
	} {
		t.Run("valid "+baseURL, func(t *testing.T) {
			preset, err := resolveProviderPreset("openai-compatible", baseURL)
			if err != nil {
				t.Fatalf("resolveProviderPreset() error = %v", err)
			}
			if preset.StoredBaseURL != baseURL {
				t.Fatalf("stored base URL = %q, want %q", preset.StoredBaseURL, baseURL)
			}
		})
	}
}

func TestProviderPresetRejectsUnsafeAndMissingURLs(t *testing.T) {
	if _, err := resolveProviderPreset("openai-compatible", ""); err == nil || !strings.Contains(err.Error(), "base URL") {
		t.Fatalf("missing compatible URL error = %v, want base URL error", err)
	}
	if _, err := resolveProviderPreset("unknown", ""); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("unknown preset error = %v, want unsupported error", err)
	}

	unsafeURLs := []string{
		"/v1",
		"ftp://models.example/v1",
		"https:///v1",
		"https://:443/v1",
		"https://user:password@models.example/v1",
		"https://models.example/v1?account=secret",
		"https://models.example/v1#credentials",
	}
	for _, kind := range []string{"openai", "anthropic", "openrouter", "openai-compatible"} {
		for _, baseURL := range unsafeURLs {
			t.Run(kind+" "+baseURL, func(t *testing.T) {
				if _, err := resolveProviderPreset(kind, baseURL); err == nil {
					t.Fatalf("resolveProviderPreset(%q, %q) accepted unsafe URL", kind, baseURL)
				}
			})
		}
	}

	if _, _, err := effectiveCatalogConnection("primary", config.ProviderConnection{
		Type:    "openai",
		BaseURL: "https://models.example/v1?credential=secret",
	}, "scope"); err == nil {
		t.Fatal("effectiveCatalogConnection() accepted persisted URL query")
	}
}

func TestCatalogConnectionRecognizesOpenRouterAfterReload(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, "config.toml")
	body := `[providers.router]
type = "openai"
api_key = "secret://provider/router/api-key"
base_url = "https://eu.openrouter.ai:443/api/v1/"
`
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}
	connection, openRouter, err := effectiveCatalogConnection("router", cfg.Providers["router"], "scope")
	if err != nil {
		t.Fatalf("effectiveCatalogConnection() error = %v", err)
	}
	if !openRouter {
		t.Fatal("effectiveCatalogConnection() did not classify OpenRouter subdomain after reload")
	}

	store := modelcatalog.NewStore(home)
	if err := store.Save(connection, []modelcatalog.Model{{ID: "old-account/model"}}, time.Now()); err != nil {
		t.Fatalf("Save() old cache error = %v", err)
	}
	if err := store.Invalidate(connection.Name); err != nil {
		t.Fatalf("Invalidate() error = %v", err)
	}
	if _, err := store.Load(connection); err == nil {
		t.Fatal("Load() succeeded after cache deletion")
	}

	path := catalogueRequestPath(t, connection, openRouter)
	if path != "/api/v1/models/user" {
		t.Fatalf("catalogue request path = %q, want /api/v1/models/user", path)
	}
}

func TestCatalogConnectionTreatsCustomOverrideAsGenericOpenAI(t *testing.T) {
	preset, err := resolveProviderPreset("openrouter", "https://gateway.example/v1")
	if err != nil {
		t.Fatalf("resolveProviderPreset() error = %v", err)
	}
	connection, openRouter, err := effectiveCatalogConnection("router", config.ProviderConnection{
		Type:    preset.RuntimeType,
		BaseURL: preset.StoredBaseURL,
	}, "scope")
	if err != nil {
		t.Fatalf("effectiveCatalogConnection() error = %v", err)
	}
	if openRouter {
		t.Fatal("custom OpenRouter override classified as OpenRouter")
	}
	path := catalogueRequestPath(t, connection, openRouter)
	if path != "/v1/models" {
		t.Fatalf("catalogue request path = %q, want /v1/models", path)
	}

	for _, baseURL := range []string{
		"https://evilopenrouter.ai/v1",
		"https://openrouter.ai.evil.example/v1",
	} {
		connection, openRouter, err := effectiveCatalogConnection("router", config.ProviderConnection{
			Type:    "openai",
			BaseURL: baseURL,
		}, "scope")
		if err != nil {
			t.Fatalf("effectiveCatalogConnection(%q) error = %v", baseURL, err)
		}
		if openRouter {
			t.Fatalf("effectiveCatalogConnection(%q) accepted hostname lookalike: %+v", baseURL, connection)
		}
	}
}

func TestProviderCatalogServiceNewEnrollmentNeverReadsCache(t *testing.T) {
	home := t.TempDir()
	blockedRoot := filepath.Join(home, "not-a-directory")
	if err := os.WriteFile(blockedRoot, []byte("cache access must fail"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 19, 13, 0, 0, 0, time.UTC)
	source := &testCatalogueSource{models: []modelcatalog.Model{{ID: "z"}, {ID: "a"}}}
	service := &providerCatalogueService{
		store:     &modelcatalog.Store{Root: blockedRoot, Now: func() time.Time { return now }},
		now:       func() time.Time { return now },
		newSource: func(modelcatalog.Connection, string, bool) (modelcatalog.Source, error) { return source, nil },
	}
	connection := modelcatalog.Connection{Name: "new-enrollment", Type: "openai", BaseURL: openaip.DefaultBaseURL}
	result, err := service.Discover(t.Context(), connection, "secret")
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if source.calls.Load() != 1 {
		t.Fatalf("source calls = %d, want 1", source.calls.Load())
	}
	if result.Connection != connection || !result.FetchedAt.Equal(now) || result.SchemaVersion != modelcatalog.SchemaVersion {
		t.Fatalf("Discover() result metadata = %+v, want connection and fixed fetch time", result)
	}
	if got := catalogueModelIDs(result.Models); !reflect.DeepEqual(got, []string{"a", "z"}) {
		t.Fatalf("Discover() model IDs = %v, want normalized IDs", got)
	}

	t.Setenv("WAFFLE_HOME", home)
	opened, err := defaultProviderCatalogue()
	if err != nil {
		t.Fatalf("defaultProviderCatalogue() error = %v", err)
	}
	defaultService, ok := opened.(*providerCatalogueService)
	if !ok {
		t.Fatalf("defaultProviderCatalogue() type = %T, want *providerCatalogueService", opened)
	}
	if want := filepath.Join(home, "cache", "model-catalogs"); defaultService.store.Root != want {
		t.Fatalf("default cache root = %q, want %q", defaultService.store.Root, want)
	}
	committedConnection := connection
	committedConnection.ScopeID = " committed-opaque-scope "
	if err := opened.Save(committedConnection, result.Models, result.FetchedAt); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	saved, err := defaultService.store.Load(committedConnection)
	if err != nil {
		t.Fatalf("Load() saved cache error = %v", err)
	}
	if saved.Connection.ScopeID != committedConnection.ScopeID {
		t.Fatalf("saved scope ID = %q, want unaltered opaque scope %q", saved.Connection.ScopeID, committedConnection.ScopeID)
	}
	cachePath := filepath.Join(home, "cache", "model-catalogs", connection.Name+".json")
	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("saved cache stat error = %v", err)
	}
	if err := opened.Invalidate(connection.Name); err != nil {
		t.Fatalf("Invalidate() error = %v", err)
	}
	if _, err := os.Stat(cachePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalidated cache stat error = %v, want not exist", err)
	}
}

func TestProviderCatalogServiceSaveRequiresCommittedScope(t *testing.T) {
	service := &providerCatalogueService{}
	for _, scopeID := range []string{"", " \t "} {
		connection := modelcatalog.Connection{
			Name:    "primary",
			Type:    "openai",
			BaseURL: openaip.DefaultBaseURL,
			ScopeID: scopeID,
		}
		err := service.Save(connection, []modelcatalog.Model{{ID: "model"}}, time.Now())
		if err == nil || !strings.Contains(err.Error(), "scope ID") {
			t.Fatalf("Save() scope %q error = %v, want scope ID error before store access", scopeID, err)
		}
	}
}

func TestPromptLineNoReadAheadPreservesHiddenSecretInputAndCapsLines(t *testing.T) {
	reader := &oneBytePromptReader{reader: strings.NewReader("openrouter\nhidden-secret\n"), strict: true}
	var output strings.Builder
	value, err := promptLineNoReadAhead(reader, &output, "Provider", "")
	if err != nil {
		t.Fatal(err)
	}
	if value != "openrouter" {
		t.Fatalf("prompt value = %q", value)
	}
	reader.strict = false
	remaining, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(remaining) != "hidden-secret\n" {
		t.Fatalf("remaining input = %q, want untouched hidden secret", remaining)
	}

	tooLong := strings.NewReader(strings.Repeat("x", 64*1024+1) + "\n")
	if _, err := promptLineNoReadAhead(tooLong, io.Discard, "value", ""); err == nil || !strings.Contains(err.Error(), "too long") {
		t.Fatalf("oversize line error = %v", err)
	}
}

func TestCataloguePromptsStopOnOutputAndEOFErrors(t *testing.T) {
	outputErr := errors.New("terminal output failed")
	if _, err := promptLineNoReadAhead(strings.NewReader("must-not-be-read\n"), errorWriter{err: outputErr}, "Provider", ""); !errors.Is(err, outputErr) {
		t.Fatalf("prompt output error = %v, want %v", err, outputErr)
	}
	if _, err := promptLineNoReadAhead(strings.NewReader(""), io.Discard, "Provider", ""); !errors.Is(err, io.EOF) {
		t.Fatalf("prompt EOF error = %v, want EOF", err)
	}
	if _, err := renderCataloguePage(errorWriter{err: outputErr}, []modelcatalog.Model{{ID: "model"}}, 0); !errors.Is(err, outputErr) {
		t.Fatalf("render output error = %v, want %v", err, outputErr)
	}
}

func TestProviderCatalogServiceUsesFreshExpiredAndForcedCache(t *testing.T) {
	now := time.Date(2026, 7, 19, 14, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		fetchedAt time.Time
		force     bool
		wantCalls int32
		wantID    string
	}{
		{name: "fresh", fetchedAt: now.Add(-time.Hour), wantID: "cached"},
		{name: "expired", fetchedAt: now.Add(-modelcatalog.DefaultTTL), wantCalls: 1, wantID: "refreshed"},
		{name: "forced", fetchedAt: now.Add(-time.Hour), force: true, wantCalls: 1, wantID: "refreshed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := modelcatalog.NewStore(t.TempDir())
			store.Now = func() time.Time { return now }
			connection := modelcatalog.Connection{
				Name:    "primary",
				Type:    "openai",
				BaseURL: openaip.DefaultBaseURL,
				ScopeID: "enrollment-scope",
			}
			if err := store.Save(connection, []modelcatalog.Model{{ID: "cached"}}, tt.fetchedAt); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			source := &testCatalogueSource{models: []modelcatalog.Model{{ID: "refreshed"}}}
			service := &providerCatalogueService{
				store: store,
				now:   func() time.Time { return now },
				newSource: func(got modelcatalog.Connection, key string, openRouter bool) (modelcatalog.Source, error) {
					if got != connection || key != "secret" || openRouter {
						t.Fatalf("source factory args = %+v/%q/%t", got, key, openRouter)
					}
					return source, nil
				},
			}
			result, err := service.Models(t.Context(), connection, "secret", tt.force)
			if err != nil {
				t.Fatalf("Models() error = %v", err)
			}
			if source.calls.Load() != tt.wantCalls {
				t.Fatalf("source calls = %d, want %d", source.calls.Load(), tt.wantCalls)
			}
			if got := catalogueModelIDs(result.Models); !reflect.DeepEqual(got, []string{tt.wantID}) {
				t.Fatalf("Models() IDs = %v, want [%s]", got, tt.wantID)
			}
		})
	}

	service := &providerCatalogueService{
		store: modelcatalog.NewStore(t.TempDir()),
		newSource: func(modelcatalog.Connection, string, bool) (modelcatalog.Source, error) {
			return &testCatalogueSource{models: []modelcatalog.Model{{ID: "unexpected"}}}, nil
		},
	}
	for _, scopeID := range []string{"", " "} {
		_, err := service.Models(t.Context(), modelcatalog.Connection{Name: "primary", Type: "openai", BaseURL: openaip.DefaultBaseURL, ScopeID: scopeID}, "secret", false)
		if err == nil || !strings.Contains(err.Error(), "scope ID") {
			t.Fatalf("Models() scope %q error = %v, want scope ID error", scopeID, err)
		}
	}
}

func TestProviderCatalogServiceRedactsRefreshErrors(t *testing.T) {
	const key = "super-secret-catalog-key"
	store := modelcatalog.NewStore(t.TempDir())
	service := &providerCatalogueService{
		store: store,
		newSource: func(modelcatalog.Connection, string, bool) (modelcatalog.Source, error) {
			return &testCatalogueSource{err: errors.New("upstream rejected credential " + key)}, nil
		},
	}
	connection := modelcatalog.Connection{
		Name:    "primary",
		Type:    "openai",
		BaseURL: openaip.DefaultBaseURL,
		ScopeID: "scope",
	}
	_, err := service.Models(t.Context(), connection, key, true)
	if err == nil {
		t.Fatal("Models() error = nil, want refresh failure")
	}
	if strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("Models() error = %q, want redacted credential", err)
	}
}

type testCatalogueSource struct {
	models []modelcatalog.Model
	err    error
	calls  atomic.Int32
}

func (s *testCatalogueSource) ListModels(context.Context) ([]modelcatalog.Model, error) {
	s.calls.Add(1)
	return s.models, s.err
}

type catalogueRoundTripper func(*http.Request) (*http.Response, error)

type oneBytePromptReader struct {
	reader io.Reader
	strict bool
}

type errorWriter struct {
	err error
}

func (w errorWriter) Write([]byte) (int, error) { return 0, w.err }

func (r *oneBytePromptReader) Read(p []byte) (int, error) {
	if r.strict && len(p) != 1 {
		return 0, fmt.Errorf("prompt requested %d bytes, want exactly 1", len(p))
	}
	return r.reader.Read(p)
}

func (f catalogueRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func catalogueRequestPath(t *testing.T, connection modelcatalog.Connection, openRouter bool) string {
	t.Helper()
	source, err := newCatalogueSource(connection, "secret", openRouter)
	if err != nil {
		t.Fatalf("newCatalogueSource() error = %v", err)
	}
	catalogue, ok := source.(*openaip.Catalog)
	if !ok {
		t.Fatalf("newCatalogueSource() type = %T, want *openaip.Catalog", source)
	}
	var path string
	catalogue.Client = &http.Client{Transport: catalogueRoundTripper(func(req *http.Request) (*http.Response, error) {
		path = req.URL.Path
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"data":[]}`)),
			Request:    req,
		}, nil
	})}
	if _, err := source.ListModels(t.Context()); err != nil {
		t.Fatalf("ListModels() error = %v", err)
	}
	return path
}

func catalogueModelIDs(models []modelcatalog.Model) []string {
	ids := make([]string, len(models))
	for i, model := range models {
		ids[i] = model.ID
	}
	return ids
}
