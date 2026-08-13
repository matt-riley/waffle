package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/matt-riley/waffle/internal/gitcred"
	"github.com/matt-riley/waffle/internal/session"
)

// LiveGitHubPRCase returns the opt-in end-to-end github_pr eval (#241), or
// nil when the live tier is not enabled. The github_pr tool shipped without
// ever being exercised against the real GitHub API — a fake API cannot catch
// a missing pull_requests:write grant, a repo the App installation cannot
// reach, or a broken workspace-binding lookup — so this case pushes a real
// branch, opens a real pull request through the tool, and confirms the
// returned URL names a real PR. It also confirms the negative: an unbound
// session is refused before a token is minted.
//
// Opt-ins (all required; skipped otherwise, matching the provider live tier):
//
//   - WAFFLE_EVAL_LIVE=1
//   - WAFFLE_EVAL_GITHUB_REPO=owner/repo — a real repo whose installation
//     the App can push to and open pull requests in (contents:write and
//     pull_requests:write on the GitHub App)
//   - a configured [github.app] (app non-nil)
func LiveGitHubPRCase(app *gitcred.App) *Case {
	if os.Getenv("WAFFLE_EVAL_LIVE") != "1" || app == nil {
		return nil
	}
	repo := strings.TrimSpace(os.Getenv("WAFFLE_EVAL_GITHUB_REPO"))
	if repo == "" {
		return nil
	}
	return &Case{Name: "github-pr-live", Run: func(ctx context.Context) error {
		return evalLiveGitHubPR(ctx, app, repo)
	}}
}

func evalLiveGitHubPR(ctx context.Context, app *gitcred.App, repo string) error {
	owner, name, err := gitcred.SplitRepo(repo)
	if err != nil {
		return fmt.Errorf("WAFFLE_EVAL_GITHUB_REPO: %w", err)
	}
	if _, err := exec.LookPath("git"); err != nil {
		return fmt.Errorf("github_pr live eval needs git on PATH: %w", err)
	}
	// The App must hold pull_requests:write; Verify mints a token and would
	// fail if the installation lacked permissions, which is the exact gap
	// that shipped untested (#241).
	if err := app.Verify(ctx); err != nil {
		return fmt.Errorf("github app verify (installation must grant contents:write and pull_requests:write): %w", err)
	}

	// The PR's base is the repository's default branch, read through the API
	// so the eval does not assume "main".
	base, err := defaultBranch(ctx, app, repo)
	if err != nil {
		return fmt.Errorf("read default branch of %s: %w", repo, err)
	}

	branch := "waffle-eval-" + time.Now().UTC().Format("20060102T150405Z")
	title := fmt.Sprintf("waffle eval: exercise github_pr end to end (%s)", branch)

	// 1. Push a throwaway branch using a contents:write credential minted the
	// same way workspace git credentials are (App.Credential).
	user, token, err := app.Credential(ctx, repo)
	if err != nil {
		return fmt.Errorf("mint contents credential for %s: %w", repo, err)
	}
	if err := pushEvalBranch(ctx, repo, user, token, branch); err != nil {
		return fmt.Errorf("push eval branch %s: %w", branch, err)
	}
	prNumber := 0
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
		defer cancel()
		if prNumber > 0 {
			_ = closePullRequest(cleanupCtx, app, repo, prNumber)
		}
		_ = deleteBranch(cleanupCtx, app, repo, branch)
	}()

	// 2. Positive: a session bound to the repo invokes github_pr; the PR must
	// be created and its URL must resolve to a real PR.
	input, err := json.Marshal(map[string]any{
		"title": title,
		"head":  branch,
		"base":  base,
		"body":  "Opened by the waffle github_pr live eval (#241).",
	})
	if err != nil {
		return err
	}
	bound := func(context.Context, string) (string, error) { return repo, nil }
	prTool := gitcred.PullRequestTool{App: app, Repo: bound}
	out, err := prTool.Run(session.WithSession(ctx, "eval-github-pr-bound"), input)
	if err != nil {
		return fmt.Errorf("github_pr refused for a bound session: %w", err)
	}
	prNumber, htmlURL, err := parsePRResult(out)
	if err != nil {
		return fmt.Errorf("github_pr returned an unparsable result %q: %w", out, err)
	}
	if err := assertPullRequestResolves(ctx, app, repo, owner, name, prNumber, htmlURL); err != nil {
		return err
	}

	// 3. Negative: the same call from a session with no workspace binding is
	// refused before a token is minted. The transport fails the eval if the
	// tool makes any API call, proving the refusal happens before minting.
	noBinding := func(context.Context, string) (string, error) {
		return "", errors.New("no workspace row for this session")
	}
	refuseTool := gitcred.PullRequestTool{App: app, Repo: noBinding, Client: &http.Client{Transport: failOnUseTransport{}}}
	_, err = refuseTool.Run(session.WithSession(ctx, "eval-github-pr-unbound"), input)
	if err == nil {
		return errors.New("github_pr accepted an unbound session")
	}
	if !strings.Contains(err.Error(), "not bound to a repo workspace") {
		return fmt.Errorf("unbound refusal error = %q, want the no-binding refusal", err.Error())
	}
	return nil
}

// parsePRResult extracts the PR number and URL from the tool's success text
// ("Opened pull request #N for owner/name: https://...").
func parsePRResult(out string) (number int, htmlURL string, err error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(out), "Opened pull request #")
	if !ok {
		return 0, "", fmt.Errorf("result does not start with the PR banner")
	}
	num, rest, ok := strings.Cut(rest, " for ")
	if !ok {
		return 0, "", fmt.Errorf("result has no repo owner/name")
	}
	n, convErr := strconv.Atoi(num)
	if convErr != nil || n <= 0 {
		return 0, "", fmt.Errorf("result has an invalid PR number %q", num)
	}
	urlStart := strings.Index(rest, "http")
	if urlStart < 0 {
		return 0, "", fmt.Errorf("result has no URL")
	}
	return n, strings.TrimSpace(rest[urlStart:]), nil
}

// assertPullRequestResolves confirms the created PR is real: the API resource
// answers with the same number under a pull_requests:write token, and the
// returned URL resolves (200 for a public repo, a login redirect for a
// private one — anything but 404).
func assertPullRequestResolves(ctx context.Context, app *gitcred.App, repo, owner, name string, number int, htmlURL string) error {
	pullToken, err := app.PullRequestToken(ctx, repo)
	if err != nil {
		return fmt.Errorf("mint pull_requests token for verification: %w", err)
	}
	apiURL := app.BaseURL() + "/repos/" + owner + "/" + name + "/pulls/" + fmt.Sprint(number)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+pullToken)
	resp, err := app.Client().Do(req)
	if err != nil {
		return fmt.Errorf("resolve pull request API resource: %w", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("pull request API resource returned %s; want 200", resp.Status)
	}

	// The html_url must be a pull URL for the repo and must resolve. The web
	// host is derived from the App's API root so GitHub Enterprise deployments
	// (api.<host> → <host>) work instead of hardcoding github.com.
	wantHost := webHostForAPI(app.BaseURL())
	if parsed, err := url.Parse(htmlURL); err != nil || parsed.Host != wantHost ||
		!strings.HasPrefix(parsed.Path, "/"+owner+"/"+name+"/pull/") {
		return fmt.Errorf("returned URL %q does not name %s/pull/<n>", htmlURL, wantHost+"/"+owner+"/"+name)
	}
	resolveCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err = http.NewRequestWithContext(resolveCtx, http.MethodGet, htmlURL, nil)
	if err != nil {
		return err
	}
	// Use the App's client so any custom TLS/proxy settings the operator
	// configured on it apply; the URL came from GitHub's own API.
	res, err := app.Client().Do(req)
	if err != nil {
		return fmt.Errorf("resolve returned URL %s: %w", htmlURL, err)
	}
	_ = res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return fmt.Errorf("returned URL %s resolves to 404", htmlURL)
	}
	return nil
}

// webHostForAPI derives the repository web host from the App's API base URL:
// https://api.github.com → github.com (strip the api. subdomain); a GitHub
// Enterprise base like https://ghe.example.com/api/v3 keeps its own host.
func webHostForAPI(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		return ""
	}
	host := u.Hostname()
	if strings.HasPrefix(host, "api.") {
		return strings.TrimPrefix(host, "api.")
	}
	return host
}

// failOnUseTransport fails the eval the moment any HTTP request is made: the
// unbound-session refusal must happen before a token is minted, and a mint is
// an HTTP call to the access_tokens endpoint.
type failOnUseTransport struct{}

func (failOnUseTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("github_pr made an API call for an unbound session; the refusal must happen before a token is minted")
}

// pushEvalBranch creates a temporary git repo with one commit and pushes it
// as branch using the App installation credential (x-access-token + token).
func pushEvalBranch(ctx context.Context, repo, user, token, branch string) error {
	dir, err := os.MkdirTemp("", "waffle-eval-github-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	git := func(args ...string) error {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			// Never echo the raw arguments on failure: the push URL carries the
			// installation token in its userinfo. Mask it, then include the
			// output (git's own diagnostics never repeat the credential).
			return fmt.Errorf("git %s: %w: %s", maskGitArgs(args), err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	if err := git("init", "-q", "-b", branch); err != nil {
		return err
	}
	if err := git("config", "user.email", "waffle-eval@localhost"); err != nil {
		return err
	}
	if err := git("config", "user.name", "waffle eval"); err != nil {
		return err
	}
	payload := fmt.Sprintf("waffle github_pr live eval %s\n", time.Now().UTC().Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(dir, "waffle-eval.txt"), []byte(payload), 0o644); err != nil {
		return err
	}
	if err := git("add", "waffle-eval.txt"); err != nil {
		return err
	}
	if err := git("commit", "-q", "-m", "waffle eval: exercise github_pr end to end"); err != nil {
		return err
	}
	pushURL := "https://" + user + ":" + token + "@github.com/" + repo + ".git"
	return git("push", "-q", pushURL, "HEAD:refs/heads/"+branch)
}

// maskGitArgs redacts credential-bearing userinfo from git arguments before
// they reach an error message: the push URL embeds the installation token
// (https://x-access-token:<token>@host/repo.git) and must never be logged.
func maskGitArgs(args []string) string {
	masked := make([]string, len(args))
	for i, arg := range args {
		u, err := url.Parse(arg)
		if err == nil && u.Scheme != "" && u.Host != "" && u.User != nil {
			// Drop the userinfo wholesale: percent-encoding a placeholder
			// (%2A%2A%2A) would be misleading.
			u.User = nil
			masked[i] = u.String()
			continue
		}
		masked[i] = arg
	}
	return strings.Join(masked, " ")
}

// defaultBranch reads the repository's default branch with a pull_requests
// token (installation tokens carry repository metadata read).
func defaultBranch(ctx context.Context, app *gitcred.App, repo string) (string, error) {
	owner, name, err := gitcred.SplitRepo(repo)
	if err != nil {
		return "", err
	}
	token, err := app.PullRequestToken(ctx, repo)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, app.BaseURL()+"/repos/"+owner+"/"+name, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := app.Client().Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("GET /repos/%s returned %s: %s", repo, resp.Status, strings.TrimSpace(string(raw)))
	}
	var out struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.DefaultBranch == "" {
		return "", errors.New("repository response has no default_branch")
	}
	return out.DefaultBranch, nil
}

// closePullRequest closes the opened PR (cleanup).
func closePullRequest(ctx context.Context, app *gitcred.App, repo string, number int) error {
	owner, name, err := gitcred.SplitRepo(repo)
	if err != nil {
		return err
	}
	token, err := app.PullRequestToken(ctx, repo)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]string{"state": "closed"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, app.BaseURL()+"/repos/"+owner+"/"+name+"/pulls/"+fmt.Sprint(number), strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Client().Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("close pull request %d: %s", number, resp.Status)
	}
	return nil
}

// deleteBranch deletes the pushed eval branch (cleanup) with a contents:write
// credential.
func deleteBranch(ctx context.Context, app *gitcred.App, repo, branch string) error {
	owner, name, err := gitcred.SplitRepo(repo)
	if err != nil {
		return err
	}
	_, token, err := app.Credential(ctx, repo)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, app.BaseURL()+"/repos/"+owner+"/"+name+"/git/refs/heads/"+branch, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := app.Client().Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusUnprocessableEntity {
		return fmt.Errorf("delete branch %s: %s", branch, resp.Status)
	}
	return nil
}
