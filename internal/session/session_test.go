package session

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/store"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() }) //nolint:errcheck // test teardown
	return New(st)
}

func TestSessionAndTurnRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	sess, err := s.Create(ctx, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := s.SetTitle(ctx, sess.ID, "fix the deploy script"); err != nil {
		t.Fatalf("SetTitle: %v", err)
	}

	turns := []llm.Message{
		llm.UserText("the deploy script is broken"),
		{Role: llm.RoleAssistant, Blocks: []llm.Block{
			{Type: llm.BlockThinking, Text: "check the script", Signature: "sig"},
			{Type: llm.BlockToolUse, ToolUse: &llm.ToolUse{ID: "t1", Name: "bash", Input: json.RawMessage(`{"command":"cat deploy.sh"}`)}},
		}},
		{Role: llm.RoleUser, Blocks: []llm.Block{
			{Type: llm.BlockToolResult, ToolResult: &llm.ToolResult{ToolUseID: "t1", Content: "rsync --delete typo-here"}},
		}},
		{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "found the rsync typo"}}},
	}
	for _, m := range turns {
		if err := s.AppendTurn(ctx, sess.ID, m); err != nil {
			t.Fatalf("AppendTurn: %v", err)
		}
	}

	got, err := s.Turns(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Turns: %v", err)
	}
	if len(got) != len(turns) {
		t.Fatalf("turns = %d, want %d", len(got), len(turns))
	}
	// Tool use and thinking blocks must survive the round trip intact.
	if got[1].Blocks[0].Signature != "sig" {
		t.Errorf("thinking signature lost: %+v", got[1].Blocks[0])
	}
	if got[1].Blocks[1].ToolUse.Name != "bash" || string(got[1].Blocks[1].ToolUse.Input) != `{"command":"cat deploy.sh"}` {
		t.Errorf("tool use mangled: %+v", got[1].Blocks[1].ToolUse)
	}
	if got[2].Blocks[0].ToolResult.ToolUseID != "t1" {
		t.Errorf("tool result mangled: %+v", got[2].Blocks[0].ToolResult)
	}
}

func TestSearchFindsTextAndToolResults(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	sess, _ := s.Create(ctx, "")
	if err := s.SetTitle(ctx, sess.ID, "deploy debugging"); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendTurn(ctx, sess.ID, llm.UserText("why does the deploy fail on staging")); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendTurn(ctx, sess.ID, llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{
		{Type: llm.BlockToolResult, ToolResult: &llm.ToolResult{ToolUseID: "x", Content: "error: flimflam not configured"}},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSummary(ctx, sess.ID, "diagnosed the staging deploy"); err != nil {
		t.Fatal(err)
	}

	hits, err := s.Search(ctx, "flimflam", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits = %d, want 1", len(hits))
	}
	if hits[0].SessionID != sess.ID || hits[0].Summary != "diagnosed the staging deploy" {
		t.Errorf("hit = %+v", hits[0])
	}
	if !strings.Contains(hits[0].Snippet, "[flimflam]") {
		t.Errorf("snippet = %q", hits[0].Snippet)
	}

	// Operator syntax must not error, just match nothing or something.
	if _, err := s.Search(ctx, `"unbalanced AND (`, 5); err != nil {
		t.Errorf("hostile query errored: %v", err)
	}
	if hits, _ := s.Search(ctx, "", 5); hits != nil {
		t.Errorf("empty query returned hits")
	}
}

func TestDeleteRemovesSessionTurnsAndFTS(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	sess, err := s.Create(ctx, "remove me")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AppendTurn(ctx, sess.ID, llm.UserText("private forgettable phrase")); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE id = ?`, sess.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("deleted session still present")
	}
	hits, err := s.Search(ctx, "forgettable", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("hits after delete = %d", len(hits))
	}
}

func TestRetainDeletesOnlyExpiredUnboundSessions(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	old, err := s.Create(ctx, "old")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE sessions SET updated_at = ? WHERE id = ?`, time.Now().Add(-48*time.Hour).UTC().Format(time.RFC3339Nano), old.ID); err != nil {
		t.Fatal(err)
	}
	newer, _ := s.Create(ctx, "new")
	n, err := s.Retain(ctx, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("deleted = %d, want 1", n)
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE id = ?`, newer.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("recent session deleted")
	}
}

func TestLatestAndList(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if _, err := s.Latest(ctx); !errors.Is(err, ErrNotFound) {
		t.Errorf("Latest on empty = %v, want ErrNotFound", err)
	}

	first, _ := s.Create(ctx, "")
	second, _ := s.Create(ctx, "")
	// Touching the first session makes it the most recently updated.
	if err := s.AppendTurn(ctx, first.ID, llm.UserText("hello again")); err != nil {
		t.Fatal(err)
	}

	latest, err := s.Latest(ctx)
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if latest.ID != first.ID {
		t.Errorf("latest = %s, want %s (the touched one, not %s)", latest.ID, first.ID, second.ID)
	}

	all, err := s.List(ctx, 10)
	if err != nil || len(all) != 2 {
		t.Fatalf("List = %d sessions, %v", len(all), err)
	}
}

func TestRepairWithReclaimPreservesOrderAndFabricatesMissing(t *testing.T) {
	history := []llm.Message{{Role: llm.RoleAssistant, Blocks: []llm.Block{
		{Type: llm.BlockToolUse, ToolUse: &llm.ToolUse{ID: "done"}},
		{Type: llm.BlockToolUse, ToolUse: &llm.ToolUse{ID: "lost"}},
	}}}
	got := RepairWithReclaim(history, func(ids []string) (map[string]llm.ToolResult, error) {
		if len(ids) != 2 || ids[0] != "done" || ids[1] != "lost" {
			t.Fatalf("ids = %v", ids)
		}
		return map[string]llm.ToolResult{"done": {ToolUseID: "done", Content: "real output"}}, nil
	})
	blocks := got[1].Blocks
	if blocks[0].ToolResult.Content != "real output" || blocks[1].ToolResult.ToolUseID != "lost" || !blocks[1].ToolResult.IsError {
		t.Fatalf("repaired blocks = %#v", blocks)
	}
}
