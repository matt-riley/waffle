package workset

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/store"
)

func TestUpdateToolAndDropAssumptions(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s := &Store{DB: st.DB}
	tool := UpdateTool{Store: s}
	ctx = session.WithSession(ctx, "s1")

	out, err := tool.Run(ctx, json.RawMessage(`{"op":"add","kind":"assumption","body":"maybe redis"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "id=") {
		t.Fatalf("out=%q", out)
	}
	// pinned assumption should survive clear_assumptions
	if _, err := s.Add(ctx, "s1", KindAssumption, "keep me", SourceModel, true); err != nil {
		t.Fatal(err)
	}
	out, err = tool.Run(ctx, json.RawMessage(`{"op":"clear_assumptions"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "dropped 1") {
		t.Fatalf("out=%q", out)
	}
	list, _ := s.List(ctx, "s1")
	if len(list) != 1 || !list[0].Pinned {
		t.Fatalf("list=%+v", list)
	}

	// Stale filter
	if _, err := s.Add(ctx, "s1", KindAssumption, "old", SourceModel, false); err != nil {
		t.Fatal(err)
	}
	// force old updated_at
	list, _ = s.List(ctx, "s1")
	var oldID string
	for _, e := range list {
		if e.Body == "old" {
			oldID = e.ID
		}
	}
	past := time.Now().UTC().Add(-48 * time.Hour).Format(time.RFC3339Nano)
	if _, err := st.DB.ExecContext(ctx, `UPDATE working_set_entries SET updated_at = ? WHERE id = ?`, past, oldID); err != nil {
		t.Fatal(err)
	}
	n, err := s.DropStaleAssumptions(ctx, "s1", 24*time.Hour, false)
	if err != nil || n != 1 {
		t.Fatalf("stale drop n=%d err=%v", n, err)
	}
}
