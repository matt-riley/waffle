package instance

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
	defer lease.Release()

	second := testCoordinator(path, os.Getpid(), time.Now)
	if _, err := second.Acquire(context.Background()); !errors.Is(err, ErrHeld) {
		t.Fatalf("second Acquire = %v, want ErrHeld", err)
	}
}

func TestAcquireDoesNotStealStaleHeartbeatFromLivePID(t *testing.T) {
	now := time.Now()
	path := filepath.Join(t.TempDir(), "serve.lock")
	writeRecord(t, path, Record{PID: os.Getpid(), Owner: "live", Heartbeat: now.Add(-time.Hour)})
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
	defer lease.Release()
	got, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.PID != os.Getpid() || got.Owner == "dead" || !got.Heartbeat.Equal(now) {
		t.Fatalf("recovered record = %#v", got)
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
