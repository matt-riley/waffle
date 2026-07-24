// Package filecommit provides a shared crash-safe write-then-rename commit
// for durable JSON/text files (temp → fsync → rename → directory sync).
package filecommit

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Write replaces path with data using a same-directory temp file, fsync,
// atomic rename, and parent-directory sync. mode is applied to the temp file
// before the rename (typically 0o600 for private journals).
func Write(path string, data []byte, mode os.FileMode) (retErr error) {
	dir := filepath.Dir(path)
	temporary, err := os.CreateTemp(dir, ".filecommit-*")
	if err != nil {
		return fmt.Errorf("create staging file: %w", err)
	}
	temporaryPath := temporary.Name()
	closed := false
	committed := false
	defer func() {
		if !closed {
			retErr = errors.Join(retErr, temporary.Close())
		}
		if !committed {
			if removeErr := os.Remove(temporaryPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				retErr = errors.Join(retErr, removeErr)
			}
		}
	}()
	if mode != 0 {
		if err := temporary.Chmod(mode); err != nil {
			return fmt.Errorf("chmod staging file: %w", err)
		}
	}
	if _, err := temporary.Write(data); err != nil {
		return fmt.Errorf("write staging file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync staging file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close staging file: %w", err)
	}
	closed = true
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("commit staging file: %w", err)
	}
	committed = true
	if err := SyncDir(dir); err != nil {
		return fmt.Errorf("sync parent directory: %w", err)
	}
	return nil
}

// SyncDir fsyncs a directory so renames/unlinks are durable.
func SyncDir(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	return directory.Sync()
}
