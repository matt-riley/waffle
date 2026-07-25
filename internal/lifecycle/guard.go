// Package lifecycle contains synchronization primitives shared by lifecycle
// mutations that cross package boundaries.
package lifecycle

import "sync"

// Guard serializes skill attachment and uninstall transitions for one store.
// Its zero value is ready for use.
type Guard struct {
	mu sync.Mutex
}

// NewGuard returns a lifecycle guard for a store's shared skill mutations.
func NewGuard() *Guard { return &Guard{} }

// Lock acquires the guard.
func (g *Guard) Lock() {
	if g != nil {
		g.mu.Lock()
	}
}

// Unlock releases the guard.
func (g *Guard) Unlock() {
	if g != nil {
		g.mu.Unlock()
	}
}
