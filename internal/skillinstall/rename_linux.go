//go:build linux

package skillinstall

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

func atomicRenameNoReplace(oldPath, newPath string) error {
	err := unix.Renameat2(unix.AT_FDCWD, oldPath, unix.AT_FDCWD, newPath, unix.RENAME_NOREPLACE)
	if errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("%w: target exists", ErrSkillExists)
	}
	return err
}
