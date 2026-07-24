//go:build darwin

package skillinstall

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

func atomicRenameNoReplace(oldPath, newPath string) error {
	err := unix.RenamexNp(oldPath, newPath, unix.RENAME_EXCL)
	if errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("%w: target exists", ErrSkillExists)
	}
	return err
}
