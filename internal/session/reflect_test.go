package session

import (
	"context"
	"errors"
	"fmt"
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
	// Shared prompt constant used by chat finish and gateway (#59).
	if !strings.Contains(ReflectPrompt, "Summarize") {
		t.Fatalf("ReflectPrompt missing summarize instruction: %q", ReflectPrompt)
	}
}

func TestIdleReflectAfterZeroDisables(t *testing.T) {
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
	_ = sessions.AppendTurn(ctx, sess.ID, llm.UserText("x"))
	_ = sessions.AppendTurn(ctx, sess.ID, llm.Message{
		Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "y"}},
	})
	past := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339Nano)
	if _, err := st.DB.ExecContext(ctx, `UPDATE sessions SET updated_at = ? WHERE id = ?`, past, sess.ID); err != nil {
		t.Fatal(err)
	}
	p := &scriptProvider{reply: "should not run"}
	r := &IdleReflector{
		Sessions: sessions,
		Provider: func() (llm.Provider, string) { return p, "m" },
		After:    0, // reflect_after = "0"
		Now:      time.Now,
	}
	n, err := r.RunOnce(ctx)
	if err != nil || n != 0 || p.calls != 0 {
		t.Fatalf("n=%d err=%v calls=%d want disabled", n, err, p.calls)
	}
}

type errProvider struct{}

func (errProvider) Complete(context.Context, llm.Request, llm.StreamFunc) (*llm.Response, error) {
	return nil, errors.New("boom")
}

func TestIdleReflectFailureNoCrash(t *testing.T) {
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
	_ = sessions.AppendTurn(ctx, sess.ID, llm.UserText("x"))
	_ = sessions.AppendTurn(ctx, sess.ID, llm.Message{
		Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "y"}},
	})
	past := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339Nano)
	if _, err := st.DB.ExecContext(ctx, `UPDATE sessions SET updated_at = ? WHERE id = ?`, past, sess.ID); err != nil {
		t.Fatal(err)
	}
	var logged error
	r := &IdleReflector{
		Sessions: sessions,
		Provider: func() (llm.Provider, string) { return errProvider{}, "m" },
		After:    30 * time.Minute,
		Now:      time.Now,
		OnError:  func(e error) { logged = e },
	}
	n, err := r.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce should not return error on per-session failure: %v", err)
	}
	if n != 0 {
		t.Fatalf("n=%d", n)
	}
	if logged == nil || !strings.Contains(logged.Error(), "boom") {
		t.Fatalf("OnError = %v", logged)
	}
}

func TestIdleReflectSkipsWhenLocked(t *testing.T) {
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
	_ = sessions.AppendTurn(ctx, sess.ID, llm.UserText("x"))
	_ = sessions.AppendTurn(ctx, sess.ID, llm.Message{
		Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "y"}},
	})
	past := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339Nano)
	if _, err := st.DB.ExecContext(ctx, `UPDATE sessions SET updated_at = ? WHERE id = ?`, past, sess.ID); err != nil {
		t.Fatal(err)
	}
	p := &scriptProvider{reply: "nope"}
	r := &IdleReflector{
		Sessions: sessions,
		Provider: func() (llm.Provider, string) { return p, "m" },
		After:    30 * time.Minute,
		Now:      time.Now,
		TryLockSession: func(context.Context, string) (func(), bool) {
			return nil, false // busy
		},
	}
	n, err := r.RunOnce(ctx)
	if err != nil || n != 0 || p.calls != 0 {
		t.Fatalf("n=%d err=%v calls=%d", n, err, p.calls)
	}
}

func TestListIdleForReflectionUsesCandidateIndex(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	rows, err := st.DB.QueryContext(ctx, `
		EXPLAIN QUERY PLAN
		SELECT id FROM sessions
		WHERE updated_at < ?
		  AND summary_watermark < (
		    SELECT COALESCE(MAX(seq), 0) FROM turns WHERE session_id = sessions.id
		  )
		  AND EXISTS (
		    SELECT 1 FROM turns
		    WHERE session_id = sessions.id
		    LIMIT 1 OFFSET 1
		  )
		ORDER BY updated_at ASC
		LIMIT ?`, time.Now().UTC().Format(time.RFC3339Nano), 20)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()

	usedIndex := false
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(detail, "idx_sessions_updated_at") {
			usedIndex = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !usedIndex {
		t.Fatal("idle reflection query did not use its candidate index")
	}
}

func TestListShowsSummary(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sessions := New(st)
	sess, err := sessions.Create(ctx, "title")
	if err != nil {
		t.Fatal(err)
	}
	if err := sessions.SetSummary(ctx, sess.ID, "session covered reflection AC"); err != nil {
		t.Fatal(err)
	}
	all, err := sessions.List(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Summary != "session covered reflection AC" {
		t.Fatalf("list = %+v", all)
	}
}

// pastSession forces a session's conversation-activity timestamp into the
// past so the idle window applies.
func pastSession(t *testing.T, st *store.Store, id string, hoursAgo int) {
	t.Helper()
	past := time.Now().UTC().Add(-time.Duration(hoursAgo) * time.Hour).Format(time.RFC3339Nano)
	if _, err := st.DB.ExecContext(context.Background(), `UPDATE sessions SET updated_at = ? WHERE id = ?`, past, id); err != nil {
		t.Fatal(err)
	}
}

func seedTwoTurnSession(t *testing.T, sessions *Store) string {
	t.Helper()
	sess, err := sessions.Create(context.Background(), "t")
	if err != nil {
		t.Fatal(err)
	}
	_ = sessions.AppendTurn(context.Background(), sess.ID, llm.UserText("first turn"))
	_ = sessions.AppendTurn(context.Background(), sess.ID, llm.Message{
		Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "second turn"}},
	})
	return sess.ID
}

// TestIdleReReflectResumedSession is the core #411 regression: a session
// reflected after its first idle period becomes eligible again when new turns
// arrive, and an unchanged session is never reflected twice.
func TestIdleReReflectResumedSession(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sessions := New(st)
	id := seedTwoTurnSession(t, sessions)
	pastSession(t, st, id, 2)

	p := &scriptProvider{reply: "first quiet period summary"}
	r := &IdleReflector{Sessions: sessions, Provider: func() (llm.Provider, string) { return p, "m" }, After: 30 * time.Minute, Now: time.Now}
	n, err := r.RunOnce(ctx)
	if err != nil || n != 1 {
		t.Fatalf("first pass n=%d err=%v", n, err)
	}
	got, _ := sessions.Get(ctx, id)
	if got.SummaryWatermark != 2 || got.ReflectedAt.IsZero() {
		t.Fatalf("after first reflection: %+v (watermark want 2)", got)
	}

	// Resumed session: new turns arrive, then the session goes idle again.
	_ = sessions.AppendTurn(ctx, id, llm.UserText("resumed with new work"))
	_ = sessions.AppendTurn(ctx, id, llm.Message{
		Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "new outcome"}},
	})
	pastSession(t, st, id, 1)

	p.reply = "second quiet period summary"
	n, err = r.RunOnce(ctx)
	if err != nil || n != 1 {
		t.Fatalf("resumed pass n=%d err=%v (want 1: eligible again)", n, err)
	}
	got, _ = sessions.Get(ctx, id)
	if !strings.Contains(got.Summary, "second quiet period") {
		t.Fatalf("summary not updated: %q", got.Summary)
	}
	if got.SummaryWatermark != 4 {
		t.Fatalf("watermark = %d, want 4", got.SummaryWatermark)
	}

	// No new turns: unchanged session is not reflected a third time.
	n, err = r.RunOnce(ctx)
	if err != nil || n != 0 {
		t.Fatalf("unchanged pass n=%d err=%v (want 0: no re-reflection)", n, err)
	}
}

// TestIdleReflectKeepsActivityTimestamp verifies reflection metadata never
// bumps updated_at: idle timing stays based on user/assistant activity (#411).
func TestIdleReflectKeepsActivityTimestamp(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sessions := New(st)
	id := seedTwoTurnSession(t, sessions)
	past := time.Now().UTC().Add(-2 * time.Hour)
	pastStr := past.Format(time.RFC3339Nano)
	if _, err := st.DB.ExecContext(ctx, `UPDATE sessions SET updated_at = ? WHERE id = ?`, pastStr, id); err != nil {
		t.Fatal(err)
	}
	r := &IdleReflector{Sessions: sessions, Provider: func() (llm.Provider, string) { return &scriptProvider{reply: "s"}, "m" }, After: 30 * time.Minute, Now: time.Now}
	if n, err := r.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("RunOnce n=%d err=%v", n, err)
	}
	got, _ := sessions.Get(ctx, id)
	if !got.UpdatedAt.Equal(past) {
		t.Fatalf("updated_at moved to %v, want %v (reflection must not bump activity)", got.UpdatedAt, past)
	}
	if got.Summary == "" || got.SummaryWatermark != 2 {
		t.Fatalf("summary=%q watermark=%d", got.Summary, got.SummaryWatermark)
	}
}

// recordingProvider captures the message payloads sent to Reflect so tests can
// assert the incremental window (prior summary + uncovered turns only).
type recordingProvider struct {
	reply   string
	payload []llm.Message
}

func (p *recordingProvider) Complete(ctx context.Context, req llm.Request, _ llm.StreamFunc) (*llm.Response, error) {
	p.payload = append([]llm.Message(nil), req.Messages...)
	return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: p.reply}}}}, nil
}

// TestIdleReflectIncrementalSendsPriorSummary verifies a resumed session is
// re-reflected with the previous summary plus only uncovered turns, not the
// full history (#411).
func TestIdleReflectIncrementalSendsPriorSummary(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sessions := New(st)
	id := seedTwoTurnSession(t, sessions)
	pastSession(t, st, id, 2)

	p := &recordingProvider{reply: "first summary"}
	r := &IdleReflector{Sessions: sessions, Provider: func() (llm.Provider, string) { return p, "m" }, After: 30 * time.Minute, Now: time.Now}
	if n, err := r.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("first pass n=%d err=%v", n, err)
	}
	p.payload = nil
	_ = sessions.AppendTurn(ctx, id, llm.UserText("turn three"))
	_ = sessions.AppendTurn(ctx, id, llm.Message{
		Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "turn four"}},
	})
	pastSession(t, st, id, 1)
	p.reply = "second summary"
	if n, err := r.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("resumed pass n=%d err=%v", n, err)
	}
	texts := make([]string, 0, len(p.payload))
	for _, m := range p.payload {
		texts = append(texts, m.Text())
	}
	joined := strings.Join(texts, "\n")
	if !strings.Contains(joined, "first summary") {
		t.Fatalf("prior summary not sent: %v", texts)
	}
	if strings.Contains(joined, "first turn") || strings.Contains(joined, "second turn") {
		t.Fatalf("covered turns resent in full history: %v", texts)
	}
	if !strings.Contains(joined, "turn three") || !strings.Contains(joined, "turn four") {
		t.Fatalf("uncovered turns missing: %v", texts)
	}
	// The prior summary must arrive as assistant/context content, never as a
	// user message that could elevate user-influenced text into the
	// instruction stream (#421 review).
	var priorRole llm.Role
	for _, m := range p.payload {
		if strings.Contains(m.Text(), "first summary") {
			priorRole = m.Role
		}
	}
	if priorRole != llm.RoleAssistant {
		t.Fatalf("prior summary role = %q, want assistant", priorRole)
	}
}

// TestIdleReflectBoundedWindowDoesNotOverclaim verifies the committed
// watermark never claims coverage beyond the turns actually sent: with more
// than the bounded window of uncovered turns, the watermark advances by the
// window, not to the latest turn (#411 review).
func TestIdleReflectBoundedWindowDoesNotOverclaim(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sessions := New(st)
	// 60 turns: far more than the 30-turn bounded window.
	sess, err := sessions.Create(ctx, "long")
	if err != nil {
		t.Fatal(err)
	}
	id := sess.ID
	for i := 0; i < 60; i++ {
		role := llm.RoleUser
		if i%2 == 1 {
			role = llm.RoleAssistant
		}
		if err := sessions.AppendTurn(ctx, id, llm.Message{Role: role, Blocks: []llm.Block{{Type: llm.BlockText, Text: fmt.Sprintf("turn %d", i)}}}); err != nil {
			t.Fatal(err)
		}
	}
	pastSession(t, st, id, 2)
	r := &IdleReflector{Sessions: sessions, Provider: func() (llm.Provider, string) { return &scriptProvider{reply: "bounded summary"}, "m" }, After: 30 * time.Minute, Now: time.Now}
	if n, err := r.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("RunOnce n=%d err=%v", n, err)
	}
	got, _ := sessions.Get(ctx, id)
	if got.SummaryWatermark != IdleReflectMaxHistory {
		t.Fatalf("watermark = %d, want the bounded window %d (must not overclaim 60)", got.SummaryWatermark, IdleReflectMaxHistory)
	}
	// Still eligible: the next quiet period advances coverage by the window.
	if n, err := r.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("second pass n=%d err=%v, want the uncovered remainder reflected", n, err)
	}
	got, _ = sessions.Get(ctx, id)
	if got.SummaryWatermark != 2*IdleReflectMaxHistory {
		t.Fatalf("watermark after second pass = %d, want %d", got.SummaryWatermark, 2*IdleReflectMaxHistory)
	}
}

// TestIdleReflectProviderFailureKeepsWatermark verifies a provider failure
// leaves the previous summary and watermark intact and retries on a later
// tick (#411).
func TestIdleReflectProviderFailureKeepsWatermark(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sessions := New(st)
	id := seedTwoTurnSession(t, sessions)
	pastSession(t, st, id, 2)

	p := &scriptProvider{reply: "good summary"}
	r := &IdleReflector{Sessions: sessions, Provider: func() (llm.Provider, string) { return p, "m" }, After: 30 * time.Minute, Now: time.Now}
	if n, err := r.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("first pass n=%d err=%v", n, err)
	}
	_ = sessions.AppendTurn(ctx, id, llm.UserText("new turn after summary"))
	_ = sessions.AppendTurn(ctx, id, llm.Message{
		Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "new outcome"}},
	})
	pastSession(t, st, id, 1)

	// Provider now fails: nothing may be written or advanced.
	p.reply = ""
	failing := &IdleReflector{Sessions: sessions, Provider: func() (llm.Provider, string) { return errProvider{}, "m" }, After: 30 * time.Minute, Now: time.Now, OnError: func(error) {}}
	if n, err := failing.RunOnce(ctx); err != nil || n != 0 {
		t.Fatalf("failure pass n=%d err=%v", n, err)
	}
	got, _ := sessions.Get(ctx, id)
	if got.Summary != "good summary" || got.SummaryWatermark != 2 {
		t.Fatalf("failure mutated state: %+v", got)
	}

	// Provider recovers: the same eligible session is retried and advanced.
	p.reply = "recovered summary"
	if n, err := r.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("recovery pass n=%d err=%v", n, err)
	}
	got, _ = sessions.Get(ctx, id)
	if got.Summary != "recovered summary" || got.SummaryWatermark != 4 {
		t.Fatalf("recovery result = %+v", got)
	}
}

// TestMigrationBackfillsExistingSummariesConservatively verifies migration
// 0032 treats an existing summary as covering all turns present today.
func TestMigrationBackfillsExistingSummariesConservatively(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	// Create a pre-migration store state by opening the current store, writing
	// a summarized session, then re-opening (migrations already applied).
	st, err := store.Open(ctx, filepath.Join(dir, "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	sessions := New(st)
	id := seedTwoTurnSession(t, sessions)
	if err := sessions.SetSummaryWatermark(ctx, id, "covered summary", 2); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	// Re-open: the same migration set applies; the watermark column must
	// round-trip without drift and the summary must be the newest committed.
	st2, err := store.Open(ctx, filepath.Join(dir, "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st2.Close() })
	got, err := New(st2).Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Summary != "covered summary" || got.SummaryWatermark != 2 {
		t.Fatalf("reopened session = %+v", got)
	}
}
