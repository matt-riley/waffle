// Package broker is the host-side credential broker (docs/plan.md,
// "Secret management"). One rule: raw keys exist only here. Sandboxes hold
// wk_ session tokens (DefaultTokenTTL) and talk to the broker, which injects
// the real credential upstream. Phase 4 ships the LLM face (Anthropic and
// OpenAI-compatible passthrough); the git and egress faces arrive with
// repo workspaces (phase 5).
package broker

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
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
	// BaseURL of the real provider (e.g. https://api.anthropic.com).
	BaseURL string
	// Header is the auth header to inject ("x-api-key" or "Authorization").
	Header string
	// Value is the real credential (for Authorization: pass "Bearer ...").
	Value string
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

// Broker mints session tokens and proxies authenticated requests.
type Broker struct {
	audit     *sql.DB
	upstreams map[string]*httputil.ReverseProxy
	egress    map[string]*EgressTarget

	// GitCredential, when set, enables the /git-credential face used by
	// `waffle git-credential` inside workspace containers.
	GitCredential GitCredentialFunc
	// GitBackend is audit metadata, never a secret. Typical values are pat or github-app.
	GitBackend string

	mu       sync.Mutex
	tokens   map[string]tokenEntry // token → session + expiry
	sessions map[string]string     // session id → current token
	gitScope map[string]string     // session id → bound repo (owner/name)
	limits   map[string]usage.Limits
	budgets  map[string]string
	Usage    *usage.Store
	Limits   usage.Limits
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
		upstreams: map[string]*httputil.ReverseProxy{},
		egress:    map[string]*EgressTarget{},
		tokens:    map[string]tokenEntry{},
		sessions:  map[string]string{},
		gitScope:  map[string]string{},
		limits:    map[string]usage.Limits{},
		budgets:   map[string]string{},
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

// Mint issues a wk_ session token bound to sessionID.
func (b *Broker) Mint(ctx context.Context, sessionID string) (string, error) {
	return b.mint(ctx, sessionID, "", usage.Limits{}, false)
}

// MintScoped issues a token with a group-specific limit and stable accounting
// identity. The concrete session remains the authorization/audit identity.
func (b *Broker) MintScoped(ctx context.Context, sessionID, budgetKey string, limits usage.Limits) (string, error) {
	return b.mint(ctx, sessionID, budgetKey, limits, true)
}

func (b *Broker) mint(ctx context.Context, sessionID, budgetKey string, limits usage.Limits, scoped bool) (string, error) {
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
	b.mu.Unlock()
	b.record(ctx, token, sessionID, "mint", "")
	return token, nil
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
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == r.Header.Get("Authorization") {
		if scheme, value, ok := strings.Cut(r.Header.Get("Proxy-Authorization"), " "); ok && strings.EqualFold(scheme, "Basic") {
			if decoded, err := base64.StdEncoding.DecodeString(value); err == nil {
				token, _, _ = strings.Cut(string(decoded), ":")
			}
		}
	}
	sessionID := ""
	budgetKey := ""
	limits := b.Limits
	if strings.HasPrefix(token, "wk_") {
		var expired bool
		sessionID, budgetKey, limits, expired = b.usageScope(token)
		if expired {
			// Distinguish expired from unknown in broker_audit (action=expired).
			b.record(r.Context(), token, sessionID, "expired", r.URL.Path)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}
	if sessionID == "" {
		b.record(r.Context(), token, "", "denied", r.URL.Path)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
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

	if r.URL.Path == "/git-credential" {
		b.serveGitCredential(w, r, token, sessionID)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/egress") || r.URL.IsAbs() {
		b.serveEgress(w, r, token, sessionID)
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
	reserved := 0
	if b.Usage != nil {
		var err error
		reserved, err = b.Usage.ReserveRequestAt(r.Context(), budgetKey, limits, requestAt, declared, reserveRemaining)
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
		if err := b.Usage.ReconcileReservationAt(context.WithoutCancel(r.Context()), budgetKey, requestAt, reserved, capture.providerUsage()); err != nil {
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
	input := billedInputTokens(raw)
	output := maxJSONInt(raw, "output_tokens", "completion_tokens")
	if input == 0 && output == 0 {
		input = maxJSONInt(raw, "total_tokens")
	}
	return llm.Usage{InputTokens: input, OutputTokens: output}
}

func mergeUsage(dst *llm.Usage, src llm.Usage) {
	if src.InputTokens > dst.InputTokens {
		dst.InputTokens = src.InputTokens
	}
	if src.OutputTokens > dst.OutputTokens {
		dst.OutputTokens = src.OutputTokens
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
		input := billedInputTokens(raw)
		output := maxJSONInt(raw, "output_tokens", "completion_tokens")
		if input == 0 && output == 0 {
			input = maxJSONInt(raw, "total_tokens")
		}
		if input > result.InputTokens {
			result.InputTokens = input
		}
		if output > result.OutputTokens {
			result.OutputTokens = output
		}
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

// billedInputTokens follows Anthropic's billing contract: cached input is
// additive, not an alternative observation. OpenAI-compatible prompt_tokens
// remains an alias of input_tokens when cache fields are absent.
func billedInputTokens(m map[string]any) int {
	_, hasCreation := m["cache_creation_input_tokens"]
	_, hasRead := m["cache_read_input_tokens"]
	if !hasCreation && !hasRead {
		return maxJSONInt(m, "input_tokens", "prompt_tokens")
	}
	total := maxJSONInt(m, "input_tokens")
	total = saturatingAdd(total, maxJSONInt(m, "cache_creation_input_tokens"))
	return saturatingAdd(total, maxJSONInt(m, "cache_read_input_tokens"))
}

func saturatingAdd(a, b int) int {
	maxInt := int(^uint(0) >> 1)
	if b > maxInt-a {
		return maxInt
	}
	return a + b
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
	proxy.Transport = safeTransport{}
	// ReverseProxy.ServeHTTP clones r internally before calling Director, so
	// the rewrite only needs to happen once, here — not again on a
	// pre-mutated copy of r.
	proxy.Director = func(out *http.Request) {
		out.URL = &outURL
		out.Host = base.Host
		out.Header.Del("Authorization")
		out.Header.Del("Proxy-Authorization")
		if target.Header != "" {
			out.Header.Set(target.Header, target.Value)
		}
	}
	b.record(r.Context(), token, sessionID, "egress", host+targetURL.EscapedPath())
	proxy.ServeHTTP(w, r)
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
	if _, err := b.audit.ExecContext(ctx, `
		INSERT INTO broker_audit (at, token_prefix, session, action, detail)
		VALUES (?, ?, ?, ?, ?)`,
		b.now().Format(time.RFC3339Nano), prefix, sessionID, action, detail); err != nil {
		slog.Default().Error("broker audit insert failed", "err", err, "token_prefix", prefix, "action", action)
	}
}
