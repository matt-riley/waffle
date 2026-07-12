package workspace

import (
	"context"
	"fmt"
	"time"
)

// Reaper sweeps workspaces from the single serve owner. A zero timeout
// disables that part of the sweep.
type Reaper struct {
	Manager     *Manager
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

// Sweep performs one deterministic lifecycle pass.
func (r *Reaper) Sweep(ctx context.Context) error {
	if r == nil || r.Manager == nil {
		return nil
	}
	items, err := r.Manager.List(ctx)
	if err != nil {
		return err
	}
	now := r.now()
	for _, ws := range items {
		last := ws.LastActive
		if last.IsZero() {
			last = ws.UpdatedAt
		}
		age := now.Sub(last)
		if ws.Status == StatusOpen && r.IdleTimeout > 0 && age >= r.IdleTimeout {
			if err := r.Manager.Idle(ctx, ws.ID); err != nil {
				return fmt.Errorf("idle workspace %s: %w", ws.ID, err)
			}
			ws.Status = StatusIdle
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
				return fmt.Errorf("notify workspace %s: %w", ws.ID, err)
			}
			continue
		}
		return fmt.Errorf("close workspace %s: %w", ws.ID, closeErr)
	}
	return nil
}
