package broker

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/store"
	"github.com/matt-riley/waffle/internal/usage"
)

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
	rows, err := b.Usage.List(context.Background(), "budget-a")
	if err != nil {
		t.Fatal(err)
	}
	var requests, tokens int
	for _, row := range rows {
		if row.Period == "day" {
			requests += row.Requests
			tokens += row.InputTokens + row.OutputTokens
		}
	}
	if requests != 1 || tokens != 12 {
		t.Fatalf("requests=%d tokens=%d rows=%+v", requests, tokens, rows)
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
	rows, err := b.Usage.List(context.Background(), "stream-budget")
	if err != nil {
		t.Fatal(err)
	}
	var requests, tokens int
	for _, row := range rows {
		if row.Period == "day" {
			requests += row.Requests
			tokens += row.InputTokens + row.OutputTokens
		}
	}
	if requests != 1 || tokens != 13 {
		t.Fatalf("requests=%d tokens=%d rows=%+v", requests, tokens, rows)
	}
}

func TestAnthropicJSONCacheUsageConsumesFullDailyBudget(t *testing.T) {
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
	if got := do(30); got != http.StatusTooManyRequests {
		t.Fatalf("cached follow-up status=%d", got)
	}
	assertDayUsage(t, b.Usage, "anthropic-json-cache", 65, 0)
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls=%d", got)
	}
}

func TestAnthropicSSECacheUsageConsumesFullDailyBudget(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10,\"cache_creation_input_tokens\":20,\"cache_read_input_tokens\":30,\"output_tokens\":0}}}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":5}}\n\n")
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
	if got := do(30); got != http.StatusTooManyRequests {
		t.Fatalf("cached follow-up status=%d", got)
	}
	assertDayUsage(t, b.Usage, "anthropic-sse-cache", 65, 0)
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls=%d", got)
	}
}

func TestProviderUsageCacheSumSaturatesAndPreservesOpenAIAliases(t *testing.T) {
	openAI := parseProviderUsage([]byte(`{"usage":{"input_tokens":7,"prompt_tokens":9,"completion_tokens":5}}`))
	if openAI.InputTokens != 9 || openAI.OutputTokens != 5 {
		t.Fatalf("OpenAI aliases changed: %+v", openAI)
	}
	maxInt := int(^uint(0) >> 1)
	anthropic := parseProviderUsage([]byte(`{"usage":{"input_tokens":9e100,"cache_creation_input_tokens":9e100,"cache_read_input_tokens":9e100,"output_tokens":1}}`))
	if anthropic.InputTokens != maxInt || anthropic.OutputTokens != 1 {
		t.Fatalf("Anthropic saturation failed: %+v", anthropic)
	}
}

func assertDayUsage(t *testing.T, u *usage.Store, session string, actual, reserved int) {
	t.Helper()
	rows, err := u.List(context.Background(), session)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Period == "day" {
			if got := row.InputTokens + row.OutputTokens; got != actual || row.ReservedTokens != reserved {
				t.Fatalf("day actual=%d reserved=%d rows=%+v", got, row.ReservedTokens, rows)
			}
			return
		}
	}
	t.Fatalf("day usage row missing: %+v", rows)
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
	rows, err := b.Usage.List(context.Background(), "large-json")
	if err != nil {
		t.Fatal(err)
	}
	var tokens int
	for _, row := range rows {
		if row.Period == "day" {
			tokens += row.InputTokens + row.OutputTokens
		}
	}
	if tokens != 17 {
		t.Fatalf("tokens=%d rows=%+v", tokens, rows)
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
