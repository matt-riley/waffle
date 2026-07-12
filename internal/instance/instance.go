// Package instance coordinates the single process that owns background work.
package instance

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
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
	stop        chan struct{}
	done        chan struct{}
}

func (c Coordinator) Acquire(ctx context.Context) (*Lease, error) {
	c = c.defaults()
	if err := os.MkdirAll(filepath.Dir(c.Path), 0o700); err != nil {
		return nil, fmt.Errorf("create instance-lock directory: %w", err)
	}
	owner, err := ownerToken()
	if err != nil {
		return nil, err
	}
	record := Record{PID: c.PID, Owner: owner, Heartbeat: c.Now().UTC()}
	for attempts := 0; attempts < 4; attempts++ {
		err := createRecord(c.Path, record)
		if err == nil {
			lease := &Lease{coordinator: c, record: record, stop: make(chan struct{}), done: make(chan struct{})}
			go lease.run(ctx)
			return lease, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("create serve owner lock: %w", err)
		}
		held, readErr := Read(c.Path)
		if readErr != nil {
			return nil, fmt.Errorf("read existing serve owner lock: %w", readErr)
		}
		age := c.Now().Sub(held.Heartbeat)
		if age <= c.StaleAfter || processAlive(held.PID) {
			return nil, fmt.Errorf("%w by pid %d (last heartbeat %s ago)", ErrHeld, held.PID, age.Round(time.Second))
		}
		// Re-read before unlinking so a heartbeat or replacement observed after
		// the first read is never mistaken for the stale owner.
		current, readErr := Read(c.Path)
		if readErr != nil {
			continue
		}
		if current.Owner != held.Owner || !current.Heartbeat.Equal(held.Heartbeat) {
			continue
		}
		if err := os.Remove(c.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("remove stale serve owner lock: %w", err)
		}
	}
	return nil, errors.New("serve owner lock changed repeatedly; retry")
}

func (c Coordinator) Check() (*Record, error) {
	c = c.defaults()
	record, err := Read(c.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	age := c.Now().Sub(record.Heartbeat)
	if age <= c.StaleAfter || processAlive(record.PID) {
		return &record, nil
	}
	return nil, nil
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
			_ = l.heartbeat()
		}
	}
}

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
		return nil
	}
	if err != nil {
		return err
	}
	if current.Owner != l.record.Owner {
		return nil
	}
	return os.Remove(l.coordinator.Path)
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
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate instance owner token: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
