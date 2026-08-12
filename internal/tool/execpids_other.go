//go:build !linux

package tool

import "os/exec"

// There is no portable process-tree PID boundary outside Linux. In
// particular, RLIMIT_NPROC on platforms such as macOS is a per-UID limit and
// can unexpectedly include unrelated owner processes, so host Bash remains
// unrestricted there. Use Docker mode when a hard process budget is required.
func configureProcessLimit(_ *exec.Cmd, _ int) func() { return func() {} }
