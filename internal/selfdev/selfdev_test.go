package selfdev

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/config"
)

func TestProviderCheck(t *testing.T) {
	openAIServer := func(t *testing.T, handler http.HandlerFunc) *httptest.Server {
		t.Helper()
		return httptest.NewServer(handler)
	}
	provider := func(url string) config.Provider {
		return config.Provider{Name: "openai", Model: "test-model", APIKey: "test-key", BaseURL: url + "/v1"}
	}

	t.Run("authenticated completion", func(t *testing.T) {
		srv := openAIServer(t, func(w http.ResponseWriter, r *http.Request) {
			if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
				t.Errorf("Authorization = %q, want Bearer test-key", got)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		})
		defer srv.Close()

		info, err := providerCheck(context.Background(), provider(srv.URL))
		if err != nil {
			t.Fatalf("providerCheck: %v", err)
		}
		if info != "authenticated completion" {
			t.Errorf("info = %q", info)
		}
	})

	t.Run("missing key skips", func(t *testing.T) {
		info, err := providerCheck(context.Background(), config.Provider{Name: "openai"})
		if err != nil {
			t.Fatalf("providerCheck: %v", err)
		}
		if info != "no API key configured (skipped)" {
			t.Errorf("info = %q", info)
		}
	})

	t.Run("unresolved key reference fails", func(t *testing.T) {
		t.Setenv("WAFFLE_HOME", t.TempDir())
		if _, err := providerCheck(context.Background(), config.Provider{Name: "openai", APIKey: "secret://openai/api-key"}); err == nil || !strings.Contains(err.Error(), "no secret store is available") {
			t.Fatalf("providerCheck error = %v, want unresolved secret reference", err)
		}
	})

	t.Run("rejected authentication fails", func(t *testing.T) {
		srv := openAIServer(t, func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "bad key", http.StatusUnauthorized)
		})
		defer srv.Close()

		if _, err := providerCheck(context.Background(), provider(srv.URL)); err == nil || !strings.Contains(err.Error(), "bad key") {
			t.Fatalf("providerCheck error = %v, want authentication failure", err)
		}
	})

	t.Run("deadline is bounded", func(t *testing.T) {
		orig := providerProbeTimeout
		providerProbeTimeout = 10 * time.Millisecond
		t.Cleanup(func() { providerProbeTimeout = orig })
		release := make(chan struct{})
		srv := openAIServer(t, func(http.ResponseWriter, *http.Request) {
			<-release
		})
		defer srv.Close()
		defer close(release)

		if _, err := providerCheck(context.Background(), provider(srv.URL)); err == nil || !strings.Contains(err.Error(), "context deadline exceeded") {
			t.Fatalf("providerCheck error = %v, want deadline exceeded", err)
		}
	})
}

func TestDoctorPasses(t *testing.T) {
	t.Setenv("WAFFLE_HOME", t.TempDir())
	checks, ok, err := Doctor(context.Background())
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if !ok {
		t.Errorf("doctor failed on a clean home: %+v", checks)
	}
	// Expect config, database, and secret-store checks.
	if len(checks) < 3 {
		t.Errorf("checks = %d, want >= 3", len(checks))
	}
	for _, c := range checks {
		if !c.OK {
			t.Errorf("check %q failed: %s", c.Name, c.Info)
		}
	}
}

func TestDoctorReportsProviderFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WAFFLE_HOME", home)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad key", http.StatusUnauthorized)
	}))
	defer srv.Close()
	configText := "[provider]\nname = \"openai\"\nmodel = \"test-model\"\napi_key = \"test-key\"\nbase_url = \"" + srv.URL + "/v1\"\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}

	checks, ok, err := Doctor(context.Background())
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if ok {
		t.Fatalf("Doctor ok = true, want false; checks = %+v", checks)
	}
	for _, check := range checks {
		if check.Name == "provider reachable" {
			if check.OK || !strings.Contains(check.Info, "bad key") {
				t.Errorf("provider check = %+v, want failed bad-key check", check)
			}
			return
		}
	}
	t.Fatal("Doctor did not report a provider reachable check")
}

func TestUpgradeRejectsOptionRefs(t *testing.T) {
	// repoDir is an empty temp dir, not a git repo: if the ref reached git
	// the error would be git's, so a validation error proves the ref was
	// rejected before any git command ran.
	for _, ref := range []string{"--help", "-c", "-"} {
		_, err := Upgrade(context.Background(), t.TempDir(), ref, io.Discard)
		if err == nil {
			t.Fatalf("Upgrade(ref=%q): want error, got nil", ref)
		}

		if !strings.Contains(err.Error(), "may not start with '-'") {
			t.Errorf("Upgrade(ref=%q) error = %q, want ref validation error", ref, err)
		}
	}

}

func TestValidateRef(t *testing.T) {
	if err := validateRef(""); err == nil {
		t.Error("validateRef(\"\"): want error, got nil")
	}
	for _, ref := range []string{"main", "v1.2.3", "abc123", "feature/x"} {
		if err := validateRef(ref); err != nil {
			t.Errorf("validateRef(%q) = %v, want nil", ref, err)
		}
	}
}

func TestRollbackWithoutBackup(t *testing.T) {
	if _, err := Rollback(); err == nil {
		t.Skip("this binary happens to have a .prev; skipping")
	}
}

func TestCopyFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("hello"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copyFile: %v", err)
	}
	b, err := os.ReadFile(dst)
	if err != nil || string(b) != "hello" {
		t.Errorf("dst = %q, %v", b, err)
	}
	info, _ := os.Stat(dst)
	if info.Mode().Perm()&0o100 == 0 {
		t.Error("dst not executable")
	}
}

func TestSandboxRunnerCheck(t *testing.T) {
	// A configured, existing runner binary passes and is named in the info.
	f := filepath.Join(t.TempDir(), "waffle-linux")
	if err := os.WriteFile(f, []byte("binary"), 0o755); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}

	if info, err := sandboxRunnerCheck(f); err != nil || !strings.Contains(info, f) {
		t.Errorf("existing runner_binary: info=%q err=%v", info, err)
	}

	// A configured but missing runner binary fails.
	if _, err := sandboxRunnerCheck(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("missing runner_binary accepted, want error")
	}
}
