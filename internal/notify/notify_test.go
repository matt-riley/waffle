package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// schemaProps returns the property names of the notify tool's input schema.
func schemaProps(t *testing.T) map[string]bool {
	t.Helper()
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal((Tool{}).Def().InputSchema, &schema); err != nil {
		t.Fatalf("input schema: %v", err)
	}
	props := make(map[string]bool, len(schema.Properties))
	for name := range schema.Properties {
		props[name] = true
	}
	return props
}

// TestNotifyToolInputSchemaHasNoDestinationFields asserts the tool resolves
// its destination from session origin only: no channel or chat-id field
// exists for an injected instruction to redirect a message to a third party.
func TestNotifyToolInputSchemaHasNoDestinationFields(t *testing.T) {
	props := schemaProps(t)
	if !props["message"] {
		t.Fatalf("schema properties = %v, want message", props)
	}
	for name := range props {
		lower := strings.ToLower(name)
		if strings.Contains(lower, "channel") || strings.Contains(lower, "chat") || strings.Contains(lower, "chat_id") || strings.Contains(lower, "destination") || strings.Contains(lower, "target") || strings.Contains(lower, "to") {
			t.Errorf("schema property %q looks like a destination field; destination must come from session origin only", name)
		}
	}
	if len(props) != 1 {
		t.Errorf("schema properties = %v, want exactly {message}", props)
	}
}

func TestNotifyBehaviorSendsToSessionSender(t *testing.T) {
	var got []string
	ctx := WithSender(context.Background(), func(ctx context.Context, text string) error {
		got = append(got, text)
		return nil
	})
	out, err := (Tool{}).Run(ctx, json.RawMessage(`{"message":"  60% through the migration  "}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "notified") {
		t.Fatalf("out = %q, want notified", out)
	}
	if len(got) != 1 || got[0] != "60% through the migration" {
		t.Fatalf("sender received %q, want trimmed message", got)
	}
}

func TestNotifyBehaviorNoSenderDegradesToNoop(t *testing.T) {
	out, err := (Tool{}).Run(context.Background(), json.RawMessage(`{"message":"hello"}`))
	if err != nil {
		t.Fatalf("no-sender run must not error, got %v", err)
	}
	if !strings.Contains(out, "no owner channel") {
		t.Fatalf("out = %q, want a clear no-op message", out)
	}
}

func TestNotifyBehaviorRejectsEmptyMessage(t *testing.T) {
	ctx := WithSender(context.Background(), func(ctx context.Context, text string) error { return nil })
	for _, input := range []string{`{}`, `{"message":""}`, `{"message":"   "}`} {
		if _, err := (Tool{}).Run(ctx, json.RawMessage(input)); err == nil {
			t.Errorf("input %s: want error for empty message", input)
		}
	}
}

// TestNotifyBehaviorCapsMessageLength covers the length cap: at the cap the
// message is delivered; over it the tool errors and nothing is sent.
func TestNotifyBehaviorCapsMessageLength(t *testing.T) {
	tests := []struct {
		name    string
		length  int
		wantErr bool
	}{
		{name: "at-cap-delivers", length: MaxMessageLen, wantErr: false},
		{name: "one-over-rejected", length: MaxMessageLen + 1, wantErr: true},
		{name: "well-over-rejected", length: MaxMessageLen * 10, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var sent atomic.Int64
			ctx := WithSender(context.Background(), func(ctx context.Context, text string) error {
				sent.Add(1)
				return nil
			})
			input, _ := json.Marshal(map[string]string{"message": strings.Repeat("x", tt.length)})
			_, err := (Tool{}).Run(ctx, input)
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "too long") {
					t.Fatalf("err = %v, want too-long error", err)
				}
				if sent.Load() != 0 {
					t.Fatalf("sender called %d times for an over-cap message", sent.Load())
				}
			} else if err != nil {
				t.Fatalf("at-cap message failed: %v", err)
			}
		})
	}
}

// TestNotifyBehaviorBoundsPerRun covers the per-run budget: the first
// MaxPerRun notifications go through, the next one errors, and a failed
// send still consumes budget (attempts are what is bounded).
func TestNotifyBehaviorBoundsPerRun(t *testing.T) {
	tests := []struct {
		name      string
		fail      bool // sender returns an error for every call
		wantCalls int
	}{
		{name: "happy-path", fail: false, wantCalls: MaxPerRun},
		{name: "failed-sends-consume-budget", fail: true, wantCalls: MaxPerRun},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int64
			ctx := WithSender(context.Background(), func(ctx context.Context, text string) error {
				calls.Add(1)
				if tt.fail {
					return errors.New("adapter down")
				}
				return nil
			})
			for i := 0; i < MaxPerRun; i++ {
				_, err := (Tool{}).Run(ctx, json.RawMessage(`{"message":"update"}`))
				if tt.fail {
					if err == nil || strings.Contains(err.Error(), "limit") {
						t.Fatalf("call %d err = %v, want send-failed (not limit)", i+1, err)
					}
				} else if err != nil {
					t.Fatalf("call %d within budget failed: %v", i+1, err)
				}
			}
			if _, err := (Tool{}).Run(ctx, json.RawMessage(`{"message":"one too many"}`)); err == nil || !strings.Contains(err.Error(), "limit") {
				t.Fatalf("over-budget call err = %v, want limit error", err)
			}
			if got := calls.Load(); got != int64(tt.wantCalls) {
				t.Fatalf("sender called %d times, want %d", got, tt.wantCalls)
			}
		})
	}
}

func TestNotifyBehaviorFailedSendReturnsToolError(t *testing.T) {
	ctx := WithSender(context.Background(), func(ctx context.Context, text string) error {
		return errors.New("adapter down")
	})
	_, err := (Tool{}).Run(ctx, json.RawMessage(`{"message":"hello"}`))
	if err == nil || !strings.Contains(err.Error(), "send failed") || !strings.Contains(err.Error(), "adapter down") {
		t.Fatalf("err = %v, want send-failed error wrapping the sender error", err)
	}
}

// TestNotifyBehaviorConcurrentSendsStayBounded exercises the shared per-run
// budget under parallel tool dispatch (the agent runs tools concurrently)
// and runs clean under -race.
func TestNotifyBehaviorConcurrentSendsStayBounded(t *testing.T) {
	var calls atomic.Int64
	ctx := WithSender(context.Background(), func(ctx context.Context, text string) error {
		calls.Add(1)
		return nil
	})
	const goroutines = 32
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < MaxPerRun; j++ {
				_, _ = (Tool{}).Run(ctx, json.RawMessage(fmt.Sprintf(`{"message":"update %d"}`, j)))
			}
		}()
	}
	wg.Wait()
	if got := calls.Load(); got != int64(MaxPerRun) {
		t.Fatalf("sender called %d times across %d goroutines, want exactly %d", got, goroutines, MaxPerRun)
	}
}

func TestSenderFromContextAbsentWithoutAttachment(t *testing.T) {
	if _, ok := SenderFromContext(context.Background()); ok {
		t.Fatal("SenderFromContext on bare context should report absent")
	}
}

// TestNotifyBoundSenderHonorsTimeout pins the #253 review fix: a delivery
// on a deadline-free context is still capped by SendTimeout (an
// unresponsive owner channel must not block the agent run, which holds the
// gateway conversation lock), and an earlier deadline on the delivery
// context still wins.
func TestNotifyBoundSenderHonorsTimeout(t *testing.T) {
	waiting := make(chan struct{})
	var entered atomic.Int32
	s := bound(func(ctx context.Context, _ string) error {
		entered.Add(1)
		close(waiting) // the sender is blocked inside delivery
		<-ctx.Done()
		return ctx.Err()
	}, 60*time.Millisecond)
	start := time.Now()
	if err := s(context.Background(), "hi"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("send took %v, want the 60ms bound to fire", elapsed)
	}
	<-waiting
	// An earlier deadline on the delivery context wins over the bound.
	shortCtx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	s2 := bound(func(ctx context.Context, _ string) error {
		<-ctx.Done()
		return ctx.Err()
	}, 5*time.Second)
	start = time.Now()
	if err := s2(shortCtx, "hi"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("short err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("short send took %v, want the earlier deadline to fire", elapsed)
	}
}
