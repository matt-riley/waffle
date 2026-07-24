package dashboard

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"sync"
	"time"
)

const (
	previewStoreCapacity   = 128
	previewHistoryCapacity = 128
	previewTokenBytes      = 32
	previewTokenAttempts   = 8
)

var (
	ErrPreviewExpired  = errors.New("preview token expired")
	ErrPreviewEvicted  = errors.New("preview token was evicted")
	ErrPreviewMismatch = errors.New("preview token does not match operation or resource")
	ErrPreviewUnknown  = errors.New("preview token is unknown")
	ErrPreviewUsed     = errors.New("preview token was already used")
)

type previewEntry struct {
	operation  string
	resourceID string
}

// PreviewStore retains short-lived, resource-bound confirmation tokens in
// process memory. Tokens and their recent terminal outcomes are bounded.
// Live tokens and outcome history use the shared TTL/bounded helpers (#154).
type PreviewStore struct {
	mu        sync.Mutex
	entropyMu sync.Mutex
	now       func() time.Time
	entropy   io.Reader
	live      ttlMap[previewEntry]
	outcomes  boundedRing
}

// NewPreviewStore returns a process-local confirmation store. Nil dependencies
// use the wall clock and the operating system's cryptographic random source.
func NewPreviewStore(now func() time.Time, entropy io.Reader) *PreviewStore {
	if now == nil {
		now = time.Now
	}
	if entropy == nil {
		entropy = rand.Reader
	}
	return &PreviewStore{
		now:      now,
		entropy:  entropy,
		live:     newTTLMap[previewEntry](previewStoreCapacity),
		outcomes: newBoundedRing(previewHistoryCapacity),
	}
}

// Issue creates a URL-safe opaque token bound to operation and resourceID.
// Entropy failure is fatal because issuing a predictable confirmation token is
// less safe than refusing to continue.
func (s *PreviewStore) Issue(operation, resourceID string, ttl time.Duration) string {
	if ttl <= 0 {
		panic("dashboard: preview token TTL must be positive")
	}

	s.entropyMu.Lock()
	defer s.entropyMu.Unlock()

	for attempt := 0; attempt < previewTokenAttempts; attempt++ {
		random := make([]byte, previewTokenBytes)
		if _, err := io.ReadFull(s.entropy, random); err != nil {
			panic("dashboard: preview token entropy unavailable")
		}
		candidate := base64.RawURLEncoding.EncodeToString(random)

		s.mu.Lock()
		registered := func() bool {
			defer s.mu.Unlock()
			if _, exists := s.live.get(candidate); exists {
				return false
			}
			if _, exists := s.outcomes.get(candidate); exists {
				return false
			}

			now := s.now()
			s.live.pruneExpired(now, func(token string, _ ttlRecord[previewEntry]) {
				s.outcomes.put(token, ErrPreviewExpired)
			})
			s.live.makeSpace(func(token string, _ ttlRecord[previewEntry]) {
				s.outcomes.put(token, ErrPreviewEvicted)
			})
			s.live.put(candidate, ttlRecord[previewEntry]{
				Value: previewEntry{
					operation:  operation,
					resourceID: resourceID,
				},
				ExpiresAt: now.Add(ttl),
			})
			return true
		}()
		if registered {
			return candidate
		}
	}
	panic("dashboard: preview token entropy exhausted")
}

// Consume atomically spends a token. Both successful and mismatched attempts
// permanently burn the token.
func (s *PreviewStore) Consume(token, operation, resourceID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	s.live.pruneExpired(now, func(token string, _ ttlRecord[previewEntry]) {
		s.outcomes.put(token, ErrPreviewExpired)
	})
	entry, ok := s.live.get(token)
	if !ok {
		if outcome, known := s.outcomes.get(token); known {
			return outcome
		}
		return ErrPreviewUnknown
	}

	s.live.delete(token)
	if !entry.ExpiresAt.After(now) {
		s.outcomes.put(token, ErrPreviewExpired)
		return ErrPreviewExpired
	}

	s.outcomes.put(token, ErrPreviewUsed)
	if entry.Value.operation != operation || entry.Value.resourceID != resourceID {
		return ErrPreviewMismatch
	}
	return nil
}
