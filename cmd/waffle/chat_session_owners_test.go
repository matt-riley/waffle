package main

import (
	"context"
	"testing"
	"time"
)

func TestAcquireWaitsForDrainingOwner(t *testing.T) {
	t.Parallel()
	owners := newChatSessionOwners()
	holder := &chatRuntime{closed: true}
	owners.owners["s"] = holder
	next := &chatRuntime{}
	released := make(chan struct{})
	go func() {
		time.Sleep(20 * time.Millisecond)
		if err := owners.releaseContext(context.Background(), holder, "s"); err != nil {
			t.Errorf("release: %v", err)
		}
		close(released)
	}()
	if !owners.acquireWait(next, "s", time.Second) {
		t.Fatal("expected acquire to wait for a draining owner")
	}
	<-released
	if owners.owners["s"] != next {
		t.Fatal("acquire did not take the released session")
	}
}

func TestAcquireFailsFastForLiveOwner(t *testing.T) {
	t.Parallel()
	owners := newChatSessionOwners()
	holder := &chatRuntime{}
	owners.owners["s"] = holder
	next := &chatRuntime{}
	start := time.Now()
	if owners.acquireWait(next, "s", time.Second) {
		t.Fatal("expected a live owner to fail immediately")
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("acquire waited for a live owner")
	}
}
