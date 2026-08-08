//go:build unix

package flock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// pollInterval is the backoff between non-blocking flock attempts.
const pollInterval = 25 * time.Millisecond

// Acquire opens the sidecar lockfile at path (creating it and its parent
// directory if needed) and takes an exclusive advisory flock. It retries with
// pollInterval until timeout elapses. On success, the returned release
// function unlocks and closes the lockfile. A crashed process releases the
// flock when its FD is closed by the kernel, so a live contender is the only
// case that waits until timeout.
//
// subject names the guarded resource in error messages ("secret store",
// "MEMORY.md"), so a busy lock reads as the thing the operator recognizes.
func Acquire(path, subject string, timeout time.Duration) (func() error, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create %s lock dir: %w", subject, err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open %s lock: %w", subject, err)
	}
	// Best-effort: normalize mode even when the file already existed.
	_ = f.Chmod(0o600)

	deadline := time.Now().Add(timeout)
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return func() error {
				return errors.Join(unix.Flock(int(f.Fd()), unix.LOCK_UN), f.Close())
			}, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			_ = f.Close()
			return nil, fmt.Errorf("lock %s: %w", subject, err)
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			_ = f.Close()
			return nil, fmt.Errorf("%s busy: could not acquire lock within %s", subject, timeout)
		}
		sleep := pollInterval
		if remaining < sleep {
			sleep = remaining
		}
		time.Sleep(sleep)
	}
}
