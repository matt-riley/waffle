package dashboard

import (
	"testing"
	"time"
)

func TestTTLMapPruneAndCapacityEviction(t *testing.T) {
	m := newTTLMap[string](2)
	now := time.Unix(100, 0)
	m.put("a", ttlRecord[string]{Value: "A", ExpiresAt: now.Add(time.Hour)})
	m.put("b", ttlRecord[string]{Value: "B", ExpiresAt: now.Add(2 * time.Hour)})
	if m.len() != 2 {
		t.Fatalf("len = %d", m.len())
	}
	// At capacity: makeSpace must drop earliest expiry.
	if !m.makeSpace(nil) {
		t.Fatal("makeSpace failed with non-sticky residents")
	}
	if m.len() != 1 {
		t.Fatalf("after eviction len = %d, want 1", m.len())
	}
	if _, ok := m.get("a"); ok {
		t.Fatal("earliest-expiring key a should be evicted")
	}
	if _, ok := m.get("b"); !ok {
		t.Fatal("later-expiring key b should remain")
	}

	// Sticky blocks capacity eviction.
	m.put("sticky", ttlRecord[string]{Value: "S", Sticky: true})
	m.put("c", ttlRecord[string]{Value: "C", ExpiresAt: now.Add(3 * time.Hour)})
	// Now len=3, capacity=2 is already exceeded; makeSpace for another insert.
	// sticky + c may leave one non-sticky.
	m = newTTLMap[string](1)
	m.put("sticky", ttlRecord[string]{Value: "S", Sticky: true})
	if m.makeSpace(nil) {
		t.Fatal("makeSpace should fail when only sticky residents remain")
	}

	// Expiry prune skips sticky and keeps future expiry.
	m = newTTLMap[string](8)
	m.put("old", ttlRecord[string]{Value: "O", ExpiresAt: now.Add(-time.Second)})
	m.put("live", ttlRecord[string]{Value: "L", ExpiresAt: now.Add(time.Minute)})
	m.put("busy", ttlRecord[string]{Value: "B", Sticky: true, ExpiresAt: now.Add(-time.Hour)})
	var dropped []string
	m.pruneExpired(now, func(key string, _ ttlRecord[string]) { dropped = append(dropped, key) })
	if len(dropped) != 1 || dropped[0] != "old" {
		t.Fatalf("dropped = %v, want [old]", dropped)
	}
	if _, ok := m.get("busy"); !ok {
		t.Fatal("sticky must survive prune")
	}
	if _, ok := m.get("live"); !ok {
		t.Fatal("unexpired must survive prune")
	}
}

func TestBoundedRingDropsOldest(t *testing.T) {
	r := newBoundedRing(2)
	r.put("t1", ErrPreviewUsed)
	r.put("t2", ErrPreviewExpired)
	r.put("t3", ErrPreviewEvicted)
	if _, ok := r.get("t1"); ok {
		t.Fatal("oldest outcome should be dropped")
	}
	if err, ok := r.get("t3"); !ok || err != ErrPreviewEvicted {
		t.Fatalf("newest outcome = %v ok=%v", err, ok)
	}
}

func TestTokenStoresShareTTLHelpers(t *testing.T) {
	// Structural proof: PreviewStore and IdempotencyStore construct from the shared helpers.
	previews := NewPreviewStore(func() time.Time { return time.Unix(1, 0) }, nil)
	if previews.live.capacity != previewStoreCapacity {
		t.Fatalf("preview live capacity = %d", previews.live.capacity)
	}
	if previews.outcomes.capacity != previewHistoryCapacity {
		t.Fatalf("preview outcomes capacity = %d", previews.outcomes.capacity)
	}
	idem := NewIdempotencyStore(func() time.Time { return time.Unix(1, 0) }, 4, time.Minute)
	if idem.entries.capacity != 4 {
		t.Fatalf("idempotency capacity = %d", idem.entries.capacity)
	}
	// ChatClients is the third bounded registry (client IDs as tokens with idle TTL).
	clients := NewChatClients(nil, nil)
	if clients.maxClients <= 0 || clients.idleTTL <= 0 {
		t.Fatal("chat clients must expose bounded capacity and idle TTL")
	}
}
