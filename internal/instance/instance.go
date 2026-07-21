// Package instance coordinates the single process that owns background work.
package instance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/matt-riley/waffle/internal/id"
)

var ErrHeld = errors.New("serve owner lock is held")

type Record struct {
	PID       int       `json:"pid"`
	Owner     string    `json:"owner"`
	Heartbeat time.Time `json:"heartbeat"`
}

type Coordinator struct {
	Path              string
	PID               int
	Now               func() time.Time
	HeartbeatInterval time.Duration
	StaleAfter        time.Duration
}

func Default(path string) Coordinator {
	return Coordinator{
		Path: path, PID: os.Getpid(), Now: time.Now,
		HeartbeatInterval: 5 * time.Second,
		StaleAfter:        30 * time.Second,
	}
}

type Lease struct {
	coordinator Coordinator
	record      Record
	lock        *os.File
	stop        chan struct{}
	done        chan struct{}
	errors      chan error
}

func (c Coordinator) Acquire(ctx context.Context) (*Lease, error) {
	c = c.defaults()
	if err := os.MkdirAll(filepath.Dir(c.Path), 0o700); err != nil {
		return nil, fmt.Errorf("create instance-lock directory: %w", err)
	}
	lock, err := os.OpenFile(c.Path+".guard", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open serve advisory lock: %w", err)
	}
	if err := lockFile(lock); err != nil {
		_ = lock.Close()
		if errors.Is(err, ErrHeld) {
			held, readErr := Read(c.Path)
			if readErr == nil {
				age := c.Now().Sub(held.Heartbeat)
				return nil, fmt.Errorf("%w by pid %d (last heartbeat %s ago)", ErrHeld, held.PID, age.Round(time.Second))
			}
			return nil, ErrHeld
		}
		return nil, fmt.Errorf("acquire serve advisory lock: %w", err)
	}
	owner, err := ownerToken()
	if err != nil {
		_ = unlockFile(lock)
		_ = lock.Close()
		return nil, err
	}
	record := Record{PID: c.PID, Owner: owner, Heartbeat: c.Now().UTC()}
	if err := replaceRecord(c.Path, record); err != nil {
		_ = unlockFile(lock)
		_ = lock.Close()
		return nil, fmt.Errorf("write serve owner record: %w", err)
	}
	lease := &Lease{coordinator: c, record: record, lock: lock, stop: make(chan struct{}), done: make(chan struct{}), errors: make(chan error, 1)}
	go lease.run(ctx)
	return lease, nil
}

func (c Coordinator) Check() (*Record, error) {
	c = c.defaults()
	lock, err := os.OpenFile(c.Path+".guard", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	defer func() { _ = lock.Close() }()
	if err := lockFile(lock); err == nil {
		_ = unlockFile(lock)
		return nil, nil
	} else if !errors.Is(err, ErrHeld) {
		return nil, err
	}
	record, err := Read(c.Path)
	if err != nil {
		return nil, err
	}
	return &record, nil
}

func (c Coordinator) defaults() Coordinator {
	if c.PID == 0 {
		c.PID = os.Getpid()
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.HeartbeatInterval <= 0 {
		c.HeartbeatInterval = 5 * time.Second
	}
	if c.StaleAfter <= 0 {
		c.StaleAfter = 30 * time.Second
	}
	return c
}

func (l *Lease) run(ctx context.Context) {
	defer close(l.done)
	ticker := time.NewTicker(l.coordinator.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-l.stop:
			return
		case <-ticker.C:
			if err := l.heartbeat(); err != nil {
				l.errors <- err
				return
			}
		}
	}
}

func (l *Lease) Errors() <-chan error { return l.errors }

func (l *Lease) heartbeat() error {
	current, err := Read(l.coordinator.Path)
	if err != nil {
		return err
	}
	if current.Owner != l.record.Owner {
		return ErrHeld
	}
	l.record.Heartbeat = l.coordinator.Now().UTC()
	return replaceRecord(l.coordinator.Path, l.record)
}

func (l *Lease) Release() error {
	select {
	case <-l.stop:
	default:
		close(l.stop)
	}
	<-l.done
	current, err := Read(l.coordinator.Path)
	if errors.Is(err, os.ErrNotExist) {
		unlockErr := unlockFile(l.lock)
		closeErr := l.lock.Close()
		return errors.Join(unlockErr, closeErr)
	}
	if err != nil {
		unlockErr := unlockFile(l.lock)
		closeErr := l.lock.Close()
		return errors.Join(err, unlockErr, closeErr)
	}
	if current.Owner != l.record.Owner {
		_ = unlockFile(l.lock)
		return l.lock.Close()
	}
	removeErr := os.Remove(l.coordinator.Path)
	unlockErr := unlockFile(l.lock)
	closeErr := l.lock.Close()
	if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return removeErr
	}
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}

func Read(path string) (Record, error) {
	var record Record
	b, err := os.ReadFile(path)
	if err != nil {
		return record, err
	}
	if err := json.Unmarshal(b, &record); err != nil {
		return record, fmt.Errorf("parse %s: %w", path, err)
	}
	if record.PID <= 0 || record.Owner == "" || record.Heartbeat.IsZero() {
		return record, fmt.Errorf("parse %s: incomplete owner record", path)
	}
	return record, nil
}

func createRecord(path string, record Record) error {
	b, err := json.Marshal(record)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	return f.Close()
}

func replaceRecord(path string, record Record) error {
	tmp := path + ".tmp-" + record.Owner
	_ = os.Remove(tmp)
	if err := createRecord(tmp, record); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func ownerToken() (string, error) {
	tok, err := id.NewBytes(16)
	if err != nil {
		return "", fmt.Errorf("generate instance owner token: %w", err)
	}
	return tok, nil
}
