//go:build unix

package selfdev

import (
	"os"
	"syscall"
)

// reexec replaces the process image with path (execve), preserving the
// environment. On success it does not return.
func reexec(path string, args []string) error {
	return syscall.Exec(path, args, os.Environ())
}
