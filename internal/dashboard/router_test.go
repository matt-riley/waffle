package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/chat"
	"github.com/matt-riley/waffle/internal/llm"
)

func TestMutationHandlerReplaysExactEndpointAndBodyOnce(t *testing.T) {
	security := mustSecurity(t, "127.0.0.1:8422")
	store := NewIdempotencyStore(nil, 512, 10*time.Minute)
	calls := 0
	mux := http.NewServeMux()
	mux.Handle("/api/v1/desk/test", NewMutationHandler(security, store, 1024, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read callback body: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(append([]byte("saved:"), body...))
	})))

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8422/api/v1/desk/test", bytes.NewBufferString("exact body"))
		req.Host = "127.0.0.1:8422"
		req.Header.Set("X-Waffle-Desk-Token", security.Token())
		req.Header.Set("Idempotency-Key", "key")
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated || rec.Body.String() != "saved:exact body" {
			t.Fatalf("request %d = %d %q", i, rec.Code, rec.Body.String())
		}
	}
	if calls != 1 {
		t.Errorf("callback calls = %d, want 1", calls)
	}
}

func TestMutationHandlerRejectsOversizedBodyBeforeCallback(t *testing.T) {
	security := mustSecurity(t, "127.0.0.1:8422")
	called := false
	handler := NewMutationHandler(security, NewIdempotencyStore(nil, 512, time.Minute), 4, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8422/api/v1/desk/test", bytes.NewBufferString("five!"))
	req.Host = "127.0.0.1:8422"
	req.Header.Set("X-Waffle-Desk-Token", security.Token())
	req.Header.Set("Idempotency-Key", "key")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
	if called {
		t.Fatal("callback ran for oversized request")
	}
}

func TestMutationHandlerRejectsMissingTokenBeforeCallback(t *testing.T) {
	called := false
	handler := NewMutationHandler(mustSecurity(t, "127.0.0.1:8422"), NewIdempotencyStore(nil, 512, time.Minute), 1024, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8422/api/v1/desk/test", bytes.NewBufferString("body"))
	req.Host = "127.0.0.1:8422"
	req.Header.Set("Idempotency-Key", "key")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if called {
		t.Fatal("callback ran without a token")
	}
}

func TestMutationHandlerCachesTheFirstStatusWhenHandlerWritesTwice(t *testing.T) {
	security := mustSecurity(t, "127.0.0.1:8422")
	handler := NewMutationHandler(security, NewIdempotencyStore(nil, 512, time.Minute), 1024, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("first status"))
	}))
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8422/api/v1/desk/test", nil)
		req.Host = "127.0.0.1:8422"
		req.Header.Set("X-Waffle-Desk-Token", security.Token())
		req.Header.Set("Idempotency-Key", "key")
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || rec.Body.String() != "first status" {
			t.Fatalf("request %d = %d %q, want 200 first status", i, rec.Code, rec.Body.String())
		}
	}
}

func TestMutationHandlerPreservesHeadersAndRunsAfterResponseOnceAfterFlush(t *testing.T) {
	security := mustSecurity(t, "127.0.0.1:8422")
	store := NewIdempotencyStore(nil, 512, time.Minute)
	callbacks := 0
	observed := make([]RestartScheduleOutcome, 0, 1)
	var recorder *httptest.ResponseRecorder
	handler := NewMutationHandler(
		security,
		store,
		1024,
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			after, ok := w.(AfterResponseWriter)
			if !ok {
				t.Fatal("mutation response does not implement AfterResponseWriter")
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Waffle-Result", "committed")
			after.AfterResponse(func() RestartScheduleOutcome {
				callbacks++
				if recorder == nil || recorder.Body.String() != `{"ok":true}` {
					t.Fatalf("callback ran before response copy: %q", recorder.Body.String())
				}
				return RestartScheduleOutcome{Scheduled: true, Code: "restart_scheduled", Message: "scheduled"}
			})
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"ok":true}`))
		}),
		func(outcome RestartScheduleOutcome) {
			observed = append(observed, outcome)
		},
	)

	request := func() *httptest.ResponseRecorder {
		recorder = httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8422/api/v1/desk/test", strings.NewReader(`{"intent":"same"}`))
		req.Host = "127.0.0.1:8422"
		req.Header.Set("X-Waffle-Desk-Token", security.Token())
		req.Header.Set("Idempotency-Key", "after-response")
		handler.ServeHTTP(recorder, req)
		return recorder
	}

	first := request()
	replay := request()
	for index, response := range []*httptest.ResponseRecorder{first, replay} {
		if response.Code != http.StatusAccepted || response.Body.String() != `{"ok":true}` {
			t.Fatalf("response %d = %d %q", index, response.Code, response.Body.String())
		}
		if got := response.Header().Get("Content-Type"); got != "application/json" {
			t.Fatalf("response %d content type = %q", index, got)
		}
		if got := response.Header().Get("X-Waffle-Result"); got != "committed" {
			t.Fatalf("response %d result header = %q", index, got)
		}
	}
	if callbacks != 1 {
		t.Fatalf("after-response callbacks = %d, want 1", callbacks)
	}
	if len(observed) != 1 || observed[0].Code != "restart_scheduled" {
		t.Fatalf("observed outcomes = %#v", observed)
	}
}

func TestChatOpenRouteUsesMutationProtectionAndIdempotency(t *testing.T) {
	security := mustSecurity(t, "127.0.0.1:8422")
	backend := &fakeChatBackend{}
	clients := NewChatClients(func(context.Context) (chat.Backend, error) { return backend, nil }, bytes.NewReader(bytes.Repeat([]byte{5}, 32)))
	mux := http.NewServeMux()
	RegisterRoutes(mux, APIConfig{
		Security:    security,
		Hub:         NewEventHub(4),
		ChatClients: clients,
		Idempotency: NewIdempotencyStore(nil, 4, time.Minute),
	})

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8422/api/v1/desk/chat/open", bytes.NewBufferString(`{"session_id":"one"}`))
		req.Host = "127.0.0.1:8422"
		req.Header.Set("X-Waffle-Desk-Token", security.Token())
		req.Header.Set("Idempotency-Key", "open-once")
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d status = %d, body = %q", i, rec.Code, rec.Body.String())
		}
	}
	if backend.openCount() != 1 {
		t.Fatalf("backend opens = %d, want 1", backend.openCount())
	}
}

func TestChatOpenRouteReturnsResumedHistory(t *testing.T) {
	const canary = "persisted transcript canary"
	security := mustSecurity(t, "127.0.0.1:8422")
	backend := &fakeChatBackend{openState: chat.State{
		SessionID: "resumed",
		History:   []llm.Message{llm.UserText(canary)},
	}}
	clients := NewChatClients(func(context.Context) (chat.Backend, error) { return backend, nil }, nil)
	mux := http.NewServeMux()
	RegisterRoutes(mux, APIConfig{
		Security:    security,
		Hub:         NewEventHub(4),
		ChatClients: clients,
		Idempotency: NewIdempotencyStore(nil, 4, time.Minute),
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8422/api/v1/desk/chat/open", strings.NewReader(`{"session_id":"resumed"}`))
	req.Host = "127.0.0.1:8422"
	req.Header.Set("X-Waffle-Desk-Token", security.Token())
	req.Header.Set("Idempotency-Key", "resume")
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
	var response struct {
		State chat.State `json:"state"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.State.History) != 1 || response.State.History[0].Blocks[0].Text != canary {
		t.Fatalf("history = %+v, want persisted transcript", response.State.History)
	}
}

func TestChatRoutesAllowlistStableErrors(t *testing.T) {
	const canary = "hostile backend-controlled message"
	for _, test := range []struct {
		name        string
		backendCode string
		wantStatus  int
		wantCode    string
		wantMessage string
	}{
		{
			name:        "unknown backend code",
			backendCode: "backend_owned_code",
			wantStatus:  http.StatusBadRequest,
			wantCode:    "open_failed",
			wantMessage: "chat request could not be completed",
		},
		{
			name:        "session active",
			backendCode: "session_active",
			wantStatus:  http.StatusConflict,
			wantCode:    "session_active",
			wantMessage: "chat session is already active",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			security := mustSecurity(t, "127.0.0.1:8422")
			backend := &fakeChatBackend{openErr: hostileChatError{code: test.backendCode, message: canary}}
			clients := NewChatClients(func(context.Context) (chat.Backend, error) { return backend, nil }, nil)
			mux := http.NewServeMux()
			RegisterRoutes(mux, APIConfig{
				Security:    security,
				Hub:         NewEventHub(4),
				ChatClients: clients,
				Idempotency: NewIdempotencyStore(nil, 4, time.Minute),
			})
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8422/api/v1/desk/chat/open", strings.NewReader(`{}`))
			req.Host = "127.0.0.1:8422"
			req.Header.Set("X-Waffle-Desk-Token", security.Token())
			req.Header.Set("Idempotency-Key", test.name)
			mux.ServeHTTP(rec, req)
			if rec.Code != test.wantStatus {
				t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
			}
			var response struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.Code != test.wantCode || response.Message != test.wantMessage {
				t.Fatalf("response = %+v, want code %q message %q", response, test.wantCode, test.wantMessage)
			}
			if strings.Contains(rec.Body.String(), canary) {
				t.Fatalf("response leaked backend-controlled message: %q", rec.Body.String())
			}
		})
	}
}

type hostileChatError struct {
	code    string
	message string
}

func (e hostileChatError) Error() string       { return e.message }
func (e hostileChatError) ErrorCode() string   { return e.code }
func (e hostileChatError) SafeMessage() string { return e.message }

func TestChatRoutesRejectMissingMutationHeadersAndOversizedBodies(t *testing.T) {
	security := mustSecurity(t, "127.0.0.1:8422")
	factoryCalls := 0
	clients := NewChatClients(func(context.Context) (chat.Backend, error) { factoryCalls++; return &fakeChatBackend{}, nil }, bytes.NewReader(bytes.Repeat([]byte{9}, 128)))
	mux := http.NewServeMux()
	RegisterRoutes(mux, APIConfig{Security: security, Hub: NewEventHub(4), ChatClients: clients, Idempotency: NewIdempotencyStore(nil, 8, time.Minute)})
	routes := []string{"open", "turn", "command", "cancel", "close"}
	for _, route := range routes {
		t.Run(route+" missing token", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8422/api/v1/desk/chat/"+route, bytes.NewBufferString(`{}`))
			req.Host = "127.0.0.1:8422"
			req.Header.Set("Idempotency-Key", route)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d", rec.Code)
			}
		})
		t.Run(route+" wrong token", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8422/api/v1/desk/chat/"+route, bytes.NewBufferString(`{}`))
			req.Host = "127.0.0.1:8422"
			req.Header.Set("X-Waffle-Desk-Token", "wrong")
			req.Header.Set("Idempotency-Key", route+"-wrong")
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d", rec.Code)
			}
		})
		t.Run(route+" missing key", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8422/api/v1/desk/chat/"+route, bytes.NewBufferString(`{}`))
			req.Host = "127.0.0.1:8422"
			req.Header.Set("X-Waffle-Desk-Token", security.Token())
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d", rec.Code)
			}
		})
		t.Run(route+" oversized", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8422/api/v1/desk/chat/"+route, bytes.NewReader(bytes.Repeat([]byte("x"), dashboardChatMaxBodyBytes+1)))
			req.Host = "127.0.0.1:8422"
			req.Header.Set("X-Waffle-Desk-Token", security.Token())
			req.Header.Set("Idempotency-Key", route+"-large")
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("status = %d", rec.Code)
			}
		})
	}
	if factoryCalls != 0 {
		t.Fatalf("factory calls = %d", factoryCalls)
	}
}

func TestChatRoutesReplayWithoutReinvocationAndRejectConflicts(t *testing.T) {
	for _, route := range []string{"open", "turn", "command", "cancel", "close"} {
		t.Run(route, func(t *testing.T) {
			security := mustSecurity(t, "127.0.0.1:8422")
			backend := &fakeChatBackend{}
			clients := NewChatClients(func(context.Context) (chat.Backend, error) { return backend, nil }, nil)
			id := ""
			if route != "open" {
				var err error
				id, _, err = clients.Open(context.Background(), chat.OpenOptions{})
				if err != nil {
					t.Fatal(err)
				}
			}
			mux := http.NewServeMux()
			RegisterRoutes(mux, APIConfig{Security: security, Hub: NewEventHub(8), ChatClients: clients, Idempotency: NewIdempotencyStore(nil, 16, time.Minute)})
			body := `{}`
			switch route {
			case "turn":
				body = `{"client_id":"` + id + `","text":"hi"}`
			case "command":
				body = `{"client_id":"` + id + `","command":{"name":"status"}}`
			case "cancel", "close":
				body = `{"client_id":"` + id + `"}`
			}
			do := func(path, payload string) *httptest.ResponseRecorder {
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8422/api/v1/desk/chat/"+path, strings.NewReader(payload))
				req.Host = "127.0.0.1:8422"
				req.Header.Set("X-Waffle-Desk-Token", security.Token())
				req.Header.Set("Idempotency-Key", "same")
				mux.ServeHTTP(rec, req)
				return rec
			}
			first, replay := do(route, body), do(route, body)
			if first.Code != replay.Code || first.Code != http.StatusOK {
				t.Fatalf("replay statuses %d %d", first.Code, replay.Code)
			}
			if got := do(route, body+" ").Code; got != http.StatusConflict {
				t.Fatalf("body conflict %d", got)
			}
			counter := func(operation string) int {
				return map[string]int{
					"open":    backend.openCount(),
					"turn":    backend.turnCount(),
					"command": backend.commandCount(),
					"cancel":  backend.cancelCount(),
					"close":   backend.closeCount(),
				}[operation]
			}
			conflictingRoute, conflictingBody := "open", `{}`
			if route == "open" {
				var response struct {
					ClientID string `json:"client_id"`
				}
				if err := json.Unmarshal(first.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				conflictingRoute = "turn"
				conflictingBody = `{"client_id":"` + response.ClientID + `","text":"must not run"}`
			}
			originalCalls, conflictingCalls := counter(route), counter(conflictingRoute)
			if got := do(conflictingRoute, conflictingBody).Code; got != http.StatusConflict {
				t.Fatalf("operation conflict status = %d", got)
			}
			if got := counter(route); got != originalCalls {
				t.Fatalf("original %s calls after operation conflict = %d, want %d", route, got, originalCalls)
			}
			if got := counter(conflictingRoute); got != conflictingCalls {
				t.Fatalf("conflicting %s calls = %d, want %d", conflictingRoute, got, conflictingCalls)
			}
			if originalCalls != 1 {
				t.Fatalf("%s calls = %d, want 1", route, originalCalls)
			}
		})
	}
}
