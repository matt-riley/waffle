package dashboard

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"
)

func TestIdempotencyStoreReplaysFirstCanonicalResponse(t *testing.T) {
	store := NewIdempotencyStore(nil, 512, 10*time.Minute)
	calls := 0
	run := func(context.Context) (int, []byte) {
		calls++
		return http.StatusCreated, []byte(`{"ok":true}`)
	}

	status, body, err := store.Do(context.Background(), "key", "POST /api/v1/desk/test", "digest", run)
	if err != nil {
		t.Fatalf("first Do() error = %v", err)
	}
	if status != http.StatusCreated || string(body) != `{"ok":true}` {
		t.Fatalf("first Do() = %d %q", status, body)
	}
	body[0] = '!'
	status, body, err = store.Do(context.Background(), "key", "POST /api/v1/desk/test", "digest", run)
	if err != nil {
		t.Fatalf("replay Do() error = %v", err)
	}
	if status != http.StatusCreated || string(body) != `{"ok":true}` {
		t.Fatalf("replay Do() = %d %q", status, body)
	}
	if calls != 1 {
		t.Errorf("callback calls = %d, want 1", calls)
	}
}

func TestIdempotencyStoreRejectsKeyReuseWithDifferentRequest(t *testing.T) {
	store := NewIdempotencyStore(nil, 512, 10*time.Minute)
	_, _, err := store.Do(context.Background(), "key", "POST /api/v1/desk/a", "first", func(context.Context) (int, []byte) {
		return http.StatusNoContent, nil
	})
	if err != nil {
		t.Fatalf("initial Do() error = %v", err)
	}
	for _, test := range []struct {
		name      string
		operation string
		digest    string
	}{
		{name: "different operation", operation: "POST /api/v1/desk/b", digest: "first"},
		{name: "different body", operation: "POST /api/v1/desk/a", digest: "second"},
	} {
		t.Run(test.name, func(t *testing.T) {
			called := false
			status, body, err := store.Do(context.Background(), "key", test.operation, test.digest, func(context.Context) (int, []byte) {
				called = true
				return http.StatusNoContent, nil
			})
			if err != nil {
				t.Fatalf("Do() error = %v", err)
			}
			if status != http.StatusConflict || string(body) != "idempotency_conflict" {
				t.Fatalf("Do() = %d %q, want 409 idempotency_conflict", status, body)
			}
			if called {
				t.Fatal("callback ran for conflicting idempotency key")
			}
		})
	}
}

func TestIdempotencyStoreJoinsConcurrentIdenticalRequest(t *testing.T) {
	store := NewIdempotencyStore(nil, 512, 10*time.Minute)
	started := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		status, body, err := store.Do(context.Background(), "key", "POST /api/v1/desk/test", "digest", func(context.Context) (int, []byte) {
			close(started)
			<-release
			return http.StatusAccepted, []byte("first")
		})
		if err != nil || status != http.StatusAccepted || string(body) != "first" {
			t.Errorf("first Do() = %d %q %v", status, body, err)
		}
	}()
	<-started

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		status, body, err := store.Do(context.Background(), "key", "POST /api/v1/desk/test", "digest", func(context.Context) (int, []byte) {
			t.Error("joined callback must not run")
			return http.StatusInternalServerError, nil
		})
		if err != nil || status != http.StatusAccepted || string(body) != "first" {
			t.Errorf("joined Do() = %d %q %v", status, body, err)
		}
	}()
	close(release)
	<-firstDone
	wg.Wait()
}

func TestIdempotencyStoreExpiresAndEvictsCompletedEntries(t *testing.T) {
	now := time.Date(2026, time.July, 23, 12, 0, 0, 0, time.UTC)
	store := NewIdempotencyStore(func() time.Time { return now }, 1, time.Minute)
	runs := 0
	run := func(context.Context) (int, []byte) {
		runs++
		return http.StatusOK, []byte("result")
	}
	if _, _, err := store.Do(context.Background(), "key", "POST /a", "one", run); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if _, _, err := store.Do(context.Background(), "key", "POST /a", "one", run); err != nil {
		t.Fatal(err)
	}
	if runs != 2 {
		t.Fatalf("expired entry callback runs = %d, want 2", runs)
	}
	if _, _, err := store.Do(context.Background(), "other", "POST /b", "two", run); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Do(context.Background(), "key", "POST /a", "one", run); err != nil {
		t.Fatal(err)
	}
	if runs != 4 {
		t.Errorf("evicted entry callback runs = %d, want 4", runs)
	}
}

func TestIdempotencyStoreDoesNotEvictInFlightEntry(t *testing.T) {
	store := NewIdempotencyStore(nil, 1, time.Minute)
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = store.Do(context.Background(), "first", "POST /a", "one", func(context.Context) (int, []byte) {
			close(started)
			<-release
			return http.StatusNoContent, nil
		})
	}()
	<-started
	status, body, err := store.Do(context.Background(), "second", "POST /b", "two", func(context.Context) (int, []byte) {
		t.Fatal("callback ran after in-flight capacity rejection")
		return http.StatusNoContent, nil
	})
	if err == nil || status != http.StatusServiceUnavailable || string(body) != "idempotency_unavailable" {
		t.Fatalf("Do() = %d %q %v, want 503 idempotency_unavailable and error", status, body, err)
	}
	close(release)
	<-done
}
