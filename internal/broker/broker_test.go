package broker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/store"
	"github.com/matt-riley/waffle/internal/usage"
)

// waitDayRows polls a session's day usage rows until want is satisfied or a
// short deadline passes, then returns the rows. The broker reconciles a
// proxied request's final usage only after the response body completes, so
// an assertion immediately after the client consumed the response can race
// the reconcile commit and observe the still-open reservation (seen as a
// flake in CI under load, e.g. TestAnthropicSSECacheUsageBindsOnTrueCost).
// Callers assert on the returned rows so a genuinely wrong reconcile still
// fails loudly.
func waitDayRows(t *testing.T, b *Broker, session string, want func([]usage.Row) bool) []usage.Row {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		rows, err := b.Usage.List(context.Background(), session)
		if err != nil {
			t.Fatal(err)
		}
		var day []usage.Row
		for _, r := range rows {
			if r.Period == "day" {
				day = append(day, r)
			}
		}
		if want(day) {
			return day
		}
		if time.Now().After(deadline) {
			return day
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestProxyAccountsReturnedProviderUsageAndEnforcesBothCaps(t *testing.T) {
	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"usage":{"prompt_tokens":7,"completion_tokens":5}}`)
	}))
	defer upstream.Close()
	st := openStore(t)
	b := New(st, []Upstream{{Name: "openai", BaseURL: upstream.URL, Header: "Authorization", Value: "Bearer real"}})
	b.Usage = usage.New(st)
	b.Limits = usage.Limits{TokensPerDay: 12, RequestsPerHour: 2}
	b.Now = func() time.Time { return time.Date(2026, 7, 13, 8, 0, 0, 0, time.UTC) }
	token, err := b.Mint(context.Background(), "budget-a")
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(b)
	defer front.Close()

	do := func() *http.Response {
		req, _ := http.NewRequest(http.MethodPost, front.URL+"/openai/v1/chat/completions", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp
	}
	if got := do().StatusCode; got != http.StatusOK {
		t.Fatalf("first status=%d", got)
	}
	day := waitDayRows(t, b, "budget-a", func(rows []usage.Row) bool {
		return len(rows) == 1 && rows[0].Requests == 1 && rows[0].InputTokens+rows[0].OutputTokens == 12
	})
	if len(day) != 1 || day[0].Requests != 1 || day[0].InputTokens+day[0].OutputTokens != 12 {
		t.Fatalf("requests=%d tokens=%d rows=%+v", day[0].Requests, day[0].InputTokens+day[0].OutputTokens, day)
	}
	if got := do().StatusCode; got != http.StatusTooManyRequests {
		t.Fatalf("token-capped status=%d", got)
	}
	if calls != 1 {
		t.Fatalf("upstream calls=%d", calls)
	}

	// A separate identity proves the request cap is enforced independently.
	b.Limits = usage.Limits{RequestsPerHour: 1}
	token, err = b.Mint(context.Background(), "budget-b")
	if err != nil {
		t.Fatal(err)
	}
	if got := do().StatusCode; got != http.StatusOK {
		t.Fatalf("request first status=%d", got)
	}
	if got := do().StatusCode; got != http.StatusTooManyRequests {
		t.Fatalf("request-capped status=%d", got)
	}
}

func TestKeylessUpstreamDoesNotInjectEmptyAuthHeader(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("X-Api-Key"); got != "" {
			t.Errorf("X-Api-Key = %q", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	b := New(nil, []Upstream{{Name: "local", BaseURL: upstream.URL}})
	token, err := b.Mint(context.Background(), "keyless")
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(b)
	defer front.Close()
	req, _ := http.NewRequest(http.MethodPost, front.URL+"/local/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}
}

func TestNamedUpstreamRoutesStripLocalPrefixAndJoinProviderPath(t *testing.T) {
	type observedRequest struct {
		path, escapedPath, query, authorization, apiKey string
	}
	observed := make(chan observedRequest, 4)
	newUpstream := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			observed <- observedRequest{
				path:          r.URL.Path,
				escapedPath:   r.URL.EscapedPath(),
				query:         r.URL.RawQuery,
				authorization: r.Header.Get("Authorization"),
				apiKey:        r.Header.Get("X-Api-Key"),
			}
			w.WriteHeader(http.StatusNoContent)
		}))
	}
	anthropic := newUpstream()
	defer anthropic.Close()
	openrouter := newUpstream()
	defer openrouter.Close()
	local := newUpstream()
	defer local.Close()

	b := New(nil, []Upstream{
		{Name: "claude-cloud", BaseURL: anthropic.URL, Header: "x-api-key", Value: "anthropic-real"},
		{Name: "openrouter", BaseURL: openrouter.URL + "/v1", Header: "Authorization", Value: "Bearer openrouter-real"},
		{Name: "local", BaseURL: local.URL + "/v1"},
	})
	token, err := b.Mint(context.Background(), "named-routes")
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(b)
	defer front.Close()

	tests := []struct {
		requestPath     string
		wantPath        string
		wantEscapedPath string
		wantQuery       string
		wantAuth        string
		wantAPIKey      string
	}{
		{requestPath: "/claude-cloud/v1/messages?beta=tools", wantPath: "/v1/messages", wantEscapedPath: "/v1/messages", wantQuery: "beta=tools", wantAPIKey: "anthropic-real"},
		{requestPath: "/openrouter/chat/completions?stream=true", wantPath: "/v1/chat/completions", wantEscapedPath: "/v1/chat/completions", wantQuery: "stream=true", wantAuth: "Bearer openrouter-real"},
		{requestPath: "/openrouter/v1/responses?legacy=1", wantPath: "/v1/responses", wantEscapedPath: "/v1/responses", wantQuery: "legacy=1", wantAuth: "Bearer openrouter-real"},
		{requestPath: "/local/models%2Flist?raw=1", wantPath: "/v1/models/list", wantEscapedPath: "/v1/models%2Flist", wantQuery: "raw=1"},
	}
	for _, tc := range tests {
		req, _ := http.NewRequest(http.MethodPost, front.URL+tc.requestPath, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("%s status = %d", tc.requestPath, resp.StatusCode)
		}
		got := <-observed
		if got.path != tc.wantPath || got.escapedPath != tc.wantEscapedPath || got.query != tc.wantQuery || got.authorization != tc.wantAuth || got.apiKey != tc.wantAPIKey {
			t.Fatalf("%s upstream = %#v, want path=%q escaped=%q query=%q auth=%q apiKey=%q", tc.requestPath, got, tc.wantPath, tc.wantEscapedPath, tc.wantQuery, tc.wantAuth, tc.wantAPIKey)
		}
	}
}

func TestConcurrentDeclaredTokenReservationsCannotOvershoot(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	var upstreamBodies []string
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream body: %v", err)
		}
		mu.Lock()
		calls++
		upstreamBodies = append(upstreamBodies, string(body))
		mu.Unlock()
		started <- struct{}{}
		<-release
		_, _ = io.WriteString(w, `{"usage":{"input_tokens":5,"output_tokens":5}}`)
	}))
	defer upstream.Close()
	st := openStore(t)
	b := New(st, []Upstream{{Name: "p", BaseURL: upstream.URL, Header: "Authorization", Value: "real"}})
	b.Usage = usage.New(st)
	b.Limits = usage.Limits{TokensPerDay: 100}
	token, _ := b.Mint(context.Background(), "concurrent-streams")
	front := httptest.NewServer(b)
	defer front.Close()
	start := make(chan struct{})
	statuses := make(chan int, 2)
	for range 2 {
		go func() {
			<-start
			req, _ := http.NewRequest(http.MethodPost, front.URL+"/p/v1", strings.NewReader(`{"max_tokens":60}`))
			req.Header.Set("Authorization", "Bearer "+token)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				statuses <- 0
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			statuses <- resp.StatusCode
		}()
	}
	close(start)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("no upstream request")
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	a, z := <-statuses, <-statuses
	if (a == http.StatusOK) == (z == http.StatusOK) || (a == http.StatusTooManyRequests) == (z == http.StatusTooManyRequests) {
		t.Fatalf("statuses=%d,%d", a, z)
	}
	mu.Lock()
	gotCalls := calls
	mu.Unlock()
	if gotCalls != 1 {
		t.Fatalf("upstream calls=%d", gotCalls)
	}
	if len(upstreamBodies) != 1 || upstreamBodies[0] != `{"max_tokens":60}` {
		t.Fatalf("upstream request bodies=%q", upstreamBodies)
	}
}

func TestConcurrentReservationsIncludePromptUpperBound(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, `{"usage":{"input_tokens":50,"output_tokens":10}}`)
	}))
	defer upstream.Close()
	st := openStore(t)
	b := New(st, []Upstream{{Name: "p", BaseURL: upstream.URL, Header: "Authorization", Value: "real"}})
	b.Usage = usage.New(st)
	b.Limits = usage.Limits{TokensPerDay: 180}
	token, _ := b.Mint(context.Background(), "large-prompts")
	front := httptest.NewServer(b)
	defer front.Close()
	body := `{"prompt":"` + strings.Repeat("x", 70) + `","max_tokens":40}`
	statuses := make(chan int, 2)
	start := make(chan struct{})
	for range 2 {
		go func() {
			<-start
			req, _ := http.NewRequest(http.MethodPost, front.URL+"/p/v1", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+token)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				statuses <- 0
				return
			}
			_ = resp.Body.Close()
			statuses <- resp.StatusCode
		}()
	}
	close(start)
	a, z := <-statuses, <-statuses
	if (a == http.StatusOK) == (z == http.StatusOK) || (a == http.StatusTooManyRequests) == (z == http.StatusTooManyRequests) {
		t.Fatalf("statuses=%d,%d", a, z)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls=%d", got)
	}
}

func TestAbortedOrUnmeteredResponseKeepsReservationButActualUsageReconciles(t *testing.T) {
	var responseMode string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch responseMode {
		case "actual":
			_, _ = io.WriteString(w, `{"usage":{"input_tokens":4,"output_tokens":6}}`)
		default:
			w.WriteHeader(http.StatusBadGateway)
		}
	}))
	defer upstream.Close()
	newRuntime := func(session string) (*Broker, string, *httptest.Server) {
		st := openStore(t)
		b := New(st, []Upstream{{Name: "p", BaseURL: upstream.URL, Header: "Authorization", Value: "real"}})
		b.Usage = usage.New(st)
		b.Limits = usage.Limits{TokensPerDay: 100}
		token, _ := b.Mint(context.Background(), session)
		return b, token, httptest.NewServer(b)
	}
	do := func(front *httptest.Server, token, body string) int {
		req, _ := http.NewRequest(http.MethodPost, front.URL+"/p/v1", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	responseMode = "abort"
	_, token, front := newRuntime("aborted")
	if do(front, token, `{"max_tokens":60}`) != http.StatusBadGateway {
		t.Fatal("aborted request status")
	}
	if do(front, token, `{"max_tokens":50}`) != http.StatusTooManyRequests {
		t.Fatal("aborted request released reservation")
	}
	front.Close()

	responseMode = "actual"
	_, token, front = newRuntime("reconciled")
	defer front.Close()
	if do(front, token, `{"max_tokens":80}`) != http.StatusOK {
		t.Fatal("first actual request")
	}
	if do(front, token, `{"max_completion_tokens":60}`) != http.StatusOK {
		t.Fatal("lower actual usage did not restore capacity")
	}

	responseMode = "abort"
	_, token, missingFront := newRuntime("missing-max")
	defer missingFront.Close()
	if do(missingFront, token, `{}`) != http.StatusBadGateway {
		t.Fatal("missing max first status")
	}
	if do(missingFront, token, `{"max_tokens":1}`) != http.StatusTooManyRequests {
		t.Fatal("missing max did not reserve remaining allowance")
	}
	_, token, invalidFront := newRuntime("invalid-max")
	defer invalidFront.Close()
	if do(invalidFront, token, `{"max_tokens":"many"}`) != http.StatusBadGateway {
		t.Fatal("invalid max first status")
	}
	if do(invalidFront, token, `{"max_tokens":1}`) != http.StatusTooManyRequests {
		t.Fatal("invalid max did not reserve remaining allowance")
	}
}

func TestClientAbortBeforeFinalUsageCannotEvadeReservation(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer func() {
		close(release)
		upstream.Close()
	}()
	st := openStore(t)
	b := New(st, []Upstream{{Name: "p", BaseURL: upstream.URL, Header: "Authorization", Value: "real"}})
	b.Usage = usage.New(st)
	b.Limits = usage.Limits{TokensPerDay: 100}
	token, _ := b.Mint(context.Background(), "client-abort")
	front := httptest.NewServer(b)
	defer front.Close()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, front.URL+"/p/v1", strings.NewReader(`{"max_tokens":60}`))
	req.Header.Set("Authorization", "Bearer "+token)
	done := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("upstream not started")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("client abort returned nil error")
		}
	case <-time.After(time.Second):
		t.Fatal("client abort did not return")
	}
	req, _ = http.NewRequest(http.MethodPost, front.URL+"/p/v1", strings.NewReader(`{"max_tokens":50}`))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("post-abort status=%d", resp.StatusCode)
	}
}

func TestPartialSSEUsageDoesNotReleaseReservation(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\n")
	}))
	defer upstream.Close()
	st := openStore(t)
	b := New(st, []Upstream{{Name: "p", BaseURL: upstream.URL, Header: "Authorization", Value: "real"}})
	b.Usage = usage.New(st)
	b.Limits = usage.Limits{TokensPerDay: 100}
	token, _ := b.Mint(context.Background(), "partial-sse")
	front := httptest.NewServer(b)
	defer front.Close()
	do := func(max int) int {
		req, _ := http.NewRequest(http.MethodPost, front.URL+"/p/v1", strings.NewReader(fmt.Sprintf(`{"max_tokens":%d}`, max)))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if got := do(80); got != http.StatusOK {
		t.Fatalf("partial stream status=%d", got)
	}
	if got := do(10); got != http.StatusTooManyRequests {
		t.Fatalf("post-partial status=%d, reservation was released", got)
	}
}

func TestExternalTokenSourceReservesRemainingAllowance(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer upstream.Close()
	st := openStore(t)
	b := New(st, []Upstream{{Name: "p", BaseURL: upstream.URL, Header: "Authorization", Value: "real"}})
	b.Usage = usage.New(st)
	b.Limits = usage.Limits{TokensPerDay: 100}
	token, _ := b.Mint(context.Background(), "external-input")
	front := httptest.NewServer(b)
	defer front.Close()
	do := func(body string) int {
		req, _ := http.NewRequest(http.MethodPost, front.URL+"/p/v1", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	imageRequest := `{"messages":[{"content":[{"type":"image_url","image_url":{"url":"https://example.com/large.png"}}]}],"max_tokens":10}`
	if got := do(imageRequest); got != http.StatusBadGateway {
		t.Fatalf("image request status=%d", got)
	}
	if got := do(`{"max_tokens":1}`); got != http.StatusTooManyRequests {
		t.Fatalf("post-image status=%d, external input did not reserve remaining allowance", got)
	}
}

// TestSandboxedRequestCarryingImageProxiesIntact re-verifies the broker's
// content-part inspection against real image traffic: a sandboxed request
// (wk_ session token, the only credential a sandbox holds) carrying a
// base64 image — in both the Anthropic image-block shape and the
// OpenAI-compatible data-URI image_url shape — is inspected, forwarded to
// the upstream with the payload intact, and treated as an external token
// source (remaining allowance reserved, since the image is not counted by
// the textual upper bound).
func TestSandboxedRequestCarryingImageProxiesIntact(t *testing.T) {
	imgData := base64.StdEncoding.EncodeToString([]byte("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJ fake png bytes"))
	for name, body := range map[string]string{
		"anthropic image block": `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"what is this?"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + imgData + `"}}]}],"max_tokens":10}`,
		"openai data uri":       `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"what is this?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,` + imgData + `"}}]}],"max_tokens":10}`,
	} {
		t.Run(name, func(t *testing.T) {
			var gotBody []byte
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotBody, _ = io.ReadAll(r.Body)
				w.WriteHeader(http.StatusBadGateway)
			}))
			defer upstream.Close()
			st := openStore(t)
			b := New(st, []Upstream{{Name: "p", BaseURL: upstream.URL, Header: "Authorization", Value: "real"}})
			b.Usage = usage.New(st)
			b.Limits = usage.Limits{TokensPerDay: 100}
			token, _ := b.Mint(context.Background(), "sandboxed-session")
			front := httptest.NewServer(b)
			defer front.Close()

			req, _ := http.NewRequest(http.MethodPost, front.URL+"/p/v1", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+token)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusBadGateway {
				t.Fatalf("status = %d, want proxied upstream response (502)", resp.StatusCode)
			}
			// The forwarded request carries the image payload untouched.
			if string(gotBody) != body {
				t.Fatalf("upstream body mangled:\n got %s\nwant %s", gotBody, body)
			}
			// Image input is not bounded by the text upper bound: the full
			// allowance was reserved, so a follow-up request is refused.
			follow, _ := http.NewRequest(http.MethodPost, front.URL+"/p/v1", strings.NewReader(`{"max_tokens":1}`))
			follow.Header.Set("Authorization", "Bearer "+token)
			fr, err := http.DefaultClient.Do(follow)
			if err != nil {
				t.Fatal(err)
			}
			_ = fr.Body.Close()
			if fr.StatusCode != http.StatusTooManyRequests {
				t.Fatalf("post-image status = %d, want 429 (remaining allowance reserved)", fr.StatusCode)
			}
		})
	}
}

// TestLargeImageBodyBeyondInspectCapProxiesUntruncated covers the broker
// boundary against a real large image: a payload larger than the 1 MiB
// inspection prefix must still be forwarded in full (the prefix-restore
// path), never truncated into invalid base64, and it must reserve the
// remaining allowance like any other external token source.
func TestLargeImageBodyBeyondInspectCapProxiesUntruncated(t *testing.T) {
	// ~2.5 MiB of base64 image data: comfortably past maxRequestInspectBytes.
	imgData := base64.StdEncoding.EncodeToString(make([]byte, 2*1024*1024))
	body := `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,` + imgData + `"}}]}],"max_tokens":10}`

	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer upstream.Close()
	st := openStore(t)
	b := New(st, []Upstream{{Name: "p", BaseURL: upstream.URL, Header: "Authorization", Value: "real"}})
	b.Usage = usage.New(st)
	b.Limits = usage.Limits{TokensPerDay: 1000}
	token, _ := b.Mint(context.Background(), "large-image-session")
	front := httptest.NewServer(b)
	defer front.Close()

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/p/v1", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if string(gotBody) != body {
		t.Fatalf("large image body truncated or mangled: got %d bytes, want %d", len(gotBody), len(body))
	}
	follow, _ := http.NewRequest(http.MethodPost, front.URL+"/p/v1", strings.NewReader(`{"max_tokens":1}`))
	follow.Header.Set("Authorization", "Bearer "+token)
	fr, err := http.DefaultClient.Do(follow)
	if err != nil {
		t.Fatal(err)
	}
	_ = fr.Body.Close()
	if fr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("post-large-image status = %d, want 429", fr.StatusCode)
	}
}

func TestProviderSideContextReferencesReserveRemainingAllowance(t *testing.T) {
	for name, body := range map[string]string{
		"previous response":  `{"previous_response_id":"resp_123","max_tokens":10}`,
		"conversation":       `{"conversation":"conv_123","max_tokens":10}`,
		"hosted file search": `{"tools":[{"type":"file_search","vector_store_ids":["vs_123"]}],"max_tokens":10}`,
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/p/v1", strings.NewReader(body))
			declared, reserveRemaining, err := inspectDeclaredTokenMaximum(req)
			if err != nil {
				t.Fatal(err)
			}
			if !reserveRemaining || declared != 0 {
				t.Fatalf("declared=%d reserveRemaining=%v", declared, reserveRemaining)
			}
		})
	}
}

func TestSelfContainedToolResultsAndTextURLsUseBoundedReservation(t *testing.T) {
	for name, body := range map[string]string{
		"anthropic tool result": `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_123","content":"done"}]}],"max_tokens":10}`,
		"plain text URL":        `{"messages":[{"role":"user","content":"read https://example.com as plain text"}],"max_tokens":10}`,
		"normal tool choice":    `{"messages":[{"role":"user","content":"use a tool"}],"tool_choice":{"type":"auto"},"max_tokens":10}`,
		"JSON response format":  `{"messages":[{"role":"user","content":"return JSON"}],"response_format":{"type":"json_object"},"max_tokens":10}`,
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/p/v1", strings.NewReader(body))
			declared, reserveRemaining, err := inspectDeclaredTokenMaximum(req)
			if err != nil {
				t.Fatal(err)
			}
			if reserveRemaining || declared != 10+len(body) {
				t.Fatalf("declared=%d reserveRemaining=%v want=%d,false", declared, reserveRemaining, 10+len(body))
			}
		})
	}
}

func TestProxyAccountsStreamingUsageOnce(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":9,\"output_tokens\":0}}}\n\n")
		// Usage commonly arrives only in the final event. Keep it accountable
		// even after a response much larger than the bounded JSON capture.
		filler := strings.Repeat("x", 1024)
		for range maxUsageCaptureBytes/len(filler) + 2 {
			_, _ = io.WriteString(w, "data: {\"delta\":\""+filler+"\"}\n\n")
		}
		_, _ = io.WriteString(w, "data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":4}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()
	st := openStore(t)
	b := New(st, []Upstream{{Name: "anthropic", BaseURL: upstream.URL, Header: "x-api-key", Value: "real"}})
	b.Usage = usage.New(st)
	b.Now = func() time.Time { return time.Date(2026, 7, 13, 8, 0, 0, 0, time.UTC) }
	token, _ := b.Mint(context.Background(), "stream-budget")
	front := httptest.NewServer(b)
	defer front.Close()
	req, _ := http.NewRequest(http.MethodPost, front.URL+"/anthropic/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	day := waitDayRows(t, b, "stream-budget", func(rows []usage.Row) bool {
		return len(rows) == 1 && rows[0].Requests == 1 && rows[0].InputTokens+rows[0].OutputTokens == 13
	})
	if len(day) != 1 || day[0].Requests != 1 || day[0].InputTokens+day[0].OutputTokens != 13 {
		t.Fatalf("requests=%d tokens=%d rows=%+v", day[0].Requests, day[0].InputTokens+day[0].OutputTokens, day)
	}
}

// TestAnthropicJSONCacheUsageBindsOnTrueCost pins the accounting half of
// prompt caching (#247): the persisted row carries raw uncached/cache-write/
// cache-read counters, and the budget binds on true cost (10 + 20*1.25 +
// 30*0.1 + 5 = 43 billed tokens) rather than the pre-cache total (65), so a
// follow-up that would have been refused under the old arithmetic is allowed.
func TestAnthropicJSONCacheUsageBindsOnTrueCost(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"usage":{"input_tokens":10,"cache_creation_input_tokens":20,"cache_read_input_tokens":30,"output_tokens":5}}`)
	}))
	defer upstream.Close()
	st := openStore(t)
	b := New(st, []Upstream{{Name: "anthropic", BaseURL: upstream.URL, Header: "x-api-key", Value: "real"}})
	b.Usage = usage.New(st)
	b.Limits = usage.Limits{TokensPerDay: 100}
	token, _ := b.Mint(context.Background(), "anthropic-json-cache")
	front := httptest.NewServer(b)
	defer front.Close()
	do := func(max int) int {
		req, _ := http.NewRequest(http.MethodPost, front.URL+"/anthropic/v1/messages", strings.NewReader(fmt.Sprintf(`{"max_tokens":%d}`, max)))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if got := do(70); got != http.StatusOK {
		t.Fatalf("first status=%d", got)
	}
	// The first request's counters reconcile as raw 10/20/30/5.
	day := waitDayRows(t, b, "anthropic-json-cache", func(rows []usage.Row) bool {
		return len(rows) == 1 && rows[0].InputTokens == 10 && rows[0].CacheCreationInputTokens == 20 && rows[0].CacheReadInputTokens == 30 && rows[0].OutputTokens == 5 && rows[0].ReservedTokens == 0
	})
	if len(day) != 1 || day[0].InputTokens != 10 || day[0].CacheCreationInputTokens != 20 || day[0].CacheReadInputTokens != 30 || day[0].OutputTokens != 5 || day[0].ReservedTokens != 0 {
		t.Fatalf("day row = %+v, want raw counters 10/20/30/5 reserved 0", day[0])
	}
	// Declared 46 (max_tokens 30 + 16-byte body): naive total 65+46=111 would
	// exceed the 100-token budget; true cost 43+46=89 must pass.
	if got := do(30); got != http.StatusOK {
		t.Fatalf("cached follow-up status=%d, want OK under true-cost binding", got)
	}
	// Declared 76 (max_tokens 58 + 18-byte body): 43+76=119 > 100 even at
	// true cost, so the budget still binds.
	if got := do(58); got != http.StatusTooManyRequests {
		t.Fatalf("over-budget status=%d, want 429", got)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls=%d", got)
	}
}

// TestAnthropicSSECacheUsageBindsOnTrueCost is the SSE twin of
// TestAnthropicJSONCacheUsageBindsOnTrueCost: the cache counters are split
// out of the streamed message_start/message_delta usage objects and the
// budget binds on the same true cost as the JSON path.
func TestAnthropicSSECacheUsageBindsOnTrueCost(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10,\"cache_creation_input_tokens\":20,\"output_tokens\":0}}}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"message_delta\",\"usage\":{\"cache_read_input_tokens\":30,\"output_tokens\":5}}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
	}))
	defer upstream.Close()
	st := openStore(t)
	b := New(st, []Upstream{{Name: "anthropic", BaseURL: upstream.URL, Header: "x-api-key", Value: "real"}})
	b.Usage = usage.New(st)
	b.Limits = usage.Limits{TokensPerDay: 100}
	token, _ := b.Mint(context.Background(), "anthropic-sse-cache")
	front := httptest.NewServer(b)
	defer front.Close()
	do := func(max int) int {
		req, _ := http.NewRequest(http.MethodPost, front.URL+"/anthropic/v1/messages", strings.NewReader(fmt.Sprintf(`{"max_tokens":%d}`, max)))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if got := do(70); got != http.StatusOK {
		t.Fatalf("first status=%d", got)
	}
	// The streamed message_start/message_delta usage reconciles as raw
	// 10/20/30/5, byte-identical to the JSON path.
	day := waitDayRows(t, b, "anthropic-sse-cache", func(rows []usage.Row) bool {
		return len(rows) == 1 && rows[0].InputTokens == 10 && rows[0].CacheCreationInputTokens == 20 && rows[0].CacheReadInputTokens == 30 && rows[0].OutputTokens == 5 && rows[0].ReservedTokens == 0
	})
	if len(day) != 1 || day[0].InputTokens != 10 || day[0].CacheCreationInputTokens != 20 || day[0].CacheReadInputTokens != 30 || day[0].OutputTokens != 5 || day[0].ReservedTokens != 0 {
		t.Fatalf("day row = %+v, want raw counters 10/20/30/5 reserved 0", day[0])
	}
	// Naive total 65+46=111 would exceed the 100-token budget; true cost
	// 43+46=89 must pass.
	if got := do(30); got != http.StatusOK {
		t.Fatalf("cached follow-up status=%d, want OK under true-cost binding", got)
	}
	if got := do(58); got != http.StatusTooManyRequests {
		t.Fatalf("over-budget status=%d, want 429", got)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls=%d", got)
	}
}

func TestProviderUsageCacheFieldsSplitAndSaturate(t *testing.T) {
	// OpenAI-compatible aliases: prompt_tokens wins over input_tokens, and
	// no cache fields means zeroed cache counters.
	openAI := parseProviderUsage([]byte(`{"usage":{"input_tokens":7,"prompt_tokens":9,"completion_tokens":5}}`))
	if openAI != (llm.Usage{InputTokens: 9, OutputTokens: 5}) {
		t.Fatalf("OpenAI aliases changed: %+v", openAI)
	}
	// OpenAI-compatible cached_tokens: prompt_tokens includes the cached
	// subset, which is split out into CacheReadInputTokens.
	cached := parseProviderUsage([]byte(`{"usage":{"prompt_tokens":10,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":3}}}`))
	if cached != (llm.Usage{InputTokens: 7, OutputTokens: 5, CacheReadInputTokens: 3}) {
		t.Fatalf("OpenAI cached split failed: %+v", cached)
	}
	// Anthropic disjoint fields: input_tokens stays uncached, cache fields
	// are carried separately.
	anthropic := parseProviderUsage([]byte(`{"usage":{"input_tokens":10,"cache_creation_input_tokens":20,"cache_read_input_tokens":30,"output_tokens":5}}`))
	if anthropic != (llm.Usage{InputTokens: 10, OutputTokens: 5, CacheCreationInputTokens: 20, CacheReadInputTokens: 30}) {
		t.Fatalf("Anthropic cache fields failed: %+v", anthropic)
	}
	// A provider response with no usage object yields zeroed counters —
	// never a panic, never a negative total.
	if got := parseProviderUsage([]byte(`{"id":"msg_1"}`)); got != (llm.Usage{}) {
		t.Fatalf("missing usage = %+v, want zeroed", got)
	}
	if got := parseProviderUsage([]byte(`{"usage":{"prompt_tokens":5,"prompt_tokens_details":{"cached_tokens":9}}}`)); got.InputTokens != 0 || got.CacheReadInputTokens != 9 {
		t.Fatalf("over-reported cached tokens went negative: %+v", got)
	}
	maxInt := int(^uint(0) >> 1)
	saturated := parseProviderUsage([]byte(`{"usage":{"input_tokens":9e100,"cache_creation_input_tokens":9e100,"cache_read_input_tokens":9e100,"output_tokens":1}}`))
	if saturated != (llm.Usage{InputTokens: maxInt, OutputTokens: 1, CacheCreationInputTokens: maxInt, CacheReadInputTokens: maxInt}) {
		t.Fatalf("Anthropic saturation failed: %+v", saturated)
	}
}

// TestParseTrailingUsageCarriesCacheFields pins the large-JSON tail path for
// both wire dialects (#247).
func TestParseTrailingUsageCarriesCacheFields(t *testing.T) {
	anthropic := parseTrailingUsage([]byte(`{"id":"msg_1","content":[],"usage":{"input_tokens":10,"cache_creation_input_tokens":20,"cache_read_input_tokens":30,"output_tokens":5}}`))
	if anthropic != (llm.Usage{InputTokens: 10, OutputTokens: 5, CacheCreationInputTokens: 20, CacheReadInputTokens: 30}) {
		t.Fatalf("anthropic tail = %+v", anthropic)
	}
	openai := parseTrailingUsage([]byte(`{"id":"x","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":3}}}`))
	if openai != (llm.Usage{InputTokens: 7, OutputTokens: 5, CacheReadInputTokens: 3}) {
		t.Fatalf("openai tail = %+v", openai)
	}
	if got := parseTrailingUsage([]byte(`{"id":"x"}`)); got != (llm.Usage{}) {
		t.Fatalf("tail without usage = %+v, want zeroed", got)
	}
}

// TestSandboxedMetersByteIdenticallyToHost pins that the broker's parse
// paths split cache fields exactly as the translators do, so sandboxed
// traffic meters identically to host traffic for the same wire usage. The
// fixtures are byte-identical to the ones the translator tests consume
// (anthropicp: message usage object; openaip: final stream chunk).
func TestSandboxedMetersByteIdenticallyToHost(t *testing.T) {
	// Anthropic wire shape — anthropicp.fromMessage maps the same object to
	// llm.Usage{InputTokens:10, OutputTokens:5, CacheCreationInputTokens:20,
	// CacheReadInputTokens:30}.
	anthropicWire := `{"usage":{"input_tokens":10,"cache_creation_input_tokens":20,"cache_read_input_tokens":30,"output_tokens":5}}`
	if got := parseProviderUsage([]byte(anthropicWire)); got != (llm.Usage{InputTokens: 10, OutputTokens: 5, CacheCreationInputTokens: 20, CacheReadInputTokens: 30}) {
		t.Fatalf("anthropic metering diverged from host translator: %+v", got)
	}
	if got := parseTrailingUsage([]byte(`{"id":"m","content":[],` + anthropicWire[1:])); got != (llm.Usage{InputTokens: 10, OutputTokens: 5, CacheCreationInputTokens: 20, CacheReadInputTokens: 30}) {
		t.Fatalf("anthropic tail metering diverged from host translator: %+v", got)
	}
	// OpenAI wire shape — openaip.readStream maps the same final chunk to
	// llm.Usage{InputTokens:7, OutputTokens:5, CacheReadInputTokens:3}.
	openaiWire := `{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":3}}}`
	if got := parseProviderUsage([]byte(openaiWire)); got != (llm.Usage{InputTokens: 7, OutputTokens: 5, CacheReadInputTokens: 3}) {
		t.Fatalf("openai metering diverged from host translator: %+v", got)
	}
}

// TestSSECacheUsageSplitAcrossEvents pins the incremental accumulation: the
// cache-creation count arrives on message_start and the cache-read count on
// message_delta, and providerUsage merges them per-field.
func TestSSECacheUsageSplitAcrossEvents(t *testing.T) {
	recorder := httptest.NewRecorder()
	recorder.Header().Set("Content-Type", "text/event-stream")
	w := &usageResponseWriter{ResponseWriter: recorder}
	for _, line := range []string{
		"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10,\"cache_creation_input_tokens\":20,\"output_tokens\":0}}}\n\n",
		"data: {\"type\":\"message_delta\",\"usage\":{\"cache_read_input_tokens\":30,\"output_tokens\":5}}\n\n",
		"data: {\"type\":\"message_stop\"}\n\n",
	} {
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	if got := w.providerUsage(); got != (llm.Usage{InputTokens: 10, OutputTokens: 5, CacheCreationInputTokens: 20, CacheReadInputTokens: 30}) {
		t.Fatalf("sse usage = %+v", got)
	}
}

// TestPartialSSECacheUsageDoesNotRecordPartialCounts pins the cancellation
// contract: a stream aborted mid-flight (here after message_start, which
// already carried a cache-creation count) must not record a partial cache
// count as if complete — the reservation stays charged instead.
func TestPartialSSECacheUsageDoesNotRecordPartialCounts(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10,\"cache_creation_input_tokens\":20,\"output_tokens\":0}}}\n\n")
	}))
	defer upstream.Close()
	st := openStore(t)
	b := New(st, []Upstream{{Name: "p", BaseURL: upstream.URL, Header: "Authorization", Value: "real"}})
	b.Usage = usage.New(st)
	b.Limits = usage.Limits{TokensPerDay: 100}
	token, _ := b.Mint(context.Background(), "partial-sse-cache")
	front := httptest.NewServer(b)
	defer front.Close()
	do := func(max int) int {
		req, _ := http.NewRequest(http.MethodPost, front.URL+"/p/v1", strings.NewReader(fmt.Sprintf(`{"max_tokens":%d}`, max)))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if got := do(80); got != http.StatusOK {
		t.Fatalf("partial stream status=%d", got)
	}
	// The reservation (80 + 17-byte body = 97) is retained; nothing was
	// recorded as final, so no cache tokens leaked into the day total.
	rows, err := b.Usage.List(context.Background(), "partial-sse-cache")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Period == "day" {
			if row.InputTokens != 0 || row.CacheCreationInputTokens != 0 || row.CacheReadInputTokens != 0 || row.OutputTokens != 0 || row.ReservedTokens != 97 {
				t.Fatalf("day row = %+v, want zeroed counters with reservation retained", row)
			}
		}
	}
	if got := do(1); got != http.StatusTooManyRequests {
		t.Fatalf("post-partial status=%d, reservation was released", got)
	}
}

func TestSSEResponseWriterDoesNotRetainResponseBody(t *testing.T) {
	recorder := httptest.NewRecorder()
	recorder.Header().Set("Content-Type", "text/event-stream")
	w := &usageResponseWriter{ResponseWriter: recorder}
	filler := []byte("data: {\"delta\":\"" + strings.Repeat("x", 1024) + "\"}\n\n")
	for range maxUsageCaptureBytes/len(filler) + 100 {
		if _, err := w.Write(filler); err != nil {
			t.Fatal(err)
		}
	}
	_, _ = w.Write([]byte("data: {\"usage\":{\"input_tokens\":2,\"output_tokens\":3}}\n\n"))
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	if w.body.Len() != 0 || len(w.tail) != 0 {
		t.Fatalf("SSE retained body prefix=%d tail=%d", w.body.Len(), len(w.tail))
	}
	if len(w.ssePending) != 0 {
		t.Fatalf("SSE pending bytes=%d", len(w.ssePending))
	}
	if got := w.providerUsage(); got.InputTokens != 2 || got.OutputTokens != 3 {
		t.Fatalf("usage=%+v", got)
	}
}

func TestSSECompletionWithoutFinalUsageKeepsReservation(t *testing.T) {
	recorder := httptest.NewRecorder()
	recorder.Header().Set("Content-Type", "text/event-stream")
	w := &usageResponseWriter{ResponseWriter: recorder}
	_, _ = w.Write([]byte("data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":9,\"output_tokens\":0}}}\n\n"))
	_, _ = w.Write([]byte("data: {\"type\":\"message_stop\"}\n\n"))
	if got := w.providerUsage(); got.InputTokens != 0 || got.OutputTokens != 0 {
		t.Fatalf("incomplete final usage was trusted: %+v", got)
	}
}

func TestProxyAccountsTrailingUsageInLargeJSONResponse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"`)
		_, _ = io.WriteString(w, strings.Repeat("x", maxUsageCaptureBytes+1024))
		_, _ = io.WriteString(w, `"}}],"usage":{"input_tokens":11,"output_tokens":6}}`)
	}))
	defer upstream.Close()
	st := openStore(t)
	b := New(st, []Upstream{{Name: "openai", BaseURL: upstream.URL, Header: "Authorization", Value: "Bearer real"}})
	b.Usage = usage.New(st)
	token, _ := b.Mint(context.Background(), "large-json")
	front := httptest.NewServer(b)
	defer front.Close()
	req, _ := http.NewRequest(http.MethodPost, front.URL+"/openai/v1/chat/completions", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	day := waitDayRows(t, b, "large-json", func(rows []usage.Row) bool {
		var tokens int
		for _, r := range rows {
			tokens += r.InputTokens + r.OutputTokens
		}
		return tokens == 17
	})
	var tokens int
	for _, r := range day {
		tokens += r.InputTokens + r.OutputTokens
	}
	if tokens != 17 {
		t.Fatalf("tokens=%d rows=%+v", tokens, day)
	}
}

func TestScopedTokenUsesItsGroupLimitAndBudgetKey(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"usage":{"input_tokens":3,"output_tokens":2}}`)
	}))
	defer upstream.Close()
	st := openStore(t)
	b := New(st, []Upstream{{Name: "p", BaseURL: upstream.URL, Header: "Authorization", Value: "real"}})
	b.Usage = usage.New(st)
	b.Limits = usage.Limits{TokensPerDay: 100}
	scoped, err := b.MintScoped(context.Background(), "session-issue", "issue-budget", usage.Limits{TokensPerDay: 5})
	if err != nil {
		t.Fatal(err)
	}
	mainToken, err := b.Mint(context.Background(), "session-main")
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(b)
	defer front.Close()
	do := func(token string) int {
		req, _ := http.NewRequest(http.MethodPost, front.URL+"/p/v1", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if do(scoped) != http.StatusOK {
		t.Fatal("issue first request failed")
	}
	scopedRetry, err := b.MintScoped(context.Background(), "session-issue-retry", "issue-budget", usage.Limits{TokensPerDay: 5})
	if err != nil {
		t.Fatal(err)
	}
	if do(scopedRetry) != http.StatusTooManyRequests {
		t.Fatal("issue override was not enforced")
	}
	mainFirst, mainSecond := do(mainToken), do(mainToken)
	if mainFirst != http.StatusOK || mainSecond != http.StatusOK {
		t.Fatal("main default was incorrectly tightened")
	}
	rows, err := b.Usage.List(context.Background(), "issue-budget")
	if err != nil || len(rows) == 0 {
		t.Fatalf("stable scoped budget rows=%v err=%v", rows, err)
	}
}

func TestGitCredentialDenialAuditNamesRequestedAndBoundRepo(t *testing.T) {
	st := openStore(t)
	b := New(st, nil)
	b.GitCredential = func(context.Context, string, string, string) (string, string, error) {
		return "", "", fmt.Errorf("repository outside session scope")
	}
	b.BindGitRepo("sess-a", "owner/A")
	token, err := b.Mint(context.Background(), "sess-a")
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(b)
	defer front.Close()
	req, _ := http.NewRequest(http.MethodPost, front.URL+"/git-credential",
		strings.NewReader("protocol=https\nhost=github.com\npath=owner/B.git\n"))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var sessionID, action, detail string
	if err := st.DB.QueryRow(`SELECT session, action, detail FROM broker_audit WHERE action='denied' ORDER BY id DESC LIMIT 1`).Scan(&sessionID, &action, &detail); err != nil {
		t.Fatal(err)
	}
	if sessionID != "sess-a" || action != "denied" || !strings.Contains(detail, "requested=owner/B.git") || !strings.Contains(detail, "bound=owner/A") {
		t.Fatalf("denial audit session=%q action=%q detail=%q", sessionID, action, detail)
	}

	unboundToken, err := b.Mint(context.Background(), "sess-unbound")
	if err != nil {
		t.Fatal(err)
	}
	unboundReq, _ := http.NewRequest(http.MethodPost, front.URL+"/git-credential",
		strings.NewReader("protocol=https\nhost=github.com\npath=owner/A\n"))
	unboundReq.Header.Set("Authorization", "Bearer "+unboundToken)
	unboundResp, err := http.DefaultClient.Do(unboundReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = unboundResp.Body.Close()
	if err := st.DB.QueryRow(`SELECT session, detail FROM broker_audit WHERE action='denied' ORDER BY id DESC LIMIT 1`).Scan(&sessionID, &detail); err != nil {
		t.Fatal(err)
	}
	if sessionID != "sess-unbound" || !strings.Contains(detail, "requested=owner/A") || !strings.Contains(detail, "bound=") {
		t.Fatalf("unbound denial audit session=%q detail=%q", sessionID, detail)
	}
}

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
		_, _ = io.WriteString(w, `{"ok":true}`) // intentional discard in test handler
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
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close body: %v", err)
		}
	}()
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
	if err := resp.Body.Close(); err != nil {
		t.Errorf("close body: %v", err)
	}
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
	if err := resp.Body.Close(); err != nil {
		t.Errorf("close body: %v", err)
	}
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
	if err := resp.Body.Close(); err != nil {
		t.Errorf("close body: %v", err)
	}
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
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return st
}

func TestGitRepoScopeBindReadRevoke(t *testing.T) {
	ctx := context.Background()
	b := New(nil, nil)

	if _, ok := b.GitRepoScope("s1"); ok {
		t.Fatal("unbound session reports a scope")
	}

	b.BindGitRepo("s1", "owner/A")
	if repo, ok := b.GitRepoScope("s1"); !ok || repo != "owner/A" {
		t.Fatalf("GitRepoScope after bind = (%q, %v), want (owner/A, true)", repo, ok)
	}

	// A token re-mint (resume) must not drop the binding.
	if _, err := b.Mint(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.GitRepoScope("s1"); !ok {
		t.Fatal("binding lost after re-mint")
	}

	// Ending the session clears the binding.
	b.RevokeSession("s1")
	if _, ok := b.GitRepoScope("s1"); ok {
		t.Fatal("binding survived RevokeSession")
	}
}

func TestTokenExpiresOnProxyAndGitCredential(t *testing.T) {
	// AC1: expired tokens stop authorizing both faces.
	// AC4: expiry driven by b.now().
	clock := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	st := openStore(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer upstream.Close()

	b := New(st, []Upstream{{Name: "x", BaseURL: upstream.URL, Header: "x-api-key", Value: "k"}})
	b.TokenTTL = time.Hour
	b.Now = func() time.Time { return clock }
	b.GitCredential = func(context.Context, string, string, string) (string, string, error) {
		return "user", "pass", nil
	}
	b.BindGitRepo("sess", "owner/A")

	token, err := b.Mint(context.Background(), "sess")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	front := httptest.NewServer(b)
	defer front.Close()

	// Still valid before TTL.
	req, _ := http.NewRequest(http.MethodPost, front.URL+"/x/v1/thing", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pre-expiry proxy status=%d", resp.StatusCode)
	}

	// Advance past TTL.
	clock = clock.Add(time.Hour + time.Second)

	req, _ = http.NewRequest(http.MethodPost, front.URL+"/x/v1/thing", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("post-expiry proxy status=%d, want 401", resp.StatusCode)
	}

	// Re-mint so git face has a live session binding only after renewal path;
	// this request uses the expired token (already swept on prior lookup).
	req, _ = http.NewRequest(http.MethodPost, front.URL+"/git-credential",
		strings.NewReader("protocol=https\nhost=github.com\npath=owner/A.git\n"))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("post-expiry git-credential status=%d, want 401", resp.StatusCode)
	}
}

func TestExpiredTokenSweepsSessionMaps(t *testing.T) {
	// AC2: expired entries leave tokens/sessions/gitScope/limits/budgets.
	clock := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	b := New(nil, nil)
	b.TokenTTL = time.Minute
	b.Now = func() time.Time { return clock }
	b.BindGitRepo("sess", "owner/A")

	token, err := b.MintScoped(context.Background(), "sess", "budget-k", usage.Limits{TokensPerDay: 9})
	if err != nil {
		t.Fatalf("MintScoped: %v", err)
	}
	if got := b.session(token); got != "sess" {
		t.Fatalf("session before expiry = %q", got)
	}
	if repo, ok := b.GitRepoScope("sess"); !ok || repo != "owner/A" {
		t.Fatalf("git scope before expiry = (%q, %v)", repo, ok)
	}

	clock = clock.Add(time.Minute + time.Nanosecond)
	if got := b.session(token); got != "" {
		t.Fatalf("session after expiry = %q, want empty", got)
	}

	b.mu.Lock()
	_, tokenPresent := b.tokens[token]
	_, sessionPresent := b.sessions["sess"]
	_, gitPresent := b.gitScope["sess"]
	_, limitsPresent := b.limits["sess"]
	_, budgetPresent := b.budgets["sess"]
	b.mu.Unlock()
	if tokenPresent || sessionPresent || gitPresent || limitsPresent || budgetPresent {
		t.Fatalf("maps after expiry: token=%v session=%v git=%v limits=%v budget=%v",
			tokenPresent, sessionPresent, gitPresent, limitsPresent, budgetPresent)
	}
}

func TestRemintAfterExpiryAuthorizesAgain(t *testing.T) {
	// AC3: session outliving TTL continues via re-mint.
	clock := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer upstream.Close()

	b := New(nil, []Upstream{{Name: "x", BaseURL: upstream.URL, Header: "x-api-key", Value: "k"}})
	b.TokenTTL = time.Hour
	b.Now = func() time.Time { return clock }

	first, err := b.Mint(context.Background(), "sess")
	if err != nil {
		t.Fatalf("first Mint: %v", err)
	}
	clock = clock.Add(2 * time.Hour)
	if got := b.session(first); got != "" {
		t.Fatalf("expired first token still resolves to %q", got)
	}

	second, err := b.Mint(context.Background(), "sess")
	if err != nil {
		t.Fatalf("re-mint: %v", err)
	}
	if second == first {
		t.Fatal("re-mint returned the same token string")
	}
	if got := b.session(second); got != "sess" {
		t.Fatalf("fresh token session = %q", got)
	}

	front := httptest.NewServer(b)
	defer front.Close()
	req, _ := http.NewRequest(http.MethodPost, front.URL+"/x/v1/thing", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+second)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("re-minted token status=%d", resp.StatusCode)
	}
}

func TestExpiredVsUnknownAuditActions(t *testing.T) {
	// AC5: rejected-because-expired is distinguishable from unknown.
	clock := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	st := openStore(t)
	b := New(st, []Upstream{{Name: "x", BaseURL: "http://127.0.0.1:1", Header: "x-api-key", Value: "k"}})
	b.TokenTTL = time.Hour
	b.Now = func() time.Time { return clock }
	front := httptest.NewServer(b)
	defer front.Close()

	// Unknown token.
	req, _ := http.NewRequest(http.MethodPost, front.URL+"/x/v1/thing", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer wk_unknown_token_xx")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unknown status=%d", resp.StatusCode)
	}
	var action, sessionID, detail string
	if err := st.DB.QueryRow(`SELECT action, session, detail FROM broker_audit WHERE action='denied' ORDER BY id DESC LIMIT 1`).Scan(&action, &sessionID, &detail); err != nil {
		t.Fatalf("unknown audit: %v", err)
	}
	if action != "denied" || sessionID != "" || !strings.Contains(detail, "/x/v1/thing") {
		t.Fatalf("unknown audit action=%q session=%q detail=%q", action, sessionID, detail)
	}

	// Expired token.
	token, err := b.Mint(context.Background(), "sess-exp")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	clock = clock.Add(time.Hour + time.Second)
	req, _ = http.NewRequest(http.MethodPost, front.URL+"/x/v1/thing", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expired status=%d", resp.StatusCode)
	}
	if err := st.DB.QueryRow(`SELECT action, session, detail FROM broker_audit WHERE action='expired' ORDER BY id DESC LIMIT 1`).Scan(&action, &sessionID, &detail); err != nil {
		t.Fatalf("expired audit: %v", err)
	}
	if action != "expired" || sessionID != "sess-exp" || !strings.Contains(detail, "/x/v1/thing") {
		t.Fatalf("expired audit action=%q session=%q detail=%q", action, sessionID, detail)
	}

	var deniedCount, expiredCount int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM broker_audit WHERE action='denied'`).Scan(&deniedCount); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM broker_audit WHERE action='expired'`).Scan(&expiredCount); err != nil {
		t.Fatal(err)
	}
	if deniedCount < 1 || expiredCount != 1 {
		t.Fatalf("audit counts denied=%d expired=%d", deniedCount, expiredCount)
	}
}

func TestTokenValidUntilExactExpiry(t *testing.T) {
	// Token authorizes while expiresAt.After(now); rejects at the exact instant.
	clock := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	b := New(nil, nil)
	b.TokenTTL = time.Hour
	b.Now = func() time.Time { return clock }

	token, err := b.Mint(context.Background(), "sess")
	if err != nil {
		t.Fatal(err)
	}
	// One nanosecond before expiry: still valid.
	clock = clock.Add(time.Hour - time.Nanosecond)
	if got := b.session(token); got != "sess" {
		t.Fatalf("just before expiry session=%q", got)
	}
	// Exact expiry: invalid.
	clock = clock.Add(time.Nanosecond)
	if got := b.session(token); got != "" {
		t.Fatalf("at exact expiry session=%q, want empty", got)
	}
}

// connectThroughBroker performs a CONNECT against the broker and returns the
// response line plus the hijacked connection when the tunnel is established.
func connectThroughBroker(t *testing.T, brokerAddr, authority, proxyAuth string) (string, net.Conn) {
	t.Helper()
	conn, err := net.Dial("tcp", brokerAddr)
	if err != nil {
		t.Fatal(err)
	}
	req := "CONNECT " + authority + " HTTP/1.1\r\nHost: " + authority + "\r\n"
	if proxyAuth != "" {
		req += "Proxy-Authorization: Basic " +
			base64.StdEncoding.EncodeToString([]byte(proxyAuth+":")) + "\r\n"
	}
	req += "\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil {
		_ = conn.Close()
		t.Fatal(err)
	}
	return strings.TrimSpace(status), conn
}

// HTTPS cannot be forward-proxied by URL rewriting, so without a tunnel every
// https:// fetch from a workspace fails -- git clone and every package manager.
func TestConnectTunnelsToAnAllowlistedHostEndToEnd(t *testing.T) {
	origin, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = origin.Close() }()
	go func() {
		conn, acceptErr := origin.Accept()
		if acceptErr != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		buf := make([]byte, 5)
		if _, readErr := io.ReadFull(conn, buf); readErr != nil {
			return
		}
		_, _ = conn.Write([]byte("pong:" + string(buf)))
	}()
	_, originPort, _ := net.SplitHostPort(origin.Addr().String())

	st := openStore(t)
	b := New(st, nil)
	token, err := b.Mint(context.Background(), "budget")
	if err != nil {
		t.Fatal(err)
	}
	// safeDialContext refuses private addresses, so the allowlisted host must
	// be the loopback origin reached by name; use the literal the dialer will
	// resolve, and assert the refusal separately below.
	b.SetEgress([]EgressTarget{{Host: "localhost", BaseURL: "http://localhost:" + originPort}})
	// The default dialer refuses private addresses, which a loopback origin is.
	// Overridden only to exercise the relay; the refusal itself is asserted in
	// TestConnectRefusesPrivateAddressesByDefault.
	b.DialEgress = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	front := httptest.NewServer(b)
	defer front.Close()
	addr := strings.TrimPrefix(front.URL, "http://")

	status, conn := connectThroughBroker(t, addr, "localhost:"+originPort, token)
	defer func() { _ = conn.Close() }()
	if !strings.Contains(status, "200") {
		t.Fatalf("CONNECT status = %q, want 200", status)
	}
	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 10)
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatalf("tunnel relay failed: %v", err)
	}
	if string(reply) != "pong:hello" {
		t.Fatalf("tunnel payload = %q, want %q", reply, "pong:hello")
	}
}

// curl and git only retry with credentials after a 407. A 401 surfaces as a
// hard "CONNECT tunnel failed, response 401" with credentials never sent.
func TestConnectWithoutCredentialsChallengesWith407(t *testing.T) {
	st := openStore(t)
	b := New(st, nil)
	front := httptest.NewServer(b)
	defer front.Close()
	addr := strings.TrimPrefix(front.URL, "http://")

	status, conn := connectThroughBroker(t, addr, "github.com:443", "")
	defer func() { _ = conn.Close() }()

	if !strings.Contains(status, "407") {
		t.Fatalf("CONNECT status = %q, want 407 so the client retries with credentials", status)
	}
}

func TestConnectRefusesHostsAndPortsOutsideTheAllowlist(t *testing.T) {
	st := openStore(t)
	b := New(st, nil)
	token, err := b.Mint(context.Background(), "budget")
	if err != nil {
		t.Fatal(err)
	}
	b.SetEgress([]EgressTarget{{Host: "github.com", BaseURL: "https://github.com"}})
	front := httptest.NewServer(b)
	defer front.Close()
	addr := strings.TrimPrefix(front.URL, "http://")

	for _, tc := range []struct {
		name      string
		authority string
	}{
		{"host not allowlisted", "evil.example:443"},
		{"port not allowlisted", "github.com:22"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, conn := connectThroughBroker(t, addr, tc.authority, token)
			defer func() { _ = conn.Close() }()
			if !strings.Contains(status, "403") {
				t.Fatalf("CONNECT status = %q, want 403", status)
			}
		})
	}
}

// The tunnel must not become a way to reach the host's own network. The
// rewriting path is protected by safeDialContext; the tunnel uses the same
// dialer by default, and this pins that it is not quietly bypassed.
func TestConnectRefusesPrivateAddressesByDefault(t *testing.T) {
	st := openStore(t)
	b := New(st, nil)
	token, err := b.Mint(context.Background(), "budget")
	if err != nil {
		t.Fatal(err)
	}
	if b.DialEgress != nil {
		t.Fatal("DialEgress must default to the private-address-refusing dialer")
	}
	b.SetEgress([]EgressTarget{{Host: "localhost", BaseURL: "https://localhost"}})
	front := httptest.NewServer(b)
	defer front.Close()

	status, conn := connectThroughBroker(t, strings.TrimPrefix(front.URL, "http://"), "localhost:443", token)
	defer func() { _ = conn.Close() }()

	if !strings.Contains(status, "502") {
		t.Fatalf("CONNECT status = %q, want 502: a private address must not be tunnelled", status)
	}
}

// A client that finishes writing before the origin has flushed its response
// must still receive the rest of it. Closing both sides when either direction
// ends would drop the in-flight bytes.
func TestConnectRelayHalfClosesSoLateResponsesSurvive(t *testing.T) {
	origin, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = origin.Close() }()
	body := strings.Repeat("late-response-", 512)
	go func() {
		conn, acceptErr := origin.Accept()
		if acceptErr != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		// Drain until the client half-closes, then reply after a pause. The
		// pause makes the failure deterministic: a relay that tears both sides
		// down when the first direction ends has already closed this socket.
		_, _ = io.Copy(io.Discard, conn)
		time.Sleep(100 * time.Millisecond)
		_, _ = io.WriteString(conn, body)
	}()
	_, originPort, _ := net.SplitHostPort(origin.Addr().String())

	st := openStore(t)
	b := New(st, nil)
	token, err := b.Mint(context.Background(), "budget")
	if err != nil {
		t.Fatal(err)
	}
	b.SetEgress([]EgressTarget{{Host: "localhost", BaseURL: "http://localhost:" + originPort}})
	b.DialEgress = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	front := httptest.NewServer(b)
	defer front.Close()

	status, conn := connectThroughBroker(t, strings.TrimPrefix(front.URL, "http://"), "localhost:"+originPort, token)
	defer func() { _ = conn.Close() }()
	if !strings.Contains(status, "200") {
		t.Fatalf("CONNECT status = %q, want 200", status)
	}
	if _, err := conn.Write([]byte("request")); err != nil {
		t.Fatal(err)
	}
	// Finish writing: the client→origin direction now ends.
	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("reading the origin's reply: %v", err)
	}
	if string(got) != body {
		t.Fatalf("received %d bytes, want %d: the relay closed before the origin finished",
			len(got), len(body))
	}
}

// A CONNECT request has no URL path, so recording one writes a blank detail and
// loses what the client tried to reach.
func TestConnectDenialAuditNamesTheTarget(t *testing.T) {
	st := openStore(t)
	b := New(st, nil)
	front := httptest.NewServer(b)
	defer front.Close()

	_, conn := connectThroughBroker(t, strings.TrimPrefix(front.URL, "http://"), "blocked.example:443", "wk_unknown")
	_ = conn.Close()

	var detail string
	if err := st.DB.QueryRow(
		`SELECT detail FROM broker_audit WHERE action='denied' ORDER BY id DESC LIMIT 1`).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detail, "blocked.example:443") {
		t.Fatalf("denial detail = %q, want the CONNECT target named", detail)
	}
}

// TestOpenAIKindCacheUsageBindsOnHalfRate pins finding 1 of the #247
// review through the broker: metered traffic fronting an upstream whose
// Kind is "openai" records rows attributed to the openai cost model, so
// budget binding prices cache reads at 0.5x instead of Anthropic's 0.1x.
// The same counters under the old hardcoded Anthropic model (true cost 105)
// would let the follow-up request through; under OpenAI pricing (true cost
// 305) the 300-token budget binds.
func TestOpenAIKindCacheUsageBindsOnHalfRate(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"usage":{"input_tokens":50,"cache_read_input_tokens":500,"output_tokens":5}}`)
	}))
	defer upstream.Close()
	st := openStore(t)
	b := New(st, []Upstream{{Name: "openai", Kind: "openai", BaseURL: upstream.URL, Header: "Authorization", Value: "Bearer real"}})
	b.Usage = usage.New(st)
	b.Limits = usage.Limits{TokensPerDay: 300}
	token, _ := b.Mint(context.Background(), "openai-kind-cache")
	front := httptest.NewServer(b)
	defer front.Close()
	do := func(max int) int {
		req, _ := http.NewRequest(http.MethodPost, front.URL+"/openai/v1/chat/completions", strings.NewReader(fmt.Sprintf(`{"max_tokens":%d}`, max)))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if got := do(70); got != http.StatusOK {
		t.Fatalf("first status=%d", got)
	}
	// The reconciled row is attributed to openai with raw counters
	// 50/500/5, and its true cost is 50 + 500*0.5 + 5 = 305.
	day := waitDayRows(t, b, "openai-kind-cache", func(rows []usage.Row) bool {
		return len(rows) == 1 && rows[0].Provider == "openai" && rows[0].InputTokens == 50 && rows[0].CacheReadInputTokens == 500 && rows[0].OutputTokens == 5 && rows[0].ReservedTokens == 0
	})
	if len(day) != 1 || day[0].Provider != "openai" || day[0].InputTokens != 50 || day[0].CacheReadInputTokens != 500 || day[0].OutputTokens != 5 || day[0].ReservedTokens != 0 {
		t.Fatalf("day row = %+v, want openai 50/500/5 reserved 0", day[0])
	}
	// True cost 305 exceeds the 300-token budget: the follow-up is refused.
	// Under Anthropic pricing (105) it would have passed.
	if got := do(20); got != http.StatusTooManyRequests {
		t.Fatalf("follow-up status=%d, want 429 under openai 0.5x pricing", got)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls=%d, want 1 (follow-up refused before dispatch)", got)
	}
}

// A tunnel is metered once per CONNECT unless the relay bytes are charged
// against the session budget (#244). With a byte budget configured, a
// tunnelled session that relays more than the budget has its tunnel cut, the
// bytes are persisted, and the next CONNECT is refused.
func TestConnectRelayBytesAreMeteredAgainstTheSessionBudget(t *testing.T) {
	origin, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = origin.Close() }()
	go func() {
		conn, acceptErr := origin.Accept()
		if acceptErr != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = io.Copy(conn, conn) // echo back whatever is relayed
	}()
	_, originPort, _ := net.SplitHostPort(origin.Addr().String())

	st := openStore(t)
	b := New(st, nil)
	const budget int64 = 32
	token, err := b.Mint(context.Background(), "budget")
	if err != nil {
		t.Fatal(err)
	}
	b.Usage = usage.New(st)
	b.Limits = usage.Limits{TunnelBytesPerSession: budget}
	b.SetEgress([]EgressTarget{{Host: "localhost", BaseURL: "http://localhost:" + originPort}})
	b.DialEgress = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	front := httptest.NewServer(b)
	defer front.Close()
	addr := strings.TrimPrefix(front.URL, "http://")

	status, conn := connectThroughBroker(t, addr, "localhost:"+originPort, token)
	if !strings.Contains(status, "200") {
		t.Fatalf("CONNECT status = %q, want 200", status)
	}
	// A payload far larger than the budget: the relay must cut the tunnel
	// instead of relaying it all.
	if _, err := conn.Write(bytes.Repeat([]byte("x"), 4096)); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	reply := make([]byte, 4096)
	if n, err := conn.Read(reply); err == nil {
		t.Fatalf("tunnel relayed %d bytes back despite a %d-byte budget", n, budget)
	}
	_ = conn.Close()

	// The relayed bytes are persisted, so the next CONNECT sees them. The
	// recording happens after both relay directions finish, so poll briefly.
	usageStore := usage.New(st)
	deadline := time.Now().Add(2 * time.Second)
	var relayed int64
	for {
		relayed, err = usageStore.TunnelBytesAt(context.Background(), "budget", time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if relayed > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("tunnel bytes were never recorded")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if relayed > budget {
		t.Fatalf("relayed %d bytes, want <= the %d-byte budget", relayed, budget)
	}

	// The budget is consumed: a second CONNECT is refused before any byte
	// flows, and the refusal is audited (denied connect budget).
	status, conn = connectThroughBroker(t, addr, "localhost:"+originPort, token)
	defer func() { _ = conn.Close() }()
	if !strings.Contains(status, "429") {
		t.Fatalf("CONNECT status = %q, want 429 once the byte budget is exhausted", status)
	}
}

// Without a configured byte budget the relay is unlimited and the session's
// tunnel bytes are still recorded for reporting.
func TestConnectRecordsTunnelBytesWithoutABudget(t *testing.T) {
	origin, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = origin.Close() }()
	go func() {
		conn, acceptErr := origin.Accept()
		if acceptErr != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = io.Copy(conn, conn)
	}()
	_, originPort, _ := net.SplitHostPort(origin.Addr().String())

	st := openStore(t)
	b := New(st, nil)
	token, err := b.Mint(context.Background(), "budget")
	if err != nil {
		t.Fatal(err)
	}
	b.Usage = usage.New(st)
	b.SetEgress([]EgressTarget{{Host: "localhost", BaseURL: "http://localhost:" + originPort}})
	b.DialEgress = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	front := httptest.NewServer(b)
	defer front.Close()

	status, conn := connectThroughBroker(t, strings.TrimPrefix(front.URL, "http://"), "localhost:"+originPort, token)
	if !strings.Contains(status, "200") {
		t.Fatalf("CONNECT status = %q, want 200", status)
	}
	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, len("hello"))
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatalf("tunnel relay failed: %v", err)
	}
	if string(reply) != "hello" {
		t.Fatalf("tunnel payload = %q, want %q", reply, "hello")
	}
	_ = conn.Close()

	deadline := time.Now().Add(2 * time.Second)
	for {
		n, err := usage.New(st).TunnelBytesAt(context.Background(), "budget", time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if n >= 10 { // 5 client bytes + 5 echoed bytes
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("tunnel bytes not recorded; got %d", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
