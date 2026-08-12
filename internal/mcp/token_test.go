package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/secret"
)

// tokenHarness wires a TokenManager over an in-memory store with a
// controllable clock.
type tokenHarness struct {
	store memStore
	now   time.Time
	tm    *TokenManager
}

func newTokenHarness(server string) *tokenHarness {
	h := &tokenHarness{
		store: memStore{},
		now:   time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC),
	}
	h.tm = &TokenManager{
		Store:  h.store,
		Server: server,
		HTTP:   http.DefaultClient,
		Now:    func() time.Time { return h.now },
	}
	return h
}

// saveToken stores a token state as `waffle mcp login` would.
func (h *tokenHarness) saveToken(access, refresh string, expiresIn time.Duration, tokenEndpoint string) error {
	return h.tm.Save(&TokenSet{
		AccessToken:  access,
		RefreshToken: refresh,
		TokenType:    "bearer",
		ExpiresIn:    int(expiresIn.Seconds()),
	}, TokenMeta{TokenEndpoint: tokenEndpoint, ClientID: "client-1", Scope: "read"})
}

// TestTokenStatePersistsOnlyInSecretStore: saving and loading a token goes
// through the store (age-encrypted in production) under the canonical
// mcp/<server> names — never config.toml.
func TestTokenStatePersistsOnlyInSecretStore(t *testing.T) {
	h := newTokenHarness("github")
	if err := h.saveToken("access-123456", "refresh-123456", time.Hour, "http://token.test"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"mcp/github/access-token", "mcp/github/refresh-token", "mcp/github/token-meta"} {
		if _, ok := h.store[name]; !ok {
			t.Fatalf("token state missing from secret store under %q", name)
		}
	}
	if h.store["mcp/github/access-token"] != "access-123456" {
		t.Fatalf("access token value not stored")
	}
	var meta TokenMeta
	if err := json.Unmarshal([]byte(h.store["mcp/github/token-meta"]), &meta); err != nil {
		t.Fatal(err)
	}
	if meta.ClientID != "client-1" || meta.TokenEndpoint != "http://token.test" {
		t.Fatalf("meta = %+v", meta)
	}
	if err := h.tm.Clear(); err != nil {
		t.Fatal(err)
	}
	for name := range h.store {
		t.Fatalf("token state %q survived Clear", name)
	}
}

// TestMissingTokenFailsClosedWithLoginHint: a server with no stored token errors
// with the login hint — its tools never register.
func TestMissingTokenFailsClosedWithLoginHint(t *testing.T) {
	h := newTokenHarness("github")
	_, err := h.tm.AccessToken(context.Background())
	if err == nil {
		t.Fatal("AccessToken succeeded with no stored token")
	}
	if !errors.Is(err, ErrNoToken) {
		t.Fatalf("error = %v, want ErrNoToken", err)
	}
	if !strings.Contains(err.Error(), "waffle mcp login github") {
		t.Fatalf("error %q lacks the login hint", err)
	}
}

// TestExpiringTokenRefreshesAheadAndFreshTokenSkipsNetwork: with the clock seam, a token
// inside the refresh window is refreshed before it expires; a fresh token
// is served without a network call.
func TestExpiringTokenRefreshesAheadAndFreshTokenSkipsNetwork(t *testing.T) {
	var refreshes atomic.Int32
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshes.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"fresh-access-999999","refresh_token":"fresh-refresh-999999","token_type":"bearer","expires_in":3600,"scope":"read"}`)
	}))
	defer tokenSrv.Close()

	h := newTokenHarness("github")
	h.tm.HTTP = tokenSrv.Client()
	// Expires in 2 minutes: inside DefaultRefreshAhead (5m) → refresh.
	if err := h.saveToken("old-access-123456", "old-refresh-123456", 2*time.Minute, tokenSrv.URL); err != nil {
		t.Fatal(err)
	}
	tok, err := h.tm.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	if tok != "fresh-access-999999" {
		t.Fatalf("token = %q, want refreshed value", tok)
	}
	if refreshes.Load() != 1 {
		t.Fatalf("refreshes = %d, want 1", refreshes.Load())
	}
	// The new state was persisted (secret store), not just held in memory.
	if h.store["mcp/github/access-token"] != "fresh-access-999999" {
		t.Fatalf("persisted access token = %q", h.store["mcp/github/access-token"])
	}
	if h.store["mcp/github/refresh-token"] != "fresh-refresh-999999" {
		t.Fatalf("persisted refresh token = %q", h.store["mcp/github/refresh-token"])
	}

	// A fresh token is served without any refresh call.
	refreshes.Store(0)
	tok, err = h.tm.AccessToken(context.Background())
	if err != nil || tok != "fresh-access-999999" {
		t.Fatalf("fresh read: %v %q", err, tok)
	}
	if refreshes.Load() != 0 {
		t.Fatalf("refresh fired for a fresh token")
	}
}

// TestFailedRefreshDisablesServerWithoutRetryStorm: a refresh failure disables
// the manager with an actionable error; every later call fails with the
// same reason instead of retrying in a hot loop.
func TestFailedRefreshDisablesServerWithoutRetryStorm(t *testing.T) {
	var attempts atomic.Int32
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
	}))
	defer tokenSrv.Close()

	h := newTokenHarness("github")
	h.tm.HTTP = tokenSrv.Client()
	if err := h.saveToken("old-access-123456", "old-refresh-123456", time.Minute, tokenSrv.URL); err != nil {
		t.Fatal(err)
	}
	_, err := h.tm.AccessToken(context.Background())
	if err == nil {
		t.Fatal("AccessToken succeeded after refresh failure")
	}
	if !strings.Contains(err.Error(), "token refresh failed") || !strings.Contains(err.Error(), "waffle mcp login github") {
		t.Fatalf("error %q lacks the refresh failure + login hint", err)
	}
	// Fail closed: no retry storm against the token endpoint.
	for i := 0; i < 5; i++ {
		if _, err := h.tm.AccessToken(context.Background()); err == nil {
			t.Fatal("call after disable succeeded")
		}
	}
	if attempts.Load() != 1 {
		t.Fatalf("token endpoint hit %d times after disable, want 1", attempts.Load())
	}
}

// TestExpiredTokenWithoutRefreshFailsClosed: an expired token with
// no refresh token disables the server rather than presenting tools that
// error on every call.
func TestExpiredTokenWithoutRefreshFailsClosed(t *testing.T) {
	h := newTokenHarness("github")
	if err := h.saveToken("expired-access-123456", "", -time.Minute, "http://token.test"); err != nil {
		t.Fatal(err)
	}
	_, err := h.tm.AccessToken(context.Background())
	if err == nil {
		t.Fatal("expired token without refresh succeeded")
	}
	if !strings.Contains(err.Error(), "no refresh token") {
		t.Fatalf("error %q", err)
	}
}

// TestStoredTokenRedactedFromToolOutput: a token stored through the
// TokenManager is picked up by the existing secret-redaction filter, so a
// tool result echoing it never reaches the model.
func TestStoredTokenRedactedFromToolOutput(t *testing.T) {
	h := newTokenHarness("github")
	const access = "mcp_access_token_abcdef123456"
	if err := h.saveToken(access, "refresh-123456", time.Hour, "http://token.test"); err != nil {
		t.Fatal(err)
	}
	// The redactor is built from the same store the tokens live in.
	r, err := secret.NewRedactor(h.store)
	if err != nil {
		t.Fatal(err)
	}
	redacted := r.Redact("tool result: the token is " + access + " end")
	if strings.Contains(redacted, access) {
		t.Fatalf("access token reached the model: %q", redacted)
	}
	if !strings.Contains(redacted, "[REDACTED") && !strings.Contains(redacted, "tool result") {
		t.Fatalf("redaction shape unexpected: %q", redacted)
	}
}

// TestConcurrentTokenAccessServesStateSafely: concurrent AccessToken calls (parallel tool
// dispatch) must not corrupt state; the token is served or refreshed
// exactly-once under -race.
func TestConcurrentTokenAccessServesStateSafely(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"fresh-000000","token_type":"bearer","expires_in":3600}`)
	}))
	defer tokenSrv.Close()

	h := newTokenHarness("github")
	h.tm.HTTP = tokenSrv.Client()
	if err := h.saveToken("old-111111", "refresh-222222", time.Minute, tokenSrv.URL); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := h.tm.AccessToken(context.Background())
			if err != nil {
				errs <- err
				return
			}
			if tok == "" {
				errs <- errors.New("empty token")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if h.store["mcp/github/access-token"] == "" {
		t.Fatal("token state lost under concurrency")
	}
}

// TestStatusReportsStoredStateWithoutNetwork: `waffle mcp status` shape without network.
func TestStatusReportsStoredStateWithoutNetwork(t *testing.T) {
	h := newTokenHarness("github")
	st := h.tm.Status()
	if st.HasToken {
		t.Fatal("status reports a token before any login")
	}
	if err := h.saveToken("access-123456", "refresh-123456", time.Hour, "http://token.test"); err != nil {
		t.Fatal(err)
	}
	st = h.tm.Status()
	if !st.HasToken || st.Server != "github" || st.Scope != "read" {
		t.Fatalf("status = %+v", st)
	}
}

// TestTokenRefreshRoutesThroughConfiguredProxy: a TokenManager built with
// NewTokenHTTPClient refreshes through the configured proxy — the proxy
// receives the broker credential, and the refresh request never dials the
// token endpoint directly (#249). The token endpoint host (.invalid) is
// unresolvable, so a client that bypassed the proxy could not complete
// the refresh at all.
func TestTokenRefreshRoutesThroughConfiguredProxy(t *testing.T) {
	var proxyHits atomic.Int32
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"fresh-through-proxy-000","refresh_token":"rt-2","token_type":"bearer","expires_in":3600,"scope":"read"}`)
	}))
	defer tokenSrv.Close()

	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits.Add(1)
		if got := r.Header.Get("Proxy-Authorization"); got != "Basic broker-cred-1" {
			http.Error(w, "missing or wrong proxy credential", http.StatusProxyAuthRequired)
			return
		}
		// Forward the absolute-form request to the token server.
		out, err := http.NewRequest(r.Method, tokenSrv.URL, r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		out.Header = r.Header.Clone()
		out.Header.Del("Proxy-Authorization")
		resp, err := http.DefaultClient.Do(out)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		_, _ = io.Copy(w, resp.Body)
	}))
	defer proxy.Close()

	h := newTokenHarness("github")
	h.tm.HTTP = NewTokenHTTPClient(proxy.URL, func() (string, error) { return "Basic broker-cred-1", nil })
	// Expires in 2 minutes: inside DefaultRefreshAhead → the refresh fires
	// on the first AccessToken call.
	if err := h.saveToken("old-access-123456", "refresh-1", 2*time.Minute, "http://token-endpoint.invalid/token"); err != nil {
		t.Fatal(err)
	}
	tok, err := h.tm.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	if tok != "fresh-through-proxy-000" {
		t.Fatalf("token = %q, want the refreshed value", tok)
	}
	if proxyHits.Load() != 1 {
		t.Fatalf("proxy requests = %d, want exactly 1 (refresh must not bypass the proxy)", proxyHits.Load())
	}
	if h.store["mcp/github/access-token"] != "fresh-through-proxy-000" {
		t.Fatalf("persisted access token = %q", h.store["mcp/github/access-token"])
	}
}
