//go:build unix

package tool

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Regression: a timed-out Bash command must not leave orphaned
// grandchildren — context cancellation kills the whole process group.
func TestBashTimeoutKillsProcessGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	cmd := fmt.Sprintf(`sleep 300 & echo $! > %s; sleep 300`, pidFile)
	_, err := run(t, Bash{}, fmt.Sprintf(`{"command":%q,"timeout_seconds":1}`, cmd))
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("want timeout error, got %v", err)
	}

	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read grandchild pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("parse pid %q: %v", raw, err)
	}

	// The grandchild may take a moment to reap; poll briefly.
	dead := false
	for range 50 {
		if killErr := syscall.Kill(pid, 0); killErr == syscall.ESRCH {
			dead = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !dead {
		t.Fatalf("grandchild process %d still alive after bash timeout", pid)
	}
}
