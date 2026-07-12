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
	"strings"
	"sync"
	"time"
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

// Credential returns a GitHub credential for the canonical owner/repo path.
func (a *App) Credential(ctx context.Context, repo string) (string, string, error) {
	repo = strings.Trim(strings.TrimSuffix(repo, ".git"), "/")
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid github repo scope %q", repo)
	}
	now := a.now().UTC()
	a.mu.Lock()
	cached, ok := a.tokens[strings.ToLower(repo)]
	a.mu.Unlock()
	if ok && now.Add(5*time.Minute).Before(cached.expires) {
		return "x-access-token", cached.value, nil
	}
	token, expires, err := a.mint(ctx, parts[1], now)
	if err != nil {
		return "", "", err
	}
	a.mu.Lock()
	a.tokens[strings.ToLower(repo)] = cachedToken{value: token, expires: expires}
	a.mu.Unlock()
	return "x-access-token", token, nil
}

func (a *App) mint(ctx context.Context, repository string, now time.Time) (token string, expires time.Time, err error) {
	jwt, err := a.jwt(now)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign github app JWT: %w", err)
	}
	body, _ := json.Marshal(map[string]any{"repositories": []string{repository}, "permissions": map[string]string{"contents": "write"}})
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
