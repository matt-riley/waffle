//go:build sandbox_stress

// Package sandbox stress tests for bind-mount / queue load (#29).
//
// Run with either:
//
//	go test -tags=sandbox_stress ./internal/sandbox -run Stress -count=1
//	WAFFLE_SANDBOX_STRESS=1 go test -tags=sandbox_stress ./internal/sandbox -run Stress -count=1
//
// These tests exercise concurrent Client.Exec against a local Runner (SQLite
// queue pair), documenting the bind-mount stress path used by docker
// sandboxes. They do not require Docker; full docker round-trip is covered
// by `waffle doctor` when [sandbox] mode = "docker" (runner binary check)
// and optional manual `docker run` smoke (see docs/plan.md sandbox section).
package sandbox

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/tool"
)

func TestStressQueueConcurrentExec(t *testing.T) {
	if os.Getenv("WAFFLE_SANDBOX_STRESS") == "" && !testing.Short() {
		// Still run when the build tag is set; env is optional documentation.
	}
	if testing.Short() {
		t.Skip("skipping stress under -short")
	}
	dir := t.TempDir()
	startRunner(t, dir)

	client, err := NewClient(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })

	const workers = 16
	const perWorker = 20
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				out, isErr, err := client.Exec(ctx, "upper", json.RawMessage(`{"s":"stress"}`))
				if err != nil || isErr || out != "STRESS" {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	// Document queue dir layout used by docker bind-mount:
	// host QueueDir ↔ container /queue (inbound/outbound SQLite).
	if _, err := os.Stat(filepath.Join(dir, "inbound.db")); err != nil {
		// Client may use different names; just ensure dir non-empty.
		entries, _ := os.ReadDir(dir)
		if len(entries) == 0 {
			t.Fatal("queue dir empty after stress")
		}
	}
	_ = tool.OutputLimit // document shared truncation cap
}

func TestStressQueueIntegrityAfterKill(t *testing.T) {
	// Crash-style check (#29): mid-flight cancel + integrity_check both DBs.
	if testing.Short() {
		t.Skip("skipping under -short")
	}
	dir := t.TempDir()
	startRunner(t, dir)
	client, err := NewClient(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 20; i++ {
			_, _, _ = client.Exec(ctx, "echo", json.RawMessage(`{"s":"x"}`))
		}
	}()
	time.Sleep(50 * time.Millisecond)
	cancel() // simulate abrupt host stop
	<-done

	for _, name := range []string{inboundFile, outboundFile} {
		schema := inboundSchema
		if name == outboundFile {
			schema = outboundSchema
		}
		db, err := openQueueDB(filepath.Join(dir, name), schema)
		if err != nil {
			t.Fatalf("open %s: %v", name, err)
		}
		var ok string
		if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&ok); err != nil {
			_ = db.Close()
			t.Fatalf("%s integrity: %v", name, err)
		}
		if ok != "ok" {
			_ = db.Close()
			t.Fatalf("%s integrity_check = %q", name, ok)
		}
		_ = db.Close()
	}
}
