package gitcred

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/matt-riley/waffle/internal/intake"
)

// App mints and caches GitHub installation tokens for one app installation.
type App struct {
	appID, installationID int64
	key                   *rsa.PrivateKey
	baseURL               string
	client                *http.Client
	now                   func() time.Time
	mu                    sync.Mutex
	tokens                map[string]cachedToken
}

// BaseURL returns the configured API root (default https://api.github.com).
// Exported so host-side tools and the live eval can compose verification and
// cleanup requests against the same endpoint the App mints for.
func (a *App) BaseURL() string { return a.baseURL }

// Client returns the HTTP client the App uses for GitHub API calls.
func (a *App) Client() *http.Client { return a.client }

type cachedToken struct {
	value   string
	expires time.Time
}

func NewApp(appID, installationID int64, privateKey []byte, baseURL string, client *http.Client, now func() time.Time) (*App, error) {
	if appID <= 0 || installationID <= 0 {
		return nil, fmt.Errorf("github app and installation IDs must be positive")
	}
	block, _ := pem.Decode(privateKey)
	if block == nil {
		return nil, fmt.Errorf("github app private key is not PEM")
	}
	var key *rsa.PrivateKey
	if parsed, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		key = parsed
	} else if parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		var ok bool
		key, ok = parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("github app private key is not RSA")
		}
	} else {
		return nil, fmt.Errorf("parse github app private key: %w", err)
	}
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	if client == nil {
		client = http.DefaultClient
	}
	if now == nil {
		now = time.Now
	}
	return &App{appID: appID, installationID: installationID, key: key, baseURL: strings.TrimRight(baseURL, "/"), client: client, now: now, tokens: map[string]cachedToken{}}, nil
}

func (a *App) jwt(now time.Time) (string, error) {
	enc := base64.RawURLEncoding
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	payload, _ := json.Marshal(map[string]any{"iat": now.Add(-30 * time.Second).Unix(), "exp": now.Add(9 * time.Minute).Unix(), "iss": a.appID})
	unsigned := enc.EncodeToString(header) + "." + enc.EncodeToString(payload)
	hash := sha256.Sum256([]byte(unsigned))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, hash[:])
	if err != nil {
		return "", err
	}
	return unsigned + "." + enc.EncodeToString(sig), nil
}

// Permission sets requested when minting an installation token. Each caller
// asks for the narrowest set that does its job: the git credential handed to a
// workspace can only push code, and never gains the ability to open a pull
// request even though the same app grants it. The host-side tools (#252) each
// pick exactly one of these per call.
var (
	permContentsWrite    = map[string]string{"contents": "write"}
	permPullRequests     = map[string]string{"pull_requests": "write"}
	permPullRequestsRead = map[string]string{"pull_requests": "read"}
	permIssuesWrite      = map[string]string{"issues": "write"}
	permIssuesRead       = map[string]string{"issues": "read"}
	permChecksRead       = map[string]string{"checks": "read"}
)

// SplitRepo validates and splits a canonical owner/repo path.
func SplitRepo(repo string) (owner, name string, err error) {
	repo = strings.Trim(strings.TrimSuffix(repo, ".git"), "/")
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid github repo scope %q", repo)
	}
	return parts[0], parts[1], nil
}

// Credential returns a GitHub credential for the canonical owner/repo path.
func (a *App) Credential(ctx context.Context, repo string) (string, string, error) {
	owner, name, err := SplitRepo(repo)
	if err != nil {
		return "", "", err
	}
	// Compose the cache key from the split parts rather than re-normalising the
	// raw input: a second copy of that logic could drift from SplitRepo and
	// silently change which entry a lookup hits.
	cacheKey := strings.ToLower(owner + "/" + name)
	now := a.now().UTC()
	a.mu.Lock()
	cached, ok := a.tokens[cacheKey]
	a.mu.Unlock()
	if ok && now.Add(5*time.Minute).Before(cached.expires) {
		return "x-access-token", cached.value, nil
	}
	token, expires, err := a.mint(ctx, name, permContentsWrite, now)
	if err != nil {
		return "", "", err
	}
	a.mu.Lock()
	a.tokens[cacheKey] = cachedToken{value: token, expires: expires}
	a.mu.Unlock()
	return "x-access-token", token, nil
}

// Token mints a single-use installation token for repo carrying exactly
// permissions, and nothing else. It is deliberately not cached alongside
// Credential's tokens: the two carry different permissions, and one cache
// keyed only by repo would hand a write token to a workspace asking for git
// access. Host-side tools mint per call and use the token once, so a leaked
// token expires in about an hour and no later request can re-use it.
func (a *App) Token(ctx context.Context, repo string, permissions map[string]string) (string, error) {
	_, name, err := SplitRepo(repo)
	if err != nil {
		return "", err
	}
	token, _, err := a.mint(ctx, name, permissions, a.now().UTC())
	if err != nil {
		return "", err
	}
	return token, nil
}

// PullRequestToken mints a token that may open a pull request on repo, and
// nothing else: Token with the pull_requests:write permission set.
func (a *App) PullRequestToken(ctx context.Context, repo string) (string, error) {
	return a.Token(ctx, repo, permPullRequests)
}

// CheckRun is one check run reported for a commit.
type CheckRun struct {
	Name       string
	Status     string // completed | in_progress | queued | pending
	Conclusion string // success | failure | cancelled | timed_out | action_required | skipped | neutral
	DetailsURL string
	HeadSHA    string // the commit the run actually targeted
}

// CheckRunsForCommit lists check runs for one exact commit SHA using a
// checks:read installation token, following Link-header pagination. Runs whose
// head_sha differs from the requested SHA are returned as-is so the CI gate
// can fail closed on stale or wrong-SHA evidence (#415).
func (a *App) CheckRunsForCommit(ctx context.Context, repo, sha string) ([]CheckRun, error) {
	owner, name, err := SplitRepo(repo)
	if err != nil {
		return nil, err
	}
	token, err := a.Token(ctx, repo, permChecksRead)
	if err != nil {
		return nil, fmt.Errorf("mint checks read token: %w", err)
	}
	var all []CheckRun
	pageURL := fmt.Sprintf("%s/repos/%s/%s/commits/%s/check-runs?per_page=100", a.baseURL, owner, name, url.PathEscape(sha))
	for page := 1; page <= maxPages; page++ {
		resp, cancel, err := a.do(ctx, pageURL, token)
		if err != nil {
			return nil, err
		}
		raw, readErr := readBody(resp, jsonBodyCap)
		cancel()
		if readErr != nil {
			return nil, readErr
		}
		if resp.StatusCode/100 != 2 {
			return nil, refused("check runs", resp, raw)
		}
		var body struct {
			CheckRuns []struct {
				Name       string `json:"name"`
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
				DetailsURL string `json:"details_url"`
				HeadSHA    string `json:"head_sha"`
			} `json:"check_runs"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			return nil, fmt.Errorf("github check runs response unreadable: %w", err)
		}
		for _, r := range body.CheckRuns {
			all = append(all, CheckRun{
				Name: r.Name, Status: r.Status, Conclusion: r.Conclusion,
				DetailsURL: r.DetailsURL, HeadSHA: r.HeadSHA,
			})
		}
		nextURL, ok := intake.NextLinkURL(resp.Header.Get("Link"))
		if !ok {
			break
		}
		if page == maxPages {
			break
		}
		pageURL = nextURL
	}
	return all, nil
}

// do performs one authenticated GitHub GET against the app's API root.
// The returned cancel must be called once the caller has finished reading the
// response body.
func (a *App) do(ctx context.Context, apiURL, token string) (*http.Response, func(), error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", jsonAccept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := a.client.Do(req)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	return resp, cancel, nil
}

// Verify reports whether the configured app credentials can still mint an
// installation token. It mints a minimally scoped token and discards it: the
// token, its value, and the app identifiers never leave this call. Callers use
// it as a health probe only.
func (a *App) Verify(ctx context.Context) error {
	_, _, err := a.mint(ctx, "", permContentsWrite, a.now().UTC())
	return err
}

// mint exchanges the app JWT for an installation token. An empty repository
// requests the installation's default scope, which Verify uses as a health
// probe without naming a repository.
func (a *App) mint(ctx context.Context, repository string, permissions map[string]string, now time.Time) (token string, expires time.Time, err error) {
	jwt, err := a.jwt(now)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign github app JWT: %w", err)
	}
	request := map[string]any{"permissions": permissions}
	if repository != "" {
		request["repositories"] = []string{repository}
	}
	body, _ := json.Marshal(request)
	url := fmt.Sprintf("%s/app/installations/%d/access_tokens", a.baseURL, a.installationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("github app token request: %w", err)
	}
	defer func() {
		if cerr := resp.Body.Close(); err == nil {
			err = cerr
		}
	}()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode/100 != 2 {
		return "", time.Time{}, fmt.Errorf("github app token request: %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", time.Time{}, fmt.Errorf("github app token response invalid: %w", err)
	}
	if out.Token == "" {
		return "", time.Time{}, fmt.Errorf("github app token response invalid: missing token")
	}
	if out.ExpiresAt.IsZero() {
		out.ExpiresAt = now.Add(time.Hour)
	}
	return out.Token, out.ExpiresAt, nil
}
