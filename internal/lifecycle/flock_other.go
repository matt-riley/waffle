//go:build !unix

package lifecycle

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
)

var processFileLocks struct {
	mu    sync.Mutex
	locks map[string]chan struct{}
}

func init() { processFileLocks.locks = make(map[string]chan struct{}) }

func acquireFileLock(ctx context.Context, path string) (func() error, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = filepath.Clean(path)
	}
	processFileLocks.mu.Lock()
	lock, ok := processFileLocks.locks[abs]
	if !ok {
		lock = make(chan struct{}, 1)
		lock <- struct{}{}
		processFileLocks.locks[abs] = lock
	}
	processFileLocks.mu.Unlock()
	select {
	case <-lock:
		return func() error {
			lock <- struct{}{}
			return nil
		}, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("lock skill lifecycle: %w", ctx.Err())
	}
}
