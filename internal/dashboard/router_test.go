package dashboard

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/chat"
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
			do := func(path, payload string) int {
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8422/api/v1/desk/chat/"+path, strings.NewReader(payload))
				req.Host = "127.0.0.1:8422"
				req.Header.Set("X-Waffle-Desk-Token", security.Token())
				req.Header.Set("Idempotency-Key", "same")
				mux.ServeHTTP(rec, req)
				return rec.Code
			}
			if a, b := do(route, body), do(route, body); a != b || a != http.StatusOK {
				t.Fatalf("replay statuses %d %d", a, b)
			}
			if got := do(route, body+" "); got != http.StatusConflict {
				t.Fatalf("body conflict %d", got)
			}
			if route != "open" && do("open", `{}`) != http.StatusConflict {
				t.Fatal("operation conflict missing")
			}
			calls := map[string]int{"open": backend.openCount(), "turn": backend.turnCount(), "command": backend.commandCount(), "cancel": backend.cancelCount(), "close": backend.closeCount()}[route]
			if calls != 1 {
				t.Fatalf("calls=%d", calls)
			}
		})
	}
}
