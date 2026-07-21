package secret

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"filippo.io/age"
)

// storeLockTimeout is how long Set/Get/Delete/List wait for the exclusive
// flock under .secret-locks/ before failing. A crashed holder releases the
// flock when its FD closes, so this bounds wait for a live contender rather
// than permanently wedging access. Overridable in tests.
var storeLockTimeout = 5 * time.Second

// FileStore keeps secrets as a JSON object age-encrypted to an X25519
// recipient, in a single file (default ~/.waffle/secrets.age). The identity
// needed to decrypt lives in the OS keyring or $WAFFLE_AGE_IDENTITY — never
// next to the file (see identity.go).
//
// Concurrent access: mu serializes goroutines in one process; an exclusive
// flock on a lockfile under .secret-locks/ serializes OS processes so
// load-modify-save cannot lose updates across waffle secret set vs serve.
// Locks live in a dedicated subdirectory so they are not mistaken for
// providerconfig staging temps (CreateTemp ".provider-stage-*").
type FileStore struct {
	path string
	id   *age.X25519Identity

	mu sync.Mutex
}

// OpenFile returns a FileStore at path using id. The file need not exist
// yet; it is created on first Set.
func OpenFile(path string, id *age.X25519Identity) *FileStore {
	return &FileStore{path: path, id: id}
}

// lockPath returns the exclusive-lock path for this store. It lives under
// <dir>/.secret-locks/<basename>.lock so a store at
// .../.provider-stage-XYZ does not leave .../.provider-stage-XYZ.lock
// matching provider staging globs after the stage file is removed.
func (s *FileStore) lockPath() string {
	dir := filepath.Dir(s.path)
	base := filepath.Base(s.path)
	return filepath.Join(dir, ".secret-locks", base+".lock")
}

// withStoreLock runs fn while holding both the process mutex and the
// exclusive sidecar flock. Order is always mu then flock to avoid deadlocks.
// On lock timeout, returns an error like
// "secret store busy: could not acquire lock within 5s".
func (s *FileStore) withStoreLock(fn func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	release, err := acquireStoreLock(s.lockPath(), storeLockTimeout)
	if err != nil {
		return err
	}
	defer func() { _ = release() }()
	return fn()
}

func (s *FileStore) load() (map[string]string, error) {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("read secret store: %w", err)
	}
	r, err := age.Decrypt(bytes.NewReader(raw), s.id)
	if err != nil {
		return nil, fmt.Errorf("decrypt secret store (wrong identity?): %w", err)
	}
	plain, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("decrypt secret store: %w", err)
	}
	m := map[string]string{}
	if err := json.Unmarshal(plain, &m); err != nil {
		return nil, fmt.Errorf("parse secret store: %w", err)
	}
	return m, nil
}

func (s *FileStore) save(m map[string]string) error {
	plain, err := json.Marshal(m)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, s.id.Recipient())
	if err != nil {
		return fmt.Errorf("encrypt secret store: %w", err)
	}
	if _, err := w.Write(plain); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}

	// Write-then-rename so a crash never leaves a truncated store.
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".secrets-*.age")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}

// Set stores value under name, overwriting any previous value.
func (s *FileStore) Set(name, value string) error {
	if !ValidName(name) {
		return fmt.Errorf("invalid secret name %q (want e.g. \"anthropic/api-key\")", name)
	}
	return s.withStoreLock(func() error {
		m, err := s.load()
		if err != nil {
			return err
		}
		m[name] = value
		return s.save(m)
	})
}

// Get returns the value stored under name, or ErrNotFound.
func (s *FileStore) Get(name string) (string, error) {
	var v string
	err := s.withStoreLock(func() error {
		m, err := s.load()
		if err != nil {
			return err
		}
		got, ok := m[name]
		if !ok {
			return fmt.Errorf("%q: %w", name, ErrNotFound)
		}
		v = got
		return nil
	})
	return v, err
}

// Delete removes name from the store; deleting a missing name is an error
// so typos are caught.
func (s *FileStore) Delete(name string) error {
	return s.withStoreLock(func() error {
		m, err := s.load()
		if err != nil {
			return err
		}
		if _, ok := m[name]; !ok {
			return fmt.Errorf("%q: %w", name, ErrNotFound)
		}
		delete(m, name)
		return s.save(m)
	})
}

// List returns all secret names, sorted. Never values.
func (s *FileStore) List() ([]string, error) {
	var names []string
	err := s.withStoreLock(func() error {
		m, err := s.load()
		if err != nil {
			return err
		}
		names = make([]string, 0, len(m))
		for k := range m {
			names = append(names, k)
		}
		sort.Strings(names)
		return nil
	})
	return names, err
}
