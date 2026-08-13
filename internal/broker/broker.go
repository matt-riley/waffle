// Package broker is the host-side credential broker (docs/plan.md,
// "Secret management"). One rule: raw keys exist only here. Sandboxes hold
// wk_ session tokens (DefaultTokenTTL) and talk to the broker, which injects
// the real credential upstream. Phase 4 ships the LLM face (Anthropic and
// OpenAI-compatible passthrough); the git and egress faces arrive with
// repo workspaces (phase 5); credentialed API faces (#254) generalize the
// LLM face to any third-party API under /api/<name>/<path>.
package broker

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/matt-riley/waffle/internal/id"
	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/store"
	"github.com/matt-riley/waffle/internal/usage"
)

// DefaultTokenTTL is how long a minted wk_ session token authorizes broker
// requests on the upstream-proxy and git-credential faces. After expiry the
// token is rejected and its map entries are swept; long-running work renews
// via Mint/MintScoped (workspace resume already replaces the token).
const DefaultTokenTTL = 24 * time.Hour

// tokenEntry is one minted wk_ token and its lifetime.
type tokenEntry struct {
	sessionID string
	expiresAt time.Time
}

// Upstream is one provider the broker can front.
type Upstream struct {
	// Name routes requests: the broker serves /<name>/<upstream path>.
	Name string
	// Kind is the provider type the upstream speaks ("anthropic" or
	// "openai"; the latter covers any OpenAI-compatible endpoint). It
	// selects the cost model used when budget binding prices the cache
	// tokens of metered traffic; empty means unknown and prices as
	// Anthropic, the legacy default (#247).
	Kind string
	// BaseURL of the real provider (e.g. https://api.anthropic.com).
	BaseURL string
	// Header is the auth header to inject ("x-api-key" or "Authorization").
	Header string
	// Value is the real credential (for Authorization: pass "Bearer ...").
	Value string
}

// APIFace is one named credentialed API face the broker serves at
// /api/<name>/<path> (#254). It generalises Upstream for third-party APIs:
// the same host-side header injection, but scoped by an explicit method
// allowlist and path-prefix allowlist, deny-by-default per session, and
// fully audited. Value is the real credential — it exists only inside the
// broker process and never leaves the host.
type APIFace struct {
	// Name routes the face: the broker serves /api/<name>/<path>.
	Name string
	// BaseURL of the real API (e.g. https://api.example.com).
	BaseURL string
	// Header is the auth header to inject ("x-api-key" or "Authorization").
	Header string
	// Value is the real credential (for Authorization: pass "Bearer ...").
	Value string
	// Methods is the explicit method allowlist (upper-case).
	Methods []string
	// Paths is the explicit path-prefix allowlist.
	Paths []string
}

// EgressTarget is an allowlisted HTTP(S) destination. Value is injected only
// for this host and is never written to audit logs.
type EgressTarget struct {
	Host    string
	BaseURL string
	Header  string
	Value   string
}

// GitCredentialFunc returns a git credential for host/path on behalf of a
// session. First iteration: a stored fine-grained PAT; a GitHub App
// minting short-lived installation tokens slots in behind the same
// signature (docs/plan.md, "Secret management").
type GitCredentialFunc func(ctx context.Context, sessionID, host, path string) (username, password string, err error)

// apiFace is one configured face plus its ready-to-serve proxy.
type apiFace struct {
	face    APIFace
	base    *url.URL
	methods map[string]bool
	paths   []string
	proxy   *httputil.ReverseProxy
}

// Broker mints session tokens and proxies authenticated requests.
type Broker struct {
	audit     *sql.DB
	upstreams map[string]*httputil.ReverseProxy
	egress    map[string]*EgressTarget
	faces     map[string]*apiFace

	// Redact, when set, scrubs credential values from audit rows, error
	// text, and proxied response bodies before they leave the broker.
	// Wire it to a secret-store redactor (internal/secret) so a response
	// body that echoes the credential is redacted before reaching the
	// caller, and a credential-shaped path cannot land in an audit row.
	Redact func(string) string
	// RedactOverlap is the tail bytes retained while scrubbing a streaming
	// response body. A credential longer than this could straddle a flush
	// boundary unseen, so set it to the longest enrolled secret value
	// (secret.Redactor.MaxLen). Zero uses redactOverlapDefault.
	RedactOverlap int

	// GitCredential, when set, enables the /git-credential face used by
	// `waffle git-credential` inside workspace containers.
	GitCredential GitCredentialFunc
	// GitBackend is audit metadata, never a secret. Typical values are pat or github-app.
	GitBackend string
	// DialEgress dials a CONNECT tunnel target. Nil means safeDialContext,
	// which resolves the name and refuses private addresses; leave it nil
	// outside tests, where an override is the only way to reach a loopback
	// origin without disabling that refusal for everyone.
	DialEgress func(ctx context.Context, network, address string) (net.Conn, error)

	mu         sync.Mutex
	tokens     map[string]tokenEntry // token → session + expiry
	sessions   map[string]string     // session id → current token
	gitScope   map[string]string     // session id → bound repo (owner/name)
	limits     map[string]usage.Limits
	budgets    map[string]string
	kinds      map[string]string          // upstream name → provider type ("anthropic"/"openai")
	faceGrants map[string]map[string]bool // session id → granted face names
	// tunnelLive is the reserved-but-unpersisted tunnelled relay bytes per
	// budget key (#244). A CONNECT reserves its allowance here so concurrent
	// tunnels share one cap instead of each snapshotting the full remaining
	// budget; the reservation is released when the tunnel's bytes are
	// persisted. Entries are deleted once they drain to zero.
	tunnelLive map[string]*atomic.Int64
	Usage      *usage.Store
	Limits     usage.Limits
	// TokenTTL is the lifetime of minted wk_ tokens. Zero means DefaultTokenTTL.
	TokenTTL time.Duration
	// Now is injectable for deterministic budget-boundary and TTL tests.
	Now func() time.Time
}

func (b *Broker) now() time.Time {
	if b.Now != nil {
		return b.Now().UTC()
	}
	return time.Now().UTC()
}

func (b *Broker) tokenTTL() time.Duration {
	if b.TokenTTL > 0 {
		return b.TokenTTL
	}
	return DefaultTokenTTL
}

// New builds a broker over the given upstreams; st may be nil to skip
// audit persistence (tests).
func New(st *store.Store, upstreams []Upstream) *Broker {
	b := &Broker{
		upstreams:  map[string]*httputil.ReverseProxy{},
		egress:     map[string]*EgressTarget{},
		faces:      map[string]*apiFace{},
		tokens:     map[string]tokenEntry{},
		sessions:   map[string]string{},
		gitScope:   map[string]string{},
		limits:     map[string]usage.Limits{},
		budgets:    map[string]string{},
		kinds:      map[string]string{},
		faceGrants: map[string]map[string]bool{},
		tunnelLive: map[string]*atomic.Int64{},
	}

	if st != nil {
		b.audit = st.DB
	}
	for _, u := range upstreams {
		target, err := url.Parse(u.BaseURL)
		if err != nil {
			continue
		}
		header, value := u.Header, u.Value
		proxy := &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				trimDuplicateBasePath(pr.Out.URL, target)
				pr.SetURL(target)
				pr.Out.Host = target.Host
				// Strip the sandbox's token, inject the real key. This is
				// the only place a raw credential touches a request.
				pr.Out.Header.Del("Authorization")
				pr.Out.Header.Del("X-Api-Key")
				if header != "" {
					pr.Out.Header.Set(header, value)
				}
			},
		}
		b.upstreams[u.Name] = proxy
		b.kinds[u.Name] = u.Kind
	}
	return b
}

// SetEgress configures the broker's HTTP egress face. Hosts are matched
// exactly (callers should list each required host explicitly).
func (b *Broker) SetEgress(targets []EgressTarget) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, target := range targets {
		host := strings.ToLower(strings.TrimSpace(target.Host))
		if host == "" || target.BaseURL == "" {
			continue
		}
		t := target
		b.egress[host] = &t
	}
}

// SetAPIFaces configures the broker's credentialed API faces (#254), served
// at /api/<name>/<path>. Faces are deny-by-default: a session token carries
// grants minted with the token (MintScopedFaces), and a request for an
// un-granted face is refused and audited. Malformed entries (bad base URL,
// empty allowlists) are skipped, mirroring SetEgress; config.Load already
// rejects them before serve starts.
func (b *Broker) SetAPIFaces(faces []APIFace) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.faces = make(map[string]*apiFace, len(faces))
	for _, f := range faces {
		base, err := url.Parse(f.BaseURL)
		if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
			continue
		}
		if len(f.Methods) == 0 || len(f.Paths) == 0 {
			continue
		}
		face := &apiFace{
			face:    f,
			base:    base,
			methods: make(map[string]bool, len(f.Methods)),
			paths:   append([]string(nil), f.Paths...),
		}
		for _, m := range f.Methods {
			face.methods[strings.ToUpper(m)] = true
		}
		face.proxy = newAPIFaceProxy(face, b.redact)
		b.faces[f.Name] = face
	}
}

// newAPIFaceProxy builds the face's reverse proxy. The credential is
// injected here, host-side, exactly like Upstream — this is the only place a
// face credential touches a request. The transport never follows redirects:
// net/http's RoundTripper contract returns 3xx responses without following
// them, and the face transport keeps that property explicit so a redirect
// can never carry the credential header to a host outside base_url.
func newAPIFaceProxy(face *apiFace, redact func(string) string) *httputil.ReverseProxy {
	header, value := face.face.Header, face.face.Value
	proxy := &httputil.ReverseProxy{
		Transport: &faceTransport{},
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(face.base)
			pr.Out.Host = face.base.Host
			// Strip the sandbox's token and any caller-supplied auth;
			// inject the real credential.
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("Proxy-Authorization")
			pr.Out.Header.Del("X-Api-Key")
			if header != "" {
				pr.Out.Header.Set(header, value)
			}
		},
	}
	// Proxy transport errors can include the request URL; scrub them so a
	// credential-shaped path cannot reach the logs.
	proxy.ErrorLog = redactingLog(redact)
	return proxy
}

// faceHTTPTransport is the shared RoundTripper for face proxies. Like the
// provider upstreams it dials whatever the operator's base_url names
// (faces are host config, not untrusted input), but it never follows
// redirects — see faceTransport.
var faceHTTPTransport = func() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	return t
}()

// faceTransport is the RoundTripper for face proxies. net/http transports
// never follow redirects (that is http.Client behavior), so a 3xx response
// is returned to the caller un-followed and the credential header never
// travels to the redirect target. The type exists to pin that invariant in
// one place: a face can never be used to reach a host outside its base_url,
// including via redirect (#254).
type faceTransport struct{}

func (faceTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return faceHTTPTransport.RoundTrip(r)
}

// Mint issues a wk_ session token bound to sessionID.
func (b *Broker) Mint(ctx context.Context, sessionID string) (string, error) {
	return b.mint(ctx, sessionID, "", usage.Limits{}, false, nil)
}

// MintScoped issues a token with a group-specific limit and stable accounting
// identity. The concrete session remains the authorization/audit identity.
func (b *Broker) MintScoped(ctx context.Context, sessionID, budgetKey string, limits usage.Limits) (string, error) {
	return b.mint(ctx, sessionID, budgetKey, limits, true, nil)
}

// MintScopedFaces issues a token like MintScoped but also grants the named
// API faces (#254). Faces are deny-by-default: a token minted without a
// grant (Mint, MintScoped) can call no face, and an unconfigured face name
// in faces is silently dropped, never granted. Grants are bound to the
// session, so a token cannot be re-scoped after minting.
func (b *Broker) MintScopedFaces(ctx context.Context, sessionID, budgetKey string, limits usage.Limits, faces []string) (string, error) {
	return b.mint(ctx, sessionID, budgetKey, limits, true, faces)
}

func (b *Broker) mint(ctx context.Context, sessionID, budgetKey string, limits usage.Limits, scoped bool, faces []string) (string, error) {
	raw, err := id.NewBytes(16)
	if err != nil {
		return "", fmt.Errorf("mint broker token: %w", err)
	}
	token := "wk_" + raw
	now := b.now()
	entry := tokenEntry{
		sessionID: sessionID,
		expiresAt: now.Add(b.tokenTTL()),
	}
	b.mu.Lock()
	if old := b.sessions[sessionID]; old != "" {
		delete(b.tokens, old)
	}
	b.tokens[token] = entry
	b.sessions[sessionID] = token
	if scoped {
		b.limits[sessionID] = limits
		if budgetKey == "" {
			budgetKey = sessionID
		}
		b.budgets[sessionID] = budgetKey
	} else {
		delete(b.limits, sessionID)
		delete(b.budgets, sessionID)
	}
	b.faceGrants[sessionID] = configuredFaceSet(b.faces, faces)
	b.mu.Unlock()
	b.record(ctx, token, sessionID, "mint", "")
	return token, nil
}

// configuredFaceSet keeps only the granted names that name a configured
// face. An unknown name is never granted, deny-by-default. Caller holds no
// lock; b.faces is read-only after SetAPIFaces.
func configuredFaceSet(faces map[string]*apiFace, grants []string) map[string]bool {
	set := make(map[string]bool, len(grants))
	for _, name := range grants {
		if faces[name] != nil {
			set[name] = true
		}
	}
	return set
}

// Revoke invalidates a token (session ended).
func (b *Broker) Revoke(token string) {
	b.mu.Lock()
	if entry, ok := b.tokens[token]; ok && b.sessions[entry.sessionID] == token {
		b.clearSessionMapsLocked(entry.sessionID)
	}
	delete(b.tokens, token)
	b.mu.Unlock()
}

// RevokeSession invalidates the current token for sessionID, if any.
func (b *Broker) RevokeSession(sessionID string) {
	b.mu.Lock()
	if token := b.sessions[sessionID]; token != "" {
		delete(b.tokens, token)
	}
	b.clearSessionMapsLocked(sessionID)
	b.mu.Unlock()
}

// clearSessionMapsLocked drops session-keyed state. Caller holds b.mu.
func (b *Broker) clearSessionMapsLocked(sessionID string) {
	delete(b.sessions, sessionID)
	delete(b.gitScope, sessionID)
	delete(b.limits, sessionID)
	delete(b.budgets, sessionID)
	delete(b.faceGrants, sessionID)
}

// dropTokenLocked removes a token and, when it is still the session's current
// token, the session's git/limit/budget maps. Caller holds b.mu.
func (b *Broker) dropTokenLocked(token string, sessionID string) {
	delete(b.tokens, token)
	if b.sessions[sessionID] == token {
		b.clearSessionMapsLocked(sessionID)
	}
}

// sweepExpiredLocked removes every token whose expiresAt is not after now.
// Caller holds b.mu.
func (b *Broker) sweepExpiredLocked() {
	now := b.now()
	for token, entry := range b.tokens {
		if entry.expiresAt.After(now) {
			continue
		}
		b.dropTokenLocked(token, entry.sessionID)
	}
}

// BindGitRepo records the repo a session is entitled to, so the git-credential
// face can refuse any other repo. Set at workspace-open time — before the
// initial clone runs — because the durable workspaces row is only written
// after the clone succeeds; without this the first credential request during
// `git clone` would find no binding and be refused. Survives a token re-mint
// (resume); cleared when the session is revoked.
func (b *Broker) BindGitRepo(sessionID, repo string) {
	b.mu.Lock()
	b.gitScope[sessionID] = repo
	b.mu.Unlock()
}

// GitRepoScope returns the repo bound to sessionID, and whether one is set.
func (b *Broker) GitRepoScope(sessionID string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	repo, ok := b.gitScope[sessionID]
	return repo, ok
}

// session resolves a valid (non-expired) bearer token to its session, or "".
// Expired tokens are swept and treated as absent.
func (b *Broker) session(token string) string {
	sessionID, _, _, expired := b.usageScope(token)
	if expired {
		return ""
	}
	return sessionID
}

// usageScope resolves a token. On success expired is false and sessionID is
// set. When the token existed but its TTL elapsed, expired is true, sessionID
// is the former session (for audit only — do not authorize), and maps are
// swept. Unknown tokens return empty sessionID and expired false.
func (b *Broker) usageScope(token string) (sessionID, budgetKey string, limits usage.Limits, expired bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	entry, ok := b.tokens[token]
	if !ok {
		b.sweepExpiredLocked()
		return "", "", usage.Limits{}, false
	}
	now := b.now()
	if !entry.expiresAt.After(now) {
		former := entry.sessionID
		b.dropTokenLocked(token, former)
		b.sweepExpiredLocked()
		return former, "", usage.Limits{}, true
	}
	sessionID = entry.sessionID
	budgetKey = sessionID
	limits = b.Limits
	if scoped, ok := b.limits[sessionID]; ok {
		limits = scoped
	}
	if key := b.budgets[sessionID]; key != "" {
		budgetKey = key
	}
	b.sweepExpiredLocked()
	return sessionID, budgetKey, limits, false
}

// ServeHTTP implements the broker's HTTP face: /<upstream>/<path>, bearer
// wk_ token required.
func (b *Broker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Proxy clients (CONNECT or absolute-form URI) authenticate with the
	// proxy credential. On such a request the Authorization header belongs
	// to the origin (e.g. an MCP OAuth bearer for the remote server) and
	// must not be mistaken for the broker session token — an MCP client
	// legitimately sends both, and preferring Authorization would refuse
	// every proxied request it makes (#249). API-face requests keep
	// Authorization.
	token := ""
	if proxyStyleRequest(r) {
		token = proxyCredential(r)
	} else {
		token = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	sessionID := ""
	budgetKey := ""
	limits := b.Limits
	if strings.HasPrefix(token, "wk_") {
		var expired bool
		sessionID, budgetKey, limits, expired = b.usageScope(token)
		if expired {
			// Distinguish expired from unknown in broker_audit (action=expired).
			b.record(r.Context(), token, sessionID, "expired", auditDetail(r))
			denyUnauthenticated(w, r)
			return
		}
	}
	if sessionID == "" {
		b.record(r.Context(), token, "", "denied", auditDetail(r))
		denyUnauthenticated(w, r)
		return
	}
	requestAt := b.now()
	if b.Usage != nil {
		if paused, err := b.Usage.Paused(r.Context()); err != nil || paused {
			http.Error(w, "waffle is paused", http.StatusTooManyRequests)
			return
		}
		if err := b.Usage.Check(r.Context(), budgetKey, limits, requestAt); err != nil {
			http.Error(w, err.Error(), http.StatusTooManyRequests)
			return
		}
	}

	// HTTPS cannot be forward-proxied by rewriting an absolute URL: the client
	// asks for a tunnel and then speaks TLS to the origin through it. Without
	// this, every https:// fetch from a workspace fails -- git clone, and every
	// package manager too.
	if r.Method == http.MethodConnect {
		b.serveConnect(w, r, token, sessionID, budgetKey, limits)
		return
	}
	if r.URL.Path == "/git-credential" {
		b.serveGitCredential(w, r, token, sessionID)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/egress") || r.URL.IsAbs() {
		b.serveEgress(w, r, token, sessionID)
		return
	}
	// Credentialed API faces (#254) own the /api/ prefix. The egress and
	// CONNECT checks above run first so an absolute-form or tunnel request
	// can never be aimed at a face.
	if r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/") {
		b.serveAPIFace(w, r, token, sessionID, budgetKey, limits, requestAt)
		return
	}

	name, rest, escapedRest := splitUpstreamRoute(r.URL)
	proxy, ok := b.upstreams[name]
	if !ok {
		http.Error(w, "unknown upstream", http.StatusNotFound)
		return
	}

	b.record(r.Context(), token, sessionID, "proxy", name+"/"+rest)
	declared, reserveRemaining, inspectErr := inspectDeclaredTokenMaximum(r)
	if inspectErr != nil {
		http.Error(w, "invalid provider request body", http.StatusBadRequest)
		return
	}
	kind := b.kinds[name]
	reserved := 0
	if b.Usage != nil {
		var err error
		reserved, err = b.Usage.ReserveRequestAt(r.Context(), budgetKey, kind, limits, requestAt, declared, reserveRemaining)
		if err != nil {
			status := http.StatusInternalServerError
			if strings.Contains(err.Error(), "usage limit exceeded") {
				status = http.StatusTooManyRequests
			}
			http.Error(w, err.Error(), status)
			return
		}
	}
	r.URL.Path = "/" + rest
	r.URL.RawPath = "/" + escapedRest
	if r.URL.RawPath == r.URL.Path {
		r.URL.RawPath = ""
	}
	capture := &usageResponseWriter{ResponseWriter: w}
	proxy.ServeHTTP(capture, r)
	if b.Usage != nil {
		// Attribute the captured usage to the upstream's provider type so
		// its cache tokens price at the provider's own multipliers (#247).
		usage := capture.providerUsage()
		usage.Provider = kind
		if err := b.Usage.ReconcileReservationAt(context.WithoutCancel(r.Context()), budgetKey, requestAt, reserved, usage); err != nil {
			b.record(context.WithoutCancel(r.Context()), token, sessionID, "usage-error", err.Error())
		}
	}
}

// splitUpstreamRoute returns the local connection name and provider-relative
// decoded/escaped paths. Keeping both forms in sync prevents an escaped RawPath
// from retaining Waffle's /<connection>/ routing prefix upstream.
func splitUpstreamRoute(u *url.URL) (name, rest, escapedRest string) {
	name, rest, _ = strings.Cut(strings.TrimPrefix(u.Path, "/"), "/")
	_, escapedRest, _ = strings.Cut(strings.TrimPrefix(u.EscapedPath(), "/"), "/")
	return name, rest, escapedRest
}

// trimDuplicateBasePath accepts both broker request forms for compatibility:
// /<connection>/chat/completions and /<connection>/v1/chat/completions when
// the configured provider base already ends in /v1. ReverseProxy.SetURL joins
// the remaining request path onto the base, so remove one matching base prefix
// first while preserving escaped path segments.
func trimDuplicateBasePath(requestURL, target *url.URL) {
	path, trimmed := trimURLPathPrefix(requestURL.Path, target.Path)
	if !trimmed {
		return
	}
	escapedPath, escapedTrimmed := trimURLPathPrefix(requestURL.EscapedPath(), target.EscapedPath())
	requestURL.Path = path
	requestURL.RawPath = ""
	if escapedTrimmed && escapedPath != path {
		requestURL.RawPath = escapedPath
	}
}

func trimURLPathPrefix(path, prefix string) (string, bool) {
	prefix = strings.TrimSuffix(prefix, "/")
	if prefix == "" || prefix == "/" {
		return path, false
	}
	if path == prefix {
		return "/", true
	}
	if strings.HasPrefix(path, prefix+"/") {
		return strings.TrimPrefix(path, prefix), true
	}
	return path, false
}

const maxRequestInspectBytes = 1 << 20

type replayReadCloser struct {
	io.Reader
	closer io.Closer
}

func (r replayReadCloser) Close() error { return r.closer.Close() }

// inspectDeclaredTokenMaximum reads a bounded prefix and restores the body.
// For textual JSON requests, the body byte length is a conservative upper
// bound on input tokens, so the returned reservation covers both that prompt
// bound and the declared output maximum. A missing, invalid, or oversized
// declaration reserves the remaining daily allowance.
func inspectDeclaredTokenMaximum(r *http.Request) (declared int, reserveRemaining bool, err error) {
	if r.Body == nil {
		return 0, true, nil
	}
	original := r.Body
	prefix, err := io.ReadAll(io.LimitReader(original, maxRequestInspectBytes+1))
	if err != nil {
		return 0, true, err
	}
	if len(prefix) > maxRequestInspectBytes {
		r.Body = replayReadCloser{Reader: io.MultiReader(bytes.NewReader(prefix), original), closer: original}
		return 0, true, nil
	}
	_ = original.Close()
	r.Body = io.NopCloser(bytes.NewReader(prefix))
	var payload map[string]json.RawMessage
	if json.Unmarshal(prefix, &payload) != nil {
		return 0, true, nil
	}
	found := false
	for _, key := range []string{"max_tokens", "max_completion_tokens"} {
		raw, ok := payload[key]
		if !ok {
			continue
		}
		found = true
		var n int
		if json.Unmarshal(raw, &n) != nil || n <= 0 {
			return 0, true, nil
		}
		if n > declared {
			declared = n
		}
	}
	if !found {
		return 0, true, nil
	}
	var value any
	if json.Unmarshal(prefix, &value) != nil || !isSelfContainedProviderPayload(payload) || hasExternalTokenSource(value) {
		return 0, true, nil
	}
	if declared > int(^uint(0)>>1)-len(prefix) {
		return 0, true, nil
	}
	return declared + len(prefix), false, nil
}

// isSelfContainedProviderPayload allowlists top-level fields whose input is
// carried in this request. Unknown provider extensions are conservative: they
// may be server-side context handles whose token cost is not represented by
// the request bytes.
func isSelfContainedProviderPayload(payload map[string]json.RawMessage) bool {
	for key := range payload {
		switch strings.ToLower(key) {
		case "model", "messages", "input", "prompt", "instructions", "system",
			"max_tokens", "max_completion_tokens", "temperature", "top_p", "top_k",
			"n", "stream", "stream_options", "stop", "stop_sequences", "tools",
			"tool_choice", "parallel_tool_calls", "response_format", "metadata", "user",
			"reasoning", "thinking", "service_tier", "store", "verbosity", "modalities",
			"prediction", "frequency_penalty", "presence_penalty", "seed", "logprobs",
			"top_logprobs", "logit_bias":
		default:
			return false
		}
	}
	return true
}

// hasExternalTokenSource identifies inputs whose billed token count is not
// bounded by the JSON bytes forwarded through the broker (for example a
// remote image URL or provider-side file ID). These requests reserve the
// remaining allowance instead of relying on the textual-body upper bound.
func hasExternalTokenSource(value any) bool {
	switch v := value.(type) {
	case []any:
		for _, item := range v {
			if hasExternalTokenSource(item) {
				return true
			}
		}
	case map[string]any:
		for key, child := range v {
			lowerKey := strings.ToLower(key)
			correlationID := lowerKey == "tool_use_id" || lowerKey == "tool_call_id" || lowerKey == "call_id"
			if !correlationID && (strings.HasSuffix(lowerKey, "_id") || strings.HasSuffix(lowerKey, "_ids") || strings.HasSuffix(lowerKey, "_url")) {
				return true
			}
			switch lowerKey {
			case "image_url", "file_id", "file_url", "audio_url", "video_url":
				return true
			case "type":
				if kind, ok := child.(string); ok {
					switch strings.ToLower(kind) {
					case "text", "input_text", "output_text", "message", "function", "custom",
						"tool_use", "tool_result", "thinking", "redacted_thinking", "json_schema",
						"json_object", "function_call", "function_call_output", "auto", "any", "tool",
						"none", "ephemeral", "grammar", "regex", "object", "array", "string",
						"number", "integer", "boolean", "null":
					default:
						return true
					}
				}
			}
			if hasExternalTokenSource(child) {
				return true
			}
		}
	}
	return false
}

const maxUsageCaptureBytes = 4 << 20

// usageResponseWriter preserves streaming semantics while retaining only a
// bounded copy for post-response token accounting.
type usageResponseWriter struct {
	http.ResponseWriter
	body          bytes.Buffer
	tail          []byte
	ssePending    []byte
	sseUsage      llm.Usage
	sseFinalUsage bool
	sseComplete   bool
}

func (w *usageResponseWriter) Write(p []byte) (int, error) {
	if strings.Contains(strings.ToLower(w.Header().Get("Content-Type")), "text/event-stream") {
		w.consumeSSE(p)
		return w.ResponseWriter.Write(p)
	}
	if remaining := maxUsageCaptureBytes - w.body.Len(); remaining > 0 {
		if remaining > len(p) {
			remaining = len(p)
		}
		_, _ = w.body.Write(p[:remaining])
	}
	const tailLimit = 64 << 10
	if len(p) >= tailLimit {
		w.tail = append(w.tail[:0], p[len(p)-tailLimit:]...)
	} else {
		w.tail = append(w.tail, p...)
		if len(w.tail) > tailLimit {
			copy(w.tail, w.tail[len(w.tail)-tailLimit:])
			w.tail = w.tail[:tailLimit]
		}
	}
	return w.ResponseWriter.Write(p)
}

func (w *usageResponseWriter) providerUsage() llm.Usage {
	if strings.Contains(strings.ToLower(w.Header().Get("Content-Type")), "text/event-stream") {
		if !w.sseComplete {
			return llm.Usage{}
		}
		return w.sseUsage
	}
	result := parseProviderUsage(w.body.Bytes())
	mergeUsage(&result, parseTrailingUsage(w.tail))
	return result
}

func parseTrailingUsage(body []byte) llm.Usage {
	i := bytes.LastIndex(body, []byte(`"usage"`))
	if i < 0 {
		return llm.Usage{}
	}
	rest := body[i+len(`"usage"`):]
	colon := bytes.IndexByte(rest, ':')
	if colon < 0 {
		return llm.Usage{}
	}
	var raw map[string]any
	if json.NewDecoder(bytes.NewReader(rest[colon+1:])).Decode(&raw) != nil {
		return llm.Usage{}
	}
	var result llm.Usage
	observeUsageObject(raw, &result)
	return result
}

func mergeUsage(dst *llm.Usage, src llm.Usage) {
	if src.InputTokens > dst.InputTokens {
		dst.InputTokens = src.InputTokens
	}
	if src.OutputTokens > dst.OutputTokens {
		dst.OutputTokens = src.OutputTokens
	}
	if src.CacheCreationInputTokens > dst.CacheCreationInputTokens {
		dst.CacheCreationInputTokens = src.CacheCreationInputTokens
	}
	if src.CacheReadInputTokens > dst.CacheReadInputTokens {
		dst.CacheReadInputTokens = src.CacheReadInputTokens
	}
}

func (w *usageResponseWriter) consumeSSE(p []byte) {
	w.ssePending = append(w.ssePending, p...)
	for {
		i := bytes.IndexByte(w.ssePending, '\n')
		if i < 0 {
			if len(w.ssePending) > 1<<20 {
				w.ssePending = w.ssePending[:0]
			}
			return
		}
		line := bytes.TrimSpace(w.ssePending[:i])
		w.ssePending = w.ssePending[i+1:]
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(data) == 0 {
			continue
		}
		if bytes.Equal(data, []byte("[DONE]")) {
			w.sseComplete = w.sseFinalUsage
			continue
		}
		observeProviderUsage(data, &w.sseUsage)
		var event struct {
			Type  string          `json:"type"`
			Usage json.RawMessage `json:"usage"`
		}
		if json.Unmarshal(data, &event) == nil {
			if len(event.Usage) > 0 && (event.Type == "" || event.Type == "message_delta") {
				w.sseFinalUsage = true
			}
			if event.Type == "message_stop" {
				w.sseComplete = w.sseFinalUsage
			}
		}
	}
}

func (w *usageResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func parseProviderUsage(body []byte) llm.Usage {
	var result llm.Usage
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		observeProviderUsage(trimmed, &result)
		return result
	}
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if !bytes.Equal(data, []byte("[DONE]")) {
			observeProviderUsage(data, &result)
		}
	}
	return result
}

func observeProviderUsage(raw []byte, result *llm.Usage) {
	var value any
	if json.Unmarshal(raw, &value) == nil {
		walkUsage(value, result)
	}
}

func walkUsage(value any, result *llm.Usage) {
	m, ok := value.(map[string]any)
	if !ok {
		if list, ok := value.([]any); ok {
			for _, item := range list {
				walkUsage(item, result)
			}
		}
		return
	}
	if raw, ok := m["usage"].(map[string]any); ok {
		observeUsageObject(raw, result)
	}
	for _, child := range m {
		walkUsage(child, result)
	}
}

func maxJSONInt(m map[string]any, keys ...string) int {
	best := 0
	maxInt := int(^uint(0) >> 1)
	for _, key := range keys {
		if n, ok := m[key].(float64); ok && n > 0 {
			if n >= float64(maxInt) {
				return maxInt
			}
			if candidate := int(n); candidate > best {
				best = candidate
			}
		}
	}
	return best
}

// observeUsageObject extracts the token counters from one provider usage
// object into result, carrying cache fields separately so sandboxed traffic
// meters byte-identically to host traffic (the translators split the same
// wire fields). InputTokens always means uncached input:
//
//   - Anthropic reports disjoint fields: input_tokens already excludes
//     cache_creation_input_tokens and cache_read_input_tokens, which are
//     additive for billing.
//   - OpenAI-compatible endpoints report prompt_tokens as the *total*
//     including prompt_tokens_details.cached_tokens; the cached subset is
//     subtracted out so the three counters sum to the reported total.
//
// A usage object with none of these fields yields zeroed counters — never a
// panic, never a negative total.
func observeUsageObject(raw map[string]any, result *llm.Usage) {
	input := maxJSONInt(raw, "input_tokens", "prompt_tokens")
	output := maxJSONInt(raw, "output_tokens", "completion_tokens")
	if input == 0 && output == 0 {
		input = maxJSONInt(raw, "total_tokens")
	}
	_, hasCreation := raw["cache_creation_input_tokens"]
	_, hasRead := raw["cache_read_input_tokens"]
	switch {
	case hasCreation || hasRead:
		result.CacheCreationInputTokens = max(result.CacheCreationInputTokens, maxJSONInt(raw, "cache_creation_input_tokens"))
		result.CacheReadInputTokens = max(result.CacheReadInputTokens, maxJSONInt(raw, "cache_read_input_tokens"))
	case input > 0:
		if details, ok := raw["prompt_tokens_details"].(map[string]any); ok {
			if cached := maxJSONInt(details, "cached_tokens"); cached > 0 {
				result.CacheReadInputTokens = max(result.CacheReadInputTokens, cached)
				if cached >= input {
					input = 0
				} else {
					input -= cached
				}
			}
		}
	}
	if input > result.InputTokens {
		result.InputTokens = input
	}
	if output > result.OutputTokens {
		result.OutputTokens = output
	}
}

func (b *Broker) serveEgress(w http.ResponseWriter, r *http.Request, token, sessionID string) {
	targetURL := r.URL
	if !targetURL.IsAbs() {
		http.Error(w, "egress requires an absolute URL", http.StatusBadRequest)
		b.record(r.Context(), token, sessionID, "denied", "egress relative")
		return
	}
	host := strings.ToLower(targetURL.Hostname())
	b.mu.Lock()
	target := b.egress[host]
	b.mu.Unlock()
	if target == nil {
		http.Error(w, "egress host not allowlisted", http.StatusForbidden)
		b.record(r.Context(), token, sessionID, "denied", "egress "+host)
		return
	}
	base, err := url.Parse(target.BaseURL)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") {
		http.Error(w, "invalid egress target", http.StatusInternalServerError)
		return
	}
	if targetURL.Port() != "" && targetURL.Port() != base.Port() {
		http.Error(w, "egress port not allowlisted", http.StatusForbidden)
		return
	}
	outURL := *targetURL
	outURL.Scheme, outURL.Host = base.Scheme, base.Host
	proxy := httputil.NewSingleHostReverseProxy(base)
	proxy.Transport = b.egressTransport()
	// ReverseProxy.ServeHTTP clones r internally before calling Director, so
	// the rewrite only needs to happen once, here — not again on a
	// pre-mutated copy of r.
	proxy.Director = func(out *http.Request) {
		out.URL = &outURL
		out.Host = base.Host
		out.Header.Del("Proxy-Authorization")
		if target.Header != "" {
			// The broker owns the credential for this host: strip whatever the
			// client sent and inject the configured one (git/package-manager
			// egress). Value is never written to audit rows.
			out.Header.Del("Authorization")
			out.Header.Set(target.Header, target.Value)
		}
		// Without a configured credential the client's Authorization passes
		// through unchanged: a host-side MCP client legitimately carries an
		// OAuth bearer for the origin (#249). The egress allowlist still
		// bounds which hosts are reachable, and every request is audited.
	}
	b.record(r.Context(), token, sessionID, "egress", host+targetURL.EscapedPath())
	proxy.ServeHTTP(w, r)
}

// serveAPIFace handles one credentialed API face request (#254). Every
// decision — allowed and denied — is audited via b.record, naming the face
// and the session. Deny-by-default: the token's mint-time grants decide
// access first, then the face's method allowlist, then the path allowlist,
// then the usage gates (Paused and Check already ran in ServeHTTP; this is
// the atomic request reservation).
func (b *Broker) serveAPIFace(w http.ResponseWriter, r *http.Request, token, sessionID, budgetKey string, limits usage.Limits, requestAt time.Time) {
	name, rest, escapedRest, ok := splitAPIRoute(r.URL)
	if !ok {
		b.record(r.Context(), token, sessionID, "denied", "api route")
		http.Error(w, "unknown api face", http.StatusNotFound)
		return
	}
	b.mu.Lock()
	face := b.faces[name]
	granted := face != nil && b.faceGrants[sessionID][name]
	b.mu.Unlock()
	if face == nil {
		// The same message for every unknown name: an unknown face must not
		// disclose which faces exist.
		b.record(r.Context(), token, sessionID, "denied", "api "+name)
		http.Error(w, "unknown api face", http.StatusNotFound)
		return
	}
	if !granted {
		b.record(r.Context(), token, sessionID, "denied", "api "+name+" not granted")
		http.Error(w, "api face not granted for this session", http.StatusForbidden)
		return
	}
	if !face.methods[r.Method] {
		b.record(r.Context(), token, sessionID, "denied", "api "+name+" method "+r.Method)
		http.Error(w, "api face does not allow method "+r.Method, http.StatusForbidden)
		return
	}
	if traversalRefused(r.URL) {
		b.record(r.Context(), token, sessionID, "denied", "api "+name+" traversal")
		http.Error(w, "api face path refused", http.StatusForbidden)
		return
	}
	if !face.allowsPath(rest) {
		b.record(r.Context(), token, sessionID, "denied", "api "+name+" path "+rest)
		http.Error(w, "api face path not allowed", http.StatusForbidden)
		return
	}
	if b.Usage != nil {
		// Atomic request-count and cap enforcement. Faces carry no token
		// payload to meter, so the reservation is 0 tokens and the request
		// row is the accounting. ReserveRequestAt returns a usage error for
		// both cap refusals; the attempt itself is audited below.
		if _, err := b.Usage.ReserveRequestAt(r.Context(), budgetKey, name, limits, requestAt, 0, false); err != nil {
			b.record(r.Context(), token, sessionID, "denied", "api "+name+" usage")
			status := http.StatusInternalServerError
			if strings.Contains(err.Error(), "usage limit exceeded") {
				status = http.StatusTooManyRequests
			}
			http.Error(w, b.redact(err.Error()), status)
			return
		}
	}

	b.record(r.Context(), token, sessionID, "api", name+"/"+rest)
	r.URL.Path = "/" + rest
	r.URL.RawPath = "/" + escapedRest
	if r.URL.RawPath == r.URL.Path {
		r.URL.RawPath = ""
	}
	// Scrub the response body and any proxy error so a body that echoes the
	// credential is redacted before reaching the caller.
	capture := &redactResponseWriter{ResponseWriter: w, redact: b.redact, overlap: b.redactOverlap()}
	face.proxy.ServeHTTP(capture, r)
	_ = capture.finish()
}

// splitAPIRoute returns the face name and the decoded/escaped paths under
// it for a /api/<name>/<path> request. ok is false when the path does not
// name a face (no name or empty name).
func splitAPIRoute(u *url.URL) (name, rest, escapedRest string, ok bool) {
	trimmed := strings.TrimPrefix(u.Path, "/api/")
	if trimmed == u.Path {
		return "", "", "", false
	}
	name, rest, _ = strings.Cut(trimmed, "/")
	if name == "" {
		return "", "", "", false
	}
	rawTrimmed := strings.TrimPrefix(u.EscapedPath(), "/api/")
	_, escapedRest, _ = strings.Cut(rawTrimmed, "/")
	return name, rest, escapedRest, true
}

// allowsPath reports whether rest matches the face's path-prefix allowlist.
// A prefix matches the whole path or a path boundary, so "/v1" never matches
// "/v1evil".
func (f *apiFace) allowsPath(rest string) bool {
	full := "/" + rest // splitAPIRoute strips the leading slash
	for _, prefix := range f.paths {
		if full == prefix || strings.HasPrefix(full, prefix+"/") {
			return true
		}
	}
	return false
}

// traversalRefused reports whether a request path tries to escape its face
// via "..", encoded separators, or double-encoding (#254). Both the decoded
// Path and the escaped RawPath are checked because each encoding variant
// survives in a different form: net/http decodes %2e%2e to ".." in Path
// while RawPath keeps the original, and a double-encoded "%252e" survives
// Path decoding as "%2e".
func traversalRefused(u *url.URL) bool {
	raw := u.RawPath
	if raw == "" {
		raw = u.EscapedPath()
	}
	lower := strings.ToLower(raw)
	if strings.Contains(lower, "%25") { // double-encoding
		return true
	}
	if strings.Contains(lower, "%2e") { // encoded dots
		return true
	}
	if strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c") { // encoded separators
		return true
	}
	for _, p := range []string{u.Path, raw} {
		if strings.Contains(p, "\\") {
			return true
		}
		for _, seg := range strings.Split(p, "/") {
			if seg == ".." {
				return true
			}
		}
	}
	return false
}

// redactOverlapDefault retains this many tail bytes while scrubbing a
// streaming response body when the operator did not size the overlap from
// the redactor (Broker.RedactOverlap). 256KiB covers any realistic
// credential value.
const redactOverlapDefault = 256 << 10

func (b *Broker) redactOverlap() int {
	if b.RedactOverlap > 0 {
		return b.RedactOverlap
	}
	return redactOverlapDefault
}

func (b *Broker) redact(s string) string {
	if b.Redact == nil {
		return s
	}
	return b.Redact(s)
}

// maxRedactBodyBytes is the largest response body the broker buffers to
// scrub credentials exactly. Almost every API response fits; a body at or
// under this cap is redacted as one string, so a credential can never be
// split across flush boundaries and reassembled by the caller.
const maxRedactBodyBytes = 4 << 20

// redactResponseWriter scrubs a proxied response before it reaches the
// caller (#254): response headers are scrubbed when the header block is
// sent, trailers when the body completes, and the body as it streams.
// Without it, a token-echo endpoint could hand the model the very
// credential the broker is supposed to contain.
//
// Bodies up to maxRedactBodyBytes are buffered and redacted whole — exact
// containment. Larger bodies stream with an overlap window (streaming
// true): each flush redacts the whole window (the flushed prefix plus the
// retained overlap) as one string, then emits the redacted prefix and
// retains the redacted overlap. A credential straddling a flush boundary
// is therefore replaced once — neither the emitted bytes nor the retained
// bytes carry any part of it, so the caller cannot reassemble a split
// value across flushes.
type redactResponseWriter struct {
	http.ResponseWriter
	redact    func(string) string
	overlap   int
	pending   []byte
	streaming bool
}

func (w *redactResponseWriter) Write(p []byte) (int, error) {
	if len(w.pending)+len(p) > maxRedactBodyBytes {
		w.streaming = true
	}
	w.pending = append(w.pending, p...)
	if w.streaming && len(w.pending) > w.overlap {
		cut := len(w.pending) - w.overlap
		redacted := w.redact(string(w.pending))
		if cut > len(redacted) {
			cut = len(redacted)
		}
		if _, err := io.WriteString(w.ResponseWriter, redacted[:cut]); err != nil {
			return 0, err
		}
		w.pending = []byte(redacted[cut:])
	}
	return len(p), nil
}

// WriteHeader scrubs every upstream response header value before the header
// block is sent. The ReverseProxy copies upstream headers through the
// embedded writer untouched, so an upstream that echoes the injected
// credential in a response header (not just the body) would otherwise hand
// it straight to the caller (#254 review).
func (w *redactResponseWriter) WriteHeader(status int) {
	h := w.Header()
	for _, vs := range h {
		for i, v := range vs {
			vs[i] = w.redact(v)
		}
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *redactResponseWriter) finish() error {
	// Trailers (http.TrailerPrefix keys) are declared after WriteHeader, so
	// scrub them here, before the body completes and net/http sends them.
	h := w.Header()
	for k, vs := range h {
		if strings.HasPrefix(k, http.TrailerPrefix) {
			for i, v := range vs {
				vs[i] = w.redact(v)
			}
		}
	}
	if len(w.pending) == 0 {
		return nil
	}
	_, err := io.WriteString(w.ResponseWriter, w.redact(string(w.pending)))
	w.pending = nil
	return err
}

func (w *redactResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// redactingLog returns a log.Logger that scrubs credential material from
// reverse-proxy error lines before they reach the process log.
func redactingLog(redact func(string) string) *log.Logger {
	return log.New(redactingLogWriter{redact: redact}, "", 0)
}

type redactingLogWriter struct {
	redact func(string) string
}

func (w redactingLogWriter) Write(p []byte) (int, error) {
	msg := strings.TrimSpace(w.redact(string(p)))
	if msg != "" {
		slog.Default().Error("broker face proxy", "err", msg)
	}
	return len(p), nil
}

// safeHTTPTransport is built once and reused across every proxied egress
// request, so connections are pooled instead of each RoundTrip getting its
// own throwaway Transport (and connection pool).
var safeHTTPTransport = func() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	t.DialContext = safeDialContext
	return t
}()

type safeTransport struct{}

func (safeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return safeHTTPTransport.RoundTrip(r)
}

// egressTransport returns the dialing transport for the HTTP egress face.
// Production keeps the pooled safe transport (private targets refused). A
// test-supplied DialEgress — the documented escape hatch for loopback
// origins — is honored here too, exactly as it is on the CONNECT face, so
// both egress paths behave identically under the override.
func (b *Broker) egressTransport() http.RoundTripper {
	if b.DialEgress == nil {
		return safeTransport{}
	}
	t := safeHTTPTransport.Clone()
	t.DialContext = b.DialEgress
	return t
}

func safeDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return nil, err
	}
	for _, ip := range ips {
		if isPrivateIP(ip) {
			continue
		}
		return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
	}
	return nil, fmt.Errorf("egress address resolves to a private or otherwise unsafe network")
}

func isPrivateIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

// serveGitCredential speaks git's credential wire format: key=value lines
// in, key=value lines out.
func (b *Broker) serveGitCredential(w http.ResponseWriter, r *http.Request, token, sessionID string) {
	if b.GitCredential == nil {
		http.Error(w, "git credentials not configured", http.StatusNotFound)
		return
	}
	attrs := ParseGitCredential(r.Body)
	host, path := attrs["host"], attrs["path"]
	user, pass, err := b.GitCredential(r.Context(), sessionID, host, path)
	if err != nil {
		bound, _ := b.GitRepoScope(sessionID)
		b.record(r.Context(), token, sessionID, "denied",
			"git-credential host="+host+" requested="+path+" bound="+bound)
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	detail := "backend=" + b.GitBackend + " repo=" + path + " host=" + host
	b.record(r.Context(), token, sessionID, "git-credential", detail)
	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprintf(w, "username=%s\npassword=%s\n", user, pass)
}

// ParseGitCredential reads git's key=value credential format.
func ParseGitCredential(r io.Reader) map[string]string {
	attrs := map[string]string{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), "="); ok {
			attrs[k] = v
		}
	}
	return attrs
}

// Serve binds listen and runs the broker's HTTP face until ctx ends. The
// bind happens synchronously, so a caller that must know the address is
// actually held (e.g. before handing a container a broker URL) gets the
// "address already in use" error here rather than in a background goroutine.
func (b *Broker) Serve(ctx context.Context, listen string) error {
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	return b.ServeListener(ctx, ln)
}

// ServeListener runs the broker's HTTP face on an already-bound listener
// until ctx ends. Use this when the bind must be attempted (and its failure
// observed) before starting other work.
func (b *Broker) ServeListener(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           b,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	err := srv.Serve(ln)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

func (b *Broker) record(ctx context.Context, token, sessionID, action, detail string) {
	if b.audit == nil {
		return
	}
	prefix := token
	if len(prefix) > 11 {
		prefix = prefix[:11]
	}
	// detail is scrubbed before it can land in a row: a credential-shaped
	// path or header must never persist in the audit table (#254).
	if _, err := b.audit.ExecContext(ctx, `
		INSERT INTO broker_audit (at, token_prefix, session, action, detail)
		VALUES (?, ?, ?, ?, ?)`,
		b.now().Format(time.RFC3339Nano), prefix, sessionID, action, b.redact(detail)); err != nil {
		slog.Default().Error("broker audit insert failed", "err", err, "token_prefix", prefix, "action", action)
	}
}

// proxyStyleRequest reports whether the client is talking to the broker as an
// HTTP proxy rather than calling its API face. A CONNECT request, or an
// absolute request URI, is only ever produced by a proxy client.
func proxyStyleRequest(r *http.Request) bool {
	return r.Method == http.MethodConnect || r.URL.IsAbs()
}

// proxyCredential extracts the session token from a proxy-style request's
// Proxy-Authorization header ("Basic <base64 token:>").
func proxyCredential(r *http.Request) string {
	if scheme, value, ok := strings.Cut(r.Header.Get("Proxy-Authorization"), " "); ok && strings.EqualFold(scheme, "Basic") {
		if decoded, err := base64.StdEncoding.DecodeString(value); err == nil {
			token, _, _ := strings.Cut(string(decoded), ":")
			return token
		}
	}
	return ""
}

// denyUnauthenticated refuses a request that carried no usable session token.
// A proxy client needs 407 with a challenge, not 401: curl and git only retry
// with credentials after a 407, so a 401 surfaces as a hard "CONNECT tunnel
// failed" with the credentials never sent.
func denyUnauthenticated(w http.ResponseWriter, r *http.Request) {
	if proxyStyleRequest(r) {
		w.Header().Set("Proxy-Authenticate", `Basic realm="waffle"`)
		http.Error(w, "proxy authentication required", http.StatusProxyAuthRequired)
		return
	}
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

// auditDetail describes what a request was for. A CONNECT request has an empty
// URL path -- the target lives in the authority -- so recording the path would
// write a blank detail and lose what the client was trying to reach.
func auditDetail(r *http.Request) string {
	if r.Method == http.MethodConnect {
		host, port := connectTarget(r)
		return "connect " + host + ":" + port
	}
	return r.URL.Path
}

// connectTarget splits a CONNECT authority into host and port, defaulting the
// port to 443. CONNECT carries an authority, not a URL, so there is no scheme
// to infer anything else from.
func connectTarget(r *http.Request) (host, port string) {
	authority := r.Host
	if authority == "" {
		authority = r.URL.Host
	}
	host, port, err := net.SplitHostPort(authority)
	if err != nil {
		return strings.ToLower(authority), "443"
	}
	return strings.ToLower(host), port
}

// tunnelBudgetExhausted stops an io.Copy relay when a session's tunnelled
// egress byte budget is consumed mid-tunnel (#244). It is never returned to
// the client: the tunnel is cut, which is the only way to stop a relay that
// was already established.
var errTunnelBudgetExhausted = errors.New("tunnelled relay byte budget exhausted")

// limitWriter aborts a CONNECT relay once the session's tunnelled relay
// byte allowance is consumed (#244). The broker relays tunnel bytes without
// inspection, so the io.Copy byte count is the only meter; the allowance is
// shared across both relay directions, and when it is exhausted stop closes
// both connections so the peer direction's relay returns too.
type limitWriter struct {
	dst       io.Writer
	remaining *atomic.Int64 // bytes left in the session's rolling-day budget
	stop      func()
}

func (w *limitWriter) Write(p []byte) (int, error) {
	for {
		r := w.remaining.Load()
		if r <= 0 {
			w.stop()
			return 0, errTunnelBudgetExhausted
		}
		n := int64(len(p))
		if n > r {
			n = r
		}
		if !w.remaining.CompareAndSwap(r, r-n) {
			continue
		}
		written, err := w.dst.Write(p[:n])
		if err == nil && (n < int64(len(p)) || r-n == 0) {
			// The allowance is consumed by this write: stop both directions
			// now, otherwise the peer direction could block forever waiting
			// for data that will never come.
			w.stop()
			return written, errTunnelBudgetExhausted
		}
		return written, err
	}
}

// serveConnect tunnels a TLS connection to an allowlisted egress host.
//
// Authorisation is the same host allowlist the rewriting path uses, which is
// all that path ever enforced: it never inspected paths or bodies. The target
// is dialled through safeDialContext, so the private-address refusal that
// protects the rewriting path protects the tunnel too, and the connection goes
// to the address that was actually resolved rather than one a later lookup
// might return.
//
// Bytes are relayed without inspection, so the client's TLS session runs
// end to end: the broker never sees the request, the response, or the
// credential the client sends.
func (b *Broker) serveConnect(w http.ResponseWriter, r *http.Request, token, sessionID, budgetKey string, limits usage.Limits) {
	host, port := connectTarget(r)
	if host == "" {
		http.Error(w, "connect requires a host", http.StatusBadRequest)
		return
	}
	b.mu.Lock()
	target := b.egress[host]
	b.mu.Unlock()
	if target == nil {
		b.record(r.Context(), token, sessionID, "denied", "connect "+host)
		http.Error(w, "egress host not allowlisted", http.StatusForbidden)
		return
	}
	base, err := url.Parse(target.BaseURL)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") {
		http.Error(w, "invalid egress target", http.StatusInternalServerError)
		return
	}
	wantPort := base.Port()
	if wantPort == "" {
		wantPort = "443"
		if base.Scheme == "http" {
			wantPort = "80"
		}
	}
	if port != wantPort {
		b.record(r.Context(), token, sessionID, "denied", "connect port "+host+":"+port)
		http.Error(w, "egress port not allowlisted", http.StatusForbidden)
		return
	}

	// #244: a tunnel is metered once per CONNECT unless the relay bytes are
	// charged against the session budget. The Check above already refused the
	// CONNECT when the rolling-day total reached the cap. The allowance is
	// reserved per budget key so concurrent tunnels share one cap: each
	// CONNECT sees persisted bytes plus other live tunnels' reservations, and
	// the reservation is released when this tunnel's bytes are persisted.
	budgeted := b.Usage != nil && limits.TunnelBytesPerSession > 0
	var remaining atomic.Int64
	var allowance int64
	if budgeted {
		persisted, err := b.Usage.TunnelBytesAt(r.Context(), budgetKey, b.now())
		if err != nil {
			http.Error(w, "tunnel usage check failed", http.StatusInternalServerError)
			return
		}
		b.mu.Lock()
		live := b.tunnelLive[budgetKey]
		if live == nil {
			live = &atomic.Int64{}
			b.tunnelLive[budgetKey] = live
		}
		allowance = limits.TunnelBytesPerSession - persisted - live.Load()
		if allowance > 0 {
			live.Add(allowance)
		}
		b.mu.Unlock()
		if allowance <= 0 {
			b.record(r.Context(), token, sessionID, "denied", "connect budget "+host+":"+port)
			http.Error(w, "usage limit exceeded: tunnelled relay byte budget", http.StatusTooManyRequests)
			return
		}
		remaining.Store(allowance)
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "connect is not supported here", http.StatusInternalServerError)
		return
	}
	// Dial before hijacking: while the ResponseWriter is still intact a failure
	// can be reported as a normal status rather than a silently dropped socket.
	dial := b.DialEgress
	if dial == nil {
		dial = safeDialContext
	}
	upstream, err := dial(r.Context(), "tcp", net.JoinHostPort(host, port))
	if err != nil {
		b.record(r.Context(), token, sessionID, "denied", "connect dial "+host)
		http.Error(w, "egress dial failed", http.StatusBadGateway)
		return
	}
	defer func() { _ = upstream.Close() }()

	client, buffered, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, "connect hijack failed", http.StatusInternalServerError)
		return
	}
	defer func() { _ = client.Close() }()

	b.record(r.Context(), token, sessionID, "connect", host+":"+port)
	if _, err := buffered.WriteString("HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return
	}
	if err := buffered.Flush(); err != nil {
		return
	}

	// Relay both directions to completion. Read from the buffered reader, not
	// the raw conn: the client may already have written TLS bytes that Hijack
	// left buffered, and reading the conn directly would lose them.
	//
	// Each direction half-closes when it ends rather than closing outright. A
	// full close would tear down the peer's side too, so a client that finishes
	// writing before the origin has flushed its response would lose the rest of
	// it. Half-close signals EOF and lets the other direction drain.
	//
	// With a byte budget configured, both directions relay through limitWriter
	// sharing one allowance; when it is exhausted the relay is cut (#244). The
	// relayed byte count is persisted after both directions finish so the next
	// CONNECT (and the CONNECT-time Check) sees it, and this tunnel's share of
	// the in-memory reservation is released so the persisted total is not
	// double-counted against concurrent tunnels.
	var relayed atomic.Int64
	finished := make(chan struct{}, 2)
	stopOnce := sync.Once{}
	stop := func() {
		stopOnce.Do(func() {
			_ = client.Close()
			_ = upstream.Close()
		})
	}
	relay := func(dst io.Writer, src io.Reader, closeWrite net.Conn) {
		var n int64
		if budgeted {
			n, _ = io.Copy(&limitWriter{dst: dst, remaining: &remaining, stop: stop}, src)
		} else {
			n, _ = io.Copy(dst, src)
		}
		relayed.Add(n)
		if tcp, ok := closeWrite.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
		finished <- struct{}{}
	}
	go relay(upstream, buffered, upstream)
	go relay(client, upstream, client)
	<-finished
	<-finished
	if b.Usage != nil {
		// Persist first, then release the reservation: between the two the
		// account briefly over-counts (conservative — a concurrent CONNECT may
		// be refused early, never allowed to overrun), and after the release
		// the persisted total plus the remaining live reservations exactly
		// equal the bytes actually relayed.
		if err := b.Usage.AddTunnelBytesAt(context.WithoutCancel(r.Context()), budgetKey, relayed.Load(), b.now()); err != nil {
			slog.Default().Error("broker tunnel usage record failed", "err", err, "session", sessionID)
		}
		if budgeted {
			b.mu.Lock()
			if live := b.tunnelLive[budgetKey]; live != nil {
				live.Add(-allowance)
				if live.Load() == 0 {
					delete(b.tunnelLive, budgetKey)
				}
			}
			b.mu.Unlock()
		}
	}
}
