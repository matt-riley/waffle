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
	// PriorSummary, when set, is the previous summary of the conversation; it
	// is sent for continuity alongside the trailing turns (#411 incremental
	// re-reflection).
	PriorSummary string
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
	msgs := append(append([]llm.Message{}, hist...), prompt)
	if opts.PriorSummary != "" {
		msgs = append([]llm.Message{
			llm.UserText("Previous summary of this conversation (for continuity):\n" + opts.PriorSummary),
		}, msgs...)
	}
	resp, err := provider.Complete(ctx, llm.Request{
		Model:     model,
		Messages:  msgs,
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
	// Zero or negative disables RunOnce (callers should not start the loop).
	After time.Duration
	// Every is the poll interval (reflect_every).
	Every time.Duration
	// Now is optional clock for tests.
	Now func() time.Time
	// OnError is optional; failures are logged and never panic.
	OnError func(error)
	// TryLockSession, when set, serializes reflection with gateway message
	// handling for the session's channel group. ok=false means skip this tick
	// (conversation busy). unlock must be called when done.
	TryLockSession func(ctx context.Context, sessionID string) (unlock func(), ok bool)
}

func (r *IdleReflector) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// RunOnce finds idle sessions needing summaries and reflects them.
// After <= 0 disables idle reflection entirely (reflect_after = "0").
func (r *IdleReflector) RunOnce(ctx context.Context) (int, error) {
	if r.Sessions == nil || r.Provider == nil {
		return 0, nil
	}
	if r.After <= 0 {
		return 0, nil
	}
	provider, model := r.Provider()
	if provider == nil {
		return 0, nil
	}
	cutoff := r.now().UTC().Add(-r.After)
	ids, err := r.Sessions.ListIdleForReflection(ctx, cutoff, 20)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		if wrote, err := r.reflectOne(ctx, id, provider, model); err != nil {
			if r.OnError != nil {
				r.OnError(err)
			}
			continue
		} else if wrote {
			n++
		}
	}
	return n, nil
}

func (r *IdleReflector) reflectOne(ctx context.Context, id string, provider llm.Provider, model string) (bool, error) {
	if r.TryLockSession != nil {
		unlock, ok := r.TryLockSession(ctx, id)
		if !ok {
			return false, nil
		}
		defer unlock()
	}
	// Re-check under lock: only reflect when the summary does not already
	// cover the latest turn (#411). A second quiet period after new turns is
	// eligible again; an unchanged session is skipped.
	sess, err := r.Sessions.Get(ctx, id)
	if err != nil {
		return false, err
	}
	hist, err := r.Sessions.Turns(ctx, id)
	if err != nil {
		return false, err
	}
	if len(hist) < 2 {
		return false, nil
	}
	latest := int64(len(hist))
	if sess.SummaryWatermark >= latest {
		return false, nil
	}
	// Incremental: send the previous summary plus only the uncovered turns so
	// a resumed long session does not pay full-history cost on every quiet
	// period (#411). Bound the trailing window like Reflect does.
	history := hist
	var prior string
	if sess.SummaryWatermark > 0 && int64(len(hist)) > sess.SummaryWatermark {
		prior = sess.Summary
		history = hist[sess.SummaryWatermark:]
	}
	summary, err := Reflect(ctx, provider, history, ReflectOptions{Model: model, PriorSummary: prior})
	if err != nil {
		return false, err
	}
	if summary == "" {
		return false, nil
	}
	if err := r.Sessions.SetSummaryWatermark(ctx, id, summary, latest); err != nil {
		return false, err
	}
	return true, nil
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

// ListIdleForReflection returns session ids eligible for a *new* idle
// reflection: at least two turns, updated_at before cutoff, and a latest turn
// sequence newer than the summary watermark (#411). Sessions summarized after
// their last activity are not returned; a resumed session with new turns is.
func (s *Store) ListIdleForReflection(ctx context.Context, cutoff time.Time, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id FROM sessions
		WHERE updated_at < ?
		  AND summary_watermark < (
		    SELECT COALESCE(MAX(seq), 0) FROM turns WHERE session_id = sessions.id
		  )
		  AND EXISTS (
		    SELECT 1 FROM turns
		    WHERE session_id = sessions.id
		    LIMIT 1 OFFSET 1
		  )
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
