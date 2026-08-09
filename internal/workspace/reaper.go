package workspace

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// SweepManager is the Manager surface Reaper.Sweep needs. *Manager implements
// it; tests may supply a fake that fails individual Idle/Close calls (#110).
type SweepManager interface {
	List(ctx context.Context) ([]Workspace, error)
	Idle(ctx context.Context, id string) error
	Close(ctx context.Context, id string, force bool) (*CloseReport, error)
}

// ActivityProbe reports activity this process has seen that a workspace's
// stored last_active may not have recorded (#260). A failed Touch leaves
// last_active stale, and stale is indistinguishable from idle, so a sweep
// corroborates before stopping a container. *Manager implements it.
type ActivityProbe interface {
	ActiveSince(id string, since time.Time) bool
}

// Reaper sweeps workspaces from the single serve owner. A zero timeout
// disables that part of the sweep.
type Reaper struct {
	Manager     SweepManager
	IdleTimeout time.Duration
	CloseTTL    time.Duration
	Now         func() time.Time
	Notify      func(context.Context, Workspace, string) error
}

func (r *Reaper) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

// managerIdleTimeout returns the Manager's host idle timeout when the
// concrete type is *Manager. Repo policy may only shorten idle for its own
// workspace at open; the shared Manager field is never mutated (#282), so
// this is host config only.
func (r *Reaper) managerIdleTimeout() time.Duration {
	if m, ok := r.Manager.(*Manager); ok && m != nil {
		return m.IdleTimeout
	}
	return 0
}

// activeSince reports in-process activity newer than the idle cutoff, for
// managers that track it. A workspace this process has just run a command in
// is not idle, whatever last_active says (#260).
func (r *Reaper) activeSince(id string, since time.Time) bool {
	probe, ok := r.Manager.(ActivityProbe)
	return ok && probe.ActiveSince(id, since)
}

// Sweep performs one deterministic lifecycle pass. Per-workspace Idle/Close
// (and dirty-notify) failures are accumulated with errors.Join and do not
// abort the rest of the pass (#110).
func (r *Reaper) Sweep(ctx context.Context) error {
	if r == nil || r.Manager == nil {
		return nil
	}
	items, err := r.Manager.List(ctx)
	if err != nil {
		return err
	}
	now := r.now()
	idleTimeout := r.IdleTimeout
	// Prefer manager idle when set (host config; never mutated by repo policy, #282).
	if mt := r.managerIdleTimeout(); mt > 0 {
		if idleTimeout <= 0 || mt < idleTimeout {
			idleTimeout = mt
		}
	}
	var errs []error
	for _, ws := range items {
		last := ws.LastActive
		if last.IsZero() {
			last = ws.UpdatedAt
		}
		age := now.Sub(last)
		// A workspace whose repo tightened idle below the host value is idled
		// on its own policy; the shared Manager field is never touched (#282).
		wsIdle := idleTimeout
		if d, perr := time.ParseDuration(ws.IdleTimeout); perr == nil && d > 0 && (wsIdle <= 0 || d < wsIdle) {
			wsIdle = d
		}
		if ws.Status == StatusOpen && wsIdle > 0 && age >= wsIdle && !r.activeSince(ws.ID, now.Add(-wsIdle)) {
			if err := r.Manager.Idle(ctx, ws.ID); err != nil {
				errs = append(errs, fmt.Errorf("idle workspace %s: %w", ws.ID, err))
			} else {
				ws.Status = StatusIdle
			}
		}
		if r.CloseTTL <= 0 || age < r.CloseTTL || ws.Status == StatusClosed {
			continue
		}
		report, closeErr := r.Manager.Close(ctx, ws.ID, false)
		if closeErr == nil {
			continue
		}
		if report != nil && (report.Dirty != "" || report.Unpushed != "") && r.Notify != nil {
			msg := fmt.Sprintf("workspace %s passed its close TTL but has unpushed or unsaved work; it was kept", ws.Repo)
			if err := r.Notify(ctx, ws, msg); err != nil {
				errs = append(errs, fmt.Errorf("notify workspace %s: %w", ws.ID, err))
			}
			continue
		}
		errs = append(errs, fmt.Errorf("close workspace %s: %w", ws.ID, closeErr))
	}
	return errors.Join(errs...)
}
