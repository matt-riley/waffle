package gitcred

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/session"
)

// RepoForSession resolves the repo a session's workspace is bound to. It is the
// same scoping the broker's git-credential face applies, supplied here as a
// seam so the tool never has to reach into the workspace package.
type RepoForSession func(ctx context.Context, sessionID string) (string, error)

// PullRequestTool opens a pull request for the repo the calling session's
// workspace is bound to.
//
// It runs on the host, never in the workspace container, and that is the whole
// point. A workspace can read any credential it is handed — `git credential
// fill` returns the git token to anything running inside it — so the token that
// reaches a container stays pinned to contents:write. The pull-request token is
// minted here, used for one API call, and never leaves the host.
type PullRequestTool struct {
	App  *App
	Repo RepoForSession
	// BaseURL overrides the API root; empty uses the app's configured root.
	BaseURL string
	Client  *http.Client
}

// mustSchema mirrors internal/tool's helper: a malformed builtin schema is a
// programmer error, caught at init rather than on first tool call.
func mustSchema(s string) json.RawMessage {
	var v map[string]any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		panic("gitcred: bad tool schema: " + err.Error())
	}
	return json.RawMessage(s)
}

var pullRequestSchema = mustSchema(`{
	"type": "object",
	"properties": {
		"title": {"type": "string", "description": "Pull request title"},
		"head": {"type": "string", "description": "Branch containing the changes, already pushed"},
		"base": {"type": "string", "description": "Branch to merge into, for example main"},
		"body": {"type": "string", "description": "Pull request description"},
		"draft": {"type": "boolean", "description": "Open as a draft; defaults to false"}
	},
	"required": ["title", "head", "base"]
}`)

func (PullRequestTool) Def() llm.Tool {
	return llm.Tool{
		Name: "github_pr",
		Description: "Open a GitHub pull request for the repo this session's workspace is bound to. " +
			"Push the head branch first. Returns the pull request URL.",
		InputSchema: pullRequestSchema,
	}
}

func (t PullRequestTool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	if t.App == nil {
		return "", fmt.Errorf("no github app is configured; set [github.app] to open pull requests")
	}
	if t.Repo == nil {
		return "", fmt.Errorf("workspace binding lookup is not configured")
	}
	var in struct {
		Title string `json:"title"`
		Head  string `json:"head"`
		Base  string `json:"base"`
		Body  string `json:"body"`
		Draft bool   `json:"draft"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("bad input: %w", err)
	}
	for name, value := range map[string]string{"title": in.Title, "head": in.Head, "base": in.Base} {
		if strings.TrimSpace(value) == "" {
			return "", fmt.Errorf("%s is required", name)
		}
	}

	// Deny by default: an unbound session gets no pull request, exactly as it
	// gets no git credential. The repo is never taken from tool input, so a
	// prompt injection cannot redirect the pull request at another repo.
	sessionID := session.IDFromContext(ctx)
	if strings.TrimSpace(sessionID) == "" {
		return "", fmt.Errorf("no session id; refusing to open a pull request")
	}
	repo, err := t.Repo(ctx, sessionID)
	if err != nil {
		return "", fmt.Errorf("session is not bound to a repo workspace; refusing to open a pull request: %w", err)
	}
	owner, name, err := SplitRepo(repo)
	if err != nil {
		return "", err
	}

	token, err := t.App.PullRequestToken(ctx, repo)
	if err != nil {
		return "", fmt.Errorf("mint pull request token: %w", err)
	}

	payload, err := json.Marshal(map[string]any{
		"title": in.Title,
		"head":  in.Head,
		"base":  in.Base,
		"body":  in.Body,
		"draft": in.Draft,
	})
	if err != nil {
		return "", err
	}

	base := t.BaseURL
	if base == "" {
		base = t.App.baseURL
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	url := fmt.Sprintf("%s/repos/%s/%s/pulls", strings.TrimSuffix(base, "/"), owner, name)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	client := t.Client
	if client == nil {
		client = t.App.client
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("github pull request request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode/100 != 2 {
		// The response can echo back submitted content; keep it short and never
		// let it read as instructions to the model.
		return "", fmt.Errorf("github refused the pull request: %s: %s",
			resp.Status, strings.TrimSpace(truncate(string(raw), 400)))
	}
	var out struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.HTMLURL == "" {
		return "", fmt.Errorf("github returned an unreadable pull request response")
	}
	return fmt.Sprintf("Opened pull request #%d for %s/%s: %s", out.Number, owner, name, out.HTMLURL), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
