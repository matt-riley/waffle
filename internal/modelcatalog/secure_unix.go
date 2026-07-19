//go:build unix

package modelcatalog

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

func openNoFollowRegular(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("create file handle for model catalogue cache")
	}
	if err := validateOwnedRegularFile(fd, false); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func acquireRefreshLock(ctx context.Context, path string) (func() error, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("create file handle for model catalogue refresh lock")
	}
	if err := validateOwnedRegularFile(fd, true); err != nil {
		_ = file.Close()
		return nil, err
	}

	for {
		err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return func() error {
				return errors.Join(unix.Flock(fd, unix.LOCK_UN), file.Close())
			}, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			_ = file.Close()
			return nil, err
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			_ = file.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func validateOwnedRegularFile(fd int, enforceMode bool) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return errors.New("model catalogue cache path is not a regular file")
	}
	if int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("model catalogue cache path is owned by uid %d, want %d", stat.Uid, os.Geteuid())
	}
	if enforceMode {
		if err := unix.Fchmod(fd, 0o600); err != nil {
			return err
		}
		return nil
	}
	if stat.Mode&0o077 != 0 {
		return fmt.Errorf("model catalogue cache file mode %04o is not owner-only", stat.Mode&0o777)
	}
	return nil
}
