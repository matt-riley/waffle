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
)

var (
	ErrPreviewExpired  = errors.New("preview token expired")
	ErrPreviewMismatch = errors.New("preview token does not match operation or resource")
	ErrPreviewUnknown  = errors.New("preview token is unknown")
	ErrPreviewUsed     = errors.New("preview token was already used")
)

type previewEntry struct {
	operation  string
	resourceID string
	expiresAt  time.Time
}

// PreviewStore retains short-lived, resource-bound confirmation tokens in
// process memory. Tokens and their recent terminal outcomes are bounded.
type PreviewStore struct {
	mu           sync.Mutex
	now          func() time.Time
	entropy      io.Reader
	entries      map[string]previewEntry
	outcomes     map[string]error
	outcomeOrder []string
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
		entries:  make(map[string]previewEntry),
		outcomes: make(map[string]error),
	}
}

// Issue creates a URL-safe opaque token bound to operation and resourceID.
// Entropy failure is fatal because issuing a predictable confirmation token is
// less safe than refusing to continue.
func (s *PreviewStore) Issue(operation, resourceID string, ttl time.Duration) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	s.pruneExpiredLocked(now)
	s.makeSpaceLocked()

	for {
		random := make([]byte, previewTokenBytes)
		if _, err := io.ReadFull(s.entropy, random); err != nil {
			panic("dashboard: preview token entropy unavailable")
		}
		token := base64.RawURLEncoding.EncodeToString(random)
		if _, exists := s.entries[token]; exists {
			continue
		}
		if _, exists := s.outcomes[token]; exists {
			continue
		}
		s.entries[token] = previewEntry{
			operation:  operation,
			resourceID: resourceID,
			expiresAt:  now.Add(ttl),
		}
		return token
	}
}

// Consume atomically spends a token. Both successful and mismatched attempts
// permanently burn the token.
func (s *PreviewStore) Consume(token, operation, resourceID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	s.pruneExpiredLocked(now)
	entry, ok := s.entries[token]
	if !ok {
		if outcome, known := s.outcomes[token]; known {
			return outcome
		}
		return ErrPreviewUnknown
	}

	delete(s.entries, token)
	if !entry.expiresAt.After(now) {
		s.recordOutcomeLocked(token, ErrPreviewExpired)
		return ErrPreviewExpired
	}

	s.recordOutcomeLocked(token, ErrPreviewUsed)
	if entry.operation != operation || entry.resourceID != resourceID {
		return ErrPreviewMismatch
	}
	return nil
}

func (s *PreviewStore) pruneExpiredLocked(now time.Time) {
	for token, entry := range s.entries {
		if entry.expiresAt.After(now) {
			continue
		}
		delete(s.entries, token)
		s.recordOutcomeLocked(token, ErrPreviewExpired)
	}
}

func (s *PreviewStore) makeSpaceLocked() {
	if len(s.entries) < previewStoreCapacity {
		return
	}

	var victimToken string
	var victim previewEntry
	for token, entry := range s.entries {
		if victimToken == "" ||
			entry.expiresAt.Before(victim.expiresAt) ||
			(entry.expiresAt.Equal(victim.expiresAt) && token < victimToken) {
			victimToken = token
			victim = entry
		}
	}
	delete(s.entries, victimToken)
	s.recordOutcomeLocked(victimToken, ErrPreviewUsed)
}

func (s *PreviewStore) recordOutcomeLocked(token string, outcome error) {
	if _, exists := s.outcomes[token]; exists {
		s.outcomes[token] = outcome
		return
	}
	s.outcomes[token] = outcome
	s.outcomeOrder = append(s.outcomeOrder, token)
	if len(s.outcomeOrder) <= previewHistoryCapacity {
		return
	}
	oldest := s.outcomeOrder[0]
	s.outcomeOrder = s.outcomeOrder[1:]
	delete(s.outcomes, oldest)
}
