// Token lifecycle for remote MCP servers (#249): OAuth tokens live in
// internal/secret (age-encrypted), never in config.toml, and are refreshed
// ahead of expiry. A failed refresh disables the server with an
// operator-facing error — no retry hot loop, and revoked credentials fail
// closed (the server's tools never register, or stop answering).
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/matt-riley/waffle/internal/secret"
)

// tokenNames returns the secret-store names for one server's OAuth state:
// mcp/<server>/access-token, /refresh-token, /token-meta. Names follow the
// store's lowercase path convention (config validation enforces the server
// name shape for url servers).
func tokenNames(server string) (access, refresh, meta string) {
	return "mcp/" + server + "/access-token",
		"mcp/" + server + "/refresh-token",
		"mcp/" + server + "/token-meta"
}

// TokenSecretName returns the secret-store name of server's access token —
// the same name the secret redactor uses when scrubbing transcript text.
func TokenSecretName(server string) string {
	name, _, _ := tokenNames(server)
	return name
}

// DefaultRefreshAhead is how early before expiry a refresh is triggered.
const DefaultRefreshAhead = 5 * time.Minute

// ErrNoToken is returned when a server has no stored OAuth token at all.
var ErrNoToken = errors.New("no OAuth token stored for this server")

// TokenMeta is the non-secret half of the token state (expiry, scope, and
// the registered client identity). It lives in the secret store file so
// the whole credential state is one encrypted unit; the values inside are
// not secrets.
type TokenMeta struct {
	ExpiresAt     time.Time `json:"expires_at"`
	TokenType     string    `json:"token_type"`
	Scope         string    `json:"scope"`
	TokenEndpoint string    `json:"token_endpoint"`
	ClientID      string    `json:"client_id"`
}

// TokenManager loads, refreshes, and persists one server's OAuth token
// state. It is the clock seam for refresh-ahead tests (Now) and the
// fail-closed gate: once disabled (failed refresh, rejected credentials)
// every call errors with the operator-facing reason.
type TokenManager struct {
	Store  secret.Store
	Server string // config server name; token names derive from it
	HTTP   *http.Client

	// Now is injectable for deterministic expiry tests.
	Now func() time.Time
	// RefreshAhead is how early before expiry a refresh starts. Zero uses
	// DefaultRefreshAhead.
	RefreshAhead time.Duration

	mu       sync.Mutex
	loaded   bool
	access   string
	refresh  string
	meta     TokenMeta
	disabled string
}

func (m *TokenManager) now() time.Time {
	if m.Now != nil {
		return m.Now().UTC()
	}
	return time.Now().UTC()
}

func (m *TokenManager) refreshAhead() time.Duration {
	if m.RefreshAhead > 0 {
		return m.RefreshAhead
	}
	return DefaultRefreshAhead
}

// Load reads the token state from the store. It is called lazily on first
// use; a missing state is ErrNoToken (the server simply has no credential).
func (m *TokenManager) Load() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.loadLocked()
}

func (m *TokenManager) loadLocked() error {
	if m.loaded {
		return nil
	}
	accessName, refreshName, metaName := tokenNames(m.Server)
	raw, err := m.Store.Get(metaName)
	if err != nil {
		return fmt.Errorf("%w (%s): run `waffle mcp login %s`", ErrNoToken, m.Server, m.Server)
	}
	if err := json.Unmarshal([]byte(raw), &m.meta); err != nil {
		return fmt.Errorf("mcp %s: corrupt token metadata in secret store: %w", m.Server, err)
	}
	m.access, err = m.Store.Get(accessName)
	if err != nil {
		return fmt.Errorf("mcp %s: access token missing from secret store: %w", m.Server, err)
	}
	m.refresh, _ = m.Store.Get(refreshName) // optional
	m.loaded = true
	return nil
}

// AccessToken returns a usable bearer token, refreshing ahead of expiry
// when needed. A failed refresh disables the manager (sticky) so the caller
// fails closed instead of retrying in a loop.
func (m *TokenManager) AccessToken(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.disabled != "" {
		return "", errors.New(m.disabled)
	}
	if err := m.loadLocked(); err != nil {
		return "", err
	}
	if m.now().Add(m.refreshAhead()).Before(m.meta.ExpiresAt) {
		return m.access, nil
	}
	return m.refreshLocked(ctx)
}

// Disable marks the manager failed-closed with an operator-facing reason.
func (m *TokenManager) Disable(reason string) {
	m.mu.Lock()
	if m.disabled == "" {
		m.disabled = reason
	}
	m.mu.Unlock()
}

// refreshLocked performs one refresh and persists the new state. Caller
// holds m.mu.
func (m *TokenManager) refreshLocked(ctx context.Context) (string, error) {
	if m.refresh == "" {
		m.disabled = fmt.Sprintf(
			"mcp %s: access token expired and no refresh token is stored; run `waffle mcp login %s` to re-authorize",
			m.Server, m.Server)
		return "", errors.New(m.disabled)
	}
	if m.meta.TokenEndpoint == "" {
		m.disabled = fmt.Sprintf(
			"mcp %s: no token endpoint recorded; run `waffle mcp login %s` to re-authorize",
			m.Server, m.Server)
		return "", errors.New(m.disabled)
	}
	fresh, err := RefreshTokenSet(ctx, m.meta.TokenEndpoint, m.meta.ClientID, m.refresh, m.HTTP)
	if err != nil {
		m.disabled = fmt.Sprintf(
			"mcp %s: token refresh failed: %v; run `waffle mcp login %s` to re-authorize",
			m.Server, err, m.Server)
		return "", errors.New(m.disabled)
	}
	m.access = fresh.AccessToken
	if fresh.RefreshToken != "" {
		m.refresh = fresh.RefreshToken
	}
	m.meta.ExpiresAt = fresh.Expiry()
	m.meta.TokenType = fresh.TokenType
	if fresh.Scope != "" {
		m.meta.Scope = fresh.Scope
	}
	if err := m.persistLocked(); err != nil {
		// Persistence failure is not a credential failure: keep serving the
		// refreshed token for this process, but surface the storage error.
		return m.access, fmt.Errorf("mcp %s: token refreshed but not persisted: %w", m.Server, err)
	}
	return m.access, nil
}

// persistLocked writes access token, refresh token, and metadata back to
// the store. Caller holds m.mu.
func (m *TokenManager) persistLocked() error {
	accessName, refreshName, metaName := tokenNames(m.Server)
	if err := m.Store.Set(accessName, m.access); err != nil {
		return err
	}
	if err := m.Store.Set(refreshName, m.refresh); err != nil {
		return err
	}
	raw, err := json.Marshal(m.meta)
	if err != nil {
		return err
	}
	return m.Store.Set(metaName, string(raw))
}

// Save persists a freshly-obtained TokenSet (from `waffle mcp login`) with
// its metadata. Access and refresh tokens go in as separate secrets so the
// existing redactor scrubs the access token value from transcript text.
func (m *TokenManager) Save(ts *TokenSet, meta TokenMeta) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ts.ExpiresIn == 0 {
		// Spec allows the server to omit expires_in; treat as 1h so
		// refresh-ahead still applies. A negative value is preserved: it
		// records an already-expired token (fail closed on first use).
		ts.ExpiresIn = 3600
	}
	ts.ObtainedAt = m.now()
	m.access = ts.AccessToken
	m.refresh = ts.RefreshToken
	meta.ExpiresAt = ts.Expiry()
	if meta.TokenType == "" {
		meta.TokenType = ts.TokenType
	}
	if meta.Scope == "" {
		meta.Scope = ts.Scope
	}
	m.meta = meta
	m.loaded = true
	m.disabled = ""
	return m.persistLocked()
}

// Status describes one server's token state for `waffle mcp status`.
type Status struct {
	Server    string
	HasToken  bool
	ExpiresAt time.Time
	Scope     string
	Err       string
}

// Status reports the stored token state without touching the network.
func (m *TokenManager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := Status{Server: m.Server}
	if err := m.loadLocked(); err != nil {
		if !errors.Is(err, ErrNoToken) {
			st.Err = err.Error()
		}
		return st
	}
	st.HasToken = true
	st.ExpiresAt = m.meta.ExpiresAt
	st.Scope = m.meta.Scope
	return st
}

// Clear deletes every stored token for the server (`waffle mcp logout`).
func (m *TokenManager) Clear() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	accessName, refreshName, metaName := tokenNames(m.Server)
	var errs []error
	for _, name := range []string{accessName, refreshName, metaName} {
		if err := m.Store.Delete(name); err != nil && !errors.Is(err, secret.ErrNotFound) {
			errs = append(errs, fmt.Errorf("delete %s: %w", name, err))
		}
	}
	m.loaded = false
	m.disabled = ""
	return errors.Join(errs...)
}
