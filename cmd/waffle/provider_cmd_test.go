package main

import (
	"bytes"
	"context"
	"errors"
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
	"github.com/matt-riley/waffle/internal/providerconfig"
)

type fakeProviderManager struct {
	addRequest providerconfig.AddRequest
	addErr     error
	list       []byte
	testName   string
	removeName string
}

func (f *fakeProviderManager) Add(_ context.Context, req providerconfig.AddRequest) error {
	f.addRequest = req
	return f.addErr
}
func (f *fakeProviderManager) List(context.Context) ([]byte, error) { return f.list, nil }
func (f *fakeProviderManager) Test(_ context.Context, name string) error {
	f.testName = name
	return nil
}
func (f *fakeProviderManager) Remove(_ context.Context, name string) error {
	f.removeName = name
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
	old := providerSecretReader
	providerSecretReader = func(io.Reader, io.Writer) (string, error) { return "hidden-secret", nil }
	t.Cleanup(func() { providerSecretReader = old })
	// Optional values use "-" so token-oriented prompting never needs to
	// buffer ahead of the terminal password reader.
	input := strings.NewReader("openai openai - gpt=gpt-test,small=gpt-small gpt small")
	if err := providerCmd(context.Background(), []string{"add"}, input, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if fake.addRequest.DefaultModel != "gpt" || fake.addRequest.UtilityModel != "small" || len(fake.addRequest.Models) != 2 {
		t.Fatalf("bare add request = %#v", fake.addRequest)
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

var _ = config.ProviderConnection{}
