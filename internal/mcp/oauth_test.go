package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// fakeAuthServer is an in-process RFC 8414 + RFC 7591 + token endpoint
// that captures what waffle sends, so the OAuth flow is proven without any
// real network.
type fakeAuthServer struct {
	authorizationCode string
	verifier          string

	registrationBody  string
	exchangeBody      string
	refreshBody       string
	registrationCount int
	exchangeCount     int
	refreshCount      int
	meta              *OAuthMetadata
}

func newFakeAuthServer(t *testing.T) (*fakeAuthServer, *httptest.Server) {
	f := &fakeAuthServer{}
	mux := http.NewServeMux()
	var wellKnown *OAuthMetadata
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(wellKnown)
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		f.registrationCount++
		body := make([]byte, 4<<10)
		n, _ := r.Body.Read(body)
		f.registrationBody = string(body[:n])
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(RegistrationResponse{
			ClientID: "dyn-client-1", RedirectURIs: []string{"http://127.0.0.1:1/callback"},
		})
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		f.authorizationCode = q.Get("code")
		f.verifier = q.Get("verifier") // not a real server; test checks challenge instead
		http.Redirect(w, r, q.Get("redirect_uri")+"?code=auth-code-1&state="+q.Get("state"), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			f.exchangeCount++
			f.exchangeBody = r.Form.Encode()
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"access_token":"at-code-%s","refresh_token":"rt-1","token_type":"bearer","expires_in":3600,"scope":"read"}`, r.Form.Get("code"))
		case "refresh_token":
			f.refreshCount++
			f.refreshBody = r.Form.Encode()
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"access_token":"at-refresh-%s","refresh_token":"rt-2","token_type":"bearer","expires_in":3600,"scope":"read"}`, r.Form.Get("refresh_token"))
		default:
			http.Error(w, "unknown grant", http.StatusBadRequest)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	// The fake server answers at its own origin; the metadata advertises
	// those same endpoints, so discovery exercises the real request path.
	f.meta = &OAuthMetadata{
		Issuer:                srv.URL,
		AuthorizationEndpoint: srv.URL + "/authorize",
		TokenEndpoint:         srv.URL + "/token",
		RegistrationEndpoint:  srv.URL + "/register",
		ScopesSupported:       []string{"read", "write"},
	}
	wellKnown = f.meta
	return f, srv
}

// TestOAuthFlowCompletesDiscoveryRegistrationAndExchange covers the full flow: RFC 8414
// discovery, dynamic client registration (RFC 7591), PKCE challenge/verifier
// pair, authorization URL, and the code exchange.
func TestOAuthFlowCompletesDiscoveryRegistrationAndExchange(t *testing.T) {
	f, srv := newFakeAuthServer(t)
	client := srv.Client()

	meta, err := DiscoverOAuthMetadata(context.Background(), srv.URL, client)
	if err != nil {
		t.Fatalf("discovery: %v", err)
	}
	if meta.AuthorizationEndpoint != srv.URL+"/authorize" {
		t.Fatalf("authorization endpoint = %q", meta.AuthorizationEndpoint)
	}

	reg, err := RegisterDynamicClient(context.Background(), meta.RegistrationEndpoint, "http://127.0.0.1:9999/callback", client)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if reg.ClientID != "dyn-client-1" {
		t.Fatalf("client id = %q", reg.ClientID)
	}
	if !strings.Contains(f.registrationBody, `"token_endpoint_auth_method":"none"`) {
		t.Fatalf("registration did not request public-client PKCE auth: %s", f.registrationBody)
	}
	if !strings.Contains(f.registrationBody, `"grant_types"`) {
		t.Fatalf("registration missing grant types: %s", f.registrationBody)
	}

	pkce, err := NewPKCEPair()
	if err != nil {
		t.Fatal(err)
	}
	if len(pkce.Verifier) < 43 {
		t.Fatalf("PKCE verifier length = %d, want >= 43", len(pkce.Verifier))
	}
	authURL, err := BuildAuthorizationURL(meta, reg.ClientID, "http://127.0.0.1:9999/callback", "state-1", pkce, []string{"read"})
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" {
		t.Fatalf("authorization URL lacks PKCE S256 challenge: %s", authURL)
	}
	if q.Get("response_type") != "code" || q.Get("state") != "state-1" || q.Get("scope") != "read" {
		t.Fatalf("authorization URL query = %v", q)
	}

	ts, err := ExchangeAuthorizationCode(context.Background(), meta.TokenEndpoint, reg.ClientID, "auth-code-1", "http://127.0.0.1:9999/callback", pkce.Verifier, client)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if ts.AccessToken != "at-code-auth-code-1" {
		t.Fatalf("access token = %q", ts.AccessToken)
	}
	if !strings.Contains(f.exchangeBody, "code_verifier="+url.QueryEscape(pkce.Verifier)) {
		t.Fatalf("exchange did not send the code_verifier: %s", f.exchangeBody)
	}

	fresh, err := RefreshTokenSet(context.Background(), meta.TokenEndpoint, reg.ClientID, ts.RefreshToken, client)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if fresh.AccessToken != "at-refresh-rt-1" {
		t.Fatalf("refreshed token = %q", fresh.AccessToken)
	}
	if !strings.Contains(f.refreshBody, "refresh_token=rt-1") {
		t.Fatalf("refresh body = %s", f.refreshBody)
	}
	if ts.Expiry().Before(ts.ObtainedAt) {
		t.Fatal("expiry before obtain time")
	}
}

// TestOAuthDiscoveryRefusesMetadataWithoutEndpoints: metadata without the required
// endpoints is a load error, not a permissive fallback.
func TestOAuthDiscoveryRefusesMetadataWithoutEndpoints(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"issuer":"x"}`)
	}))
	defer srv.Close()
	_, err := DiscoverOAuthMetadata(context.Background(), srv.URL, srv.Client())
	if err == nil {
		t.Fatal("discovery succeeded without endpoints")
	}
	if !strings.Contains(err.Error(), "authorization_endpoint") {
		t.Fatalf("error %q", err)
	}
}

// TestOAuthDiscoverySurfacesHTTPErrorWhenMetadataAbsent: a server without RFC 8414 metadata
// answers non-200 and the flow stops with a clear error.
func TestOAuthDiscoverySurfacesHTTPErrorWhenMetadataAbsent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	_, err := DiscoverOAuthMetadata(context.Background(), srv.URL, srv.Client())
	if err == nil {
		t.Fatal("discovery succeeded on 404")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("error %q", err)
	}
}

// TestTokenRequestSurfacesServerErrorBody: a rejected grant surfaces the
// server's error without leaking anything.
func TestTokenRequestSurfacesServerErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"code expired"}`)
	}))
	defer srv.Close()
	_, err := ExchangeAuthorizationCode(context.Background(), srv.URL, "c", "code", "http://x/cb", "verifier", srv.Client())
	if err == nil {
		t.Fatal("exchange succeeded on rejected grant")
	}
	if !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("error %q", err)
	}
}

// TestRefreshWithoutStoredTokenErrors: refreshing with nothing stored
// is an error, never a silent re-auth attempt.
func TestRefreshWithoutStoredTokenErrors(t *testing.T) {
	_, err := RefreshTokenSet(context.Background(), "http://token.test", "c", "", http.DefaultClient)
	if err == nil {
		t.Fatal("refresh with empty token succeeded")
	}
}

// TestOAuthEndpointsValidatedBeforeUse is the deny-by-default contract for
// advertised credential destinations (#249): a metadata document may only
// point the authorization code, PKCE verifier, and refresh token at the
// MCP server's own origin over https (plaintext http accepted only on the
// loopback device), with no embedded credentials. Anything else is a load
// error.
func TestOAuthEndpointsValidatedBeforeUse(t *testing.T) {
	origin := &url.URL{Scheme: "https", Host: "mcp.example.com"}
	loopback := &url.URL{Scheme: "http", Host: "127.0.0.1:8080"}
	tests := []struct {
		name    string
		origin  *url.URL
		meta    OAuthMetadata
		wantErr string
	}{
		{
			name:   "https same-origin accepted",
			origin: origin,
			meta: OAuthMetadata{
				AuthorizationEndpoint: "https://mcp.example.com/authorize",
				TokenEndpoint:         "https://mcp.example.com/token",
			},
		},
		{
			name:   "same-origin registration endpoint accepted",
			origin: origin,
			meta: OAuthMetadata{
				AuthorizationEndpoint: "https://mcp.example.com/authorize",
				TokenEndpoint:         "https://mcp.example.com/token",
				RegistrationEndpoint:  "https://mcp.example.com/register",
			},
		},
		{
			name:   "empty registration endpoint accepted",
			origin: origin,
			meta: OAuthMetadata{
				AuthorizationEndpoint: "https://mcp.example.com/authorize",
				TokenEndpoint:         "https://mcp.example.com/token",
				// RegistrationEndpoint omitted: servers without dynamic
				// registration must still pass validation.
			},
		},
		{
			name:   "cross-origin registration endpoint rejected",
			origin: origin,
			meta: OAuthMetadata{
				AuthorizationEndpoint: "https://mcp.example.com/authorize",
				TokenEndpoint:         "https://mcp.example.com/token",
				RegistrationEndpoint:  "https://attacker.example.net/register",
			},
			wantErr: `registration_endpoint "https://attacker.example.net/register" is not on the MCP server's own origin "mcp.example.com"`,
		},
		{
			name:   "plaintext registration endpoint rejected",
			origin: origin,
			meta: OAuthMetadata{
				AuthorizationEndpoint: "https://mcp.example.com/authorize",
				TokenEndpoint:         "https://mcp.example.com/token",
				RegistrationEndpoint:  "http://mcp.example.com/register",
			},
			wantErr: `registration_endpoint "http://mcp.example.com/register" must use https`,
		},
		{
			name:   "userinfo in registration endpoint rejected",
			origin: origin,
			meta: OAuthMetadata{
				AuthorizationEndpoint: "https://mcp.example.com/authorize",
				TokenEndpoint:         "https://mcp.example.com/token",
				RegistrationEndpoint:  "https://attacker:secret@mcp.example.com/register",
			},
			wantErr: `registration_endpoint "https://attacker:secret@mcp.example.com/register" must not embed credentials`,
		},
		{
			name:   "loopback http accepted",
			origin: loopback,
			meta: OAuthMetadata{
				AuthorizationEndpoint: "http://127.0.0.1:8080/authorize",
				TokenEndpoint:         "http://127.0.0.1:8080/token",
			},
		},
		{
			name:   "localhost loopback http accepted",
			origin: &url.URL{Scheme: "http", Host: "localhost:9000"},
			meta: OAuthMetadata{
				AuthorizationEndpoint: "http://localhost:9000/authorize",
				TokenEndpoint:         "http://localhost:9000/token",
			},
		},
		{
			name:   "plaintext token endpoint rejected",
			origin: origin,
			meta: OAuthMetadata{
				AuthorizationEndpoint: "https://mcp.example.com/authorize",
				TokenEndpoint:         "http://mcp.example.com/token",
			},
			wantErr: `token_endpoint "http://mcp.example.com/token" must use https`,
		},
		{
			name:   "cross-origin token endpoint rejected",
			origin: origin,
			meta: OAuthMetadata{
				AuthorizationEndpoint: "https://mcp.example.com/authorize",
				TokenEndpoint:         "https://attacker.example.net/token",
			},
			wantErr: `token_endpoint "https://attacker.example.net/token" is not on the MCP server's own origin "mcp.example.com"`,
		},
		{
			name:   "cross-origin authorization endpoint rejected",
			origin: origin,
			meta: OAuthMetadata{
				AuthorizationEndpoint: "https://auth.other.example/authorize",
				TokenEndpoint:         "https://mcp.example.com/token",
			},
			wantErr: `authorization_endpoint "https://auth.other.example/authorize" is not on the MCP server's own origin`,
		},
		{
			name:   "userinfo in token endpoint rejected",
			origin: origin,
			meta: OAuthMetadata{
				AuthorizationEndpoint: "https://mcp.example.com/authorize",
				TokenEndpoint:         "https://attacker:secret@mcp.example.com/token",
			},
			wantErr: `token_endpoint "https://attacker:secret@mcp.example.com/token" must not embed credentials`,
		},
		{
			name:   "port-mismatched endpoint rejected",
			origin: origin,
			meta: OAuthMetadata{
				AuthorizationEndpoint: "https://mcp.example.com/authorize",
				TokenEndpoint:         "https://mcp.example.com:8443/token",
			},
			wantErr: `token_endpoint "https://mcp.example.com:8443/token" is not on the MCP server's own origin "mcp.example.com"`,
		},
		{
			name:   "non-loopback http origin rejected",
			origin: &url.URL{Scheme: "http", Host: "mcp.example.com"},
			meta: OAuthMetadata{
				AuthorizationEndpoint: "http://mcp.example.com/authorize",
				TokenEndpoint:         "http://mcp.example.com/token",
			},
			wantErr: `authorization_endpoint "http://mcp.example.com/authorize" must use https`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateOAuthEndpoints(tc.origin, &tc.meta)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validateOAuthEndpoints: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateOAuthEndpoints succeeded, want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestOAuthDiscoveryRefusesCrossOriginTokenEndpoint: discovery itself
// rejects a metadata document that advertises a foreign token endpoint,
// so a poisoned document is never persisted for later refresh.
func TestOAuthDiscoveryRefusesCrossOriginTokenEndpoint(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, fmt.Sprintf(`{"issuer":"x","authorization_endpoint":%q,"token_endpoint":"https://attacker.example.net/token"}`, srv.URL+"/authorize"))
	}))
	defer srv.Close()
	_, err := DiscoverOAuthMetadata(context.Background(), srv.URL, srv.Client())
	if err == nil {
		t.Fatal("discovery succeeded with a cross-origin token endpoint")
	}
	if !strings.Contains(err.Error(), "attacker.example.net") || !strings.Contains(err.Error(), "own origin") {
		t.Fatalf("error %q", err)
	}
}

// TestOAuthDiscoveryRefusesPlaintextServerURL: discovery against a
// non-loopback plaintext server URL is refused outright; the discovery
// document itself is trust input.
func TestOAuthDiscoveryRefusesPlaintextServerURL(t *testing.T) {
	_, err := DiscoverOAuthMetadata(context.Background(), "http://mcp.example.com", http.DefaultClient)
	if err == nil {
		t.Fatal("discovery over plaintext succeeded")
	}
	if !strings.Contains(err.Error(), "https") {
		t.Fatalf("error %q", err)
	}
}
