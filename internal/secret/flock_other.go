//go:build !unix

package secret

import (
	"fmt"
	"path/filepath"
	"sync"
	"time"
)

// processStoreLocks provides exclusive access to a secret store path within
// a single process on platforms without flock. Cross-process coordination
// is not available here.
var processStoreLocks struct {
	mu    sync.Mutex
	locks map[string]chan struct{}
}

func init() {
	processStoreLocks.locks = make(map[string]chan struct{})
}

// acquireStoreLock takes a process-local exclusive lock for path with the
// same timeout error semantics as the Unix flock implementation. It does
// not protect against concurrent writers from other OS processes.
func acquireStoreLock(path string, timeout time.Duration) (func() error, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}

	processStoreLocks.mu.Lock()
	lock, ok := processStoreLocks.locks[abs]
	if !ok {
		lock = make(chan struct{}, 1)
		lock <- struct{}{}
		processStoreLocks.locks[abs] = lock
	}
	processStoreLocks.mu.Unlock()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-lock:
		return func() error {
			lock <- struct{}{}
			return nil
		}, nil
	case <-timer.C:
		return nil, fmt.Errorf("secret store busy: could not acquire lock within %s", timeout)
	}
}
