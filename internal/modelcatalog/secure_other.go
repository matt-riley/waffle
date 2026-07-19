//go:build !unix

package modelcatalog

import (
	"context"
	"errors"
	"os"
	"sync"
)

var processRefreshLocks = struct {
	sync.Mutex
	locks map[string]chan struct{}
}{locks: make(map[string]chan struct{})}

func openNoFollowRegular(string) (*os.File, error) {
	return nil, errors.New("secure no-follow cache reads are unavailable on this platform")
}

func acquireRefreshLock(ctx context.Context, path string) (func() error, error) {
	processRefreshLocks.Lock()
	lock, ok := processRefreshLocks.locks[path]
	if !ok {
		lock = make(chan struct{}, 1)
		lock <- struct{}{}
		processRefreshLocks.locks[path] = lock
	}
	processRefreshLocks.Unlock()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-lock:
		return func() error {
			lock <- struct{}{}
			return nil
		}, nil
	}
}
