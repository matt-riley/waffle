//go:build !unix

package tool

import "os/exec"

// configureProcessGroup is a no-op off Unix: there is no portable
// process-group kill, so context cancellation kills only the shell.
func configureProcessGroup(cmd *exec.Cmd) {}
