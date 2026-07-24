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

// chatSessionOwners scopes session ownership to one serve process. Direct
// runtimes leave the coordinator nil and retain their standalone behavior.
type chatSessionOwners struct {
	mu     sync.Mutex
	owners map[string]*chatRuntime
}

func newChatSessionOwners() *chatSessionOwners {
	return &chatSessionOwners{owners: make(map[string]*chatRuntime)}
}

func (o *chatSessionOwners) acquire(owner *chatRuntime, sessionID string) bool {
	if o == nil || sessionID == "" {
		return true
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	current := o.owners[sessionID]
	if current != nil && current != owner {
		return false
	}
	o.owners[sessionID] = owner
	return true
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
