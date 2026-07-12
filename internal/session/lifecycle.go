package session

import (
	"context"
	"time"
)

// RetentionSweep deletes old sessions when Retain is non-zero. It is intended
// to be called only by the single serve owner.
type RetentionSweep struct {
	Store  *Store
	Retain time.Duration
	Now    func() time.Time
}

func (s RetentionSweep) Sweep(ctx context.Context) (int64, error) {
	if s.Store == nil || s.Retain <= 0 {
		return 0, nil
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	return s.Store.Retain(ctx, now().Add(-s.Retain))
}
