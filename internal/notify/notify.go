// Package notify exposes a session-scoped outbound sender that tools use to
// message the owner in the middle of a run (#253). The gateway attaches one
// sender per run, bound to the conversation's channel and chat id (the same
// adapter resolution the memory-change notifier and usage alerts already
// used). Tools reach it through the context — destination is resolved from
// session origin only, never from tool input.
//
// Sessions with no channel origin (terminal chat, eval) have no sender
// attached; the notify tool degrades to a clear no-op rather than an error
// or panic.
package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/matt-riley/waffle/internal/llm"
)

// Sender delivers one notification to the owner's channel for the current
// run. ctx is the run context; delivery must respect its cancellation.
type Sender func(ctx context.Context, text string) error

// MaxMessageLen caps a single notification in bytes. Owner channels are
// phones and chat apps, not pipes for model output.
const MaxMessageLen = 4000

// MaxPerRun bounds how many notifications the notify tool may send in one
// run, so an agent in a loop cannot flood the owner.
const MaxPerRun = 5

// SendTimeout bounds a single notification delivery. Owner channels are
// adapters over remote services: an endpoint that accepts a connection but
// never completes must not block the agent run — which holds the gateway
// conversation lock and stalls every subsequent owner turn — indefinitely
// (#253 review). The delivery context may still cancel earlier.
const SendTimeout = 30 * time.Second

// Bound wraps s so every delivery carries a SendTimeout deadline, honoring
// an earlier deadline or cancellation already present on the delivery
// context. Attach the wrapped sender so system sends (memory-change
// notices, usage alerts) and the notify tool share the bound.
func Bound(s Sender) Sender { return bound(s, SendTimeout) }

// bound is Bound with an injectable timeout for tests.
func bound(s Sender, timeout time.Duration) Sender {
	return func(ctx context.Context, text string) error {
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return s(ctx, text)
	}
}

type senderKey struct{}

// state carries the run's sender plus the per-run budget. It lives in the
// context value so every tool call in the run shares one budget.
type state struct {
	mu   sync.Mutex
	send Sender
	used int
	max  int
}

// WithSender attaches the session-scoped sender for this run. A nil sender
// is ignored (no channel origin). Callers must attach per run, not per
// process: the budget and sender are both run-scoped.
func WithSender(ctx context.Context, s Sender) context.Context {
	if s == nil {
		return ctx
	}
	return context.WithValue(ctx, senderKey{}, &state{send: s, max: MaxPerRun})
}

// SenderFromContext returns the run's sender, or ok=false when the session
// has no channel origin. System sends (memory-change notices, usage alerts)
// use the raw sender; the notify tool goes through the budgeted state.
func SenderFromContext(ctx context.Context) (Sender, bool) {
	st, _ := ctx.Value(senderKey{}).(*state)
	if st == nil || st.send == nil {
		return nil, false
	}
	return st.send, true
}

// Tool sends a short message to the owner mid-run. Fire-and-forget: a
// failure is reported to the model as a tool error but never fails the run.
type Tool struct {
	// Log, when set, records no-op and delivery-failure lines.
	Log *slog.Logger
}

// Def returns the tool definition. The input schema deliberately has no
// channel or chat-id field: an injected instruction must not be able to
// redirect a notification to a third party (#253).
func (Tool) Def() llm.Tool {
	return llm.Tool{
		Name:        "notify",
		Description: "Send a short message to the owner during a long run: progress updates, heads-ups, or anything the owner should see before the final reply. Fire-and-forget — the run continues. The destination is the session's owner channel and cannot be redirected; messages are length-capped and bounded per run.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"message": {"type": "string", "description": "The short message to send the owner"}
			},
			"required": ["message"]
		}`),
	}
}

// Run delivers one notification through the session-scoped sender, enforcing
// the per-run budget and message-length cap. With no sender attached (no
// channel origin) it returns a clear no-op instead of an error.
func (t Tool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	st, _ := ctx.Value(senderKey{}).(*state)
	if st == nil || st.send == nil {
		// Terminal chat, eval, or any session without a channel origin:
		// degrade to a clear no-op, never an error and never a panic.
		if t.Log != nil {
			t.Log.Info("notify: no owner channel for this session; nothing sent")
		}
		return "notify: no owner channel for this session; nothing sent", nil
	}
	var in struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("notify: bad input: %w", err)
	}
	msg := strings.TrimSpace(in.Message)
	if msg == "" {
		return "", errors.New("notify: message is required")
	}
	if len(msg) > MaxMessageLen {
		return "", fmt.Errorf("notify: message too long (%d bytes, max %d)", len(msg), MaxMessageLen)
	}
	st.mu.Lock()
	if st.used >= st.max {
		st.mu.Unlock()
		return "", fmt.Errorf("notify: notification limit reached for this run (%d)", st.max)
	}
	st.used++
	st.mu.Unlock()
	if err := st.send(ctx, msg); err != nil {
		return "", fmt.Errorf("notify: send failed: %w", err)
	}
	return "notified the owner", nil
}
