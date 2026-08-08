package flock

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAcquireRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "locks", "secrets.age.lock")
	release, err := Acquire(path, "secret store", time.Second)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if err := release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	// A released lock is immediately reacquirable, and the sidecar directory
	// is created on demand.
	release2, err := Acquire(path, "secret store", time.Second)
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	if err := release2(); err != nil {
		t.Fatalf("second release: %v", err)
	}
}

func TestAcquireNamesTheSubjectWhenBusy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "MEMORY.md.lock")
	release, err := Acquire(path, "MEMORY.md", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = release() }()

	// Same-process contention is enough to exercise the timeout message on
	// every platform; cross-process contention is covered in internal/secret.
	busy := make(chan error, 1)
	go func() {
		second, err := Acquire(path, "MEMORY.md", 50*time.Millisecond)
		if err == nil {
			_ = second()
		}
		busy <- err
	}()
	select {
	case err := <-busy:
		if err == nil {
			t.Fatal("second acquire succeeded while the lock was held")
		}
		if !strings.Contains(err.Error(), "MEMORY.md busy") {
			t.Errorf("error = %q, want it to name the guarded resource", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second acquire never returned")
	}
}
