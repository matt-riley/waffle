package main

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type sessionAlreadyActiveError struct{ sessionID string }

func (e sessionAlreadyActiveError) Error() string {
	return fmt.Sprintf("session %s is already active", e.sessionID)
}

func (sessionAlreadyActiveError) ErrorCode() string     { return "session_active" }
func (e sessionAlreadyActiveError) SafeMessage() string { return e.Error() }

// sessionOwnerDrainWait is how long Open waits for a closing owner to
// release. Live owners still fail immediately.
const sessionOwnerDrainWait = time.Second

// chatSessionOwners scopes session ownership to one serve process. Direct
// runtimes leave the coordinator nil and retain their standalone behavior.
type chatSessionOwners struct {
	mu     sync.Mutex
	owners map[string]*chatRuntime
}

func newChatSessionOwners() *chatSessionOwners {
	return &chatSessionOwners{owners: make(map[string]*chatRuntime)}
}

// acquireWait takes the session if it is free. A live owner still fails
// immediately. An owner already in close/cleanup is waited out up to wait
// so a same-tab reload does not lose the race against pagehide teardown.
func (o *chatSessionOwners) acquireWait(owner *chatRuntime, sessionID string, wait time.Duration) bool {
	if o == nil || sessionID == "" {
		return true
	}
	deadline := time.Now().Add(wait)
	for {
		o.mu.Lock()
		current := o.owners[sessionID]
		if current == nil || current == owner {
			o.owners[sessionID] = owner
			o.mu.Unlock()
			return true
		}
		draining := current.draining()
		o.mu.Unlock()
		if !draining || wait <= 0 || !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (o *chatSessionOwners) transfer(owner *chatRuntime, from, to string) bool {
	if o == nil || from == to {
		return true
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if current := o.owners[to]; current != nil && current != owner {
		return false
	}
	o.owners[to] = owner
	if o.owners[from] == owner {
		delete(o.owners, from)
	}
	return true
}

func (o *chatSessionOwners) releaseContext(ctx context.Context, owner *chatRuntime, sessionID string) error {
	if o == nil || sessionID == "" {
		return nil
	}
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for !o.mu.TryLock() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
	defer o.mu.Unlock()
	if o.owners[sessionID] == owner {
		delete(o.owners, sessionID)
	}
	return nil
}

func (r *chatRuntime) draining() bool {
	if r == nil {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed || r.cleanupStarted
}
