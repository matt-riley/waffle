//go:build unix

package tool

import (
	"os"
	"os/exec"
	"syscall"
)

// configureProcessGroup puts the command in its own process group and makes
// context cancellation kill the whole group, not just the shell. Without
// this, a timed-out `bash -c` leaves orphaned grandchildren behind.
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			if err == syscall.ESRCH {
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}
}
