package session

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/store"
)

type scriptProvider struct {
	reply string
	calls int
}

func (p *scriptProvider) Complete(ctx context.Context, req llm.Request, _ llm.StreamFunc) (*llm.Response, error) {
	p.calls++
	return &llm.Response{
		Message: llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: p.reply}}},
	}, nil
}

func TestReflectAndIdle(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sessions := New(st)

	sess, err := sessions.Create(ctx, "t")
	if err != nil {
		t.Fatal(err)
	}
	if err := sessions.AppendTurn(ctx, sess.ID, llm.UserText("worked on idle reflection")); err != nil {
		t.Fatal(err)
	}
	if err := sessions.AppendTurn(ctx, sess.ID, llm.Message{
		Role:   llm.RoleAssistant,
		Blocks: []llm.Block{{Type: llm.BlockText, Text: "done"}},
	}); err != nil {
		t.Fatal(err)
	}

	// Force updated_at into the past.
	past := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339Nano)
	if _, err := st.DB.ExecContext(ctx, `UPDATE sessions SET updated_at = ? WHERE id = ?`, past, sess.ID); err != nil {
		t.Fatal(err)
	}

	p := &scriptProvider{reply: "Session covered idle reflection wiring."}
	now := time.Now().UTC()
	r := &IdleReflector{
		Sessions: sessions,
		Provider: func() (llm.Provider, string) { return p, "m" },
		After:    30 * time.Minute,
		Now:      func() time.Time { return now },
	}
	n, err := r.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("reflected %d, want 1", n)
	}
	got, err := sessions.Get(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Summary, "idle reflection") {
		t.Fatalf("summary = %q", got.Summary)
	}
	// Second pass should skip (summary present).
	n, err = r.RunOnce(ctx)
	if err != nil || n != 0 {
		t.Fatalf("second pass n=%d err=%v", n, err)
	}
}

func TestReflectShared(t *testing.T) {
	p := &scriptProvider{reply: "  short summary  "}
	s, err := Reflect(context.Background(), p, []llm.Message{
		llm.UserText("hi"),
		{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "yo"}}},
	}, ReflectOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if s != "short summary" {
		t.Fatalf("got %q", s)
	}
	if p.calls != 1 {
		t.Fatalf("calls=%d", p.calls)
	}
}
