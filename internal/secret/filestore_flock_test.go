//go:build unix

package secret

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"filippo.io/age"
	"golang.org/x/sys/unix"
)

// TestFileStoreCrossProcessConcurrentSet proves that two separate OS processes
// writing different keys to the same store both persist (no lost update).
// Helper mode is activated via WAFFLE_SECRET_FLOCK_HELPER=1.
func TestFileStoreCrossProcessConcurrentSet(t *testing.T) {
	if os.Getenv("WAFFLE_SECRET_FLOCK_HELPER") == "1" {
		runCrossProcessSetHelper()
		return
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.age")
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}

	// Seed the store so both helpers perform a true load-modify-save.
	seed := OpenFile(path, id)
	if err := seed.Set("seed/key", "seed-value"); err != nil {
		t.Fatalf("seed Set: %v", err)
	}

	type write struct {
		key, value string
	}
	writes := []write{
		{"proc/a", "value-from-process-a"},
		{"proc/b", "value-from-process-b"},
	}

	var wg sync.WaitGroup
	errs := make(chan error, len(writes))
	for _, w := range writes {
		wg.Add(1)
		go func(w write) {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=^TestFileStoreCrossProcessConcurrentSet$", "-test.v=false")
			cmd.Env = append(os.Environ(),
				"WAFFLE_SECRET_FLOCK_HELPER=1",
				"WAFFLE_SECRET_PATH="+path,
				"WAFFLE_SECRET_IDENTITY="+id.String(),
				"WAFFLE_SECRET_KEY="+w.key,
				"WAFFLE_SECRET_VALUE="+w.value,
			)
			out, err := cmd.CombinedOutput()
			if err != nil {
				errs <- fmt.Errorf("helper %s: %v\n%s", w.key, err, out)
				return
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if t.Failed() {
		return
	}

	// Both keys must be present after concurrent writes.
	s := OpenFile(path, id)
	for _, w := range writes {
		got, err := s.Get(w.key)
		if err != nil {
			t.Errorf("Get(%q): %v", w.key, err)
			continue
		}
		if got != w.value {
			t.Errorf("Get(%q) = %q, want %q", w.key, got, w.value)
		}
	}
	// Seed must also survive.
	if got, err := s.Get("seed/key"); err != nil || got != "seed-value" {
		t.Errorf("seed key: got %q, %v", got, err)
	}
	names, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(names) != 3 {
		t.Errorf("List = %v, want 3 names (seed + two concurrent writes)", names)
	}
}

func runCrossProcessSetHelper() {
	path := os.Getenv("WAFFLE_SECRET_PATH")
	idStr := os.Getenv("WAFFLE_SECRET_IDENTITY")
	key := os.Getenv("WAFFLE_SECRET_KEY")
	value := os.Getenv("WAFFLE_SECRET_VALUE")
	if path == "" || idStr == "" || key == "" {
		fmt.Fprintln(os.Stderr, "helper missing env")
		os.Exit(2)
	}
	id, err := age.ParseX25519Identity(idStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse identity: %v\n", err)
		os.Exit(2)
	}
	s := OpenFile(path, id)
	if err := s.Set(key, value); err != nil {
		fmt.Fprintf(os.Stderr, "Set: %v\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// TestFileStoreLockTimeout holds the sidecar flock in this process and
// verifies that a concurrent Set fails with a clear busy error after the
// configured timeout rather than blocking forever.
func TestFileStoreLockTimeout(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.age")
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	s := OpenFile(path, id)

	// Create and exclusively flock the sidecar so Set cannot proceed.
	lockPath := s.lockPath()
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	// Keep the lock for the whole test; unlock and close on exit.
	defer func() {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		_ = f.Close()
	}()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		_ = f.Close()
		t.Fatalf("hold flock: %v", err)
	}

	// Short timeout so the test is fast.
	prev := storeLockTimeout
	storeLockTimeout = 200 * time.Millisecond
	defer func() { storeLockTimeout = prev }()

	start := time.Now()
	err = s.Set("timeout/key", "should-fail")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Set succeeded while lock held, want timeout error")
	}
	if !strings.Contains(err.Error(), "secret store busy") {
		t.Errorf("error = %q, want containing %q", err, "secret store busy")
	}
	if !strings.Contains(err.Error(), "could not acquire lock within") {
		t.Errorf("error = %q, want containing lock timeout wording", err)
	}
	// Should fail near the timeout, not immediately and not hang far past it.
	if elapsed < 150*time.Millisecond {
		t.Errorf("Set returned too quickly (%v), expected to wait ~timeout", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Errorf("Set took too long (%v), expected ~200ms timeout", elapsed)
	}
}

// TestAcquireStoreLockReleasesOnClose verifies a second process can acquire
// the lock after the first releases (basic flock release path).
func TestAcquireStoreLockRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.age.lock")
	release, err := acquireStoreLock(path, time.Second)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if err := release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	release2, err := acquireStoreLock(path, time.Second)
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	if err := release2(); err != nil {
		t.Fatalf("second release: %v", err)
	}
}
