package dashboard

import "time"

// ttlRecord is one capacity-bounded, optionally sticky, TTL-keyed value.
// Sticky records (in-flight work) are never pruned by expiry or capacity eviction.
type ttlRecord[V any] struct {
	Value     V
	ExpiresAt time.Time
	Sticky    bool
}

// ttlMap is the shared bounded/TTL map used by PreviewStore, IdempotencyStore,
// and ChatClients. Callers hold their own mutex around these helpers.
type ttlMap[V any] struct {
	capacity int
	items    map[string]ttlRecord[V]
}

func newTTLMap[V any](capacity int) ttlMap[V] {
	return ttlMap[V]{
		capacity: capacity,
		items:    make(map[string]ttlRecord[V]),
	}
}

func (m *ttlMap[V]) len() int { return len(m.items) }

func (m *ttlMap[V]) get(key string) (ttlRecord[V], bool) {
	rec, ok := m.items[key]
	return rec, ok
}

func (m *ttlMap[V]) put(key string, rec ttlRecord[V]) {
	m.items[key] = rec
}

func (m *ttlMap[V]) delete(key string) {
	delete(m.items, key)
}

// pruneExpired removes non-sticky records whose ExpiresAt is not after now.
// onDrop is invoked for each removed key (may be nil).
func (m *ttlMap[V]) pruneExpired(now time.Time, onDrop func(string, ttlRecord[V])) {
	for key, rec := range m.items {
		if rec.Sticky || rec.ExpiresAt.After(now) {
			continue
		}
		delete(m.items, key)
		if onDrop != nil {
			onDrop(key, rec)
		}
	}
}

// makeSpace ensures room for one more non-sticky insert by evicting the
// soonest-expiring non-sticky record. Returns false when every resident is sticky.
//
// Callers must run pruneExpired first when expired entries should surface as
// expiry rather than capacity eviction: makeSpace ranks purely by ExpiresAt and
// would otherwise drop an already-expired key through onDrop as an eviction.
func (m *ttlMap[V]) makeSpace(onDrop func(string, ttlRecord[V])) bool {
	if m.capacity <= 0 || len(m.items) < m.capacity {
		return true
	}
	var (
		victimKey string
		victim    ttlRecord[V]
		found     bool
	)
	for key, rec := range m.items {
		if rec.Sticky {
			continue
		}
		if !found ||
			rec.ExpiresAt.Before(victim.ExpiresAt) ||
			(rec.ExpiresAt.Equal(victim.ExpiresAt) && key < victimKey) {
			victimKey, victim, found = key, rec, true
		}
	}
	if !found {
		return false
	}
	delete(m.items, victimKey)
	if onDrop != nil {
		onDrop(victimKey, victim)
	}
	return true
}

// boundedRing keeps insertion-ordered keys with a hard capacity, dropping the oldest.
type boundedRing struct {
	capacity int
	order    []string
	values   map[string]error
}

func newBoundedRing(capacity int) boundedRing {
	return boundedRing{
		capacity: capacity,
		values:   make(map[string]error),
	}
}

func (r *boundedRing) get(key string) (error, bool) {
	v, ok := r.values[key]
	return v, ok
}

func (r *boundedRing) put(key string, value error) {
	if _, exists := r.values[key]; exists {
		r.values[key] = value
		return
	}
	r.values[key] = value
	r.order = append(r.order, key)
	if r.capacity <= 0 || len(r.order) <= r.capacity {
		return
	}
	oldest := r.order[0]
	// Copy on eviction so the backing array does not retain dropped heads.
	r.order = append([]string(nil), r.order[1:]...)
	delete(r.values, oldest)
}

// idleDeadline is the shared TTL vocabulary used by ChatClients idle reaping:
// a client token expires at lastActive+idleTTL (unless sticky/busy).
func idleDeadline(lastActive time.Time, idleTTL time.Duration) time.Time {
	return lastActive.Add(idleTTL)
}
