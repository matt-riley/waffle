//go:build unix

package memory

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/flock"
)

// TestCrossProcessAppendSurvivesConcurrentRemoveLines is #267: MEMORY.md
// mutation is read-modify-write, and the lock that guarded it was
// process-local. Two waffle processes share a $WAFFLE_HOME routinely — a chat
// REPL beside serve — so an append landing between RemoveLines' read and its
// write back was silently erased.
//
// Helper mode is activated via WAFFLE_MEMORY_FLOCK_HELPER.
func TestCrossProcessAppendSurvivesConcurrentRemoveLines(t *testing.T) {
	if mode := os.Getenv("WAFFLE_MEMORY_FLOCK_HELPER"); mode != "" {
		runMemoryHelper(mode)
		return
	}

	dir := t.TempDir()
	workspace := Workspace{Dir: dir}
	// Seed enough notes that RemoveLines has a real read-modify-write to do.
	for i := range 4 {
		if _, err := workspace.Append("seed note " + strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, mode := range []string{"append", "remove"} {
		wg.Add(1)
		go func(mode string) {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=^TestCrossProcessAppendSurvivesConcurrentRemoveLines$", "-test.v=false")
			cmd.Env = append(os.Environ(),
				"WAFFLE_MEMORY_FLOCK_HELPER="+mode,
				"WAFFLE_MEMORY_DIR="+dir,
			)
			if out, err := cmd.CombinedOutput(); err != nil {
				errs <- fmt.Errorf("helper %s: %v\n%s", mode, err, out)
			}
		}(mode)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if t.Failed() {
		return
	}

	body, err := os.ReadFile(workspace.MemoryPath())
	if err != nil {
		t.Fatal(err)
	}
	// The appending process was told its note was saved, so it must be in the
	// file whichever order the two processes ran in.
	if !strings.Contains(string(body), "concurrent append") {
		t.Fatalf("append from a second process was lost: %s", body)
	}
	// And the removal must have taken effect rather than being clobbered.
	if strings.Contains(string(body), "seed note 0") {
		t.Fatalf("RemoveLines did not take effect: %s", body)
	}
}

func runMemoryHelper(mode string) {
	workspace := Workspace{Dir: os.Getenv("WAFFLE_MEMORY_DIR")}
	var err error
	switch mode {
	case "append":
		_, err = workspace.Append("concurrent append")
	case "remove":
		err = workspace.RemoveLines([]int{1})
	default:
		fmt.Fprintf(os.Stderr, "unknown helper mode %q\n", mode)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", mode, err)
		os.Exit(1)
	}
	os.Exit(0)
}

// TestMemoryMutationReportsBusyLock proves a held lock produces a bounded,
// named error rather than a silent overwrite or an unbounded wait.
func TestMemoryMutationReportsBusyLock(t *testing.T) {
	dir := t.TempDir()
	workspace := Workspace{Dir: dir}
	if _, err := workspace.Append("seed"); err != nil {
		t.Fatal(err)
	}

	previous := memoryLockTimeout
	memoryLockTimeout = 100 * time.Millisecond
	t.Cleanup(func() { memoryLockTimeout = previous })

	release, err := flock.Acquire(memoryLockPath(workspace.MemoryPath()), "MEMORY.md", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = release() }()

	start := time.Now()
	_, err = workspace.Append("blocked note")
	if err == nil {
		t.Fatal("Append succeeded while another holder had the lock")
	}
	if !strings.Contains(err.Error(), "MEMORY.md busy") {
		t.Errorf("error = %q, want it to name MEMORY.md", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("Append waited %s, want a bounded wait", elapsed)
	}
	// The blocked note must not have reached the file.
	body, err := os.ReadFile(workspace.MemoryPath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "blocked note") {
		t.Error("a note whose lock was never acquired was written anyway")
	}
	// The sidecar lock lives out of the way of the owner's own files.
	if _, err := os.Stat(filepath.Join(dir, ".memory-locks")); err != nil {
		t.Errorf("sidecar lock directory: %v", err)
	}
}
