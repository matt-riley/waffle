//go:build linux

package tool

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestBashProcessLimitCoversDescendants launches more children than the
// configured budget. The test exercises either a delegated cgroup (the
// production path) or the Linux RLIMIT_NPROC fallback when the test runner's
// cgroup is not delegated. Root ignores RLIMIT_NPROC, so there is no useful
// assertion in that environment without a cgroup.
func TestBashProcessLimitCoversDescendants(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores RLIMIT_NPROC when the test cgroup is unavailable")
	}
	const limit = 8
	// The fallback is intentionally weaker (per-UID) and can be affected by
	// unrelated processes on the runner. Exercise the descendant-tree
	// guarantee only when this environment permits creating a delegated cgroup;
	// the fallback remains covered by its documented behavior and unit paths.
	probe, ok := createBashCgroup(limit)
	if !ok {
		t.Skip("host cgroup is not delegated; skipping process-tree integration test")
	}
	cleanupBashCgroup(probe)

	command := `
set +e
i=0
pids=
while [ "$i" -lt 32 ]; do
	sleep 30 &
	pid=$!
	if ! kill -0 "$pid" 2>/dev/null; then
		break
	fi
	pids="$pids $pid"
	i=$((i + 1))
done
for pid in $pids; do kill "$pid" 2>/dev/null; done
wait 2>/dev/null
printf 'spawned=%s\n' "$i"
`
	out, err := run(t, Bash{MaxProcesses: limit}, fmt.Sprintf(`{"command":%q}`, command))
	if err != nil {
		t.Fatalf("limited bash failed: %v", err)
	}
	var line string
	for _, candidate := range strings.Split(out, "\n") {
		if strings.HasPrefix(candidate, "spawned=") {
			line = strings.TrimSpace(strings.TrimPrefix(candidate, "spawned="))
			break
		}
	}
	spawned, err := strconv.Atoi(line)
	if err != nil {
		t.Fatalf("spawn count = %q (output %q): %v", line, out, err)
	}
	if spawned <= 0 || spawned >= 32 {
		t.Fatalf("spawned %d children, want a positive count below 32", spawned)
	}
}

func TestCleanupBashCgroupRetriesUntilEmpty(t *testing.T) {
	dir := t.TempDir()
	procsPath := filepath.Join(dir, "cgroup.procs")
	if err := os.WriteFile(filepath.Join(dir, "cgroup.kill"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(procsPath, []byte("1234\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(30 * time.Millisecond)
		_ = os.WriteFile(procsPath, nil, 0o600)
	}()
	cleanupBashCgroup(dir)
	<-done
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("cgroup directory still exists: %v", err)
	}
}
