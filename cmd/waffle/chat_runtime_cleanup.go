package main

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"
)

type chatRuntimeCleanup struct {
	mu     sync.Mutex
	client io.Closer
	agent  agentCleanupContext
}

func newChatRuntimeCleanup(client io.Closer, agentCleanup agentCleanupContext) *chatRuntimeCleanup {
	if client == nil && agentCleanup == nil {
		return nil
	}
	return &chatRuntimeCleanup{client: client, agent: agentCleanup}
}

func (c *chatRuntimeCleanup) close(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	var cleanupErr error
	if c.client != nil {
		if err := closeRuntimeResource(ctx, c.client); err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
		} else {
			c.client = nil
		}
	}
	if c.agent != nil {
		if err := c.agent(ctx); err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
		} else {
			c.agent = nil
		}
	}
	return cleanupErr
}

func (c *chatRuntimeCleanup) complete() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.client == nil && c.agent == nil
}

type contextCloser interface {
	CloseContext(context.Context) error
}

func closeRuntimeResource(ctx context.Context, closer io.Closer) error {
	if contextual, ok := closer.(contextCloser); ok {
		return contextual.CloseContext(ctx)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return closer.Close()
}

// newChatRuntime records dependencies without constructing provider, sandbox,
// or MCP resources. Open performs that work after validating client options.

func (r *chatRuntime) finishCommand(commandCancel context.CancelFunc, commandDone chan struct{}) {
	commandCancel()
	r.mu.Lock()
	if r.commandDone == commandDone {
		r.commandCancel = nil
		r.commandDone = nil
	}
	close(commandDone)
	r.mu.Unlock()
}

func detachedRuntimeCloseContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	deadline := time.Now().Add(timeout)
	if existing, ok := ctx.Deadline(); ok && existing.Before(deadline) {
		deadline = existing
	}
	return context.WithDeadline(context.WithoutCancel(ctx), deadline)
}

func (r *chatRuntime) cleanup(ctx context.Context) error {
	var reflectionErr error
	if err := r.reflectSession(ctx); err != nil {
		reflectionErr = err
	}

	var teardownErr error
	r.mu.Lock()
	wsClient := r.wsClient
	agentCleanup := r.agentCleanupContext
	resourceCancel := r.resourceCancel
	ownedSessionID := r.ownedSessionID
	r.mu.Unlock()
	if wsClient != nil {
		if err := closeRuntimeResource(ctx, wsClient); err != nil {
			teardownErr = errors.Join(teardownErr, err)
		} else {
			r.mu.Lock()
			r.wsClient = nil
			r.mu.Unlock()
		}
	}
	if agentCleanup != nil {
		if err := agentCleanup(ctx); err != nil {
			teardownErr = errors.Join(teardownErr, err)
		} else {
			r.mu.Lock()
			if r.agentCleanupContext != nil {
				r.agentCleanupContext = nil
			}
			r.mu.Unlock()
		}
	}
	teardownErr = errors.Join(teardownErr, r.cleanupRetiredResources(ctx))
	if teardownErr != nil {
		return errors.Join(reflectionErr, teardownErr)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := r.sessionOwners.releaseContext(ctx, r, ownedSessionID); err != nil {
		return err
	}
	if resourceCancel != nil {
		resourceCancel()
	}
	r.mu.Lock()
	r.resourceCancel = nil
	if r.ownedSessionID == ownedSessionID {
		r.ownedSessionID = ""
	}
	r.mu.Unlock()
	if reflectionErr != nil {
		return completedChatCleanupError{err: reflectionErr}
	}
	return nil
}

func (r *chatRuntime) cleanupRetiredResources(ctx context.Context) error {
	r.mu.Lock()
	retired := append([]*chatRuntimeCleanup(nil), r.retiredCleanup...)
	r.mu.Unlock()

	var cleanupErr error
	for _, cleanup := range retired {
		cleanupErr = errors.Join(cleanupErr, cleanup.close(ctx))
	}

	r.mu.Lock()
	remaining := r.retiredCleanup[:0]
	for _, cleanup := range r.retiredCleanup {
		if !cleanup.complete() {
			remaining = append(remaining, cleanup)
		}
	}
	for i := len(remaining); i < len(r.retiredCleanup); i++ {
		r.retiredCleanup[i] = nil
	}
	r.retiredCleanup = remaining
	r.mu.Unlock()
	return cleanupErr
}

type completedChatCleanupError struct{ err error }

func (e completedChatCleanupError) Error() string        { return e.err.Error() }
func (e completedChatCleanupError) Unwrap() error        { return e.err }
func (completedChatCleanupError) CleanupCompleted() bool { return true }

func cleanupCompleted(err error) bool {
	if err == nil {
		return true
	}
	var completed interface{ CleanupCompleted() bool }
	return errors.As(err, &completed) && completed.CleanupCompleted()
}
