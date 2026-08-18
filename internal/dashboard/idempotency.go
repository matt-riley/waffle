package dashboard

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"
)

var errIdempotencyCapacity = errors.New("idempotency store is full")

type idempotencyValue struct {
	operation string
	digest    string
	status    int
	body      []byte
	ready     chan struct{}
}

// IdempotencyStore retains completed mutation responses for a bounded period.
// Capacity and TTL eviction use the shared ttlMap helper (#154).
type IdempotencyStore struct {
	mu  sync.Mutex
	now func() time.Time
	ttl time.Duration
	// entries uses Sticky=true while a mutation is in flight (ready != nil).
	entries ttlMap[idempotencyValue]
}

func NewIdempotencyStore(now func() time.Time, capacity int, ttl time.Duration) *IdempotencyStore {
	if now == nil {
		now = time.Now
	}
	return &IdempotencyStore{
		now:     now,
		ttl:     ttl,
		entries: newTTLMap[idempotencyValue](capacity),
	}
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
// on the result being discarded here. This includes a timeout or error
// response produced because runCtx expired mid-mutation: that response is
// cached too and gets replayed verbatim to every later request reusing the
// same idempotency key, until the entry's TTL elapses — it is not retried
// automatically. Waiting for an already in-flight duplicate request is still
// bound by ctx.
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
	s.entries.pruneExpired(s.now(), nil)
	if rec, ok := s.entries.get(key); ok {
		entry := rec.Value
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
			completed, ok := s.entries.get(key)
			if ok && completed.Value.ready == nil {
				status, body = completed.Value.status, append([]byte(nil), completed.Value.body...)
				s.mu.Unlock()
				return status, body, nil
			}
			s.mu.Unlock()
			return 0, nil, context.Canceled
		case <-ctx.Done():
			return 0, nil, ctx.Err()
		}
	}
	if !s.entries.makeSpace(nil) {
		s.mu.Unlock()
		return http.StatusServiceUnavailable, []byte("idempotency_unavailable"), errIdempotencyCapacity
	}
	ready := make(chan struct{})
	entry := idempotencyValue{operation: operation, digest: requestDigest, ready: ready}
	s.entries.put(key, ttlRecord[idempotencyValue]{
		Value:  entry,
		Sticky: true, // in-flight: never capacity-evict or TTL-prune
	})
	s.mu.Unlock()

	defer func() {
		if recovered := recover(); recovered != nil {
			s.mu.Lock()
			if rec, ok := s.entries.get(key); ok && rec.Value.ready == ready {
				s.entries.delete(key)
				close(ready)
			}
			s.mu.Unlock()
			panic(recovered)
		}
	}()
	status, body = run(runCtx)
	if discardOnRunCancel {
		if err := runCtx.Err(); err != nil {
			s.mu.Lock()
			if rec, ok := s.entries.get(key); ok && rec.Value.ready == ready {
				s.entries.delete(key)
				close(ready)
			}
			s.mu.Unlock()
			return 0, nil, err
		}
	}

	s.mu.Lock()
	if rec, ok := s.entries.get(key); ok && rec.Value.ready == ready {
		if status >= http.StatusOK && status < http.StatusMultipleChoices {
			// Committed: cache as the terminal, replayable response so a
			// lost-response retry sees the same outcome.
			completed := rec.Value
			completed.status = status
			completed.body = append([]byte(nil), body...)
			completed.ready = nil
			s.entries.put(key, ttlRecord[idempotencyValue]{
				Value:     completed,
				ExpiresAt: s.now().Add(s.ttl),
				Sticky:    false,
			})
		} else {
			// The mutation did not commit: drop the entry so a retry with
			// the same key re-runs instead of replaying the failure. This is
			// the Desk's official recovery contract (#469).
			s.entries.delete(key)
		}
		close(ready)
	}
	s.mu.Unlock()
	return status, append([]byte(nil), body...), nil
}
