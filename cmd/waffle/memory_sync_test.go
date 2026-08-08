package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matt-riley/waffle/internal/memory"
	"github.com/matt-riley/waffle/internal/store"
)

// TestSyncWorkspaceOnceRetriesAfterFailure covers #259: a transient reindex
// failure used to mark the workspace done anyway, so memory search returned
// nothing for the life of the process with nothing logged to explain it.
func TestSyncWorkspaceOnceRetriesAfterFailure(t *testing.T) {
	ctx := context.Background()
	ws := memory.Workspace{Dir: t.TempDir()}
	note := "- [id=fts001] 2026-01-01 [trust=owner_stated source=]: the reindex covers pineapples\n"
	if err := os.WriteFile(ws.MemoryPath(), []byte(note), 0o600); err != nil {
		t.Fatal(err)
	}

	// A closed handle stands in for "database is locked at startup".
	closed, err := store.Open(ctx, filepath.Join(t.TempDir(), "closed.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(previous)

	syncWorkspaceOnce(&memory.NotesIndex{DB: closed.DB}, memory.DefaultAgent, ws)
	if !strings.Contains(logs.String(), "memory note index sync failed") {
		t.Fatalf("failed sync logged nothing: %s", logs.String())
	}

	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	index := &memory.NotesIndex{DB: st.DB}

	// The retry must actually happen: the first attempt failing cannot leave
	// the workspace marked as indexed.
	syncWorkspaceOnce(index, memory.DefaultAgent, ws)
	hits, err := index.Search(ctx, "pineapples", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits = %+v, want the note indexed by the retry", hits)
	}
}

func TestSyncWorkspaceOnceSkipsRepeatAfterSuccess(t *testing.T) {
	ctx := context.Background()
	ws := memory.Workspace{Dir: t.TempDir()}
	if err := os.WriteFile(ws.MemoryPath(), []byte("- [id=fts002] 2026-01-01 [trust=owner_stated source=]: kumquats are indexed once\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	index := &memory.NotesIndex{DB: st.DB}

	syncWorkspaceOnce(index, memory.DefaultAgent, ws)
	// Remove the source file: a second full sync would empty the index.
	if err := os.Remove(ws.MemoryPath()); err != nil {
		t.Fatal(err)
	}
	syncWorkspaceOnce(index, memory.DefaultAgent, ws)

	hits, err := index.Search(ctx, "kumquats", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits = %+v, want the successful sync not to be repeated", hits)
	}
}
