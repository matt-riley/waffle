package dashboard

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
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
