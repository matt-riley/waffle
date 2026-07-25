// Package lifecycle contains synchronization primitives shared by lifecycle
// mutations that cross package boundaries.
package lifecycle

import (
	"context"
	"sync"
)

// Guard serializes skill attachment and uninstall transitions for one store.
// Its zero value is ready for use.
type Guard struct {
	mu        sync.Mutex
	lockPath  string
	releaseFn func() error
}

// NewGuard returns a lifecycle guard for a store's shared skill mutations.
// When lockPath is supplied, the guard also takes an OS-level lock at that
// path so separate waffle processes coordinate their skill transitions.
func NewGuard(lockPath ...string) *Guard {
	guard := &Guard{}
	if len(lockPath) != 0 {
		guard.lockPath = lockPath[0]
	}
	return guard
}

// Lock acquires the process-local and, when configured, shared filesystem
// lock. The context bounds waiting behind another process.
func (g *Guard) Lock(ctx context.Context) error {
	if g == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	g.mu.Lock()
	if g.lockPath == "" {
		return nil
	}
	release, err := acquireFileLock(ctx, g.lockPath)
	if err != nil {
		g.mu.Unlock()
		return err
	}
	g.releaseFn = release
	return nil
}

// Unlock releases the guard.
func (g *Guard) Unlock() {
	if g != nil {
		if g.releaseFn != nil {
			_ = g.releaseFn()
			g.releaseFn = nil
		}
		g.mu.Unlock()
	}
}
