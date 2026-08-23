//go:build unix

package tool

import (
	"fmt"
	"os"
	"os/exec"
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

// Regression (#594): a grandchild that escapes the process group (setsid
// into a new session) survives the group kill and keeps the output pipe
// open. WaitDelay must bound Wait — without it Run never returns past its
// timeout — and the partial output must survive.
func TestBashTimeoutBoundsSetsidEscapee(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid not available")
	}
	start := time.Now()
	_, err := run(t, Bash{}, `{"command":"setsid sleep 300 & echo started","timeout_seconds":1}`)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("want timeout error, got %v", err)
	}
	if !strings.Contains(err.Error(), "started") {
		t.Fatalf("partial output lost: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("Run took %s, want bounded by timeout+WaitDelay", elapsed)
	}
}
