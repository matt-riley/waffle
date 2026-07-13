package instance

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAcquireRefusesHeldLiveOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "serve.lock")
	first := testCoordinator(path, os.Getpid(), time.Now)
	lease, err := first.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Release() }()

	second := testCoordinator(path, os.Getpid(), time.Now)
	if _, err := second.Acquire(context.Background()); !errors.Is(err, ErrHeld) {
		t.Fatalf("second Acquire = %v, want ErrHeld", err)
	}
}

func TestAcquireDoesNotStealStaleHeartbeatFromLivePID(t *testing.T) {
	now := time.Now()
	path := filepath.Join(t.TempDir(), "serve.lock")
	ownerNow := now.Add(-time.Hour)
	owner := testCoordinator(path, os.Getpid(), func() time.Time { return ownerNow })
	lease, err := owner.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Release() }()
	c := testCoordinator(path, os.Getpid(), func() time.Time { return now })
	if _, err := c.Acquire(context.Background()); !errors.Is(err, ErrHeld) {
		t.Fatalf("Acquire stale live PID = %v, want ErrHeld", err)
	}
}

func TestAcquireRecoversStaleDeadOwner(t *testing.T) {
	now := time.Now()
	path := filepath.Join(t.TempDir(), "serve.lock")
	writeRecord(t, path, Record{PID: 99999999, Owner: "dead", Heartbeat: now.Add(-time.Hour)})
	c := testCoordinator(path, os.Getpid(), func() time.Time { return now })
	lease, err := c.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire stale dead owner: %v", err)
	}
	defer func() { _ = lease.Release() }()
	got, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.PID != os.Getpid() || got.Owner == "dead" || !got.Heartbeat.Equal(now) {
		t.Fatalf("recovered record = %#v", got)
	}
}

func TestConcurrentStaleDeadTakeoverHasExactlyOneWinner(t *testing.T) {
	now := time.Now()
	path := filepath.Join(t.TempDir(), "serve.lock")
	writeRecord(t, path, Record{PID: 99999999, Owner: "dead", Heartbeat: now.Add(-time.Hour)})
	start := make(chan struct{})
	var winners atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			lease, err := testCoordinator(path, os.Getpid(), func() time.Time { return now }).Acquire(context.Background())
			if err == nil {
				winners.Add(1)
				t.Cleanup(func() { _ = lease.Release() })
				return
			}
			if !errors.Is(err, ErrHeld) {
				t.Errorf("takeover error = %v, want ErrHeld", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if got := winners.Load(); got != 1 {
		t.Fatalf("stale takeover winners = %d, want exactly 1", got)
	}
}

func TestHeartbeatOwnershipLossIsObservable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "serve.lock")
	c := testCoordinator(path, os.Getpid(), time.Now)
	c.HeartbeatInterval = time.Millisecond
	lease, err := c.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Release() }()
	writeRecord(t, path, Record{PID: 1234, Owner: "replacement", Heartbeat: time.Now()})
	select {
	case err := <-lease.Errors():
		if !errors.Is(err, ErrHeld) {
			t.Fatalf("ownership loss = %v, want ErrHeld", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ownership loss was not reported")
	}
}

func TestLeaseHeartbeatAndOwnerSafeRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "serve.lock")
	now := time.Now()
	c := testCoordinator(path, os.Getpid(), func() time.Time { return now })
	lease, err := c.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if err := lease.heartbeat(); err != nil {
		t.Fatal(err)
	}
	got, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Heartbeat.Equal(now) {
		t.Fatalf("heartbeat = %s, want %s", got.Heartbeat, now)
	}
	writeRecord(t, path, Record{PID: 1234, Owner: "successor", Heartbeat: now})
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	got, err = Read(path)
	if err != nil || got.Owner != "successor" {
		t.Fatalf("release removed successor: record=%#v err=%v", got, err)
	}
}

func testCoordinator(path string, pid int, now func() time.Time) Coordinator {
	return Coordinator{Path: path, PID: pid, Now: now, HeartbeatInterval: time.Hour, StaleAfter: 30 * time.Second}
}

func writeRecord(t *testing.T, path string, record Record) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}
