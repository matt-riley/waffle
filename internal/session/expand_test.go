package session

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/store"
)

func TestExpandContextTool(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "e.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sessions := New(st)
	sess, err := sessions.Create(ctx, "t")
	if err != nil {
		t.Fatal(err)
	}
	if err := sessions.AppendTurn(ctx, sess.ID, llm.UserText("first turn alpha")); err != nil {
		t.Fatal(err)
	}
	if err := sessions.AppendTurn(ctx, sess.ID, llm.Message{
		Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "second beta"}},
	}); err != nil {
		t.Fatal(err)
	}
	tool := ExpandContextTool{Sessions: sessions}
	// Tool is registered under its Def name for agent discovery (#61).
	if tool.Def().Name != "expand_context" {
		t.Fatalf("name=%q", tool.Def().Name)
	}
	ctx = WithSession(ctx, sess.ID)
	out, err := tool.Run(ctx, json.RawMessage(`{"from":1,"to":2}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "alpha") || !strings.Contains(out, "beta") {
		t.Fatalf("out=%q", out)
	}
	if !strings.Contains(out, "turn 1") || !strings.Contains(out, "turn 2") {
		t.Fatalf("missing turn markers: %q", out)
	}

	// Out-of-range / invalid range errors.
	if _, err := tool.Run(ctx, json.RawMessage(`{"from":0,"to":1}`)); err == nil {
		t.Fatal("expected invalid range error for from=0")
	}
	if _, err := tool.Run(ctx, json.RawMessage(`{"from":5,"to":3}`)); err == nil {
		t.Fatal("expected invalid range error for to < from")
	}
	// Cap size: more than 40 turns rejected.
	if _, err := tool.Run(ctx, json.RawMessage(`{"from":1,"to":50}`)); err == nil || !strings.Contains(err.Error(), "max 40") {
		t.Fatalf("cap error = %v", err)
	}
	// Empty range within bounds (no turns that high).
	empty, err := tool.Run(ctx, json.RawMessage(`{"from":10,"to":12}`))
	if err != nil {
		t.Fatal(err)
	}
	if empty != "no turns in that range" {
		t.Fatalf("empty = %q", empty)
	}
	// Explicit session_id without context session.
	out2, err := tool.Run(context.Background(), json.RawMessage(
		fmt.Sprintf(`{"from":1,"to":1,"session_id":%q}`, sess.ID)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out2, "alpha") {
		t.Fatalf("session_id path = %q", out2)
	}
}
