package broker

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matt-riley/waffle/internal/store"
)

func TestProxyInjectsRealKeyAndStripsToken(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-api-key"); got != "sk-real-key" {
			t.Errorf("x-api-key = %q", got)
		}
		if auth := r.Header.Get("Authorization"); auth != "" {
			t.Errorf("wk_ token leaked upstream: %q", auth)
		}
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path = %s", r.URL.Path)
		}
		io.WriteString(w, `{"ok":true}`) //nolint:errcheck // test handler
	}))
	defer upstream.Close()

	st := openStore(t)
	b := New(st, []Upstream{{Name: "anthropic", BaseURL: upstream.URL, Header: "x-api-key", Value: "sk-real-key"}})
	front := httptest.NewServer(b)
	defer front.Close()

	token, err := b.Mint(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if !strings.HasPrefix(token, "wk_") {
		t.Fatalf("token = %q", token)
	}

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/anthropic/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck // test client
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	// Audit: one mint + one proxy row, no raw token stored.
	var count int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM broker_audit`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Errorf("audit rows = %d, want 2", count)
	}
	var prefix string
	if err := st.DB.QueryRow(`SELECT token_prefix FROM broker_audit WHERE action = 'proxy'`).Scan(&prefix); err != nil {
		t.Fatal(err)
	}
	if len(prefix) != 11 || prefix == token {
		t.Errorf("audit stored %q (len %d)", prefix, len(prefix))
	}
}

func TestUnauthorizedAndRevoked(t *testing.T) {
	b := New(nil, []Upstream{{Name: "x", BaseURL: "http://127.0.0.1:1", Header: "x-api-key", Value: "k"}})
	front := httptest.NewServer(b)
	defer front.Close()

	// No token.
	resp, err := http.Post(front.URL+"/x/v1/thing", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close() //nolint:errcheck // test client
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no token status = %d", resp.StatusCode)
	}

	// Revoked token.
	token, err := b.Mint(context.Background(), "sess")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	b.Revoke(token)
	req, _ := http.NewRequest(http.MethodPost, front.URL+"/x/v1/thing", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close() //nolint:errcheck // test client
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("revoked token status = %d", resp.StatusCode)
	}
}

func TestUnknownUpstream(t *testing.T) {
	b := New(nil, nil)
	front := httptest.NewServer(b)
	defer front.Close()

	token, err := b.Mint(context.Background(), "sess")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	req, _ := http.NewRequest(http.MethodPost, front.URL+"/nope/v1/x", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close() //nolint:errcheck // test client
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestMintReplacesExistingSessionToken(t *testing.T) {
	b := New(nil, nil)
	first, err := b.Mint(context.Background(), "sess")
	if err != nil {
		t.Fatalf("first Mint: %v", err)
	}
	second, err := b.Mint(context.Background(), "sess")
	if err != nil {
		t.Fatalf("second Mint: %v", err)
	}
	if first == second {
		t.Fatalf("Mint returned the same token twice: %q", first)
	}
	if got := b.session(first); got != "" {
		t.Fatalf("first token still resolves to session %q after replacement", got)
	}
	if got := b.session(second); got != "sess" {
		t.Fatalf("second token resolves to %q, want sess", got)
	}
}

func TestRevokeSessionInvalidatesAllSessionTokens(t *testing.T) {
	b := New(nil, nil)
	token, err := b.Mint(context.Background(), "sess")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	b.RevokeSession("sess")
	if got := b.session(token); got != "" {
		t.Fatalf("token still resolves to session %q after RevokeSession", got)
	}
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() }) //nolint:errcheck // test teardown
	return st
}
