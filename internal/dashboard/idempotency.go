package dashboard

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"
)

var errIdempotencyCapacity = errors.New("idempotency store is full")

type idempotencyEntry struct {
	operation string
	digest    string
	status    int
	body      []byte
	expiresAt time.Time
	ready     chan struct{}
}

// IdempotencyStore retains completed mutation responses for a bounded period.
type IdempotencyStore struct {
	mu       sync.Mutex
	now      func() time.Time
	capacity int
	ttl      time.Duration
	entries  map[string]*idempotencyEntry
}

func NewIdempotencyStore(now func() time.Time, capacity int, ttl time.Duration) *IdempotencyStore {
	if now == nil {
		now = time.Now
	}
	return &IdempotencyStore{now: now, capacity: capacity, ttl: ttl, entries: make(map[string]*idempotencyEntry)}
}

// Do runs a mutation once for an idempotency key, replaying its completed
// response to identical requests and rejecting key reuse for another request.
// If ctx is cancelled while run is in flight, the result is discarded so the
// key can be retried.
func (s *IdempotencyStore) Do(
	ctx context.Context,
	key, operation, requestDigest string,
	run func(context.Context) (status int, body []byte),
) (status int, body []byte, err error) {
	return s.do(ctx, ctx, true, key, operation, requestDigest, run)
}

// DoDetached behaves like Do, except the mutation itself runs with runCtx
// rather than ctx, so it can finish even after ctx (typically an HTTP
// request context) is cancelled by client disconnect. Whatever run returns
// is cached as the authoritative, terminal result regardless of runCtx's
// state by the time run returns — callers that need a bounded runtime must
// enforce it themselves (e.g. via runCtx's own deadline) rather than relying
// on the result being discarded here. Waiting for an already in-flight
// duplicate request is still bound by ctx.
func (s *IdempotencyStore) DoDetached(
	ctx, runCtx context.Context,
	key, operation, requestDigest string,
	run func(context.Context) (status int, body []byte),
) (status int, body []byte, err error) {
	return s.do(ctx, runCtx, false, key, operation, requestDigest, run)
}

func (s *IdempotencyStore) do(
	ctx, runCtx context.Context,
	discardOnRunCancel bool,
	key, operation, requestDigest string,
	run func(context.Context) (status int, body []byte),
) (status int, body []byte, err error) {
	if err := ctx.Err(); err != nil {
		return 0, nil, err
	}

	s.mu.Lock()
	s.pruneExpiredLocked()
	if entry, ok := s.entries[key]; ok {
		if entry.operation != operation || entry.digest != requestDigest {
			s.mu.Unlock()
			return http.StatusConflict, []byte("idempotency_conflict"), nil
		}
		if entry.ready == nil {
			status, body = entry.status, append([]byte(nil), entry.body...)
			s.mu.Unlock()
			return status, body, nil
		}
		ready := entry.ready
		s.mu.Unlock()
		select {
		case <-ready:
			s.mu.Lock()
			completed, ok := s.entries[key]
			if ok && completed.ready == nil {
				status, body = completed.status, append([]byte(nil), completed.body...)
				s.mu.Unlock()
				return status, body, nil
			}
			s.mu.Unlock()
			return 0, nil, context.Canceled
		case <-ctx.Done():
			return 0, nil, ctx.Err()
		}
	}
	if !s.makeSpaceLocked() {
		s.mu.Unlock()
		return http.StatusServiceUnavailable, []byte("idempotency_unavailable"), errIdempotencyCapacity
	}
	entry := &idempotencyEntry{operation: operation, digest: requestDigest, ready: make(chan struct{})}
	s.entries[key] = entry
	s.mu.Unlock()

	defer func() {
		if recovered := recover(); recovered != nil {
			s.mu.Lock()
			if s.entries[key] == entry {
				delete(s.entries, key)
				close(entry.ready)
			}
			s.mu.Unlock()
			panic(recovered)
		}
	}()
	status, body = run(runCtx)
	if discardOnRunCancel {
		if err := runCtx.Err(); err != nil {
			s.mu.Lock()
			if s.entries[key] == entry {
				delete(s.entries, key)
				close(entry.ready)
			}
			s.mu.Unlock()
			return 0, nil, err
		}
	}

	s.mu.Lock()
	if s.entries[key] == entry {
		ready := entry.ready
		entry.status = status
		entry.body = append([]byte(nil), body...)
		entry.expiresAt = s.now().Add(s.ttl)
		entry.ready = nil
		close(ready)
	}
	s.mu.Unlock()
	return status, append([]byte(nil), body...), nil
}

func (s *IdempotencyStore) pruneExpiredLocked() {
	now := s.now()
	for key, entry := range s.entries {
		if entry.ready == nil && !entry.expiresAt.After(now) {
			delete(s.entries, key)
		}
	}
}

func (s *IdempotencyStore) makeSpaceLocked() bool {
	if s.capacity <= 0 || len(s.entries) < s.capacity {
		return true
	}
	var victimKey string
	var victim *idempotencyEntry
	for key, entry := range s.entries {
		if entry.ready != nil {
			continue
		}
		if victim == nil || entry.expiresAt.Before(victim.expiresAt) {
			victimKey, victim = key, entry
		}
	}
	if victim == nil {
		return false
	}
	delete(s.entries, victimKey)
	return true
}
