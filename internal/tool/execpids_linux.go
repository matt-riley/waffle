//go:build linux

package tool

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

// bashProcessCgroupScript is run by the outer shell before it execs the
// requested command. Moving that shell into the cgroup before exec is what
// makes the pids.max boundary apply to every descendant, rather than only to
// a process started after the command is already running.
//
// The original command is passed as $1, not interpolated into this script.
// This keeps shell metacharacters in the requested command data rather than
// changing the limit setup logic.
const bashProcessCgroupScript = `
if [ -n "$2" ] && printf '%s\n' "$$" > "$2" 2>/dev/null; then
	exec bash -c -- "$1"
fi
# A cgroup may be unavailable (for example, an undelegated systemd service).
# RLIMIT_NPROC is a useful fallback on Linux, although it is per real UID,
# not a cgroup, so it is deliberately documented as weaker.
if ulimit -u "$3" 2>/dev/null; then
	exec bash -c -- "$1"
fi
printf '%s\n' 'waffle: unable to apply the host bash process limit; use Docker sandbox or delegate the service cgroup' >&2
exit 125
`

var bashCgroupSequence atomic.Uint64

// configureProcessLimit arranges a limit for cmd without changing the
// Waffle process's own limits. Linux cgroup v2 is preferred because pids.max
// is a true process-tree boundary. The RLIMIT_NPROC fallback is scoped to the
// child shell and is only a best-effort per-UID budget.
func configureProcessLimit(cmd *exec.Cmd, limit int) (cleanup func()) {
	if limit <= 0 {
		return func() {}
	}

	original := cmd.Args[len(cmd.Args)-1]
	if cgroup, ok := createBashCgroup(limit); ok {
		cmd.Args = []string{
			"bash", "-c", bashProcessCgroupScript,
			"waffle-bash-limit", original,
			filepath.Join(cgroup, "cgroup.procs"),
			strconv.Itoa(fallbackNProcLimit(limit)),
		}
		return func() { cleanupBashCgroup(cgroup) }
	}

	// No delegated cgroup is common for manually launched binaries. Keep the
	// command useful with the weaker child-only fallback; unlike setting a
	// limit in this process, this cannot alter Waffle or sibling commands.
	fallback := fallbackNProcLimit(limit)
	cmd.Args = []string{
		"bash", "-c", bashProcessCgroupScript,
		"waffle-bash-limit", original, "", strconv.Itoa(fallback),
	}
	return func() {}
}

// createBashCgroup creates a child cgroup under the caller's delegated cgroup
// (when running under cgroup v2). An ordinary system service needs
// Delegate=yes for this operation; failure is intentionally non-fatal because
// the child RLIMIT_NPROC fallback below still protects common cases.
func createBashCgroup(limit int) (string, bool) {
	parent, err := delegatedCgroupPath()
	if err != nil {
		return "", false
	}
	name := fmt.Sprintf("waffle-bash-%d-%d", os.Getpid(), bashCgroupSequence.Add(1))
	path := filepath.Join(parent, name)
	if err := os.Mkdir(path, 0o700); err != nil {
		return "", false
	}
	if err := os.WriteFile(filepath.Join(path, "pids.max"), []byte(strconv.Itoa(limit)), 0o600); err != nil {
		_ = os.Remove(path)
		return "", false
	}
	return path, true
}

func delegatedCgroupPath() (string, error) {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	var relative string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "0::") {
			relative = strings.TrimPrefix(line, "0::")
			break
		}
	}
	if relative == "" || !filepath.IsAbs(relative) {
		return "", fmt.Errorf("cgroup v2 path not found")
	}
	path := filepath.Join("/sys/fs/cgroup", relative)
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("cgroup path unavailable: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("cgroup path is not a directory")
	}
	return path, nil
}

const (
	cgroupCleanupAttempts = 50
	cgroupCleanupInterval = 10 * time.Millisecond
)

func cleanupBashCgroup(path string) {
	// cgroup.kill is v2-specific and makes cleanup safe even if a detached
	// grandchild retained the process group after CombinedOutput returned.
	// Death and removal from cgroup.procs are asynchronous, so retry until the
	// cgroup is empty before removing it. A one-shot remove can return EBUSY and
	// leave a stale per-command cgroup behind.
	killPath := filepath.Join(path, "cgroup.kill")
	procsPath := filepath.Join(path, "cgroup.procs")
	for attempt := 0; attempt < cgroupCleanupAttempts; attempt++ {
		if err := os.WriteFile(killPath, []byte("1"), 0o600); err != nil && os.IsNotExist(err) {
			return
		}
		procs, err := os.ReadFile(procsPath)
		if os.IsNotExist(err) {
			return
		}
		if err == nil && strings.TrimSpace(string(procs)) == "" {
			if err := os.Remove(path); err == nil || os.IsNotExist(err) {
				return
			}
		}
		time.Sleep(cgroupCleanupInterval)
	}
	// Keep the cleanup best-effort: if a host kernel still reports EBUSY after
	// the bounded wait, do not block the command result indefinitely.
	_ = os.Remove(path)
}

// fallbackNProcLimit translates a per-command descendant budget into the
// per-real-UID RLIMIT_NPROC value expected by Linux. The shell itself is
// created after this count, so leave room for it plus the requested children.
// Concurrent host commands can still share the UID budget; this is why the
// cgroup path above is preferred.
func fallbackNProcLimit(descendants int) int {
	count := processCountForUID(os.Getuid())
	if count < 0 {
		count = 0
	}
	target := count + descendants + 1
	var r unix.Rlimit
	if unix.Getrlimit(unix.RLIMIT_NPROC, &r) == nil && r.Max != unix.RLIM_INFINITY && uint64(target) > r.Max {
		target = int(r.Max)
	}
	if target < 1 {
		target = 1
	}
	return target
}

func processCountForUID(uid int) int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return -1
	}
	count := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		status, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "status"))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(status), "\n") {
			if !strings.HasPrefix(line, "Uid:") {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) > 1 && fields[1] == strconv.Itoa(uid) {
				count++
			}
			break
		}
	}
	return count
}
