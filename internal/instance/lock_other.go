//go:build !unix

package instance

import (
	"errors"
	"os"
)

func lockFile(*os.File) error {
	return errors.New("serve advisory locking is unsupported on this platform")
}
func unlockFile(*os.File) error { return nil }
