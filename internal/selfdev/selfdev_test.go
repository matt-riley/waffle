package selfdev

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/llmtest"
	"github.com/matt-riley/waffle/internal/secret"
)

func writeUpgradeFixture(t *testing.T, brokenTest, brokenEval bool) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"go.mod": "module upgradefixture\n\ngo 1.25\n",
		"cmd/waffle/main.go": `package main
import "os"
func main() {
	if len(os.Args) > 1 && os.Args[1] == "eval" { ` + map[bool]string{true: `_, _ = os.Stderr.WriteString("FAIL broken-eval: expected pass\\n"); os.Exit(1)`, false: `_, _ = os.Stdout.WriteString("PASS fixture-eval\\n")`}[brokenEval] + ` }
}
`,
		"fixture_test.go": "package fixture\nimport \"testing\"\nfunc TestFixture(t *testing.T) { " + map[bool]string{true: `t.Fatal("deliberate failing test output")`, false: ``}[brokenTest] + " }\n",
	}
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestUpgradeIntoFailingTestShowsOutputAndDoesNotSwap(t *testing.T) {
	dir := writeUpgradeFixture(t, true, false)
	target := filepath.Join(t.TempDir(), "waffle")
	if err := os.WriteFile(target, []byte("original"), 0o755); err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	_, err := upgradeInto(context.Background(), dir, target, &output, true)
	if err == nil || !strings.Contains(output.String(), "deliberate failing test output") {
		t.Fatalf("upgradeInto err=%v output=%q, want failing test output", err, output.String())
	}
	got, readErr := os.ReadFile(target)
	if readErr != nil || string(got) != "original" {
		t.Fatalf("target swapped: %q err=%v", got, readErr)
	}
}

func TestUpgradeIntoBrokenEvalShowsOutputAndDoesNotSwap(t *testing.T) {
	dir := writeUpgradeFixture(t, false, true)
	target := filepath.Join(t.TempDir(), "waffle")
	if err := os.WriteFile(target, []byte("original"), 0o755); err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	_, err := upgradeInto(context.Background(), dir, target, &output, true)
	if err == nil || !strings.Contains(output.String(), "FAIL broken-eval") {
		t.Fatalf("upgradeInto err=%v output=%q, want broken eval output", err, output.String())
	}
	got, _ := os.ReadFile(target)
	if string(got) != "original" {
		t.Fatalf("target swapped: %q", got)
	}
}

func TestVerifyMissingLintWarnsWithoutFailing(t *testing.T) {
	dir := writeUpgradeFixture(t, false, false)
	var output strings.Builder
	if err := verifyRepo(context.Background(), dir, &output); err != nil {
		t.Fatalf("verifyRepo: %v\n%s", err, output.String())
	}
	if _, err := exec.LookPath("golangci-lint"); err != nil && !strings.Contains(output.String(), "lint gate skipped") {
		t.Fatalf("missing lint warning absent: %q", output.String())
	}
}

func TestVerifyReportsEachFailingMechanicalGate(t *testing.T) {
	for _, failing := range []string{"vet", "test", "lint"} {
		t.Run(failing, func(t *testing.T) {
			bin := t.TempDir()
			goScript := "#!/bin/sh\nif [ \"$1\" = \"" + failing + "\" ]; then echo '" + failing + " gate output'; exit 9; fi\nexit 0\n"
			if err := os.WriteFile(filepath.Join(bin, "go"), []byte(goScript), 0o755); err != nil {
				t.Fatal(err)
			}
			lintScript := "#!/bin/sh\n" + map[bool]string{true: "echo 'lint gate output'; exit 9\n", false: "exit 0\n"}[failing == "lint"]
			if err := os.WriteFile(filepath.Join(bin, "golangci-lint"), []byte(lintScript), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin)
			var output strings.Builder
			err := verifyRepo(context.Background(), t.TempDir(), &output)
			if err == nil || !strings.Contains(output.String(), failing+" gate output") {
				t.Fatalf("err=%v output=%q", err, output.String())
			}
		})
	}
}

func TestDoctorReportsLintGateUnarmed(t *testing.T) {
	t.Setenv("WAFFLE_HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	checks, _, err := Doctor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range checks {
		if check.Name == "golangci-lint gate" {
			if !check.OK || !strings.Contains(check.Info, "not installed") || !strings.Contains(check.Info, "skipped") {
				t.Fatalf("lint check = %+v", check)
			}
			return
		}
	}
	t.Fatal("doctor omitted golangci-lint gate")
}

func TestReviewerUsesUtilityModelAndReturnsStructuredFindings(t *testing.T) {
	p := &llmtest.Script{Responses: []llm.Response{llmtest.Text(`[{"severity":"blocker","file":"internal/selfdev/selfdev.go","summary":"gate weakened"}]`)}}
	r := Reviewer{Provider: p, Model: "cheap-review-model"}
	findings, err := r.Review(context.Background(), "diff --git a/x b/x", "fix gate")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Requests) != 1 || p.Requests[0].Model != "cheap-review-model" {
		t.Fatalf("review request = %+v", p.Requests)
	}
	if len(findings) != 1 || findings[0].Severity != SeverityBlocker || findings[0].File == "" {
		t.Fatalf("findings = %+v", findings)
	}
}

func TestReviewerModelUsesConfiguredUtilityModel(t *testing.T) {
	p := config.Provider{Model: "generation-model", UtilityModel: "configured-utility-model"}
	if got := reviewerModel(p); got != "configured-utility-model" {
		t.Fatalf("reviewerModel = %q, want configured utility model", got)
	}
}

func TestReviewGatePersistsSHAAndBlocksManualAndAutoPatch(t *testing.T) {
	for _, approval := range []string{"manual", "auto-patch"} {
		t.Run(approval, func(t *testing.T) {
			logPath := filepath.Join(t.TempDir(), "reviews.jsonl")
			findings := []Finding{{Severity: SeverityBlocker, File: "x.go", Summary: "unsafe"}}
			err := enforceReview(logPath, ReviewRecord{CommitSHA: "abc123", Approval: approval, Findings: findings})
			if err == nil || !strings.Contains(err.Error(), "blocker") {
				t.Fatalf("enforceReview = %v", err)
			}
			raw, readErr := os.ReadFile(logPath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			var got ReviewRecord
			if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &got); err != nil {
				t.Fatal(err)
			}
			if got.CommitSHA != "abc123" || len(got.Findings) != 1 {
				t.Fatalf("persisted = %+v", got)
			}
		})
	}
}

func TestUpgradeReviewerBlockerStopsManualAndAutoPatch(t *testing.T) {
	for _, approval := range []string{"manual", "auto-patch"} {
		t.Run(approval, func(t *testing.T) {
			var requestedModel string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Model string `json:"model"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode request: %v", err)
				}
				requestedModel = body.Model
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"[{\\\"severity\\\":\\\"blocker\\\",\\\"file\\\":\\\"change.go\\\",\\\"summary\\\":\\\"unsafe\\\"}]\"}}]}\n\n")
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			}))
			defer srv.Close()

			home := t.TempDir()
			t.Setenv("WAFFLE_HOME", home)
			cfg := "[provider]\nname = \"openai\"\nmodel = \"generation\"\nutility_model = \"utility-review\"\napi_key = \"test-key\"\nbase_url = \"" + srv.URL + "/v1\"\n"
			if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(cfg), 0o600); err != nil {
				t.Fatal(err)
			}

			dir := t.TempDir()
			git := func(args ...string) string {
				cmd := exec.Command("git", args...)
				cmd.Dir = dir
				cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com")
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %v: %s", args, err, out)
				}
				return strings.TrimSpace(string(out))
			}
			git("init", "-q", "-b", "base")
			if err := os.WriteFile(filepath.Join(dir, "base.go"), []byte("package fixture\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			git("add", ".")
			git("commit", "-qm", "base")
			git("checkout", "-qb", "candidate")
			if err := os.WriteFile(filepath.Join(dir, "change.go"), []byte("package fixture\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			git("add", ".")
			git("commit", "-qm", "candidate")
			sha := git("rev-parse", "HEAD")
			git("checkout", "-q", "base")

			_, err := UpgradeWithOptions(context.Background(), dir, "candidate", io.Discard, false, approval, nil)
			if err == nil || !strings.Contains(err.Error(), "blocker") {
				t.Fatalf("UpgradeWithOptions = %v", err)
			}
			if requestedModel != "utility-review" {
				t.Fatalf("review model = %q", requestedModel)
			}
			raw, readErr := os.ReadFile(filepath.Join(home, "selfdev-reviews.jsonl"))
			if readErr != nil || !strings.Contains(string(raw), sha) {
				t.Fatalf("review log=%q err=%v, want SHA %s", raw, readErr, sha)
			}
			if branch := git("branch", "--show-current"); branch != "base" {
				t.Fatalf("blocker allowed checkout to %q", branch)
			}
		})
	}
}

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

func TestProviderCheckConfigUsesExplicitDefaultAlias(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WAFFLE_HOME", home)
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("WAFFLE_AGE_IDENTITY", id.String())
	store := secret.OpenFile(filepath.Join(home, "secrets.age"), id)
	if err := store.Set("provider/anthropic/api-key", "anthropic-test-key"); err != nil {
		t.Fatal(err)
	}

	var requestedModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-api-key"); got != "anthropic-test-key" {
			t.Errorf("x-api-key = %q", got)
		}
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		requestedModel = body.Model
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_test","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"claude-test","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer srv.Close()

	cfg := config.Config{
		Providers: map[string]config.ProviderConnection{
			"anthropic": {Type: "anthropic", APIKey: "secret://provider/anthropic/api-key", BaseURL: srv.URL},
		},
		Models: map[string]config.ModelTarget{
			"primary": {Provider: "anthropic", Model: "claude-test"},
		},
	}
	cfg.Agent.DefaultModel = "primary"

	info, err := providerCheckConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("providerCheckConfig: %v", err)
	}
	if info != "authenticated completion" || requestedModel != "claude-test" {
		t.Fatalf("info/model = %q/%q", info, requestedModel)
	}
}

func TestConfiguredReviewerUsesExplicitUtilityAlias(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WAFFLE_HOME", home)
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("WAFFLE_AGE_IDENTITY", id.String())
	store := secret.OpenFile(filepath.Join(home, "secrets.age"), id)
	if err := store.Set("provider/openai/api-key", "openai-test-key"); err != nil {
		t.Fatal(err)
	}

	var requestedModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer openai-test-key" {
			t.Errorf("Authorization = %q", got)
		}
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		requestedModel = body.Model
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"[]\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()

	cfg := config.Config{
		Providers: map[string]config.ProviderConnection{
			"openai": {Type: "openai", APIKey: "secret://provider/openai/api-key", BaseURL: srv.URL + "/v1"},
		},
		Models: map[string]config.ModelTarget{
			"primary": {Provider: "openai", Model: "generation-model"},
			"review":  {Provider: "openai", Model: "utility-review"},
		},
	}
	cfg.Agent.DefaultModel = "primary"
	cfg.Agent.UtilityModel = "review"

	reviewer, err := configuredReviewer(cfg)
	if err != nil {
		t.Fatalf("configuredReviewer: %v", err)
	}
	if _, err := reviewer.Review(context.Background(), "diff", "task"); err != nil {
		t.Fatalf("Review: %v", err)
	}
	if reviewer.Model != "utility-review" || requestedModel != "utility-review" {
		t.Fatalf("reviewer/request model = %q/%q", reviewer.Model, requestedModel)
	}
}

func TestExplicitKeylessEndpointIsProbedAndCanReview(t *testing.T) {
	var requests []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization = %q, want empty", got)
		}
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		requests = append(requests, body.Model)
		w.Header().Set("Content-Type", "text/event-stream")
		text := "ok"
		if body.Model == "local-review" {
			text = "[]"
		}
		_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", text)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()

	cfg := config.Config{
		Providers: map[string]config.ProviderConnection{
			"local": {Type: "openai", BaseURL: srv.URL + "/v1"},
		},
		Models: map[string]config.ModelTarget{
			"primary": {Provider: "local", Model: "local-main"},
			"review":  {Provider: "local", Model: "local-review"},
		},
	}
	cfg.Agent.DefaultModel = "primary"
	cfg.Agent.UtilityModel = "review"

	if _, err := providerCheckConfig(context.Background(), cfg); err != nil {
		t.Fatalf("providerCheckConfig: %v", err)
	}
	if got := strings.Join(requests, ","); got != "local-main" {
		t.Fatalf("Doctor requested models = %q", got)
	}
	reviewer, err := configuredReviewer(cfg)
	if err != nil {
		t.Fatalf("configuredReviewer: %v", err)
	}
	if _, err := reviewer.Review(context.Background(), "diff", "task"); err != nil {
		t.Fatalf("Review: %v", err)
	}
	if got := strings.Join(requests, ","); got != "local-main,local-review" {
		t.Fatalf("requested models = %q", got)
	}
}

func TestExplicitNamedSecretDoesNotFallBackToProviderEnvironment(t *testing.T) {
	t.Setenv("WAFFLE_HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "must-not-be-used")
	cfg := config.Config{
		Providers: map[string]config.ProviderConnection{
			"named": {Type: "openai", APIKey: "secret://provider/named/api-key", BaseURL: "http://127.0.0.1:1/v1"},
		},
		Models: map[string]config.ModelTarget{
			"primary": {Provider: "named", Model: "model"},
		},
	}
	cfg.Agent.DefaultModel = "primary"

	_, err := providerCheckConfig(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "resolve credentials") {
		t.Fatalf("providerCheckConfig error = %v", err)
	}
	if strings.Contains(err.Error(), "must-not-be-used") {
		t.Fatalf("providerCheckConfig leaked environment credential: %v", err)
	}
}

func TestConfiguredReviewerFallsBackToExplicitDefaultAndKeepsLegacy(t *testing.T) {
	explicit := config.Config{
		Providers: map[string]config.ProviderConnection{
			"local": {Type: "openai", BaseURL: "http://127.0.0.1:1/v1"},
		},
		Models: map[string]config.ModelTarget{
			"primary": {Provider: "local", Model: "local-main"},
		},
	}
	explicit.Agent.DefaultModel = "primary"
	reviewer, err := configuredReviewer(explicit)
	if err != nil || reviewer.Model != "local-main" || reviewer.Provider == nil {
		t.Fatalf("explicit default reviewer = %#v, %v", reviewer, err)
	}

	legacy := config.Config{Provider: config.Provider{
		Name:         "openai",
		Model:        "legacy-main",
		UtilityModel: "legacy-review",
		APIKey:       "legacy-test-key",
		BaseURL:      "http://127.0.0.1:1/v1",
	}}
	reviewer, err = configuredReviewer(legacy)
	if err != nil || reviewer.Model != "legacy-review" || reviewer.Provider == nil {
		t.Fatalf("legacy reviewer = %#v, %v", reviewer, err)
	}
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

func TestDoctorSkipsConfigDependentChecksWhenConfigInvalid(t *testing.T) {
	// #114: after config.Load fails, provider/sandbox/MCP must be reported as
	// skipped — not run against a zero-value cfg.
	home := t.TempDir()
	t.Setenv("WAFFLE_HOME", home)
	// Invalid TOML (unclosed string) so Load fails while the file exists.
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("this is not = valid toml [[["), 0o600); err != nil {
		t.Fatal(err)
	}

	checks, ok, err := Doctor(context.Background())
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if ok {
		t.Fatalf("Doctor ok = true, want false; checks = %+v", checks)
	}

	byName := map[string]Check{}
	for _, c := range checks {
		byName[c.Name] = c
	}

	cfgCheck, found := byName["config parses"]
	if !found || cfgCheck.OK {
		t.Fatalf("config parses = %+v, want failed check", cfgCheck)
	}

	for _, name := range []string{"provider reachable", "sandbox runner", "mcp servers"} {
		c, found := byName[name]
		if !found {
			t.Errorf("missing check %q; checks = %+v", name, checks)
			continue
		}
		if !c.OK {
			t.Errorf("%s should be OK (skipped), got %+v", name, c)
		}
		if !strings.Contains(c.Info, "skipped: config did not parse") {
			t.Errorf("%s info = %q, want skipped: config did not parse", name, c.Info)
		}
	}
	// Must not run docker-mode probes or invent host-mode skip wording.
	for _, name := range []string{"sandbox queue round-trip", "sandbox docker round-trip"} {
		if _, found := byName[name]; found {
			t.Errorf("unexpected check %q after config parse failure", name)
		}
	}
	if c, found := byName["sandbox runner"]; found && strings.Contains(c.Info, "host mode") {
		t.Errorf("sandbox runner must not report host mode on parse failure: %+v", c)
	}

	// Config-independent checks still run.
	for _, name := range []string{"database migrates", "secret store", "golangci-lint gate"} {
		if _, found := byName[name]; !found {
			// secret store opens vs secret store depending on identity.
			if name == "secret store" {
				if _, openFound := byName["secret store opens"]; openFound {
					continue
				}
			}
			t.Errorf("missing independent check %q; checks = %+v", name, checks)
		}
	}
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
	dir := t.TempDir()
	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "-q", "-b", "base")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("base"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-qm", "base")
	git("checkout", "-qb", "candidate")
	if err := os.MkdirAll(filepath.Join(dir, "internal", "eval"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "internal", "eval", "eval.go"), []byte("package eval"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-qm", "touch protected eval")
	git("checkout", "-q", "base")
	err := rejectProtectedChanges(context.Background(), dir, "candidate", nil)
	if err == nil || !strings.Contains(err.Error(), `protected path "internal/eval/eval.go"`) {
		t.Fatalf("production protected-path check = %v", err)
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
	// No MCP configured → informational check still present.
	if _, ok := names["mcp servers"]; !ok {
		t.Errorf("missing mcp servers check; got %v", names)
	}
}

func TestDoctorReportsMCPExecutionAuthorities(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WAFFLE_HOME", home)
	cfg := `
[[mcp]]
name = "github"
command = "npx"
args = ["-y", "@modelcontextprotocol/server-github"]
execution = "host"
groups = ["main"]
env = ["GITHUB_TOKEN"]

[[mcp]]
name = "codeintel"
command = "true"
execution = "sandbox"
tools = ["definition"]
`
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	checks, _, err := Doctor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]string{}
	for _, c := range checks {
		if c.OK {
			byName[c.Name] = c.Info
		}
	}
	gh, ok := byName["mcp github authority"]
	if !ok {
		t.Fatalf("missing mcp github authority; checks=%v", byName)
	}
	if !strings.Contains(gh, "execution=host") || !strings.Contains(gh, "groups=main") || !strings.Contains(gh, "authority=host") {
		t.Errorf("github authority info = %q", gh)
	}
	ci, ok := byName["mcp codeintel authority"]
	if !ok {
		t.Fatalf("missing mcp codeintel authority; checks=%v", byName)
	}
	if !strings.Contains(ci, "execution=sandbox") {
		t.Errorf("codeintel authority info = %q", ci)
	}
	if !strings.Contains(ci, "authority=sandbox|restricted") {
		t.Errorf("codeintel should report sandbox|restricted authority, got %q", ci)
	}
	if strings.Contains(ci, "not wired") {
		t.Errorf("sandbox MCP is wired; stale note in %q", ci)
	}
}
