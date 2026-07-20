//go:build !unix

package modelcatalog

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

type secureCacheDir struct {
	root string
}

var processRefreshLocks = struct {
	sync.Mutex
	locks map[string]chan struct{}
}{locks: make(map[string]chan struct{})}

func openSecureCacheDir(path string) (*secureCacheDir, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		return nil, errors.New("model catalogue cache root is not a real 0700 directory")
	}
	return &secureCacheDir{root: path}, nil
}

func secureCacheRoot(path string) (*secureCacheDir, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("model catalogue cache root is not a real directory")
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return nil, err
	}
	return openSecureCacheDir(path)
}

func (d *secureCacheDir) validate() error {
	_, err := openSecureCacheDir(d.root)
	return err
}

func (d *secureCacheDir) close() error {
	return nil
}

func (d *secureCacheDir) openRegular(string) (*os.File, cacheGeneration, error) {
	return nil, cacheGeneration{}, errors.New("secure no-follow cache reads are unavailable on this platform")
}

func (d *secureCacheDir) validateMutationTarget(name string) error {
	info, err := os.Lstat(filepath.Join(d.root, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("model catalogue cache target is not a regular file")
	}
	return nil
}

func (d *secureCacheDir) validateTemporary(file *os.File, name string) (cacheGeneration, error) {
	opened, err := file.Stat()
	if err != nil {
		return cacheGeneration{}, err
	}
	linked, err := os.Lstat(filepath.Join(d.root, name))
	if err != nil {
		return cacheGeneration{}, err
	}
	if !os.SameFile(opened, linked) || !opened.Mode().IsRegular() || opened.Mode().Perm() != 0o600 {
		return cacheGeneration{}, errors.New("model catalogue cache staging file is not a regular 0600 file in the secured directory")
	}
	return cacheGeneration{}, nil
}

func (d *secureCacheDir) commitTemporary(temporary, destination string, _ cacheGeneration) (bool, error) {
	if err := d.validateMutationTarget(destination); err != nil {
		return false, err
	}
	if err := os.Rename(filepath.Join(d.root, temporary), filepath.Join(d.root, destination)); err != nil {
		return false, err
	}
	if err := syncDirectory(d.root); err != nil {
		return true, err
	}
	return true, nil
}

func (d *secureCacheDir) removeRegular(name string) (bool, error) {
	if err := d.validateMutationTarget(name); err != nil {
		return false, err
	}
	path := filepath.Join(d.root, name)
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if err := os.Remove(path); err != nil {
		return false, err
	}
	if err := syncDirectory(d.root); err != nil {
		return true, err
	}
	return true, nil
}

func acquireRefreshLock(ctx context.Context, directory *secureCacheDir, name string) (func() error, error) {
	path := filepath.Join(directory.root, name)
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

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open directory: %w", err)
	}
	defer func() { _ = directory.Close() }()
	return directory.Sync()
}
