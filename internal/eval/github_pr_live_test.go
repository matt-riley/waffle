package eval

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"

	"github.com/matt-riley/waffle/internal/gitcred"
)

func testApp(t *testing.T) *gitcred.App {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}
	app, err := gitcred.NewApp(42, 7, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), "http://github.test", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return app
}

func TestParsePRResult(t *testing.T) {
	cases := []struct {
		name    string
		out     string
		number  int
		htmlURL string
		wantErr bool
	}{
		{name: "happy path", out: "Opened pull request #7 for owner/repo: https://github.com/owner/repo/pull/7", number: 7, htmlURL: "https://github.com/owner/repo/pull/7"},
		{name: "enterprise host", out: "Opened pull request #12 for owner/repo: https://github.example.com/owner/repo/pull/12", number: 12, htmlURL: "https://github.example.com/owner/repo/pull/12"}, {name: "trailing noise", out: "Opened pull request #3 for owner/repo: https://github.com/owner/repo/pull/3\n", number: 3, htmlURL: "https://github.com/owner/repo/pull/3"},
		{name: "not a PR banner", out: "something else entirely", wantErr: true},
		{name: "no url", out: "Opened pull request #7 for owner/repo", wantErr: true},
		{name: "non-numeric number", out: "Opened pull request #x for owner/repo: https://github.com/o/r/pull/7", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			number, htmlURL, err := parsePRResult(tc.out)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parsePRResult(%q) = %d %q, want error", tc.out, number, htmlURL)
				}
				return
			}
			if err != nil {
				t.Fatalf("parsePRResult(%q): %v", tc.out, err)
			}
			if number != tc.number || htmlURL != tc.htmlURL {
				t.Fatalf("parsePRResult(%q) = (%d, %q), want (%d, %q)", tc.out, number, htmlURL, tc.number, tc.htmlURL)
			}
		})
	}
}

func TestLiveGitHubPRCaseIsOptIn(t *testing.T) {
	t.Setenv("WAFFLE_EVAL_LIVE", "1")
	t.Setenv("WAFFLE_EVAL_GITHUB_REPO", "owner/repo")

	// A nil app disables the case regardless of the env opt-ins.
	if c := LiveGitHubPRCase(nil); c != nil {
		t.Fatalf("LiveGitHubPRCase(nil) = %+v, want nil", c)
	}
	// A missing repo disables the case.
	t.Setenv("WAFFLE_EVAL_GITHUB_REPO", "")
	if c := LiveGitHubPRCase(testApp(t)); c != nil {
		t.Fatalf("LiveGitHubPRCase without repo = %+v, want nil", c)
	}
	// Without WAFFLE_EVAL_LIVE the case is disabled even with all opt-ins.
	t.Setenv("WAFFLE_EVAL_LIVE", "")
	t.Setenv("WAFFLE_EVAL_GITHUB_REPO", "owner/repo")
	if c := LiveGitHubPRCase(testApp(t)); c != nil {
		t.Fatalf("LiveGitHubPRCase without live env = %+v, want nil", c)
	}
	// All opt-ins present: the case is registered.
	t.Setenv("WAFFLE_EVAL_LIVE", "1")
	if c := LiveGitHubPRCase(testApp(t)); c == nil || c.Name != "github-pr-live" {
		t.Fatalf("LiveGitHubPRCase with all opt-ins = %+v, want the case", c)
	}
}

func TestMaskGitArgsRedactsCredentialUserinfo(t *testing.T) {
	got := maskGitArgs([]string{
		"push", "-q",
		"https://x-access-token:REDACTED@github.com/owner/repo.git",
		"HEAD:refs/heads/waffle-eval-x",
	})
	for _, leak := range []string{"REDACTED", "x-access-token", "@"} {
		if strings.Contains(got, leak) {
			t.Fatalf("maskGitArgs leaked %q: %q", leak, got)
		}
	}
	if !strings.Contains(got, "https://github.com/owner/repo.git") {
		t.Fatalf("maskGitArgs did not keep the host/path: %q", got)
	}
	// Non-URL arguments pass through untouched.
	if !strings.Contains(got, "HEAD:refs/heads/waffle-eval-x") {
		t.Fatalf("maskGitArgs dropped a plain argument: %q", got)
	}
}

func TestWebHostForAPIDerivesWebHost(t *testing.T) {
	cases := []struct{ api, want string }{
		{"https://api.github.com", "github.com"},
		{"https://api.github.com/", "github.com"},
		{"https://ghe.example.com/api/v3", "ghe.example.com"},
		{"https://github.example.com", "github.example.com"},
		{"not a url", ""},
	}
	for _, tc := range cases {
		if got := webHostForAPI(tc.api); got != tc.want {
			t.Fatalf("webHostForAPI(%q) = %q, want %q", tc.api, got, tc.want)
		}
	}
}
