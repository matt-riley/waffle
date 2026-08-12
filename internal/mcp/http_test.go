package mcp

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/broker"
	"github.com/matt-riley/waffle/internal/store"
)

// fakeHTTPServer is an in-process MCP streamable-HTTP server with
// configurable failure modes. Zero real network: httptest only.
type fakeHTTPServer struct {
	mu       sync.Mutex
	sessions map[string]bool
	nextID   int

	// requireSession makes every post-initialize request demand a known
	// Mcp-Session-Id (spec behavior); disable to test sessionless servers.
	requireSession bool
	// failAuth answers 401 once auth is armed.
	failAuth bool
	// sseAnswers serves responses as SSE streams instead of JSON.
	sseAnswers bool
	// interruptSSE truncates the SSE stream before the response (after one
	// unrelated event).
	interruptSSE bool
	// hang causes the handler to stall (never respond) until released.
	hang    bool
	release chan struct{}
	// ignoreSessionIDs, when true, responds to post-initialize requests even
	// without a session id.
	ignoreSessionIDs bool

	gotSession atomic.Value // last Mcp-Session-Id seen on a post-initialize request
	calls      atomic.Int32
}

func newFakeHTTPServer() *fakeHTTPServer {
	return &fakeHTTPServer{sessions: map[string]bool{}, requireSession: true}
}

func (f *fakeHTTPServer) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		if f.hang {
			select {
			case <-r.Context().Done():
			case <-f.release:
			}
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, MaxFrameBytes+1))
		if err != nil {
			http.Error(w, "read: "+err.Error(), http.StatusBadRequest)
			return
		}
		var req rpcRequest
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		sid := r.Header.Get("Mcp-Session-Id")
		f.mu.Lock()
		defer f.mu.Unlock()
		if req.Method == "initialize" {
			if sid == "" || !f.sessions[sid] {
				f.nextID++
				sid = fmt.Sprintf("sess-%d", f.nextID)
				f.sessions[sid] = true
				w.Header().Set("Mcp-Session-Id", sid)
			}
			f.answer(w, r, req.ID, map[string]any{
				"protocolVersion": remoteProtocolVersion,
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]any{"name": "fake", "version": "0"},
			})
			return
		}
		// Post-initialize: a known session is required unless waived.
		if f.requireSession && !f.ignoreSessionIDs && (sid == "" || !f.sessions[sid]) {
			w.Header().Set("Mcp-Session-Id", sid)
			http.Error(w, `{"jsonrpc":"2.0","error":{"code":-32001,"message":"session not found"}}`, http.StatusNotFound)
			return
		}
		f.gotSession.Store(sid)
		if f.failAuth {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusUnauthorized)
			return
		}
		switch req.Method {
		case "tools/list":
			f.answer(w, r, req.ID, map[string]any{
				"tools": []map[string]any{{
					"name":        "echo",
					"description": "echo text",
					"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}},
				}},
			})
		case "tools/call":
			f.answer(w, r, req.ID, map[string]any{
				"content": []map[string]any{{"type": "text", "text": "pong"}},
				"isError": false,
			})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		default:
			http.Error(w, "unknown method", http.StatusBadRequest)
		}
	})
}

// answer writes a JSON or SSE response for id, honoring the failure modes.
func (f *fakeHTTPServer) answer(w http.ResponseWriter, r *http.Request, id int, result map[string]any) {
	if f.interruptSSE {
		// Send one unrelated event, flush, then cut the stream: the response
		// for id never arrives.
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		_, _ = fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n")
		if fl != nil {
			fl.Flush()
		}
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
		}
		return
	}
	payload, _ := json.Marshal(rpcResponse{JSONRPC: "2.0", ID: id, Result: mustRaw(result)})
	if f.sseAnswers {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", payload)
		if fl != nil {
			fl.Flush()
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(payload)
}

func mustRaw(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}

func connectFake(t *testing.T, f *fakeHTTPServer) (*HTTPClient, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(f.Handler())
	t.Cleanup(srv.Close)
	client, err := ConnectHTTP(context.Background(), "fake", srv.URL, HTTPOpts{Client: srv.Client()})
	if err != nil {
		t.Fatalf("ConnectHTTP: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client, srv
}

// TestConnectHTTPDrivesListAndCallEndToEnd drives initialize, tools/list and tools/call
// end to end against an in-process streamable-HTTP server (#249).
func TestConnectHTTPDrivesListAndCallEndToEnd(t *testing.T) {
	f := newFakeHTTPServer()
	client, _ := connectFake(t, f)

	tb, err := client.Toolbox(context.Background())
	if err != nil {
		t.Fatalf("Toolbox: %v", err)
	}
	defs := tb.Defs()
	if len(defs) != 1 || defs[0].Name != "fake__echo" {
		t.Fatalf("defs = %+v", defs)
	}
	out, err := tb.Run(context.Background(), "fake__echo", json.RawMessage(`{"text":"ping"}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "pong" {
		t.Fatalf("out = %q, want pong", out)
	}
	// Post-initialize requests must have carried the session id.
	if sid := f.gotSession.Load(); sid == nil || sid == "" {
		t.Fatal("server never saw an Mcp-Session-Id on post-initialize requests")
	}
}

// TestSSEAnswersServeListAndCall drives the same surface against an SSE-answering
// server (spec: POST may be answered with a server-to-client SSE stream).
func TestSSEAnswersServeListAndCall(t *testing.T) {
	f := newFakeHTTPServer()
	f.sseAnswers = true
	client, _ := connectFake(t, f)

	tb, err := client.Toolbox(context.Background())
	if err != nil {
		t.Fatalf("Toolbox over SSE: %v", err)
	}
	out, err := tb.Run(context.Background(), "fake__echo", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Run over SSE: %v", err)
	}
	if out != "pong" {
		t.Fatalf("out = %q", out)
	}
}

// TestSSEInterruptionSurfacesErrorNotPartialResult: the stream is cut after one
// unrelated event; the call must error, never hang, never return partial
// data as complete (#249).
func TestSSEInterruptionSurfacesErrorNotPartialResult(t *testing.T) {
	f := newFakeHTTPServer()
	client, _ := connectFake(t, f)
	f.mu.Lock()
	f.interruptSSE = true
	f.mu.Unlock()

	start := time.Now()
	_, err := client.call(context.Background(), "tools/list", map[string]any{})
	if err == nil {
		t.Fatal("interrupted SSE call succeeded, want error")
	}
	if !strings.Contains(err.Error(), "stream") && !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("error %q does not mention the stream interruption", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("interrupted SSE call took %v, want prompt error", elapsed)
	}
}

// TestUnresponsiveServerFailsWithinCallersTimeoutBound: a server that accepts and
// never answers must surface a call error within the caller's bound.
func TestUnresponsiveServerFailsWithinCallersTimeoutBound(t *testing.T) {
	f := newFakeHTTPServer()
	f.hang = true
	f.release = make(chan struct{})
	srv := httptest.NewServer(f.Handler())
	t.Cleanup(func() {
		close(f.release) // unblock the handler so srv.Close can finish
		srv.Close()
	})
	client, err := ConnectHTTP(context.Background(), "fake", srv.URL, HTTPOpts{
		Client: srv.Client(), ConnectTimeout: 300 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("ConnectHTTP to a hanging server succeeded, want timeout")
	}
	_ = client
}

// TestConcurrentCallsShareOneConnectionSafely: two simultaneous tools/call invocations against
// one HTTP server both complete correctly (-race exercises the session and
// id bookkeeping).
func TestConcurrentCallsShareOneConnectionSafely(t *testing.T) {
	f := newFakeHTTPServer()
	client, _ := connectFake(t, f)

	const n = 8
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			out, err := client.call(context.Background(), "tools/call", map[string]any{
				"name": "echo", "arguments": map[string]any{"text": "ping"},
			})
			if err == nil && string(out) == "" {
				err = fmt.Errorf("empty result")
			}
			errs <- err
		}()
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent call %d: %v", i, err)
		}
	}
}

// TestLostSessionReinitializesAndRetriesCall: the server forgets the session; the client
// re-initializes (fresh session) and retries the original call once.
func TestLostSessionReinitializesAndRetriesCall(t *testing.T) {
	f := newFakeHTTPServer()
	client, _ := connectFake(t, f)

	// Server drops all sessions mid-flight.
	f.mu.Lock()
	f.sessions = map[string]bool{}
	f.mu.Unlock()

	out, err := client.call(context.Background(), "tools/list", map[string]any{})
	if err != nil {
		t.Fatalf("call after session loss: %v", err)
	}
	if !strings.Contains(string(out), "echo") {
		t.Fatalf("result after resume = %s", out)
	}
	// The resumed session must be a fresh one.
	if sid := f.gotSession.Load(); sid == nil || sid == "" {
		t.Fatal("no session id after resume")
	}
}

// TestRejectedCredentialsDisableConnectionFailClosed: a 401 disables the connection;
// every later call errors with an operator-facing reason instead of
// retrying in a loop.
func TestRejectedCredentialsDisableConnectionFailClosed(t *testing.T) {
	f := newFakeHTTPServer()
	client, _ := connectFake(t, f)

	f.mu.Lock()
	f.failAuth = true
	f.mu.Unlock()

	_, err := client.call(context.Background(), "tools/list", map[string]any{})
	if err == nil {
		t.Fatal("call with rejected credentials succeeded")
	}
	if !strings.Contains(err.Error(), "waffle mcp login") {
		t.Fatalf("error %q lacks the operator-facing login hint", err)
	}
	// Fail closed: no more network attempts.
	f.calls.Store(0)
	for i := 0; i < 3; i++ {
		if _, err := client.call(context.Background(), "tools/list", map[string]any{}); err == nil {
			t.Fatal("call after disable succeeded")
		}
	}
	if got := f.calls.Load(); got != 0 {
		t.Fatalf("disabled client still hit the server %d times", got)
	}
}

// TestAccessTokenNeverLeaksIntoErrors: the access token must not appear in any
// error string or request to a non-allowlisted host.
func TestAccessTokenNeverLeaksIntoErrors(t *testing.T) {
	const token = "mcp_access_token_secret_value_12345"
	f := newFakeHTTPServer()
	client, _ := connectFake(t, f)
	f.mu.Lock()
	f.failAuth = true
	f.mu.Unlock()
	tm := &TokenManager{Store: memStore{}, Server: "fake", HTTP: http.DefaultClient}
	if err := tm.Save(&TokenSet{
		AccessToken: token, RefreshToken: "refresh-token", ExpiresIn: 3600, TokenType: "bearer",
	}, TokenMeta{TokenEndpoint: "http://token.test", ClientID: "client-1"}); err != nil {
		t.Fatal(err)
	}
	client.token = tm
	_, err := client.call(context.Background(), "tools/list", map[string]any{})
	if err == nil {
		t.Fatal("call succeeded, want auth failure")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("error leaks the access token: %v", err)
	}
}

// memStore is a minimal secret.Store for token tests.
type memStore map[string]string

func (m memStore) Get(name string) (string, error) {
	v, ok := m[name]
	if !ok {
		return "", fmt.Errorf("secret %q not found", name)
	}
	return v, nil
}
func (m memStore) Set(name, value string) error { m[name] = value; return nil }
func (m memStore) Delete(name string) error     { delete(m, name); return nil }
func (m memStore) List() ([]string, error) {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out, nil
}

// TestBrokerEgressAllowlistsHostsAndAuditsTraffic: remote MCP traffic
// through the broker egress face is allowlisted and produces audit rows;
// a host outside the allowlist is refused (#249).
func TestBrokerEgressAllowlistsHostsAndAuditsTraffic(t *testing.T) {
	origin := httptest.NewServer(newFakeHTTPServer().Handler())
	t.Cleanup(origin.Close)
	_, originPort, _ := net.SplitHostPort(origin.Listener.Addr().String())

	st, err := store.Open(context.Background(), t.TempDir()+"/waffle.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	b := broker.New(st, nil)
	// The default dialer refuses private addresses, which a loopback origin
	// is; overridden only to exercise the relay (the refusal itself is the
	// broker's own posture, asserted in broker tests).
	b.DialEgress = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	b.SetEgress([]broker.EgressTarget{{Host: "localhost", BaseURL: "http://localhost:" + originPort}})
	token, err := b.Mint(context.Background(), "mcp-egress:main")
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(b)
	t.Cleanup(front.Close)

	proxyAuth := func() (string, error) {
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(token+":")), nil
	}
	client, err := ConnectHTTP(context.Background(), "fake", "http://localhost/", HTTPOpts{
		ProxyURL:  front.URL + "/egress",
		ProxyAuth: proxyAuth,
	})
	if err != nil {
		t.Fatalf("ConnectHTTP through broker egress: %v", err)
	}
	defer func() { _ = client.Close() }()
	tb, err := client.Toolbox(context.Background())
	if err != nil {
		t.Fatalf("Toolbox through broker egress: %v", err)
	}
	if len(tb.Defs()) != 1 {
		t.Fatalf("defs = %+v", tb.Defs())
	}

	// Audit rows must exist for the proxied egress traffic.
	rows, err := st.DB.Query(`SELECT action, detail FROM broker_audit WHERE session = 'mcp-egress:main'`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var egressRows int
	for rows.Next() {
		var action, detail string
		if err := rows.Scan(&action, &detail); err != nil {
			t.Fatal(err)
		}
		if action == "egress" && strings.Contains(detail, "localhost/") {
			egressRows++
		}
	}
	if egressRows < 2 { // initialize + tools/list at minimum
		t.Fatalf("egress audit rows = %d, want >= 2", egressRows)
	}

	// A host outside the allowlist is refused through the same broker.
	denied, err := ConnectHTTP(context.Background(), "denied", "http://denied.test/", HTTPOpts{
		ProxyURL:  front.URL + "/egress",
		ProxyAuth: proxyAuth,
	})
	if err == nil {
		_ = denied.Close()
		t.Fatal("ConnectHTTP to a non-allowlisted host succeeded")
	}
	if !strings.Contains(err.Error(), "denied.test") {
		t.Fatalf("denied error %q does not name the refused host", err)
	}
}

// TestProxyCredentialRemintedAndRetriedOn407 exercises the 407 retry against a
// stub proxy that accepts only the second credential.
func TestProxyCredentialRemintedAndRetriedOn407(t *testing.T) {
	var mints atomic.Int32
	var requests atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		var req rpcRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"ok":true}}`, req.ID)
	}))
	t.Cleanup(origin.Close)

	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cred := r.Header.Get("Proxy-Authorization")
		if cred != "Basic good-token" {
			http.Error(w, "proxy authentication required", http.StatusProxyAuthRequired)
			return
		}
		// Forward to origin as a plain absolute-form request.
		outReq, _ := http.NewRequest(r.Method, origin.URL, r.Body)
		outReq.Header = r.Header.Clone()
		outReq.Header.Del("Proxy-Authorization")
		resp, err := http.DefaultClient.Do(outReq)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(proxy.Close)

	proxyURL, _ := url.Parse(proxy.URL + "/egress")
	proxied := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	h := &HTTPClient{
		name: "t", url: "http://mcp.test/",
		http: proxied,
		// First mint returns a stale token once, then the good one: the 407
		// path must re-mint and retry instead of surfacing the failure.
		proxyAuth: func() (string, error) {
			if mints.Add(1) == 1 {
				return "Basic stale-token", nil
			}
			return "Basic good-token", nil
		},
	}
	raw, err := h.call(context.Background(), "tools/list", map[string]any{})
	if err != nil {
		t.Fatalf("call after 407 re-mint: %v", err)
	}
	if !strings.Contains(string(raw), "ok") {
		t.Fatalf("result = %s", raw)
	}
	if requests.Load() != 1 {
		t.Fatalf("origin requests = %d, want 1", requests.Load())
	}
}

// TestSSEMessageParsingHandlesCommentsUnrelatedEventsAndBatches covers SSE parsing details: comments,
// unrelated events, multi-line data, and batch data payloads.
func TestSSEMessageParsingHandlesCommentsUnrelatedEventsAndBatches(t *testing.T) {
	stream := ": keepalive\n" +
		"event: message\n" +
		"data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n" +
		"event: message\n" +
		"data: [{\"jsonrpc\":\"2.0\",\"id\":7,\"result\":{\"ok\":true}}]\n\n"
	raw, err := readSSEMessage(strings.NewReader(stream), 7)
	if err != nil {
		t.Fatalf("readSSEMessage: %v", err)
	}
	if !strings.Contains(string(raw), "ok") {
		t.Fatalf("result = %s", raw)
	}
}

func TestSSEStreamEndingBeforeResponseErrors(t *testing.T) {
	stream := "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"x\"}\n\n"
	_, err := readSSEMessage(strings.NewReader(stream), 9)
	if err == nil {
		t.Fatal("truncated stream succeeded, want error")
	}
	if !strings.Contains(err.Error(), "stream ended") {
		t.Fatalf("error %q", err)
	}
}

func TestFindMessageMatchesRequestedIDAndRejectsOthers(t *testing.T) {
	msg, err := findMessage([]byte(`{"jsonrpc":"2.0","id":3,"result":{"a":1}}`), 3)
	if err != nil || string(msg.Result) != `{"a":1}` {
		t.Fatalf("single message: %v %s", err, msg.Result)
	}
	if _, err := findMessage([]byte(`{"jsonrpc":"2.0","id":3}`), 4); err == nil {
		t.Fatal("wrong id matched")
	}
	if _, err := findMessage([]byte(`{"broken`), 1); err == nil {
		t.Fatal("malformed body matched")
	}
}

// TestSSELongDataLineDeliveredIntact: a valid SSE data line larger than
// bufio's default 4096-byte reader buffer is accumulated in fragments and
// delivered intact — it must not surface as a stream interruption (#249).
func TestSSELongDataLineDeliveredIntact(t *testing.T) {
	blob := strings.Repeat("x", 64<<10) // 64 KiB payload, well past 4 KiB
	payload := fmt.Sprintf(`{"jsonrpc":"2.0","id":7,"result":{"ok":true,"blob":%q}}`, blob)
	stream := "event: message\ndata: " + payload + "\n\n"
	raw, err := readSSEMessage(strings.NewReader(stream), 7)
	if err != nil {
		t.Fatalf("readSSEMessage over long line: %v", err)
	}
	if !json.Valid(raw) {
		t.Fatalf("result is not valid JSON: %.80s", raw)
	}
	if !strings.Contains(string(raw), blob) {
		t.Fatalf("result lost payload bytes: %d bytes, want the full %d-byte blob", len(raw), len(blob))
	}
}

// TestSSELineOverMaxFrameBytesErrorsClearly: a data line beyond
// MaxFrameBytes is a protocol violation and must error plainly — not as a
// generic "stream interrupted" transport failure (#249).
func TestSSELineOverMaxFrameBytesErrorsClearly(t *testing.T) {
	stream := "event: message\ndata: " + strings.Repeat("y", MaxFrameBytes+64) + "\n\n"
	_, err := readSSEMessage(strings.NewReader(stream), 7)
	if err == nil {
		t.Fatal("oversized SSE line succeeded")
	}
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("error %v is not ErrFrameTooLarge", err)
	}
	if strings.Contains(err.Error(), "stream interrupted") {
		t.Fatalf("oversized line reported as an interruption: %v", err)
	}
}

// TestReadSSELineAccumulatesFragments: readSSELine itself returns long
// lines whole, trims CRLF, and bounds accumulation by MaxFrameBytes.
func TestReadSSELineAccumulatesFragments(t *testing.T) {
	big := strings.Repeat("z", 16<<10) // 16 KiB: several 4 KiB bufio fragments
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr error
	}{
		{name: "short line", in: "data: hi\n", want: "data: hi"},
		{name: "long line", in: "data: " + big + "\n", want: "data: " + big},
		{name: "long CRLF line", in: "data: " + big + "\r\n", want: "data: " + big},
		{name: "over bound", in: "data: " + strings.Repeat("q", MaxFrameBytes+8) + "\n", wantErr: ErrFrameTooLarge},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			br := bufio.NewReader(strings.NewReader(tc.in))
			line, err := readSSELine(br)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("readSSELine: %v", err)
			}
			if string(line) != tc.want {
				t.Fatalf("line = %d bytes, want %d bytes (prefix %q)", len(line), len(tc.want), line[:min(len(line), 32)])
			}
		})
	}
}
