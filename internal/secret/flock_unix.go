//go:build unix

package secret

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// storeLockPoll is the backoff between non-blocking flock attempts.
const storeLockPoll = 25 * time.Millisecond

// acquireStoreLock opens the sidecar lockfile at path (creating it and its
// parent directory if needed) and takes an exclusive advisory flock. It
// retries with storeLockPoll until timeout elapses. On success, the returned
// release function unlocks and closes the lockfile. A crashed process
// releases the flock when its FD is closed by the kernel, so a live
// contender is the only case that waits until timeout.
func acquireStoreLock(path string, timeout time.Duration) (func() error, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create secret store lock dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open secret store lock: %w", err)
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
			return nil, fmt.Errorf("lock secret store: %w", err)
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			_ = f.Close()
			return nil, fmt.Errorf("secret store busy: could not acquire lock within %s", timeout)
		}
		sleep := storeLockPoll
		if remaining < sleep {
			sleep = remaining
		}
		time.Sleep(sleep)
	}
}
