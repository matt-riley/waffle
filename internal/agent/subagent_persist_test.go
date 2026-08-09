package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/tool"
)

func TestSubagentPersistFailureIsPropagated(t *testing.T) {
	ctx := WithSession(context.Background(), "parent-session")
	var gotParent, gotChild string
	tl := SubagentTool{
		Provider: fixedUsageProvider{reply: "child done"},
		Tools:    tool.NewRegistry(),
		Model:    "m",
		NewChildSession: func(context.Context, string) (string, error) {
			return "child-session", nil
		},
		Persist: func(pctx context.Context, parent, child string, _ WorkPacket, _ Handoff) error {
			gotParent, gotChild = parent, child
			return errors.New("sqlite busy")
		},
		PersistTimeout: time.Second,
	}
	_, err := tl.Run(ctx, json.RawMessage(`{"task":"child work"}`))
	if err == nil {
		t.Fatal("Run succeeded despite a failed handoff persistence")
	}
	if !strings.Contains(err.Error(), "persist subagent handoff") {
		t.Fatalf("error = %v, want the persistence failure propagated", err)
	}
	if gotParent != "parent-session" || gotChild != "child-session" {
		t.Fatalf("persist called with parent=%q child=%q", gotParent, gotChild)
	}
}

func TestSubagentPersistSuccessStillReturnsHandoff(t *testing.T) {
	ctx := WithSession(context.Background(), "parent-session")
	persisted := false
	tl := SubagentTool{
		Provider: fixedUsageProvider{reply: "child done"},
		Tools:    tool.NewRegistry(),
		Model:    "m",
		NewChildSession: func(context.Context, string) (string, error) {
			return "child-session", nil
		},
		Persist: func(context.Context, string, string, WorkPacket, Handoff) error {
			persisted = true
			return nil
		},
		PersistTimeout: time.Second,
	}
	out, err := tl.Run(ctx, json.RawMessage(`{"task":"child work"}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !persisted {
		t.Fatal("persist callback was not invoked")
	}
	if !strings.Contains(out, "child done") {
		t.Fatalf("handoff = %q, want the child's summary", out)
	}
}

func TestSubagentPersistDoesNotHangAfterParentCancellation(t *testing.T) {
	ctx := WithSession(context.Background(), "parent-session")
	ctx, cancel := context.WithCancel(ctx)
	cancel() // the parent run is already gone

	entered := make(chan struct{})
	tl := SubagentTool{
		Provider: fixedUsageProvider{reply: "child done"},
		Tools:    tool.NewRegistry(),
		Model:    "m",
		NewChildSession: func(context.Context, string) (string, error) {
			return "child-session", nil
		},
		Persist: func(pctx context.Context, _, _ string, _ WorkPacket, _ Handoff) error {
			close(entered)
			// A store that ignores the deadline would block forever; the
			// bounded detached context must cut the wait short.
			<-pctx.Done()
			return pctx.Err()
		},
		PersistTimeout: 50 * time.Millisecond,
	}

	start := time.Now()
	_, err := tl.Run(ctx, json.RawMessage(`{"task":"child work"}`))
	elapsed := time.Since(start)
	select {
	case <-entered:
	default:
		t.Fatal("persist callback was never reached")
	}
	if err == nil {
		t.Fatal("Run succeeded despite persistence timing out")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("persistence wait was not bounded: %v", elapsed)
	}
}
