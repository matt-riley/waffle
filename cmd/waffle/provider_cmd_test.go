package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/modelcatalog"
	"github.com/matt-riley/waffle/internal/providerconfig"
)

type fakeProviderManager struct {
	preflightErr     error
	preflighted      bool
	addRequest       providerconfig.AddRequest
	addErr           error
	addScopeID       string
	addModelRequest  providerconfig.AddModelRequest
	addModelErr      error
	snapshot         providerconfig.CatalogSnapshot
	snapshotName     string
	snapshotErr      error
	snapshotCalls    int
	list             []byte
	testName         string
	removeName       string
	removeErr        error
	activateAlias    string
	removeAlias      string
	replacementAlias string
	events           *[]string
}

func (f *fakeProviderManager) Preflight(context.Context) error {
	f.preflighted = true
	return f.preflightErr
}

func (f *fakeProviderManager) Add(_ context.Context, req providerconfig.AddRequest) error {
	f.addRequest = req
	if f.addErr == nil && f.addScopeID != "" {
		f.snapshot = providerconfig.CatalogSnapshot{Connection: req.Connection, APIKey: req.APIKey, ScopeID: f.addScopeID}
	}
	return f.addErr
}
func (f *fakeProviderManager) AddModel(_ context.Context, req providerconfig.AddModelRequest) error {
	f.addModelRequest = req
	return f.addModelErr
}
func (f *fakeProviderManager) CatalogSnapshot(_ context.Context, name string) (providerconfig.CatalogSnapshot, error) {
	f.snapshotCalls++
	f.snapshotName = name
	return f.snapshot, f.snapshotErr
}
func (f *fakeProviderManager) List(context.Context) ([]byte, error) { return f.list, nil }
func (f *fakeProviderManager) Test(_ context.Context, name string) error {
	f.testName = name
	return nil
}
func (f *fakeProviderManager) Remove(_ context.Context, name string) error {
	f.removeName = name
	if f.events != nil {
		*f.events = append(*f.events, "remove")
	}
	return f.removeErr
}

type fakeProviderCatalogue struct {
	result          modelcatalog.Result
	discoverErr     error
	discoverCalls   int
	modelsErr       error
	modelsCalls     int
	connection      modelcatalog.Connection
	apiKey          string
	force           bool
	invalidateName  string
	invalidateErr   error
	invalidateCalls int
	events          *[]string
	saveConnection  modelcatalog.Connection
	saveModels      []modelcatalog.Model
	saveFetchedAt   time.Time
	saveErr         error
	saveCalls       int
}

func (f *fakeProviderCatalogue) Discover(_ context.Context, connection modelcatalog.Connection, apiKey string) (modelcatalog.Result, error) {
	f.discoverCalls++
	f.connection, f.apiKey = connection, apiKey
	return f.result, f.discoverErr
}

func (f *fakeProviderCatalogue) Models(_ context.Context, connection modelcatalog.Connection, apiKey string, force bool) (modelcatalog.Result, error) {
	f.modelsCalls++
	f.connection, f.apiKey, f.force = connection, apiKey, force
	return f.result, f.modelsErr
}

func (f *fakeProviderCatalogue) Save(connection modelcatalog.Connection, models []modelcatalog.Model, fetchedAt time.Time) error {
	f.saveCalls++
	f.saveConnection = connection
	f.saveModels = append([]modelcatalog.Model(nil), models...)
	f.saveFetchedAt = fetchedAt
	return f.saveErr
}

func (f *fakeProviderCatalogue) Invalidate(name string) error {
	f.invalidateCalls++
	f.invalidateName = name
	if f.events != nil {
		*f.events = append(*f.events, "invalidate")
	}
	return f.invalidateErr
}
func (f *fakeProviderManager) ActivateModel(_ context.Context, alias string) error {
	f.activateAlias = alias
	return nil
}
func (f *fakeProviderManager) RemoveModel(_ context.Context, alias, replacement string) error {
	f.removeAlias, f.replacementAlias = alias, replacement
	return nil
}

func TestProviderCommandRejectsRawAPIKeyArgumentWithoutLeakingIt(t *testing.T) {
	fake := installFakeProviderManager(t)
	key := "raw-key-must-not-leak"
	var stdout, stderr bytes.Buffer
	err := providerCmd(context.Background(), []string{"add", "--name", "openai", "--type", "openai", "--model", "gpt=gpt-test", "--api-key", key}, strings.NewReader(""), &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("provider add error = %v, want unknown option", err)
	}
	if strings.Contains(err.Error()+stdout.String()+stderr.String(), key) {
		t.Fatal("raw API key leaked in command output")
	}
	if fake.addRequest.ConnectionName != "" {
		t.Fatal("manager called for rejected raw API key")
	}
}

func TestProviderCommandAddReadsAPIKeyFromStdinAndCreatesAliases(t *testing.T) {
	fake := installFakeProviderManager(t)
	var stdout, stderr bytes.Buffer
	err := providerCmd(context.Background(), []string{
		"add", "--name", "openai", "--type", "openai",
		"--base-url", "https://api.example/v1",
		"--model", "gpt=gpt-test", "--model", "small=gpt-small",
		"--default", "gpt", "--utility", "small", "--api-key-stdin",
	}, strings.NewReader("stdin-secret\n"), &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	req := fake.addRequest
	if req.APIKey != "stdin-secret" || req.ConnectionName != "openai" || req.Connection.Type != "openai" || req.Connection.BaseURL != "https://api.example/v1" {
		t.Fatalf("Add request = %#v", req)
	}
	if req.Models["gpt"].Model != "gpt-test" || req.Models["small"].Model != "gpt-small" || req.DefaultModel != "gpt" || req.UtilityModel != "small" {
		t.Fatalf("Add aliases = %#v", req)
	}
	if strings.Contains(stdout.String()+stderr.String(), req.APIKey) {
		t.Fatal("API key leaked in output")
	}
}

func TestProviderCommandAddUsesHiddenSecretReaderByDefault(t *testing.T) {
	fake := installFakeProviderManager(t)
	called := false
	old := providerSecretReader
	providerSecretReader = func(io.Reader, io.Writer) (string, error) {
		called = true
		return "hidden-secret", nil
	}
	t.Cleanup(func() { providerSecretReader = old })
	var stdout, stderr bytes.Buffer
	err := providerCmd(context.Background(), []string{"add", "--name", "anthropic", "--type", "anthropic", "--model", "claude=claude-test", "--default", "claude"}, strings.NewReader(""), &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if !called || fake.addRequest.APIKey != "hidden-secret" {
		t.Fatalf("hidden reader called=%v request=%#v", called, fake.addRequest)
	}
	if !strings.Contains(stderr.String(), "input hidden") {
		t.Fatalf("prompt = %q, want hidden-input notice", stderr.String())
	}
}

func TestProviderCommandBareAddCollectsAnActivatingEnrollment(t *testing.T) {
	fake := installFakeProviderManager(t)
	fake.addScopeID = "scope"
	installFakeProviderCatalogue(t, &fakeProviderCatalogue{result: discoveredCatalogue(
		modelcatalog.Model{ID: "gpt-test"},
		modelcatalog.Model{ID: "gpt-small"},
	)})
	old := providerSecretReader
	providerSecretReader = func(io.Reader, io.Writer) (string, error) { return "hidden-secret", nil }
	t.Cleanup(func() { providerSecretReader = old })
	input := strings.NewReader("openai\n\n1\n2\nn\n")
	if err := providerCmd(context.Background(), []string{"add"}, input, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if fake.addRequest.DefaultModel != "gpt-test" || fake.addRequest.UtilityModel != "gpt-small" || len(fake.addRequest.Models) != 2 {
		t.Fatalf("bare add request = %#v", fake.addRequest)
	}
}

func TestProviderCommandBareAddDiscoversAndSelectsDefaultUtilityAndFavourites(t *testing.T) {
	fake := installFakeProviderManager(t)
	fake.addScopeID = "committed-openrouter-scope"
	catalogue := &fakeProviderCatalogue{result: discoveredCatalogue(
		modelcatalog.Model{ID: "anthropic/claude-3.7-sonnet", DisplayName: "Claude Sonnet 3.7"},
		modelcatalog.Model{ID: "anthropic/claude-sonnet-4.6", DisplayName: "Claude Sonnet 4.6"},
		modelcatalog.Model{ID: "google/gemini-2.0-flash", DisplayName: "Gemini 2.0 Flash"},
		modelcatalog.Model{ID: "mistralai/mistral-small", DisplayName: "Mistral Small"},
	)}
	installFakeProviderCatalogue(t, catalogue)
	installProviderSecretReader(t, "hidden-openrouter-key", nil)

	input := strings.NewReader("openrouter\n\nclaude sonnet\n2\ngemini\n1\ny\nmistral\n1\nn\n")
	var stdout, stderr bytes.Buffer
	if err := providerCmd(t.Context(), []string{"add"}, input, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}

	wantModels := map[string]string{
		"anthropic-claude-sonnet-4-6": "anthropic/claude-sonnet-4.6",
		"google-gemini-2-0-flash":     "google/gemini-2.0-flash",
		"mistralai-mistral-small":     "mistralai/mistral-small",
	}
	if fake.addRequest.ConnectionName != "openrouter" || fake.addRequest.Connection.Type != "openai" || fake.addRequest.Connection.BaseURL != openRouterBaseURL {
		t.Fatalf("Add connection = %#v", fake.addRequest)
	}
	if len(fake.addRequest.Models) != len(wantModels) {
		t.Fatalf("Add models = %#v, want %v", fake.addRequest.Models, wantModels)
	}
	for alias, upstream := range wantModels {
		if got := fake.addRequest.Models[alias]; got != (config.ModelTarget{Model: upstream}) {
			t.Fatalf("Add model %q = %#v, want upstream %q; all models = %#v", alias, got, upstream, fake.addRequest.Models)
		}
	}
	if fake.addRequest.DefaultModel != "anthropic-claude-sonnet-4-6" || fake.addRequest.UtilityModel != "google-gemini-2-0-flash" {
		t.Fatalf("Add roles = default %q utility %q", fake.addRequest.DefaultModel, fake.addRequest.UtilityModel)
	}
	if catalogue.discoverCalls != 1 || catalogue.modelsCalls != 0 || catalogue.apiKey != "hidden-openrouter-key" {
		t.Fatalf("catalogue calls discover=%d models=%d key=%q", catalogue.discoverCalls, catalogue.modelsCalls, catalogue.apiKey)
	}
	if catalogue.connection.Name != "openrouter" || catalogue.connection.Type != "openai" || catalogue.connection.BaseURL != openRouterBaseURL || catalogue.connection.ScopeID != "" {
		t.Fatalf("discovery connection = %+v", catalogue.connection)
	}
	if catalogue.saveCalls != 1 || catalogue.saveConnection.ScopeID != "committed-openrouter-scope" || fake.snapshotCalls != 1 {
		t.Fatalf("post-commit snapshot/save calls=%d/%d connection=%+v", fake.snapshotCalls, catalogue.saveCalls, catalogue.saveConnection)
	}
	if strings.Contains(stderr.String(), "base URL") {
		t.Fatalf("OpenRouter preset unexpectedly asked for a URL: %q", stderr.String())
	}
}

func TestProviderCommandSmallCatalogueDisplaysDirectly(t *testing.T) {
	fake := installFakeProviderManager(t)
	fake.addScopeID = "scope"
	catalogue := &fakeProviderCatalogue{result: discoveredCatalogue(
		modelcatalog.Model{ID: "gpt-4.1", DisplayName: "GPT 4.1"},
		modelcatalog.Model{ID: "gpt-4.1-mini", DisplayName: "GPT 4.1 Mini"},
	)}
	installFakeProviderCatalogue(t, catalogue)
	installProviderSecretReader(t, "secret", nil)

	var stderr bytes.Buffer
	if err := providerCmd(t.Context(), []string{"add"}, strings.NewReader("openai\n\n1\n-\nn\n"), io.Discard, &stderr); err != nil {
		t.Fatal(err)
	}
	output := stderr.String()
	if !strings.Contains(output, "gpt-4.1") || !strings.Contains(output, "gpt-4.1-mini") {
		t.Fatalf("small catalogue was not displayed directly: %q", output)
	}
	if !strings.Contains(output, "Utility model") || !strings.Contains(output, "uses default") {
		t.Fatalf("utility prompt did not explain '-' fallback semantics: %q", output)
	}
	if fake.addRequest.DefaultModel != "gpt-4-1" || len(fake.addRequest.Models) != 1 {
		t.Fatalf("Add request = %#v", fake.addRequest)
	}
}

func TestProviderCommandLargeCatalogueSearchesAndPaginatesTwenty(t *testing.T) {
	fake := installFakeProviderManager(t)
	fake.addScopeID = "scope"
	models := make([]modelcatalog.Model, 51)
	for i := range models {
		models[i] = modelcatalog.Model{ID: fmt.Sprintf("vendor/model-%02d", i+1), DisplayName: fmt.Sprintf("Model %02d", i+1)}
	}
	catalogue := &fakeProviderCatalogue{result: discoveredCatalogue(models...)}
	installFakeProviderCatalogue(t, catalogue)
	installProviderSecretReader(t, "secret", nil)

	var stderr bytes.Buffer
	input := strings.NewReader("openai\n\nmodel\nn\n1\n-\nn\n")
	if err := providerCmd(t.Context(), []string{"add"}, input, io.Discard, &stderr); err != nil {
		t.Fatal(err)
	}
	if max := maxNumberedCatalogueRun(stderr.String()); max > 20 {
		t.Fatalf("catalogue page displayed %d rows, want at most 20:\n%s", max, stderr.String())
	}
	if !strings.Contains(stderr.String(), "Page 1") || !strings.Contains(stderr.String(), "Page 2") {
		t.Fatalf("large catalogue did not paginate: %q", stderr.String())
	}
	if fake.addRequest.Models["vendor-model-21"].Model != "vendor/model-21" {
		t.Fatalf("next-page selection = %#v", fake.addRequest.Models)
	}
}

func TestProviderCommandAliasCollisionPromptsExplicitAlias(t *testing.T) {
	fake := installFakeProviderManager(t)
	fake.addScopeID = "scope"
	fake.list = providerListingWithAliases(t, "vendor-model")
	catalogue := &fakeProviderCatalogue{result: discoveredCatalogue(modelcatalog.Model{ID: "vendor/model", DisplayName: "Vendor Model"})}
	installFakeProviderCatalogue(t, catalogue)
	installProviderSecretReader(t, "secret", nil)

	var stderr bytes.Buffer
	input := strings.NewReader("openai\nprimary\n1\nBAD ALIAS\nselected-model\n-\nn\n")
	if err := providerCmd(t.Context(), []string{"add"}, input, io.Discard, &stderr); err != nil {
		t.Fatal(err)
	}
	if fake.addRequest.Models["selected-model"].Model != "vendor/model" {
		t.Fatalf("Add models = %#v", fake.addRequest.Models)
	}
	if !strings.Contains(stderr.String(), "already exists") || !strings.Contains(stderr.String(), "invalid model alias") {
		t.Fatalf("collision/validation prompts missing: %q", stderr.String())
	}
}

func TestProviderCommandDiscoveryFailureOffersManualEntryWithoutCache(t *testing.T) {
	fake := installFakeProviderManager(t)
	catalogue := &fakeProviderCatalogue{discoverErr: errors.New("upstream rejected hidden-secret")}
	installFakeProviderCatalogue(t, catalogue)
	installProviderSecretReader(t, "hidden-secret", nil)

	var stderr bytes.Buffer
	input := strings.NewReader("anthropic\n\ny\nclaude=claude-manual\nclaude\n-\n")
	if err := providerCmd(t.Context(), []string{"add"}, input, io.Discard, &stderr); err != nil {
		t.Fatal(err)
	}
	if fake.addRequest.Models["claude"].Model != "claude-manual" || fake.addRequest.DefaultModel != "claude" {
		t.Fatalf("manual Add request = %#v", fake.addRequest)
	}
	if catalogue.discoverCalls != 1 || catalogue.modelsCalls != 0 || catalogue.saveCalls != 0 {
		t.Fatalf("catalogue calls discover=%d models=%d save=%d", catalogue.discoverCalls, catalogue.modelsCalls, catalogue.saveCalls)
	}
	if strings.Contains(stderr.String(), "hidden-secret") || !strings.Contains(stderr.String(), "[REDACTED]") || !strings.Contains(strings.ToLower(stderr.String()), "manual") {
		t.Fatalf("manual fallback output was unsafe or incomplete: %q", stderr.String())
	}
}

func TestProviderCommandDiscoveryEmptyOffersManualEntryWithoutCache(t *testing.T) {
	fake := installFakeProviderManager(t)
	catalogue := &fakeProviderCatalogue{result: discoveredCatalogue()}
	installFakeProviderCatalogue(t, catalogue)
	installProviderSecretReader(t, "hidden-secret", nil)

	input := strings.NewReader("anthropic\n\ny\nclaude=claude-manual\nclaude\n-\n")
	if err := providerCmd(t.Context(), []string{"add"}, input, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if fake.addRequest.Models["claude"].Model != "claude-manual" || catalogue.discoverCalls != 1 || catalogue.modelsCalls != 0 || catalogue.saveCalls != 0 {
		t.Fatalf("empty discovery fallback add=%#v catalogue=%+v", fake.addRequest, catalogue)
	}
}

func TestProviderCommandExplicitModelsBypassDiscovery(t *testing.T) {
	fake := installFakeProviderManager(t)
	catalogue := &fakeProviderCatalogue{discoverErr: errors.New("must not discover"), saveErr: errors.New("must not save")}
	installFakeProviderCatalogue(t, catalogue)

	err := providerCmd(t.Context(), []string{
		"add", "--name", "automated", "--type", "openai", "--model", "gpt=gpt-exact", "--default", "gpt", "--api-key-stdin",
	}, strings.NewReader("automation-secret\n"), io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if catalogue.discoverCalls != 0 || catalogue.modelsCalls != 0 || catalogue.saveCalls != 0 {
		t.Fatalf("explicit enrollment touched catalogue: %+v", catalogue)
	}
	if fake.addRequest.Models["gpt"].Model != "gpt-exact" {
		t.Fatalf("Add request = %#v", fake.addRequest)
	}
}

func TestProviderCommandNonInteractiveMissingModelFailsBeforeSecret(t *testing.T) {
	installFakeProviderManager(t)
	reader := &countingReader{reader: strings.NewReader("must-not-be-read")}
	err := providerCmd(t.Context(), []string{"add", "--name", "automated", "--type", "openai", "--api-key-stdin"}, reader, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "--model") {
		t.Fatalf("error = %v, want missing --model", err)
	}
	if reader.reads != 0 {
		t.Fatalf("stdin reads = %d, want zero", reader.reads)
	}
}

func TestProviderCommandPartialFlagAutomationDoesNotEnterGuidedDiscovery(t *testing.T) {
	fake := installFakeProviderManager(t)
	catalogue := &fakeProviderCatalogue{}
	installFakeProviderCatalogue(t, catalogue)
	secretRead := false
	old := providerSecretReader
	providerSecretReader = func(io.Reader, io.Writer) (string, error) {
		secretRead = true
		return "secret", nil
	}
	t.Cleanup(func() { providerSecretReader = old })

	var stderr bytes.Buffer
	err := providerCmd(t.Context(), []string{"add", "--name", "partial"}, strings.NewReader("openai\n"), io.Discard, &stderr)
	if err == nil || !strings.Contains(err.Error(), "--model") {
		t.Fatalf("error = %v, want explicit automation error", err)
	}
	if secretRead || catalogue.discoverCalls != 0 || fake.preflighted || stderr.Len() != 0 {
		t.Fatalf("partial automation prompted or progressed: secret=%t discover=%d preflight=%t stderr=%q", secretRead, catalogue.discoverCalls, fake.preflighted, stderr.String())
	}
}

func TestProviderCommandPreflightStillPrecedesSecretAndDiscovery(t *testing.T) {
	fake := installFakeProviderManager(t)
	fake.preflightErr = errors.New("identity unavailable")
	catalogue := &fakeProviderCatalogue{}
	installFakeProviderCatalogue(t, catalogue)
	secretRead := false
	old := providerSecretReader
	providerSecretReader = func(io.Reader, io.Writer) (string, error) {
		secretRead = true
		return "secret", nil
	}
	t.Cleanup(func() { providerSecretReader = old })

	err := providerCmd(t.Context(), []string{"add"}, strings.NewReader("openai\n\n"), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "identity unavailable") {
		t.Fatalf("error = %v", err)
	}
	if !fake.preflighted || secretRead || catalogue.discoverCalls != 0 {
		t.Fatalf("ordering preflight=%t secret=%t discovery=%d", fake.preflighted, secretRead, catalogue.discoverCalls)
	}
}

func TestProviderCommandBareAddRejectsInvalidConnectionBeforeSecret(t *testing.T) {
	fake := installFakeProviderManager(t)
	secretRead := false
	old := providerSecretReader
	providerSecretReader = func(io.Reader, io.Writer) (string, error) {
		secretRead = true
		return "secret", nil
	}
	t.Cleanup(func() { providerSecretReader = old })

	err := providerCmd(t.Context(), []string{"add"}, strings.NewReader("openai\nBAD NAME\n"), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "invalid connection name") {
		t.Fatalf("error = %v", err)
	}
	if secretRead || fake.preflighted {
		t.Fatalf("invalid guided input progressed to preflight/secret: preflight=%t secret=%t", fake.preflighted, secretRead)
	}
}

func TestProviderCommandFailedAddDoesNotSaveCatalogue(t *testing.T) {
	fake := installFakeProviderManager(t)
	fake.addErr = errors.New("transaction failed")
	catalogue := &fakeProviderCatalogue{result: discoveredCatalogue(modelcatalog.Model{ID: "gpt-4.1"})}
	installFakeProviderCatalogue(t, catalogue)
	installProviderSecretReader(t, "secret", nil)

	err := providerCmd(t.Context(), []string{"add"}, strings.NewReader("openai\n\n1\n-\nn\n"), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "transaction failed") {
		t.Fatalf("error = %v", err)
	}
	if catalogue.saveCalls != 0 || fake.snapshotCalls != 0 {
		t.Fatalf("post-failure snapshot/save calls = %d/%d", fake.snapshotCalls, catalogue.saveCalls)
	}
}

func TestProviderCommandCacheSaveFailureWarnsAfterCommittedSuccess(t *testing.T) {
	const apiKey = "cache-warning-secret"
	const scopeID = "committed-private-scope"
	fake := installFakeProviderManager(t)
	fake.addScopeID = scopeID
	catalogue := &fakeProviderCatalogue{
		result:  discoveredCatalogue(modelcatalog.Model{ID: "gpt-4.1"}),
		saveErr: errors.New("cannot save " + apiKey + " in " + scopeID),
	}
	installFakeProviderCatalogue(t, catalogue)
	installProviderSecretReader(t, apiKey, nil)

	var stdout, stderr bytes.Buffer
	err := providerCmd(t.Context(), []string{"add"}, strings.NewReader("openai\n\n1\n-\nn\n"), &stdout, &stderr)
	if err != nil {
		t.Fatalf("provider add returned post-commit cache error: %v", err)
	}
	if fake.addRequest.ConnectionName == "" || fake.snapshotCalls != 1 || fake.snapshotName != "openai" || catalogue.saveCalls != 1 || catalogue.saveConnection.ScopeID != scopeID {
		t.Fatalf("commit/snapshot/save state add=%#v snapshots=%d name=%q saves=%d connection=%+v", fake.addRequest, fake.snapshotCalls, fake.snapshotName, catalogue.saveCalls, catalogue.saveConnection)
	}
	if !strings.Contains(stdout.String(), "provider openai validated and saved") || !strings.Contains(strings.ToLower(stderr.String()), "warning") {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if strings.Contains(stderr.String(), apiKey) || strings.Contains(stderr.String(), scopeID) {
		t.Fatalf("cache warning leaked private values: %q", stderr.String())
	}
}

func TestProviderCommandAPIKeyFileMustBeRegularAndMode0600(t *testing.T) {
	for _, tc := range []struct {
		name string
		make func(*testing.T) string
		want string
	}{
		{name: "directory", make: func(t *testing.T) string { return t.TempDir() }, want: "regular file"},
		{name: "wide mode", make: func(t *testing.T) string {
			path := filepath.Join(t.TempDir(), "key")
			if err := os.WriteFile(path, []byte("secret"), 0o644); err != nil {
				t.Fatal(err)
			}
			return path
		}, want: "0600"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installFakeProviderManager(t)
			path := tc.make(t)
			err := providerCmd(context.Background(), []string{"add", "--name", "openai", "--type", "openai", "--model", "gpt=gpt-test", "--api-key-file", path}, strings.NewReader(""), io.Discard, io.Discard)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestProviderCommandAPIKeyFileAndStdinAreMutuallyExclusive(t *testing.T) {
	installFakeProviderManager(t)
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := providerCmd(context.Background(), []string{"add", "--name", "openai", "--type", "openai", "--model", "gpt=gpt-test", "--api-key-file", path, "--api-key-stdin"}, strings.NewReader("secret"), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("error = %v, want mutually exclusive", err)
	}
}

func TestProviderCommandListJSONAndHumanOutputNeverExposeCredentials(t *testing.T) {
	fake := installFakeProviderManager(t)
	fake.list = []byte("{\"state\":\"ready\",\"default_model\":\"gpt\",\"providers\":{\"openai\":{\"type\":\"openai\"}},\"models\":{\"gpt\":{\"provider\":\"openai\",\"model\":\"gpt-test\"}}}\n")
	for _, args := range [][]string{{"list", "--json"}, {"list"}} {
		var stdout bytes.Buffer
		if err := providerCmd(context.Background(), args, strings.NewReader(""), &stdout, io.Discard); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(stdout.String(), "api_key") || strings.Contains(stdout.String(), "secret://") {
			t.Fatalf("list output exposes credential reference: %s", stdout.String())
		}
	}
}

func TestProviderCommandTestAndRemoveForwardExactConnection(t *testing.T) {
	fake := installFakeProviderManager(t)
	for _, args := range [][]string{{"test", "openai"}, {"remove", "openai"}} {
		if err := providerCmd(context.Background(), args, strings.NewReader(""), io.Discard, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	if fake.testName != "openai" || fake.removeName != "openai" {
		t.Fatalf("forwarded test=%q remove=%q", fake.testName, fake.removeName)
	}
}

func TestProviderCommandAddRedactsManagerError(t *testing.T) {
	fake := installFakeProviderManager(t)
	fake.addErr = errors.New("provider echoed stdin-secret")
	err := providerCmd(context.Background(), []string{"add", "--name", "openai", "--type", "openai", "--model", "gpt=gpt-test", "--api-key-stdin"}, strings.NewReader("stdin-secret"), io.Discard, io.Discard)
	if err == nil || strings.Contains(err.Error(), "stdin-secret") {
		t.Fatalf("error leaked key: %v", err)
	}
}

func TestProviderCommandPreflightsBeforeReadingSecret(t *testing.T) {
	fake := installFakeProviderManager(t)
	fake.preflightErr = errors.New("identity unavailable")
	read := false
	old := providerSecretReader
	providerSecretReader = func(io.Reader, io.Writer) (string, error) {
		read = true
		return "should-not-be-read", nil
	}
	t.Cleanup(func() { providerSecretReader = old })
	err := providerCmd(context.Background(), []string{"add", "--name", "openai", "--type", "openai", "--model", "gpt=gpt-test"}, strings.NewReader(""), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "identity unavailable") {
		t.Fatalf("error = %v, want preflight error", err)
	}
	if !fake.preflighted || read {
		t.Fatalf("preflighted=%v secretRead=%v", fake.preflighted, read)
	}
}

func TestProviderCommandModelLifecycleCommands(t *testing.T) {
	fake := installFakeProviderManager(t)
	if err := providerCmd(context.Background(), []string{"model", "activate", "gpt"}, strings.NewReader(""), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := providerCmd(context.Background(), []string{"model", "remove", "gpt", "--replace-with", "small"}, strings.NewReader(""), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if fake.activateAlias != "gpt" || fake.removeAlias != "gpt" || fake.replacementAlias != "small" {
		t.Fatalf("model calls = activate:%q remove:%q replace:%q", fake.activateAlias, fake.removeAlias, fake.replacementAlias)
	}
}

func TestProviderModelsCommandUsesCacheSearchRefreshAndStableJSON(t *testing.T) {
	fake := installFakeProviderManager(t)
	fake.snapshot = providerconfig.CatalogSnapshot{
		Connection: config.ProviderConnection{Type: "openai", BaseURL: "https://models.example/v1"},
		APIKey:     "catalogue-key",
		ScopeID:    "opaque-scope",
	}
	fetchedAt := time.Date(2026, 7, 19, 12, 34, 56, 0, time.UTC)
	catalogue := &fakeProviderCatalogue{result: modelcatalog.Result{
		Record: modelcatalog.Record{
			SchemaVersion: modelcatalog.SchemaVersion,
			Connection: modelcatalog.Connection{
				Name: "primary", Type: "openai", BaseURL: "https://models.example/v1", ScopeID: "opaque-scope",
			},
			FetchedAt: fetchedAt,
			Models: []modelcatalog.Model{
				{ID: "vendor/alpha", DisplayName: "Alpha", Capabilities: []string{"text"}},
				{ID: "vendor/claude-sonnet", DisplayName: "Claude Sonnet", Owner: "vendor", ContextWindow: 200000, Capabilities: []string{"tools", "text"}},
			},
		},
		Age:     125 * time.Second,
		Stale:   true,
		Warning: "model catalogue refresh failed; using cached models",
	}}
	installFakeProviderCatalogue(t, catalogue)

	argsCases := [][]string{
		{"models", "--json", "--search", "claude", "primary", "--refresh"},
		{"models", "primary", "--refresh", "--search", "claude", "--json"},
	}
	const want = "{\"connection\":\"primary\",\"fetched_at\":\"2026-07-19T12:34:56Z\",\"age_seconds\":125,\"stale\":true,\"warning\":\"model catalogue refresh failed; using cached models\",\"models\":[{\"id\":\"vendor/claude-sonnet\",\"display_name\":\"Claude Sonnet\",\"owner\":\"vendor\",\"context_window\":200000,\"capabilities\":[\"tools\",\"text\"]}]}\n"
	for _, args := range argsCases {
		var stdout bytes.Buffer
		if err := providerCmd(t.Context(), args, strings.NewReader(""), &stdout, io.Discard); err != nil {
			t.Fatalf("providerCmd(%v) error = %v", args, err)
		}
		if stdout.String() != want {
			t.Fatalf("providerCmd(%v) output = %q, want %q", args, stdout.String(), want)
		}
	}
	if fake.snapshotName != "primary" {
		t.Fatalf("CatalogSnapshot name = %q, want primary", fake.snapshotName)
	}
	wantConnection := modelcatalog.Connection{Name: "primary", Type: "openai", BaseURL: "https://models.example/v1", ScopeID: "opaque-scope"}
	if catalogue.modelsCalls != len(argsCases) || catalogue.connection != wantConnection || catalogue.apiKey != "catalogue-key" || !catalogue.force {
		t.Fatalf("Models calls=%d connection=%+v key=%q force=%t", catalogue.modelsCalls, catalogue.connection, catalogue.apiKey, catalogue.force)
	}

	for _, args := range [][]string{
		{"models", "primary", "--json", "--json"},
		{"models", "--refresh", "primary", "--refresh"},
		{"models", "--search", "one", "primary", "--search", "two"},
		{"models", "primary", "extra"},
		{"models", "--unknown", "primary"},
	} {
		if err := providerCmd(t.Context(), args, strings.NewReader(""), io.Discard, io.Discard); err == nil {
			t.Fatalf("providerCmd(%v) succeeded, want deterministic argument error", args)
		}
	}
}

func TestProviderModelsCommandNeverLeaksCredential(t *testing.T) {
	const apiKey = "sk-catalogue-must-stay-private"
	const scopeID = "scope-must-stay-private"
	fake := installFakeProviderManager(t)
	fake.snapshot = providerconfig.CatalogSnapshot{
		Connection: config.ProviderConnection{Type: "anthropic"},
		APIKey:     apiKey,
		ScopeID:    scopeID,
	}
	catalogue := &fakeProviderCatalogue{result: modelcatalog.Result{Record: modelcatalog.Record{
		SchemaVersion: modelcatalog.SchemaVersion,
		Connection:    modelcatalog.Connection{Name: "private", Type: "anthropic", BaseURL: "https://api.anthropic.com", ScopeID: scopeID},
		FetchedAt:     time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC),
		Models:        []modelcatalog.Model{{ID: "claude-test", DisplayName: "Claude Test", Owner: apiKey, Capabilities: []string{"text", scopeID}}},
	}, Warning: "refresh failed for " + apiKey + " in " + scopeID}}
	installFakeProviderCatalogue(t, catalogue)

	for _, args := range [][]string{{"models", "private"}, {"models", "private", "--json"}} {
		var stdout, stderr bytes.Buffer
		if err := providerCmd(t.Context(), args, strings.NewReader(""), &stdout, &stderr); err != nil {
			t.Fatalf("providerCmd(%v) error = %v", args, err)
		}
		output := stdout.String() + stderr.String()
		if strings.Contains(output, apiKey) || strings.Contains(output, scopeID) || strings.Contains(output, "api_key") || strings.Contains(output, "scope_id") {
			t.Fatalf("providerCmd(%v) leaked private catalogue input: %q", args, output)
		}
		if !strings.Contains(output, "claude-test") || !strings.Contains(output, "Claude Test") {
			t.Fatalf("providerCmd(%v) omitted human catalogue fields: %q", args, output)
		}
	}
	catalogue.modelsErr = errors.New("refresh rejected " + apiKey + " in " + scopeID)
	err := providerCmd(t.Context(), []string{"models", "private"}, strings.NewReader(""), io.Discard, io.Discard)
	if err == nil || strings.Contains(err.Error(), apiKey) || strings.Contains(err.Error(), scopeID) {
		t.Fatalf("catalogue error was not safely redacted: %v", err)
	}
}

func TestProviderModelAddCommandGeneratesAndForwardsAliasAndRoles(t *testing.T) {
	fake := installFakeProviderManager(t)
	if err := providerCmd(t.Context(), []string{
		"model", "add", "openrouter", "anthropic/Claude Sonnet 4.6", "--default", "--utility",
	}, strings.NewReader(""), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	want := providerconfig.AddModelRequest{
		ConnectionName: "openrouter",
		Alias:          "anthropic-claude-sonnet-4-6",
		UpstreamModel:  "anthropic/Claude Sonnet 4.6",
		Default:        true,
		Utility:        true,
	}
	if fake.addModelRequest != want || !fake.preflighted {
		t.Fatalf("AddModel request = %+v preflighted=%t, want %+v", fake.addModelRequest, fake.preflighted, want)
	}
}

func TestProviderModelAddCommandAcceptsExactUncachedID(t *testing.T) {
	fake := installFakeProviderManager(t)
	installFakeProviderCatalogue(t, &fakeProviderCatalogue{modelsErr: errors.New("catalogue must not be consulted")})
	if err := providerCmd(t.Context(), []string{
		"model", "add", "private", "--experimental/exact-model-id", "--alias", "exact",
	}, strings.NewReader(""), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	want := providerconfig.AddModelRequest{ConnectionName: "private", Alias: "exact", UpstreamModel: "--experimental/exact-model-id"}
	if fake.addModelRequest != want {
		t.Fatalf("AddModel request = %+v, want %+v", fake.addModelRequest, want)
	}
}

func TestProviderRemoveInvalidatesCatalogueAfterCommit(t *testing.T) {
	events := []string{}
	fake := installFakeProviderManager(t)
	fake.events = &events
	catalogue := &fakeProviderCatalogue{events: &events}
	installFakeProviderCatalogue(t, catalogue)

	var stdout bytes.Buffer
	if err := providerCmd(t.Context(), []string{"remove", "primary"}, strings.NewReader(""), &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(events, ","); got != "remove,invalidate" {
		t.Fatalf("events = %q, want remove,invalidate", got)
	}
	if catalogue.invalidateName != "primary" || !strings.Contains(stdout.String(), "removed provider primary") {
		t.Fatalf("invalidate=%q stdout=%q", catalogue.invalidateName, stdout.String())
	}
}

func TestProviderRemoveCacheFailureWarnsAndReturnsSuccess(t *testing.T) {
	fake := installFakeProviderManager(t)
	fake.snapshot = providerconfig.CatalogSnapshot{
		Connection: config.ProviderConnection{Type: "openai", BaseURL: "https://models.example/v1"},
		APIKey:     "old-key",
		ScopeID:    "old-account-scope",
	}
	fake.addScopeID = "new-account-scope"
	catalogue := &fakeProviderCatalogue{invalidateErr: errors.New("cannot delete old-key old-account-scope")}
	installFakeProviderCatalogue(t, catalogue)
	store := modelcatalog.NewStore(t.TempDir())
	oldConnection, _, err := effectiveCatalogConnection("primary", fake.snapshot.Connection, fake.snapshot.ScopeID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(oldConnection, []modelcatalog.Model{{ID: "old-account/model"}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(store.Root, "primary.json")
	oldBytes, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	oldScopeID := fake.snapshot.ScopeID

	var stdout, stderr bytes.Buffer
	if err := providerCmd(t.Context(), []string{"remove", "primary"}, strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatalf("provider remove returned post-commit cache error: %v", err)
	}
	if !strings.Contains(stdout.String(), "removed provider primary") || !strings.Contains(strings.ToLower(stderr.String()), "warning") {
		t.Fatalf("stdout=%q stderr=%q, want committed success and warning", stdout.String(), stderr.String())
	}
	if strings.Contains(stderr.String(), "old-key") || strings.Contains(stderr.String(), "old-account-scope") {
		t.Fatalf("cache warning leaked private material: %q", stderr.String())
	}

	// Re-enroll the same name under a different credential. The failed
	// invalidation leaves the disposable bytes in place, but the new enrollment
	// scope prevents the prior account's cache from applying.
	if err := providerCmd(t.Context(), []string{
		"add", "--name", "primary", "--type", "openai", "--base-url", "https://models.example/v1",
		"--model", "new=new-account/model", "--api-key-stdin",
	}, strings.NewReader("new-key\n"), io.Discard, io.Discard); err != nil {
		t.Fatalf("re-enroll provider: %v", err)
	}
	if fake.addRequest.APIKey != "new-key" || oldScopeID == fake.snapshot.ScopeID {
		t.Fatal("re-enrollment reused prior catalogue scope")
	}
	newConnection, _, err := effectiveCatalogConnection("primary", fake.snapshot.Connection, fake.snapshot.ScopeID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(newConnection); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("new enrollment loaded old-account cache: %v", err)
	}
	remaining, err := os.ReadFile(cachePath)
	if err != nil || !bytes.Equal(remaining, oldBytes) {
		t.Fatalf("old cache bytes changed or disappeared: err=%v", err)
	}
}

func TestProviderRemoveFailureDoesNotInvalidateCatalogue(t *testing.T) {
	fake := installFakeProviderManager(t)
	fake.removeErr = errors.New("transaction did not commit")
	catalogue := &fakeProviderCatalogue{}
	installFakeProviderCatalogue(t, catalogue)

	err := providerCmd(t.Context(), []string{"remove", "primary"}, strings.NewReader(""), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "did not commit") {
		t.Fatalf("provider remove error = %v", err)
	}
	if catalogue.invalidateCalls != 0 {
		t.Fatalf("Invalidate calls = %d, want zero after manager failure", catalogue.invalidateCalls)
	}
}

func TestProviderCommandKeyFileRejectsSymlinkAndOversize(t *testing.T) {
	installFakeProviderManager(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readModeCheckedKeyFile(link); err == nil {
		t.Fatal("symlink key file accepted")
	}
	large := filepath.Join(dir, "large")
	if err := os.WriteFile(large, bytes.Repeat([]byte("x"), maxProviderKeyBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readModeCheckedKeyFile(large); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("oversize error = %v", err)
	}
}

func TestProviderServiceHealthRetriesDuringStartup(t *testing.T) {
	oldRetry := providerHealthRetry
	providerHealthRetry = time.Millisecond
	t.Cleanup(func() { providerHealthRetry = oldRetry })
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	home := t.TempDir()
	t.Setenv("WAFFLE_HOME", home)
	configBody := "[gateway]\nstatus_listen = \"" + strings.TrimPrefix(server.URL, "http://") + "\"\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := providerServiceHealth(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("health attempts = %d, want 3", got)
	}
}

func installFakeProviderManager(t *testing.T) *fakeProviderManager {
	t.Helper()
	fake := &fakeProviderManager{}
	old := openProviderManager
	openProviderManager = func() (providerManager, error) { return fake, nil }
	t.Cleanup(func() { openProviderManager = old })
	return fake
}

func installFakeProviderCatalogue(t *testing.T, fake providerCatalogue) {
	t.Helper()
	old := openProviderCatalogue
	openProviderCatalogue = func() (providerCatalogue, error) { return fake, nil }
	t.Cleanup(func() { openProviderCatalogue = old })
}

func discoveredCatalogue(models ...modelcatalog.Model) modelcatalog.Result {
	return modelcatalog.Result{Record: modelcatalog.Record{
		SchemaVersion: modelcatalog.SchemaVersion,
		FetchedAt:     time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC),
		Models:        models,
	}}
}

func installProviderSecretReader(t *testing.T, value string, err error) {
	t.Helper()
	old := providerSecretReader
	providerSecretReader = func(io.Reader, io.Writer) (string, error) { return value, err }
	t.Cleanup(func() { providerSecretReader = old })
}

func providerListingWithAliases(t *testing.T, aliases ...string) []byte {
	t.Helper()
	listing := providerconfig.Listing{
		State:     "installed",
		Providers: map[string]providerconfig.ProviderSummary{},
		Models:    map[string]providerconfig.ModelSummary{},
	}
	for _, alias := range aliases {
		listing.Models[alias] = providerconfig.ModelSummary{Provider: "existing", Model: "upstream"}
	}
	b, err := json.Marshal(listing)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func maxNumberedCatalogueRun(output string) int {
	maxRun, run := 0, 0
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		fields := strings.Fields(trimmed)
		if len(fields) > 0 && strings.HasSuffix(fields[0], ".") {
			var number int
			if _, err := fmt.Sscanf(fields[0], "%d.", &number); err == nil {
				run++
				if run > maxRun {
					maxRun = run
				}
				continue
			}
		}
		run = 0
	}
	return maxRun
}

type countingReader struct {
	reader io.Reader
	reads  int
}

func (r *countingReader) Read(p []byte) (int, error) {
	r.reads++
	return r.reader.Read(p)
}

var _ = config.ProviderConnection{}
