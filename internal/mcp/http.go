// Streamable HTTP transport (#249): POST for requests, SSE for the
// server-to-client stream, Mcp-Session-Id header for resumability — the
// current MCP spec's HTTP binding. This is the transport that reaches the
// remote connector ecosystem (Gmail, Notion, Linear, GitHub's own server);
// stdio stays the default for local commands.
//
// Security posture: remote servers are untrusted network endpoints. Auth
// tokens never live here — the TokenManager resolves them from
// internal/secret and the caller decides egress (direct vs broker proxy).
// A rejected credential disables the connection fail-closed; a session the
// server no longer recognises is re-initialized once.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/matt-riley/waffle/internal/tool"
)

// remoteProtocolVersion is the protocol revision waffle proposes to remote
// streamable-HTTP servers. Servers reply with the version they support; the
// client does not gate on the negotiated value for the small surface
// (initialize, tools/list, tools/call) this package speaks.
const remoteProtocolVersion = "2025-03-26"

// HTTPOpts configures a streamable-HTTP connection (#249).
type HTTPOpts struct {
	// Client is the HTTP client used for all requests. When nil, a default
	// is built: direct mode dials the configured URL, proxy mode routes
	// through ProxyURL. Tests inject an in-process client — zero real
	// network.
	Client *http.Client
	// Token, when set, resolves OAuth bearer credentials from the secret
	// store and refreshes them ahead of expiry. Calls fail closed while the
	// credential is disabled.
	Token *TokenManager
	// BearerToken, when set, is sent as "Authorization: Bearer" on every
	// request: a static credential resolved from a secret:// reference.
	// Mutually exclusive with Token in practice (config validation + the
	// agent builder resolve one path or the other).
	BearerToken string
	// ProxyURL, when set, routes every request through this HTTP proxy
	// (the gateway broker's egress face). The proxy's allowlist and audit
	// rows bound and record the traffic (#249). Exactly what the docker-mode
	// group posture requires.
	ProxyURL string
	// ProxyAuth supplies the proxy credential (an Authorization-style value
	// for the Proxy-Authorization header, e.g. "Basic <base64 wk_ token>").
	// Re-invoked when the proxy answers 407 so a long-running gateway can
	// re-mint after the broker token TTL. Nil means no proxy credential.
	ProxyAuth func() (string, error)
	// Headers are fixed extra HTTP headers sent on every request, from a
	// portable plugin's mcp.json headers object. Client-generated headers
	// (Content-Type, Accept, User-Agent, Mcp-Session-Id, Authorization,
	// Proxy-Authorization) take precedence over same-name entries, per the
	// Agent Plugins spec; nil means no extra headers.
	Headers http.Header
	// ConnectTimeout bounds the initialize handshake. Zero uses 30s.
	ConnectTimeout time.Duration
}

// HTTPClient is one MCP streamable-HTTP connection. It implements the
// Transport surface (Call/Notify/Close) and is safe for concurrent use:
// each Call is an independent POST, and the session id is mutex-guarded.
type HTTPClient struct {
	name   string
	url    string
	http   *http.Client
	token  *TokenManager
	bearer string
	// headers are fixed extra headers from a portable plugin's mcp.json.
	headers http.Header
	// proxyURL/proxyAuth are set when egress routes through the broker.
	proxyURL  string
	proxyAuth func() (string, error)

	mu        sync.Mutex
	nextID    int
	sessionID string
	disabled  string // fail-closed reason, once set
	closed    bool
}

// ConnectHTTP performs the initialize handshake against a remote
// streamable-HTTP MCP server and returns a ready client.
func ConnectHTTP(ctx context.Context, name, url string, opts HTTPOpts) (*HTTPClient, error) {
	if opts.ConnectTimeout <= 0 {
		opts.ConnectTimeout = 30 * time.Second
	}
	h := &HTTPClient{
		name:      name,
		url:       url,
		http:      opts.Client,
		token:     opts.Token,
		bearer:    opts.BearerToken,
		proxyURL:  opts.ProxyURL,
		proxyAuth: opts.ProxyAuth,
		headers:   opts.Headers,
	}
	// A proxy client must be built from ProxyURL: an injected in-process
	// client would dial the target directly and bypass the broker entirely.
	if h.http == nil || opts.ProxyURL != "" {
		h.http = newHTTPClient(opts)
	}
	initCtx, cancel := context.WithTimeout(ctx, opts.ConnectTimeout)
	defer cancel()
	if _, err := h.call(initCtx, "initialize", map[string]any{
		"protocolVersion": remoteProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "waffle", "version": "0"},
	}); err != nil {
		_ = h.Close()
		return nil, err
	}
	if err := h.notify(initCtx, "notifications/initialized"); err != nil {
		_ = h.Close()
		return nil, err
	}
	return h, nil
}

// newHTTPClient builds the HTTP client for remote MCP traffic from the
// egress configuration in opts (see newEgressTransport).
func newHTTPClient(opts HTTPOpts) *http.Client {
	return &http.Client{Transport: newEgressTransport(opts.ProxyURL, opts.ProxyAuth)}
}

// NewTokenHTTPClient builds the HTTP client for one server's OAuth token
// traffic (refresh) from the same egress configuration as its MCP
// connection (#249). Broker egress must govern credential refresh too: a
// TokenManager on http.DefaultClient would send refresh tokens around the
// broker allowlist and broker_audit entirely. proxyAuth is re-invoked per
// request so a long-running gateway re-mints after the broker token TTL;
// nil proxyAuth (direct egress) means no proxy credential is sent.
func NewTokenHTTPClient(proxyURL string, proxyAuth func() (string, error)) *http.Client {
	base := newEgressTransport(proxyURL, proxyAuth)
	if proxyURL == "" || proxyAuth == nil {
		return &http.Client{Transport: base}
	}
	// Absolute-form proxy requests (http targets) carry Proxy-Authorization
	// as a normal request header; https targets CONNECT instead, and the
	// base transport's GetProxyConnectHeader stamps the credential there.
	// The header is not set for https requests, so the broker credential
	// never travels inside the TLS tunnel to the origin.
	return &http.Client{Transport: proxyCredentialTransport{base: base, auth: proxyAuth}}
}

// proxyCredentialTransport stamps the broker egress credential onto every
// plaintext proxy request it forwards. The credential is minted per
// request (a 407 from the broker means the old one expired, and the caller
// re-mints on the next call).
type proxyCredentialTransport struct {
	base *http.Transport
	auth func() (string, error)
}

func (t proxyCredentialTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme == "https" {
		// CONNECT auth is handled by GetProxyConnectHeader on the base
		// transport; do not put the broker credential on the tunneled
		// request itself.
		return t.base.RoundTrip(r)
	}
	cred, err := t.auth()
	if err != nil {
		return nil, err
	}
	out := r.Clone(r.Context())
	out.Header.Set("Proxy-Authorization", cred)
	return t.base.RoundTrip(out)
}

// newEgressTransport builds the dialing transport for remote MCP traffic.
// Proxy mode dials only the broker and leaves host allowlisting to it;
// direct mode dials the configured URL as an operator-authored trust
// decision (the config file is the allowlist). When a proxy credential is
// configured, https targets receive it on the CONNECT request via
// GetProxyConnectHeader; http targets carry it as a per-request header set
// by the caller (doPost, or proxyCredentialTransport for token traffic).
func newEgressTransport(proxyURL string, proxyAuth func() (string, error)) *http.Transport {
	base := http.DefaultTransport.(*http.Transport).Clone()
	if proxyURL != "" {
		if proxy, err := parseProxyURL(proxyURL); err == nil {
			base.Proxy = http.ProxyURL(proxy)
		}
	}
	if proxyURL != "" && proxyAuth != nil {
		base.GetProxyConnectHeader = func(context.Context, *url.URL, string) (http.Header, error) {
			cred, err := proxyAuth()
			if err != nil {
				return nil, err
			}
			return http.Header{"Proxy-Authorization": {cred}}, nil
		}
	}
	return base
}

func parseProxyURL(raw string) (*url.URL, error) { return url.Parse(raw) }

// Call implements Transport.
func (h *HTTPClient) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return h.call(ctx, method, params)
}

// Notify implements Transport.
func (h *HTTPClient) Notify(ctx context.Context, method string) error {
	return h.notify(ctx, method)
}

// Close implements Transport: it fails the connection closed and releases
// idle connections. In-flight calls are bounded by their own contexts.
func (h *HTTPClient) Close() error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	h.disabled = "connection closed"
	h.mu.Unlock()
	if tr, ok := h.http.Transport.(*http.Transport); ok {
		tr.CloseIdleConnections()
	}
	return nil
}

// Toolbox lists the server's tools, prefixed with the server name.
func (h *HTTPClient) Toolbox(ctx context.Context) (tool.Toolbox, error) {
	return newToolbox(h, ctx, h.name)
}

// call performs one JSON-RPC request/response round trip.
func (h *HTTPClient) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	h.mu.Lock()
	if h.disabled != "" {
		reason := h.disabled
		h.mu.Unlock()
		return nil, fmt.Errorf("mcp %s: %s", h.name, reason)
	}
	h.mu.Unlock()

	id, body, err := marshalRequest(h, method, params)
	if err != nil {
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	resp, err := h.post(ctx, body, method == "initialize")
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		// The MCP spec reserves 401 for authentication failure: credentials
		// are revoked/rejected, so the connection fails closed.
		return nil, h.rejectCredentials(resp.StatusCode)
	case resp.StatusCode == http.StatusForbidden:
		// A 403 is a policy refusal (e.g. the broker egress allowlist), not a
		// credential failure: surface it, do not disable.
		return nil, fmt.Errorf("mcp %s: %s %q refused: HTTP 403: %s", h.name, method, h.url, boundedBody(resp.Body))
	case resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusNoContent:
		return nil, fmt.Errorf("mcp %s: server accepted %s without a response", h.name, method)
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return nil, fmt.Errorf("mcp %s: %s %q failed: HTTP %d: %s", h.name, method, h.url, resp.StatusCode, boundedBody(resp.Body))
	}
	return h.readResponse(resp, id)
}

// notify sends a JSON-RPC notification; the server answers 202 with an
// empty body.
func (h *HTTPClient) notify(ctx context.Context, method string) error {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method})
	if err != nil {
		return err
	}
	resp, err := h.post(ctx, body, false)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusAccepted, http.StatusOK, http.StatusNoContent:
		return nil
	case http.StatusUnauthorized:
		return h.rejectCredentials(resp.StatusCode)
	case http.StatusForbidden:
		return fmt.Errorf("mcp %s: notify %s refused: HTTP 403: %s", h.name, method, boundedBody(resp.Body))
	default:
		return fmt.Errorf("mcp %s: notify %s: HTTP %d: %s", h.name, method, resp.StatusCode, boundedBody(resp.Body))
	}
}

// post sends one JSON-RPC message and returns the server response, with
// two recovery paths: a 404 (session ended server-side) drops the session
// and re-initializes before retrying, and a 407 from the broker egress
// proxy re-mints the proxy credential once. Callers own resp.Body.
func (h *HTTPClient) post(ctx context.Context, body []byte, isInitialize bool) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		resp, err := h.doPost(ctx, body)
		if err != nil {
			return nil, err
		}
		if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
			h.setSession(sid)
		}
		switch {
		case resp.StatusCode == http.StatusProxyAuthRequired && h.proxyAuth != nil && attempt == 0:
			_ = resp.Body.Close()
			continue // re-mint the proxy credential and retry once
		case resp.StatusCode == http.StatusNotFound && !isInitialize && attempt == 0:
			_ = resp.Body.Close()
			if err := h.resume(ctx); err != nil {
				return nil, err
			}
			continue
		default:
			return resp, nil
		}
	}
}

// doPost builds and sends one POST request.
func (h *HTTPClient) doPost(ctx context.Context, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("mcp %s: %w", h.name, err)
	}
	// Portable plugin headers first: client-generated headers below take
	// precedence over same-name entries (Agent Plugins §7.2.1). Keys are
	// canonicalized so a plugin cannot smuggle a second case-variant of a
	// header onto the wire, and reserved session/credential headers are
	// dropped case-insensitively (http.Header.Del canonicalizes): a plugin
	// can never spoof the MCP session, bearer auth, or the broker proxy
	// credential.
	for name, values := range h.headers {
		req.Header[http.CanonicalHeaderKey(name)] = values
	}
	for _, reserved := range []string{"Mcp-Session-Id", "Authorization", "Proxy-Authorization"} {
		req.Header.Del(reserved)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("User-Agent", "waffle-mcp/0")
	h.mu.Lock()
	if h.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", h.sessionID)
	}
	h.mu.Unlock()
	if h.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+h.bearer)
	} else if h.token != nil {
		token, err := h.token.AccessToken(ctx)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if h.proxyAuth != nil {
		cred, err := h.proxyAuth()
		if err != nil {
			return nil, fmt.Errorf("mcp %s: broker egress credential: %w", h.name, err)
		}
		req.Header.Set("Proxy-Authorization", cred)
	}
	resp, err := h.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mcp %s: %w", h.name, err)
	}
	return resp, nil
}

// resume drops the dead session and re-runs initialize against a fresh one
// (MCP streamable HTTP resumability: a 404 means the server-side session
// is gone, not the server).
func (h *HTTPClient) resume(ctx context.Context) error {
	h.setSession("")
	initID, initBody, err := marshalRequest(h, "initialize", map[string]any{
		"protocolVersion": remoteProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "waffle", "version": "0"},
	})
	if err != nil {
		return err
	}
	resp, err := h.doPost(ctx, initBody)
	if err != nil {
		return fmt.Errorf("mcp %s: session ended: re-initialize: %w", h.name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		h.setSession(sid)
	}
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("mcp %s: session ended and re-initialize refused (HTTP 404)", h.name)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("mcp %s: session ended: re-initialize failed: HTTP %d: %s", h.name, resp.StatusCode, boundedBody(resp.Body))
	}
	if _, err := h.readResponse(resp, initID); err != nil {
		return fmt.Errorf("mcp %s: session ended: re-initialize: %w", h.name, err)
	}

	notifBody, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	if err != nil {
		return err
	}
	nresp, err := h.doPost(ctx, notifBody)
	if err != nil {
		return err
	}
	defer func() { _ = nresp.Body.Close() }()
	if sid := nresp.Header.Get("Mcp-Session-Id"); sid != "" {
		h.setSession(sid)
	}
	if nresp.StatusCode != http.StatusAccepted && nresp.StatusCode != http.StatusOK && nresp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("mcp %s: session ended: re-initialize notification refused: HTTP %d", h.name, nresp.StatusCode)
	}
	return nil
}

// marshalRequest allocates the next request id and marshals a JSON-RPC
// request under it. The id is returned so the caller can match the reply.
func marshalRequest(h *HTTPClient, method string, params any) (int, []byte, error) {
	h.mu.Lock()
	id := h.nextID
	h.nextID++
	h.mu.Unlock()
	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	return id, body, err
}

// readResponse parses a 2xx body: either a single application/json
// JSON-RPC message (or batch) or an SSE stream carrying the message for
// wantID. The body is closed on return.
func (h *HTTPClient) readResponse(resp *http.Response, wantID int) (json.RawMessage, error) {
	defer func() { _ = resp.Body.Close() }()
	ctype := resp.Header.Get("Content-Type")
	if strings.HasPrefix(ctype, "text/event-stream") {
		return readSSEMessage(resp.Body, wantID)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxFrameBytes+1))
	if err != nil {
		return nil, fmt.Errorf("mcp %s: read response: %w", h.name, err)
	}
	if len(raw) > MaxFrameBytes {
		return nil, fmt.Errorf("mcp %s: %w", h.name, ErrFrameTooLarge)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("mcp %s: empty HTTP %d response", h.name, resp.StatusCode)
	}
	msg, err := findMessage(raw, wantID)
	if err != nil {
		return nil, fmt.Errorf("mcp %s: %w", h.name, err)
	}
	if msg.Error != nil {
		return nil, msg.Error
	}
	return msg.Result, nil
}

// errNoMessage reports that a server reply carried no message for the
// request id we sent.
var errNoMessage = errors.New("server reply contains no response for this request")

// findMessage locates the JSON-RPC message carrying wantID inside a body
// that is either one message object or a batch array.
func findMessage(raw []byte, wantID int) (*rpcResponse, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, errNoMessage
	}
	if trimmed[0] == '[' {
		var batch []rpcResponse
		if err := json.Unmarshal(trimmed, &batch); err != nil {
			return nil, fmt.Errorf("malformed JSON-RPC batch: %w", err)
		}
		for i := range batch {
			if batch[i].ID == wantID {
				return &batch[i], nil
			}
		}
		return nil, errNoMessage
	}
	var msg rpcResponse
	if err := json.Unmarshal(trimmed, &msg); err != nil {
		return nil, fmt.Errorf("malformed JSON-RPC message: %w", err)
	}
	if msg.ID != wantID {
		return nil, errNoMessage
	}
	return &msg, nil
}

// readSSEMessage reads a text/event-stream body until the JSON-RPC message
// carrying wantID arrives, then returns its result. An interruption of the
// stream (EOF or transport error) before the response surfaces as an
// error, never as a partial result presented as complete. Bounded per
// event by MaxFrameBytes like the stdio framing.
func readSSEMessage(r io.Reader, wantID int) (json.RawMessage, error) {
	br := bufio.NewReader(r)
	var eventName string
	var data []byte
	flush := func() (json.RawMessage, error) {
		if eventName == "" {
			return nil, nil // not an event line; ignore comments/keepalives
		}
		eventName = ""
		if len(data) == 0 {
			return nil, nil
		}
		payload := data
		data = nil
		if len(payload) > MaxFrameBytes {
			return nil, ErrFrameTooLarge
		}
		if eventName == "error" {
			return nil, fmt.Errorf("server sent error event: %s", strings.TrimSpace(string(payload)))
		}
		msg, err := findMessage(payload, wantID)
		if err != nil {
			if errors.Is(err, errNoMessage) {
				return nil, nil // some other message (progress etc.); keep reading
			}
			return nil, err
		}
		if msg.Error != nil {
			return nil, msg.Error
		}
		return msg.Result, nil
	}
	for {
		line, err := readSSELine(br)
		if err != nil {
			if errors.Is(err, io.EOF) {
				// One last flush so a response on the final unterminated
				// event (no trailing blank line) is still honored.
				if result, ferr := flush(); ferr != nil || result != nil {
					return result, ferr
				}
				return nil, fmt.Errorf("stream ended before the response arrived")
			}
			if errors.Is(err, ErrFrameTooLarge) {
				// An oversized line is a protocol violation by the server,
				// not a transport interruption: say so plainly.
				return nil, err
			}
			return nil, fmt.Errorf("stream interrupted: %w", err)
		}
		if len(line) == 0 {
			result, ferr := flush()
			if ferr != nil || result != nil {
				return result, ferr
			}
			continue
		}
		switch {
		case bytes.HasPrefix(line, []byte(":")):
			// SSE comment / keepalive.
		case bytes.HasPrefix(line, []byte("event:")):
			eventName = strings.TrimSpace(string(line[len("event:"):]))
		case bytes.HasPrefix(line, []byte("data:")):
			chunk := line[len("data:"):]
			chunk = bytes.TrimPrefix(chunk, []byte(" "))
			// Bound the cumulative event, not just each line: a server
			// sending one unterminated event as many individually valid
			// data lines must not grow memory without limit (#249 review).
			extra := len(chunk)
			if len(data) > 0 {
				extra++ // the '\n' separator between data lines
			}
			if len(data)+extra > MaxFrameBytes {
				return nil, ErrFrameTooLarge
			}
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, chunk...)
		}
	}
}

// readSSELine reads one SSE line (newline or CRLF terminated), stripping
// the terminator. Lines larger than bufio's default 4096-byte buffer are
// accumulated in fragments up to MaxFrameBytes: a valid long data line
// must not be mistaken for a stream interruption. A line beyond the bound
// is ErrFrameTooLarge, matching the stdio framing's DoS bound (#249).
func readSSELine(br *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, err := br.ReadSlice('\n')
		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			// Fragment without a terminator yet: accumulate and continue.
			if len(line)+len(chunk) > MaxFrameBytes {
				return nil, ErrFrameTooLarge
			}
			line = append(line, chunk...)
		case err != nil:
			return nil, err
		default:
			// A complete line: the trailing terminator is not payload.
			if len(line)+len(chunk)-1 > MaxFrameBytes {
				return nil, ErrFrameTooLarge
			}
			line = append(line, chunk...)
			line = bytes.TrimSuffix(line, []byte("\n"))
			line = bytes.TrimSuffix(line, []byte("\r"))
			return line, nil
		}
	}
}

// rejectCredentials fails the connection closed: a revoked or rejected
// credential must not be retried in a loop. The returned error is the one
// the tool call surfaces.
func (h *HTTPClient) rejectCredentials(status int) error {
	reason := fmt.Sprintf("credentials rejected (HTTP %d); run `waffle mcp login %s` to re-authorize, or fix the token referenced in config", status, h.name)
	h.disable(reason)
	return fmt.Errorf("mcp %s: %s", h.name, reason)
}

// disable records the fail-closed reason once.
func (h *HTTPClient) disable(reason string) {
	h.mu.Lock()
	if h.disabled == "" {
		h.disabled = reason
	}
	h.mu.Unlock()
}

func (h *HTTPClient) setSession(id string) {
	h.mu.Lock()
	h.sessionID = id
	h.mu.Unlock()
}

// boundedBody reads a bounded snippet of an error body for operator-facing
// messages.
func boundedBody(r io.Reader) string {
	raw, _ := io.ReadAll(io.LimitReader(r, 4<<10))
	s := strings.TrimSpace(string(raw))
	if len(s) > 512 {
		s = s[:512] + "…"
	}
	return s
}

// RemoteEgress wires remote MCP traffic to the gateway broker's egress
// face (#249). A docker-mode group's remote MCP traffic must traverse the
// broker (allowlist + audit) or be refused; this type is how the agent
// builder obtains the proxy endpoint and fresh session tokens.
type RemoteEgress struct {
	// ProxyURL is the broker's /egress endpoint as reachable from the host
	// process, e.g. "http://127.0.0.1:9876/egress".
	ProxyURL string
	// MintToken returns a fresh broker session token (wk_...) for group.
	// It is called per connection and again when the proxy answers 407, so
	// a long-running gateway re-mints after the broker token TTL.
	MintToken func(ctx context.Context, group string) (string, error)
}
