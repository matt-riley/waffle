// Package gitcred implements the client side of waffle's git credential
// flow: inside a workspace container, git invokes `waffle git-credential
// get`, which asks the host broker for a credential. The container never
// holds a durable secret (docs/plan.md, "Repo workspaces").
package gitcred

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Env vars injected into workspace / sandbox containers.
const (
	EnvBroker    = "WAFFLE_BROKER"
	EnvToken     = "WAFFLE_SESSION_TOKEN"
	EnvTokenFile = "WAFFLE_SESSION_TOKEN_FILE"

	// DefaultSessionTokenPath is the conventional bind-mounted path used by
	// sandbox and workspace containers when WAFFLE_SESSION_TOKEN is unset
	// and WAFFLE_SESSION_TOKEN_FILE is not set (see #106).
	DefaultSessionTokenPath = "/waffle/queue/session.token"
)

// Get forwards a git credential request to the broker and returns the
// response body (already in git's key=value format).
func Get(ctx context.Context, brokerURL, token string, request io.Reader) (out string, err error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(brokerURL, "/")+"/git-credential", request)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "text/plain")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("git-credential: broker unreachable: %w", err)
	}
	defer func() {
		if cerr := resp.Body.Close(); err == nil {
			err = cerr
		}
	}()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("git-credential: broker: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return string(body), nil
}

// sessionToken resolves the broker session token without requiring it in the
// process environment. Preference order:
//  1. WAFFLE_SESSION_TOKEN env (legacy / host-side tooling)
//  2. file at WAFFLE_SESSION_TOKEN_FILE (path only; set by docker run)
//  3. DefaultSessionTokenPath (conventional queue bind-mount path)
func sessionToken() (string, error) {
	if t := strings.TrimSpace(os.Getenv(EnvToken)); t != "" {
		return t, nil
	}
	path := strings.TrimSpace(os.Getenv(EnvTokenFile))
	if path == "" {
		path = DefaultSessionTokenPath
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("git-credential: read session token from %s: %w", path, err)
	}
	t := strings.TrimSpace(string(data))
	if t == "" {
		return "", fmt.Errorf("git-credential: session token file %s is empty", path)
	}
	return t, nil
}

// Run is the `waffle git-credential <op>` entrypoint. Only "get" does
// anything; "store" and "erase" are silent no-ops (nothing is cached).
func Run(ctx context.Context, op string, stdin io.Reader, stdout io.Writer) error {
	if op != "get" {
		return nil
	}
	brokerURL := os.Getenv(EnvBroker)
	if brokerURL == "" {
		return fmt.Errorf("git-credential: %s must be set (am I inside a waffle workspace?)", EnvBroker)
	}
	token, err := sessionToken()
	if err != nil {
		return err
	}
	out, err := Get(ctx, brokerURL, token, stdin)
	if err != nil {
		return err
	}
	_, err = io.WriteString(stdout, out)
	return err
}
