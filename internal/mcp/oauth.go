// OAuth for remote MCP servers (#249): authorization-code with PKCE and
// dynamic client registration (RFC 7591) where the server offers it, per
// the MCP authorization spec. Tokens are returned as a TokenSet and stored
// by the caller (internal/secret) — they never appear in config or logs.
//
// Zero real network in tests: every function here takes an *http.Client
// and is driven against in-process test servers.
package mcp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// oauthWellKnownPath is the RFC 8414 discovery path relative to the MCP
// server's origin.
const oauthWellKnownPath = "/.well-known/oauth-authorization-server"

// OAuthMetadata is the RFC 8414 authorization-server metadata a remote MCP
// server advertises.
type OAuthMetadata struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	RegistrationEndpoint  string   `json:"registration_endpoint"`
	ScopesSupported       []string `json:"scopes_supported"`
}

// DiscoverOAuthMetadata fetches RFC 8414 metadata for an MCP server URL.
//
// Deny-by-default validation (#249): the authorization and token endpoints
// come from an untrusted discovery document, and the authorization code,
// PKCE verifier, and refresh token are later sent to them. Before any
// endpoint is returned (and therefore persisted or used), it must be on
// the MCP server's own origin, use https (plaintext http is accepted only
// for loopback/localhost development), and carry no embedded credentials.
// A metadata document advertising anything else is a load error, never a
// permissive fallback.
func DiscoverOAuthMetadata(ctx context.Context, serverURL string, client *http.Client) (*OAuthMetadata, error) {
	u, err := url.Parse(serverURL)
	if err != nil {
		return nil, fmt.Errorf("invalid MCP server URL %q: %w", serverURL, err)
	}
	if u.User != nil {
		return nil, fmt.Errorf("invalid MCP server URL %q: credentials must not be embedded in the URL", serverURL)
	}
	if !allowedOAuthScheme(u) {
		return nil, fmt.Errorf("invalid MCP server URL %q: OAuth discovery requires https (plaintext http is accepted only for loopback/localhost development)", serverURL)
	}
	origin := &url.URL{Scheme: u.Scheme, Host: u.Host}
	discovery := origin.String() + oauthWellKnownPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, discovery, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("OAuth metadata discovery at %s: %w", discovery, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("OAuth metadata discovery at %s: HTTP %d (server does not advertise RFC 8414 metadata)", discovery, resp.StatusCode)
	}
	var meta OAuthMetadata
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&meta); err != nil {
		return nil, fmt.Errorf("OAuth metadata discovery at %s: %w", discovery, err)
	}
	if meta.AuthorizationEndpoint == "" || meta.TokenEndpoint == "" {
		return nil, fmt.Errorf("OAuth metadata at %s is missing authorization_endpoint or token_endpoint", discovery)
	}
	// The endpoints below are where the code, verifier, and refresh token
	// travel; only validated endpoints may be persisted (TokenMeta) or
	// opened in a browser (BuildAuthorizationURL).
	if err := validateOAuthEndpoints(origin, &meta); err != nil {
		return nil, fmt.Errorf("OAuth metadata at %s: %w", discovery, err)
	}
	return &meta, nil
}

// validateOAuthEndpoints enforces the deny-by-default contract for the
// credential-bearing endpoints a server advertises. Each endpoint must use
// https (http only on the loopback device, for local development), carry
// no userinfo, and live on the MCP server's own origin — an attacker who
// can influence the discovery document must not be able to redirect the
// authorization code, PKCE verifier, or refresh token to a destination of
// their choosing (#249).
func validateOAuthEndpoints(origin *url.URL, meta *OAuthMetadata) error {
	for _, ep := range []struct{ name, raw string }{
		{"authorization_endpoint", meta.AuthorizationEndpoint},
		{"token_endpoint", meta.TokenEndpoint},
	} {
		if err := validateOAuthEndpoint(ep.name, ep.raw, origin); err != nil {
			return err
		}
	}
	return nil
}

func validateOAuthEndpoint(name, raw string, origin *url.URL) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s %q is not a valid URL: %w", name, raw, err)
	}
	if !allowedOAuthScheme(u) {
		return fmt.Errorf("%s %q must use https (plaintext http is accepted only for loopback/localhost development)", name, raw)
	}
	if u.User != nil {
		return fmt.Errorf("%s %q must not embed credentials", name, raw)
	}
	if !strings.EqualFold(u.Host, origin.Host) {
		return fmt.Errorf("%s %q is not on the MCP server's own origin %q (same-host endpoints only)", name, raw, origin.Host)
	}
	return nil
}

// allowedOAuthScheme reports whether a URL's scheme is acceptable for
// credential-bearing OAuth endpoints: https, or plaintext http only on the
// loopback device (local development).
func allowedOAuthScheme(u *url.URL) bool {
	return u.Scheme == "https" || (u.Scheme == "http" && isLoopbackHost(u.Hostname()))
}

// isLoopbackHost reports whether host is the loopback device: "localhost"
// or a loopback IP (127.0.0.0/8, ::1). DNS is not consulted: a hostname
// that happens to resolve to 127.0.0.1 is not treated as loopback, so the
// plaintext-http allowance stays explicit and local.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// RegistrationRequest is the RFC 7591 dynamic-client-registration payload
// waffle sends. PKCE keeps the public client honest: no client secret is
// requested.
type RegistrationRequest struct {
	ClientName              string   `json:"client_name"`
	RedirectURIs            []string `json:"redirect_uris"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
}

// RegistrationResponse is the RFC 7591 registration response.
type RegistrationResponse struct {
	ClientID                string   `json:"client_id"`
	ClientSecret            string   `json:"client_secret,omitempty"`
	RedirectURIs            []string `json:"redirect_uris,omitempty"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method,omitempty"`
}

// RegisterDynamicClient registers a public PKCE client at the server's
// registration endpoint (#249).
func RegisterDynamicClient(ctx context.Context, registrationEndpoint, redirectURI string, client *http.Client) (*RegistrationResponse, error) {
	payload, err := json.Marshal(RegistrationRequest{
		ClientName:              "waffle",
		RedirectURIs:            []string{redirectURI},
		GrantTypes:              []string{"authorization_code", "refresh_token"},
		ResponseTypes:           []string{"code"},
		TokenEndpointAuthMethod: "none",
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, registrationEndpoint, strings.NewReader(string(payload)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dynamic client registration at %s: %w", registrationEndpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("dynamic client registration at %s: HTTP %d: %s", registrationEndpoint, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out RegistrationResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("dynamic client registration at %s: %w", registrationEndpoint, err)
	}
	if out.ClientID == "" {
		return nil, fmt.Errorf("dynamic client registration at %s returned no client_id", registrationEndpoint)
	}
	return &out, nil
}

// TokenSet is one OAuth token response plus when it was obtained.
type TokenSet struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	TokenType    string    `json:"token_type"`
	Scope        string    `json:"scope,omitempty"`
	ExpiresIn    int       `json:"expires_in"`
	ObtainedAt   time.Time `json:"-"`
}

// Expiry returns when the access token expires.
func (t *TokenSet) Expiry() time.Time {
	return t.ObtainedAt.Add(time.Duration(t.ExpiresIn) * time.Second)
}

// PKCEPair is a code challenge/verifier pair (S256).
type PKCEPair struct {
	Verifier  string
	Challenge string
}

// NewPKCEPair generates a fresh RFC 7636 S256 pair.
func NewPKCEPair() (*PKCEPair, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("pkce: %w", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	return &PKCEPair{Verifier: verifier, Challenge: base64.RawURLEncoding.EncodeToString(sum[:])}, nil
}

// BuildAuthorizationURL assembles the authorization endpoint URL for the
// browser round trip.
func BuildAuthorizationURL(meta *OAuthMetadata, clientID, redirectURI, state string, pkce *PKCEPair, scopes []string) (string, error) {
	u, err := url.Parse(meta.AuthorizationEndpoint)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("state", state)
	q.Set("code_challenge", pkce.Challenge)
	q.Set("code_challenge_method", "S256")
	if len(scopes) > 0 {
		q.Set("scope", strings.Join(scopes, " "))
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// ExchangeAuthorizationCode trades the authorization code for a TokenSet at
// the token endpoint.
func ExchangeAuthorizationCode(ctx context.Context, tokenEndpoint, clientID, code, redirectURI, verifier string, client *http.Client) (*TokenSet, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("client_id", clientID)
	form.Set("code_verifier", verifier)
	return tokenRequest(ctx, tokenEndpoint, form, client)
}

// RefreshTokenSet refreshes an access token. A nil or empty refresh token
// is an error: there is nothing to refresh with.
func RefreshTokenSet(ctx context.Context, tokenEndpoint, clientID, refreshToken string, client *http.Client) (*TokenSet, error) {
	if refreshToken == "" {
		return nil, errors.New("no refresh token stored")
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("client_id", clientID)
	form.Set("refresh_token", refreshToken)
	return tokenRequest(ctx, tokenEndpoint, form, client)
}

func tokenRequest(ctx context.Context, tokenEndpoint string, form url.Values, client *http.Client) (*TokenSet, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token request: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var ts TokenSet
	if err := json.Unmarshal(raw, &ts); err != nil {
		return nil, fmt.Errorf("token request: %w", err)
	}
	if ts.AccessToken == "" {
		return nil, errors.New("token response contained no access_token")
	}
	if ts.ExpiresIn <= 0 {
		// Spec allows omission; treat as 1h so refresh-ahead still applies.
		ts.ExpiresIn = 3600
	}
	ts.ObtainedAt = time.Now().UTC()
	return &ts, nil
}
