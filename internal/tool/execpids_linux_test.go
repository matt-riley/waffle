//go:build linux

package tool

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
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
