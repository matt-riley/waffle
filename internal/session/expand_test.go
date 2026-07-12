package session

import (
	"context"
	"encoding/json"
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
	ctx = WithSession(ctx, sess.ID)
	out, err := tool.Run(ctx, json.RawMessage(`{"from":1,"to":2}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "alpha") || !strings.Contains(out, "beta") {
		t.Fatalf("out=%q", out)
	}
}
