package session

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/matt-riley/waffle/internal/llm"
)

// ReflectPrompt is the shared end-of-session summary instruction (#59).
const ReflectPrompt = "The conversation is over. Summarize it in 2-3 sentences for future recall: what was worked on, decisions made, and anything left unfinished. Reply with only the summary."

// ReflectOptions configure a single reflection Complete call.
type ReflectOptions struct {
	// MaxHistory caps how many trailing messages are sent (default 30).
	MaxHistory int
	// Model overrides the agent model when set.
	Model string
	// MaxTokens defaults to 1024.
	MaxTokens int
}

// Reflect generates a session summary via the provider and returns the text.
// It does not persist; callers use SetSummary.
func Reflect(ctx context.Context, provider llm.Provider, history []llm.Message, opts ReflectOptions) (string, error) {
	if provider == nil {
		return "", fmt.Errorf("no provider")
	}
	if len(history) < 2 {
		return "", nil
	}
	maxHist := opts.MaxHistory
	if maxHist <= 0 {
		maxHist = 30
	}
	hist := history
	if len(hist) > maxHist {
		hist = hist[len(hist)-maxHist:]
	}
	model := opts.Model
	maxTok := opts.MaxTokens
	if maxTok <= 0 {
		maxTok = 1024
	}
	prompt := llm.UserText(ReflectPrompt)
	resp, err := provider.Complete(ctx, llm.Request{
		Model:     model,
		Messages:  append(append([]llm.Message{}, hist...), prompt),
		MaxTokens: maxTok,
	}, nil)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(resp.Message.Text()), nil
}

// IdleReflector summarizes sessions that have gone idle without a summary (#59).
type IdleReflector struct {
	Sessions *Store
	// Provider returns a provider + model for reflection (may be nil to skip).
	Provider func() (llm.Provider, string)
	// After is how long a session must be idle before reflection (reflect_after).
	After time.Duration
	// Every is the poll interval (reflect_every).
	Every time.Duration
	// Now is optional clock for tests.
	Now func() time.Time
	// OnError is optional.
	OnError func(error)
}

func (r *IdleReflector) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// RunOnce finds idle sessions needing summaries and reflects them.
func (r *IdleReflector) RunOnce(ctx context.Context) (int, error) {
	if r.Sessions == nil || r.Provider == nil {
		return 0, nil
	}
	after := r.After
	if after <= 0 {
		after = 30 * time.Minute
	}
	provider, model := r.Provider()
	if provider == nil {
		return 0, nil
	}
	cutoff := r.now().UTC().Add(-after)
	ids, err := r.Sessions.ListIdleForReflection(ctx, cutoff, 20)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		hist, err := r.Sessions.Turns(ctx, id)
		if err != nil {
			if r.OnError != nil {
				r.OnError(err)
			}
			continue
		}
		if len(hist) < 2 {
			continue
		}
		summary, err := Reflect(ctx, provider, hist, ReflectOptions{Model: model})
		if err != nil {
			if r.OnError != nil {
				r.OnError(err)
			}
			continue
		}
		if summary == "" {
			continue
		}
		if err := r.Sessions.SetSummary(ctx, id, summary); err != nil {
			if r.OnError != nil {
				r.OnError(err)
			}
			continue
		}
		n++
	}
	return n, nil
}

// Loop ticks Every until ctx is done.
func (r *IdleReflector) Loop(ctx context.Context) {
	every := r.Every
	if every <= 0 {
		every = 5 * time.Minute
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if _, err := r.RunOnce(ctx); err != nil && r.OnError != nil {
				r.OnError(err)
			}
		}
	}
}

// ListIdleForReflection returns session ids with empty summary, at least two
// turns worth of activity, and updated_at before cutoff.
func (s *Store) ListIdleForReflection(ctx context.Context, cutoff time.Time, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id FROM sessions
		WHERE (summary IS NULL OR summary = '')
		  AND updated_at < ?
		  AND id IN (SELECT session_id FROM turns GROUP BY session_id HAVING COUNT(*) >= 2)
		ORDER BY updated_at ASC
		LIMIT ?`, cutoff.UTC().Format(time.RFC3339Nano), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
