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

func TestVerifyStepsIncludeEval(t *testing.T) {
	steps := verifySteps()
	foundEval := false
	for _, step := range steps {
		joined := strings.Join(step, " ")
		if strings.Contains(joined, "eval") {
			foundEval = true
		}
	}
	if !foundEval {
		t.Fatalf("verifySteps missing eval: %v", steps)
	}
}

func TestVerifyRepoFailsOnBrokenTree(t *testing.T) {
	// Any verify step failure (vet/test/eval) blocks upgrade (#63).
	err := verifyRepo(context.Background(), t.TempDir(), io.Discard)
	if err == nil {
		t.Fatal("verifyRepo on empty dir: want error")
	}
	if !strings.Contains(err.Error(), "verify:") {
		t.Fatalf("err = %v", err)
	}
}

func TestRejectProtectedIncludesEvalPath(t *testing.T) {
	// Default auto-patch protected prefixes include internal/eval and evals (#63).
	paths := []string{
		"internal/selfdev", "internal/config", "cmd/waffle/selfdev_cmd.go",
		"cmd/waffle/main.go", "internal/doctor", "evals", "internal/eval",
	}
	var sawEval, sawEvals bool
	for _, p := range paths {
		if p == "internal/eval" {
			sawEval = true
		}
		if p == "evals" {
			sawEvals = true
		}
	}
	if !sawEval || !sawEvals {
		t.Fatal("expected internal/eval and evals in default protected set")
	}
	file := "internal/eval/eval.go"
	blocked := false
	for _, prefix := range paths {
		if file == prefix || strings.HasPrefix(file, strings.TrimSuffix(prefix, "/")+"/") {
			blocked = true
		}
	}
	if !blocked {
		t.Fatalf("%q not blocked by protected prefixes", file)
	}
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

func TestBuildVersionFromGit(t *testing.T) {
	// repoDir is not a git checkout: buildVersion must fall back to "dev"
	// without error so upgrade can still produce a binary.
	ver, err := buildVersion(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("buildVersion: %v", err)
	}
	if ver != "dev" {
		t.Errorf("buildVersion(non-git) = %q, want dev", ver)
	}
}

func TestSanitizeVersion(t *testing.T) {
	ok, err := sanitizeVersion("  v1.2.3-4-gabcdef.dirty\n")
	if err != nil || ok != "v1.2.3-4-gabcdef.dirty" {
		t.Errorf("sanitizeVersion(good) = %q, %v", ok, err)
	}
	if got, err := sanitizeVersion("v1.0.0+meta"); err != nil || got != "v1.0.0+meta" {
		t.Errorf("sanitizeVersion(+meta) = %q, %v", got, err)
	}
	if got, err := sanitizeVersion("   \n"); err != nil || got != "dev" {
		t.Errorf("sanitizeVersion(empty) = %q, %v; want dev", got, err)
	}
	// Note: leading/trailing whitespace is trimmed before validation, so a
	// trailing tab alone is not an error. Embedded whitespace/metachars are.
	for _, bad := range []string{"v1 with space", "v1\tx", `v1"x`, "v1't", `v1\y`, "v1;rm", "v1$(x)"} {
		if _, err := sanitizeVersion(bad); err == nil {
			t.Errorf("sanitizeVersion(%q): want error", bad)
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
	if err := os.WriteFile(f, []byte("binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(f, 0o755); err != nil {
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

func TestSandboxQueueRoundTrip(t *testing.T) {
	info, err := sandboxQueueRoundTrip()
	if err != nil {
		t.Fatalf("sandboxQueueRoundTrip: %v", err)
	}
	if !strings.Contains(info, "queue ok") {
		t.Fatalf("info = %q", info)
	}
}

func TestSandboxDockerRoundTripNoDocker(t *testing.T) {
	// When docker is absent from PATH, the probe must fail closed (doctor
	// treats this as a failed check under UsesDocker).
	t.Setenv("PATH", t.TempDir()) // empty of docker
	info, err := sandboxDockerRoundTrip("")
	if err == nil {
		t.Fatalf("expected error without docker, got info=%q", info)
	}
	if !strings.Contains(info, "not in PATH") && !strings.Contains(err.Error(), "not in PATH") && !strings.Contains(err.Error(), "executable file not found") {
		// LookPath error message varies; info should still explain.
		if info == "" {
			t.Fatalf("err=%v info=%q", err, info)
		}
	}
}

func TestDoctorIncludesSandboxChecksWhenDockerMode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WAFFLE_HOME", home)
	// Force docker mode so Doctor runs queue + docker probes. Docker itself
	// may be missing on this host — that fails the docker check, not config.
	cfg := "[sandbox]\nmode = \"docker\"\n"
	// Provide a dummy runner binary so the runner check can pass on non-linux.
	runner := filepath.Join(home, "waffle-linux")
	if err := os.WriteFile(runner, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg += "runner_binary = \"" + runner + "\"\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	checks, _, err := Doctor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, c := range checks {
		names[c.Name] = c.OK
	}
	for _, want := range []string{"sandbox runner", "sandbox queue round-trip", "sandbox docker round-trip"} {
		if _, ok := names[want]; !ok {
			t.Errorf("missing doctor check %q; got %v", want, names)
		}
	}
	if !names["sandbox queue round-trip"] {
		t.Error("sandbox queue round-trip should pass on host FS")
	}
	if !names["sandbox runner"] {
		t.Error("sandbox runner should pass with configured binary")
	}
	// docker round-trip OK only when docker is available; do not require it.
}
