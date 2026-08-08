//go:build !unix

package flock

import (
	"fmt"
	"path/filepath"
	"sync"
	"time"
)

// processLocks provides exclusive access to a lock path within a single
// process on platforms without flock. Cross-process coordination is not
// available here.
var processLocks struct {
	mu    sync.Mutex
	locks map[string]chan struct{}
}

func init() {
	processLocks.locks = make(map[string]chan struct{})
}

// Acquire takes a process-local exclusive lock for path with the same timeout
// error semantics as the Unix flock implementation. It does not protect
// against concurrent writers from other OS processes.
func Acquire(path, subject string, timeout time.Duration) (func() error, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}

	processLocks.mu.Lock()
	lock, ok := processLocks.locks[abs]
	if !ok {
		lock = make(chan struct{}, 1)
		lock <- struct{}{}
		processLocks.locks[abs] = lock
	}
	processLocks.mu.Unlock()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-lock:
		return func() error {
			lock <- struct{}{}
			return nil
		}, nil
	case <-timer.C:
		return nil, fmt.Errorf("%s busy: could not acquire lock within %s", subject, timeout)
	}
}
