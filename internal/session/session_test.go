package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return New(st)
}

// TestLargeImageDoesNotBlowUpTranscriptReads pins the storage acceptance
// criterion: media payloads are stored inline (base64) in the turns table
// (decision documented in docs/plan.md, "Media content"), capped at
// llm.MaxMediaBytes per block. On the store's single SQLite connection, an
// ordinary transcript read of a session whose turns carry images at the
// limit must complete with every payload intact — no long-transaction
// hazard, no truncation.
func TestLargeImageDoesNotBlowUpTranscriptReads(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	sess, err := s.Create(ctx, "image session")
	if err != nil {
		t.Fatal(err)
	}
	// One turn per image at the canonical limit, plus text turns between.
	img, err := llm.NewImageBlock("image/png", make([]byte, llm.MaxMediaBytes))
	if err != nil {
		t.Fatal(err)
	}
	const turns = 6
	for i := 0; i < turns; i++ {
		if err := s.AppendTurn(ctx, sess.ID, llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "turn"}, img}}); err != nil {
			t.Fatalf("AppendTurn #%d: %v", i, err)
		}
	}
	got, err := s.Turns(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Turns: %v", err)
	}
	if len(got) != turns {
		t.Fatalf("turns = %d, want %d", len(got), turns)
	}
	for i, m := range got {
		if len(m.Blocks) != 2 || m.Blocks[1].Source == nil || m.Blocks[1].Source.Data != img.Source.Data {
			t.Fatalf("turn %d image mangled: %+v", i, m.Blocks)
		}
	}
}

// TestPreImageFixtureRoundTripsThroughStore pins the persisted-format
// contract end to end: a turn written before image/document blocks existed
// (checked-in fixture) survives AppendTurn/Turns and re-marshals to the
// exact canonical bytes old waffle stored.
func TestPreImageFixtureRoundTripsThroughStore(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	sess, err := s.Create(ctx, "old format")
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile(filepath.Join("..", "llm", "testdata", "turn_pre_image.json"))
	if err != nil {
		t.Fatal(err)
	}
	var msg llm.Message
	if err := json.Unmarshal(fixture, &msg); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendTurn(ctx, sess.ID, msg); err != nil {
		t.Fatal(err)
	}
	got, err := s.Turns(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(got[0].Blocks)
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"type":"tool_result","tool_result":{"tool_use_id":"toolu_1","content":"deploy.sh:3: rsync --delete typo-here","is_error":true}},{"type":"text","text":"that broke it"}]`
	if string(raw) != want {
		t.Fatalf("blocks = %s\nwant %s", raw, want)
	}
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

func TestSessionModelAliasPersistsAcrossGetLatestAndList(t *testing.T) {
	ctx := context.Background()
	sessions := newTestStore(t)
	sess, err := sessions.Create(ctx, "model session")
	if err != nil {
		t.Fatal(err)
	}
	if err := sessions.SetModelAlias(ctx, sess.ID, "claude"); err != nil {
		t.Fatal(err)
	}
	got, err := sessions.Get(ctx, sess.ID)
	if err != nil || got.ModelAlias != "claude" {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	latest, err := sessions.Latest(ctx)
	if err != nil || latest.ModelAlias != "claude" || latest.ModelAliasVersion != 1 {
		t.Fatalf("Latest = %+v, %v", latest, err)
	}
	list, err := sessions.List(ctx, 10)
	if err != nil || len(list) != 1 || list[0].ModelAlias != "claude" || list[0].ModelAliasVersion != 1 {
		t.Fatalf("List = %+v, %v", list, err)
	}
}

func TestModelAliasReferencesAreDeterministic(t *testing.T) {
	ctx := context.Background()
	sessions := newTestStore(t)
	first, err := sessions.Create(ctx, "first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := sessions.Create(ctx, "second")
	if err != nil {
		t.Fatal(err)
	}
	third, err := sessions.Create(ctx, "third")
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		id    string
		alias string
	}{
		{first.ID, "removed"},
		{second.ID, "kept"},
		{third.ID, "removed"},
	} {
		if err := sessions.SetModelAlias(ctx, item.id, item.alias); err != nil {
			t.Fatal(err)
		}
	}

	got, err := sessions.ModelAliasReferences(ctx, "removed")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{first.ID, third.ID}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("references = %v, want %v", got, want)
	}
	empty, err := sessions.ModelAliasReferences(ctx, "missing")
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Fatalf("missing references = %v, want empty", empty)
	}
}

func TestReplaceModelAliasOnlyMovesExplicitlySelectedSessions(t *testing.T) {
	ctx := context.Background()
	sessions := newTestStore(t)
	removed, err := sessions.Create(ctx, "removed")
	if err != nil {
		t.Fatal(err)
	}
	kept, err := sessions.Create(ctx, "kept")
	if err != nil {
		t.Fatal(err)
	}
	if err := sessions.SetModelAlias(ctx, removed.ID, "removed"); err != nil {
		t.Fatal(err)
	}
	if err := sessions.SetModelAlias(ctx, kept.ID, "kept"); err != nil {
		t.Fatal(err)
	}
	if err := sessions.ReplaceModelAlias(ctx, "removed", "replacement"); err != nil {
		t.Fatal(err)
	}
	gotRemoved, err := sessions.Get(ctx, removed.ID)
	if err != nil {
		t.Fatal(err)
	}
	gotKept, err := sessions.Get(ctx, kept.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotRemoved.ModelAlias != "replacement" || gotKept.ModelAlias != "kept" {
		t.Fatalf("session aliases = %q/%q", gotRemoved.ModelAlias, gotKept.ModelAlias)
	}
}

func TestModelAliasRecoveryIsExactAndDoesNotOverwriteNewerChoices(t *testing.T) {
	ctx := context.Background()
	sessions := newTestStore(t)
	first, err := sessions.Create(ctx, "first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := sessions.Create(ctx, "second")
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []*Session{first, second} {
		if err := sessions.SetModelAlias(ctx, item.ID, "removed"); err != nil {
			t.Fatal(err)
		}
	}
	first, err = sessions.Get(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err = sessions.Get(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	changes := []ModelAliasChange{
		{SessionID: first.ID, OriginalAlias: "removed", ReplacementAlias: "replacement", OriginalVersion: first.ModelAliasVersion, ReplacementVersion: first.ModelAliasVersion + 1, OriginalUpdatedAt: first.UpdatedAt.Format(time.RFC3339Nano), ReplacementUpdatedAt: "2026-07-25T22:00:00Z"},
		{SessionID: second.ID, OriginalAlias: "removed", ReplacementAlias: "replacement", OriginalVersion: second.ModelAliasVersion, ReplacementVersion: second.ModelAliasVersion + 1, OriginalUpdatedAt: second.UpdatedAt.Format(time.RFC3339Nano), ReplacementUpdatedAt: "2026-07-25T22:00:00Z"},
	}
	if err := sessions.ReplaceModelAliases(ctx, changes); err != nil {
		t.Fatal(err)
	}
	if err := sessions.SetModelAlias(ctx, first.ID, "today-newer"); err != nil {
		t.Fatal(err)
	}
	if err := sessions.RestoreModelAliases(ctx, changes); err != nil {
		t.Fatal(err)
	}
	gotFirst, err := sessions.Get(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	gotSecond, err := sessions.Get(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotFirst.ModelAlias != "today-newer" || gotSecond.ModelAlias != "removed" {
		t.Fatalf("recovered aliases = %q/%q, want newer choice/original", gotFirst.ModelAlias, gotSecond.ModelAlias)
	}
}

func TestModelAliasRecoveryPreservesConcurrentSameValueChoice(t *testing.T) {
	ctx := context.Background()
	sessions := newTestStore(t)
	item, err := sessions.Create(ctx, "same value")
	if err != nil {
		t.Fatal(err)
	}
	if err := sessions.SetModelAlias(ctx, item.ID, "removed"); err != nil {
		t.Fatal(err)
	}
	before, err := sessions.Get(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	change := ModelAliasChange{
		SessionID: item.ID, OriginalAlias: "removed", ReplacementAlias: "replacement",
		OriginalVersion: before.ModelAliasVersion, ReplacementVersion: before.ModelAliasVersion + 1,
		OriginalUpdatedAt: before.UpdatedAt.Format(time.RFC3339Nano), ReplacementUpdatedAt: "2026-07-25T22:00:00Z",
	}
	if err := sessions.ReplaceModelAliases(ctx, []ModelAliasChange{change}); err != nil {
		t.Fatal(err)
	}
	if err := sessions.SetModelAlias(ctx, item.ID, "replacement"); err != nil {
		t.Fatal(err)
	}
	if err := sessions.RestoreModelAliases(ctx, []ModelAliasChange{change}); err != nil {
		t.Fatal(err)
	}
	got, err := sessions.Get(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ModelAlias != "replacement" || got.ModelAliasVersion != change.ReplacementVersion+1 {
		t.Fatalf("same-value concurrent choice = alias %q version %d, want replacement version %d", got.ModelAlias, got.ModelAliasVersion, change.ReplacementVersion+1)
	}
}

func TestReplaceModelAliasesIsAllOrNothingWhenOneReferenceChanged(t *testing.T) {
	ctx := context.Background()
	sessions := newTestStore(t)
	first, err := sessions.Create(ctx, "first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := sessions.Create(ctx, "second")
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []*Session{first, second} {
		if err := sessions.SetModelAlias(ctx, item.ID, "removed"); err != nil {
			t.Fatal(err)
		}
	}
	first, err = sessions.Get(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err = sessions.Get(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := sessions.SetModelAlias(ctx, second.ID, "today-newer"); err != nil {
		t.Fatal(err)
	}
	changes := []ModelAliasChange{
		{SessionID: first.ID, OriginalAlias: "removed", ReplacementAlias: "replacement", OriginalVersion: first.ModelAliasVersion, ReplacementVersion: first.ModelAliasVersion + 1, OriginalUpdatedAt: first.UpdatedAt.Format(time.RFC3339Nano), ReplacementUpdatedAt: "2026-07-25T22:00:00Z"},
		{SessionID: second.ID, OriginalAlias: "removed", ReplacementAlias: "replacement", OriginalVersion: second.ModelAliasVersion, ReplacementVersion: second.ModelAliasVersion + 1, OriginalUpdatedAt: second.UpdatedAt.Format(time.RFC3339Nano), ReplacementUpdatedAt: "2026-07-25T22:00:00Z"},
	}
	if err := sessions.ReplaceModelAliases(ctx, changes); err == nil {
		t.Fatal("replacement unexpectedly succeeded after a concurrent session choice")
	}
	gotFirst, err := sessions.Get(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotFirst.ModelAlias != "removed" {
		t.Fatalf("first alias = %q, want unchanged after all-or-nothing failure", gotFirst.ModelAlias)
	}
}

func TestSettersReturnNotFoundForMissingSession(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	missingID := "ses_does_not_exist"

	tests := []struct {
		name string
		call func() error
	}{
		{
			name: "SetTitle",
			call: func() error { return s.SetTitle(ctx, missingID, "title") },
		},
		{
			name: "SetSummary",
			call: func() error { return s.SetSummary(ctx, missingID, "summary") },
		},
		{
			name: "SetModelAlias",
			call: func() error { return s.SetModelAlias(ctx, missingID, "alias") },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.call(); !errors.Is(err, ErrNotFound) {
				t.Fatalf("%s = %v, want ErrNotFound", tt.name, err)
			}
		})
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

func TestSearchSummariesPopulatesUpdatedAt(t *testing.T) {
	ctx := context.Background()
	sessions := newTestStore(t)
	clock := time.Date(2026, 7, 24, 8, 0, 0, 0, time.UTC)
	sessions.Now = func() time.Time { return clock }
	older, err := sessions.Create(ctx, "older")
	if err != nil {
		t.Fatal(err)
	}
	newer, err := sessions.Create(ctx, "newer")
	if err != nil {
		t.Fatal(err)
	}
	olderUpdatedAt := clock.Add(time.Hour)
	newerUpdatedAt := clock.Add(2 * time.Hour)
	// updated_at is conversation activity only (#411): reflection metadata
	// never bumps it, so recency comes from appending turns, not summaries.
	if _, err := sessions.db.ExecContext(ctx, `UPDATE sessions SET updated_at = ? WHERE id = ?`, olderUpdatedAt.Format(time.RFC3339Nano), older.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.db.ExecContext(ctx, `UPDATE sessions SET updated_at = ? WHERE id = ?`, newerUpdatedAt.Format(time.RFC3339Nano), newer.ID); err != nil {
		t.Fatal(err)
	}
	if err := sessions.SetSummary(ctx, older.ID, "security summary older"); err != nil {
		t.Fatal(err)
	}
	if err := sessions.SetSummary(ctx, newer.ID, "security summary newer"); err != nil {
		t.Fatal(err)
	}

	hits, err := sessions.SearchSummaries(ctx, "security summary", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("hits = %#v", hits)
	}
	if hits[0].SessionID != newer.ID || !hits[0].CreatedAt.Equal(newerUpdatedAt) {
		t.Fatalf("newest hit = %#v, want updated_at %s", hits[0], newerUpdatedAt)
	}
	if hits[1].SessionID != older.ID || !hits[1].CreatedAt.Equal(olderUpdatedAt) {
		t.Fatalf("older hit = %#v, want updated_at %s", hits[1], olderUpdatedAt)
	}
}

func TestSearchBlendsRecencyEqualRelevance(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	// Frozen "now" so age math is deterministic (#60).
	frozen := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return frozen }

	// Two sessions, identical searchable text, ages 1d and 90d.
	old, err := s.Create(ctx, "old")
	if err != nil {
		t.Fatal(err)
	}
	newSess, err := s.Create(ctx, "new")
	if err != nil {
		t.Fatal(err)
	}
	// Insert turns with fixed created_at via SQL so FTS content matches exactly.
	text := "equal relevance recency probe token"
	oldAt := frozen.Add(-90 * 24 * time.Hour).Format(time.RFC3339)
	newAt := frozen.Add(-24 * time.Hour).Format(time.RFC3339)
	for _, row := range []struct {
		sid, at string
		seq     int
	}{
		{old.ID, oldAt, 1},
		{newSess.ID, newAt, 1},
	} {
		blocks, _ := json.Marshal([]llm.Block{{Type: llm.BlockText, Text: text}})
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO turns (session_id, seq, role, blocks, text, created_at)
			VALUES (?, ?, 'user', ?, ?, ?)`, row.sid, row.seq, string(blocks), text, row.at); err != nil {
			t.Fatal(err)
		}
	}

	hits, err := s.Search(ctx, "equal relevance recency probe token", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) < 2 {
		t.Fatalf("hits = %d, want >= 2", len(hits))
	}
	if hits[0].SessionID != newSess.ID {
		t.Fatalf("expected newer session first, got %+v then %+v", hits[0], hits[1])
	}
	if hits[1].SessionID != old.ID {
		t.Fatalf("expected older second, got %+v", hits[1])
	}
}

func TestSearchPartialMatchFlag(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	sess, _ := s.Create(ctx, "")
	if err := s.AppendTurn(ctx, sess.ID, llm.UserText("alpha middle omega")); err != nil {
		t.Fatal(err)
	}
	hits, err := s.Search(ctx, "alpha omega", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || !hits[0].Partial {
		t.Fatalf("want partial hit, got %+v", hits)
	}
	if err := s.AppendTurn(ctx, sess.ID, llm.UserText("the alpha omega phrase")); err != nil {
		t.Fatal(err)
	}
	hits, err = s.Search(ctx, "alpha omega", 5)
	if err != nil {
		t.Fatal(err)
	}
	foundNonPartial := false
	for _, h := range hits {
		if !h.Partial {
			foundNonPartial = true
		}
	}
	if !foundNonPartial {
		t.Fatalf("expected a non-partial hit among %+v", hits)
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
	// Spill + working set rows must be cleared with the session (#69 / #67).
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO tool_spills (id, session_id, tool_name, content, created_at)
		VALUES ('spill-del1', ?, 'bash', 'huge spill body UNIQUE_DEL_TOKEN', ?)`,
		sess.ID, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO working_set_entries (session_id, id, kind, body, source, pinned, created_at, updated_at)
		VALUES (?, 'e1', 'goal', 'x', 'user', 0, ?, ?)`,
		sess.ID, time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
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
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tool_spills WHERE session_id = ?`, sess.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("spills remain after session delete: %d", count)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM working_set_entries WHERE session_id = ?`, sess.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("working set remains after session delete: %d", count)
	}
}

func TestSetPinnedPersistsWithoutTouchingRecency(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	a, err := s.Create(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Create(ctx, "b")
	if err != nil {
		t.Fatal(err)
	}
	beforeA, err := s.Get(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetPinned(ctx, a.ID, true); err != nil {
		t.Fatal(err)
	}
	after, err := s.Get(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !after.Pinned {
		t.Fatal("pinned flag did not persist")
	}
	if !after.UpdatedAt.Equal(beforeA.UpdatedAt) {
		t.Fatalf("pinning changed updated_at: %v -> %v", beforeA.UpdatedAt, after.UpdatedAt)
	}
	if err := s.SetPinned(ctx, a.ID, false); err != nil {
		t.Fatal(err)
	}
	after, err = s.Get(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Pinned {
		t.Fatal("unpin did not persist")
	}
	// Pinned sessions sort before ordinary recents.
	if err := s.SetPinned(ctx, b.ID, true); err != nil {
		t.Fatal(err)
	}
	list, err := s.List(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != b.ID {
		t.Fatalf("pinned session not first in list: %+v", list)
	}
}

func TestDeleteFailsClosedOnLiveWorkspaceAndCleansClosedOnes(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	sess, err := s.Create(ctx, "workspace session")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	insertWorkspace := func(status string) {
		t.Helper()
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO workspaces (id, repo, url, image, container, volume, session_id, status, created_at, updated_at)
			VALUES (?, 'repo', 'url', 'img', 'c', 'v', ?, ?, ?, ?)`,
			"ws-"+status, sess.ID, status, now, now); err != nil {
			t.Fatal(err)
		}
	}
	insertWorkspace("open")
	if err := s.Delete(ctx, sess.ID); !errors.Is(err, ErrSessionWorkspaceActive) {
		t.Fatalf("delete with open workspace = %v, want ErrSessionWorkspaceActive", err)
	}
	// Close it, then deletion succeeds and removes the workspace row.
	if _, err := s.db.ExecContext(ctx, `UPDATE workspaces SET status = 'closed' WHERE session_id = ?`, sess.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM workspaces WHERE session_id = ?`, sess.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("closed workspace rows remain after session delete: %d", count)
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

func TestExistIDsReportsPresentAndAbsent(t *testing.T) {
	ctx := context.Background()
	sessions := newTestStore(t)
	a, err := sessions.Create(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	b, err := sessions.Create(ctx, "beta")
	if err != nil {
		t.Fatal(err)
	}
	got, err := sessions.ExistIDs(ctx, []string{a.ID, "missing", b.ID, a.ID, ""})
	if err != nil {
		t.Fatal(err)
	}
	if !got[a.ID] || !got[b.ID] || got["missing"] || len(got) != 2 {
		t.Fatalf("ExistIDs = %#v", got)
	}
	empty, err := sessions.ExistIDs(ctx, nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty ExistIDs = %#v, %v", empty, err)
	}
}

func TestExistIDsChunksVeryLargeInput(t *testing.T) {
	ctx := context.Background()
	sessions := newTestStore(t)
	a, err := sessions.Create(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	b, err := sessions.Create(ctx, "beta")
	if err != nil {
		t.Fatal(err)
	}

	// This exceeds SQLite's maximum number of bind variables when sent as one
	// IN clause. Keep the second existing ID after the first batch so the test
	// also verifies that every chunk is queried and merged.
	ids := make([]string, 0, 40_002)
	ids = append(ids, a.ID)
	for i := 0; i < 40_000; i++ {
		ids = append(ids, fmt.Sprintf("missing-%05d", i))
	}
	ids = append(ids, b.ID, a.ID, "")

	got, err := sessions.ExistIDs(ctx, ids)
	if err != nil {
		t.Fatalf("ExistIDs with large input: %v", err)
	}
	if len(got) != 2 || !got[a.ID] || !got[b.ID] {
		t.Fatalf("ExistIDs = %#v, want only %q and %q", got, a.ID, b.ID)
	}
}

func TestSearchSummariesSurfacesFTSErrors(t *testing.T) {
	ctx := context.Background()
	sessions := newTestStore(t)
	sess, err := sessions.Create(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if err := sessions.SetSummary(ctx, sess.ID, "security summary"); err != nil {
		t.Fatal(err)
	}
	// Sanity: healthy FTS path works.
	if hits, err := sessions.SearchSummaries(ctx, "security", 10); err != nil || len(hits) == 0 {
		t.Fatalf("healthy search: hits=%d err=%v", len(hits), err)
	}
	// Simulate a real schema failure by dropping the FTS index. store.Open
	// always migrates before this method is reachable, so a missing table
	// here is a genuine defect — the error must surface, not degrade to the
	// old LIKE fallback (#277).
	if _, execErr := sessions.db.ExecContext(ctx, `DROP TABLE sessions_fts`); execErr != nil {
		t.Fatal(execErr)
	}
	_, err = sessions.SearchSummaries(ctx, "security", 10)
	if err == nil {
		t.Fatal("expected FTS error to surface after dropping sessions_fts")
	}
	if !strings.Contains(err.Error(), "sessions_fts") {
		t.Fatalf("error should identify the missing FTS table, got: %v", err)
	}
}

func appendExchange(t *testing.T, s *Store, sessionID, userText, assistantText string) {
	t.Helper()
	if err := s.AppendTurn(context.Background(), sessionID, llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: userText}}}); err != nil {
		t.Fatalf("append user turn: %v", err)
	}
	if err := s.AppendTurn(context.Background(), sessionID, llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: assistantText}}}); err != nil {
		t.Fatalf("append assistant turn: %v", err)
	}
}

func seedBranchSource(t *testing.T, s *Store) *Session {
	t.Helper()
	ctx := context.Background()
	sess, err := s.Create(ctx, "source")
	if err != nil {
		t.Fatal(err)
	}
	appendExchange(t, s, sess.ID, "hello", "hi there")
	appendExchange(t, s, sess.ID, "what is 2+2?", "four")
	if err := s.SetModelAlias(ctx, sess.ID, "fast"); err != nil {
		t.Fatal(err)
	}
	// Attach a skill and add a working-set entry so snapshots are asserted.
	if _, err := s.db.ExecContext(ctx, `INSERT INTO session_skills (session_id, skill_name, attached_at) VALUES (?, 'planner', ?)`, sess.ID, s.nowStr()); err != nil {
		t.Fatalf("attach skill: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO working_set_entries (session_id, id, kind, body, source, pinned, created_at, updated_at) VALUES (?, 'ws-1', 'goal', 'build a branch', 'user', 1, ?, ?)`, sess.ID, s.nowStr(), s.nowStr()); err != nil {
		t.Fatalf("add working set entry: %v", err)
	}
	return sess
}

func TestBranchCopiesCanonicalPrefixAndLineage(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	source := seedBranchSource(t, s)

	branch, err := s.Branch(ctx, source.ID, 4)
	if err != nil {
		t.Fatalf("Branch: %v", err)
	}
	if branch.ID == source.ID {
		t.Fatal("branch must be a new session")
	}
	if branch.ForkedFrom != source.ID || branch.ForkedAtSeq != 4 {
		t.Fatalf("lineage = %s/%d, want %s/4", branch.ForkedFrom, branch.ForkedAtSeq, source.ID)
	}
	if branch.ModelAlias != "fast" {
		t.Fatalf("model alias = %q, want fast", branch.ModelAlias)
	}
	turns, err := s.Turns(ctx, branch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 4 {
		t.Fatalf("branch turns = %d, want 4", len(turns))
	}
	for i, turn := range turns {
		want := []string{"hello", "hi there", "what is 2+2?", "four"}[i]
		if turn.Role != llm.RoleAssistant && i%2 == 0 {
			continue
		}
		if i%2 == 1 && turn.Text() != want {
			t.Fatalf("turn %d text = %q, want %q", i, turn.Text(), want)
		}
	}
	// Persisted round-trip: a fresh load sees the same lineage.
	loaded, err := s.Get(ctx, branch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ForkedFrom != source.ID || loaded.ForkedAtSeq != 4 {
		t.Fatalf("persisted lineage = %s/%d", loaded.ForkedFrom, loaded.ForkedAtSeq)
	}
	// Source session is untouched and keeps all its turns.
	sourceTurns, err := s.Turns(ctx, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(sourceTurns) != 4 {
		t.Fatalf("source turns changed to %d", len(sourceTurns))
	}
	if sourceTurns[3].Text() != "four" {
		t.Fatalf("source last turn changed to %q", sourceTurns[3].Text())
	}
	// Skills and working-set entries copied as independent snapshots.
	var skills, wsEntries int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_skills WHERE session_id = ?`, branch.ID).Scan(&skills); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM working_set_entries WHERE session_id = ?`, branch.ID).Scan(&wsEntries); err != nil {
		t.Fatal(err)
	}
	if skills != 1 || wsEntries != 1 {
		t.Fatalf("copied skills=%d working-set=%d, want 1/1", skills, wsEntries)
	}
}

func TestBranchRejectsMidToolLoopBoundary(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	sess, err := s.Create(ctx, "tool loop")
	if err != nil {
		t.Fatal(err)
	}
	// user asks, assistant issues tool_use (seq 2), results land at seq 3.
	if err := s.AppendTurn(ctx, sess.ID, llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "run it"}}}); err != nil {
		t.Fatal(err)
	}
	toolUse := llm.Block{Type: llm.BlockToolUse, ToolUse: &llm.ToolUse{ID: "tool-1", Name: "run", Input: json.RawMessage(`{}`)}}
	if err := s.AppendTurn(ctx, sess.ID, llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{toolUse}}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendTurn(ctx, sess.ID, llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockToolResult, ToolResult: &llm.ToolResult{ToolUseID: "tool-1", Content: "done"}}}}); err != nil {
		t.Fatal(err)
	}
	// The mid-loop assistant turn is not a completed exchange.
	if _, err := s.Branch(ctx, sess.ID, 2); !errors.Is(err, ErrInvalidBranchBoundary) {
		t.Fatalf("Branch at seq 2 = %v, want ErrInvalidBranchBoundary", err)
	}
	// The tool-result user turn is the end of the tool sequence: cutting
	// right after results leaves valid continuation history.
	afterResults, err := s.Branch(ctx, sess.ID, 3)
	if err != nil {
		t.Fatalf("Branch at seq 3: %v", err)
	}
	turns, err := s.Turns(ctx, afterResults.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 3 {
		t.Fatalf("branch at seq 3 turns = %d, want 3", len(turns))
	}
	// Branching after the loop closes is a valid completed exchange.
	appendExchange(t, s, sess.ID, "summarize", "it ran")
	if _, err := s.Branch(ctx, sess.ID, 5); err != nil {
		t.Fatalf("Branch at seq 5: %v", err)
	}
	// Out-of-range boundary fails closed.
	if _, err := s.Branch(ctx, sess.ID, 99); !errors.Is(err, ErrInvalidBranchBoundary) {
		t.Fatalf("Branch at seq 99 = %v, want ErrInvalidBranchBoundary", err)
	}
	// None of the rejected attempts created sessions; valid boundaries did.
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("sessions = %d, want 3 (source + two branches)", count)
	}
}

func TestBranchFailsAtomicallyOnUnknownSource(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if _, err := s.Branch(ctx, "missing", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Branch missing source = %v, want ErrNotFound", err)
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("sessions = %d, want 0", count)
	}
}
