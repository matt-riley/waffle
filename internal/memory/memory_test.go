package memory

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/spill"
	"github.com/matt-riley/waffle/internal/store"
	"github.com/matt-riley/waffle/internal/tool"
)

func testWorkspace(t *testing.T) Workspace {
	t.Helper()
	t.Setenv("WAFFLE_HOME", t.TempDir())
	ws, err := Open(DefaultAgent)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return ws
}

func TestSystemContextRendersPromptFiles(t *testing.T) {
	ws := testWorkspace(t)

	// Empty workspace → empty context, no errors.
	got, err := ws.SystemContext()
	if err != nil || got != "" {
		t.Fatalf("empty workspace context = %q, %v", got, err)
	}

	if err := os.WriteFile(filepath.Join(ws.Dir, "AGENT.md"), []byte("Be terse."), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.Append("user prefers tabs"); err != nil {
		t.Fatalf("Append: %v", err)
	}

	got, err = ws.SystemContext()
	if err != nil {
		t.Fatalf("SystemContext: %v", err)
	}
	if !strings.Contains(got, "<AGENT.md>\nBe terse.\n</AGENT.md>") {
		t.Errorf("AGENT.md missing: %q", got)
	}
	if !strings.Contains(got, "user prefers tabs") || !strings.Contains(got, "<MEMORY.md>") {
		t.Errorf("MEMORY.md missing: %q", got)
	}
	// AGENT.md renders before MEMORY.md.
	if strings.Index(got, "AGENT.md") > strings.Index(got, "MEMORY.md") {
		t.Errorf("prompt files out of order: %q", got)
	}
	// New notes carry stable IDs.
	if !noteIDRE.MatchString(got) {
		t.Errorf("MEMORY.md injection missing id= marker: %q", got)
	}
}

func TestRememberTool(t *testing.T) {
	ws := testWorkspace(t)
	tool := RememberTool{WS: ws}

	out, err := tool.Run(context.Background(), json.RawMessage(`{"note":"deploys only from CI"}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "MEMORY.md") || !strings.Contains(out, "id=") {
		t.Errorf("out = %q, want id in result", out)
	}
	id := extractReportedID(t, out)
	b, err := os.ReadFile(ws.MemoryPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "deploys only from CI") {
		t.Errorf("MEMORY.md = %q", b)
	}
	if !strings.Contains(string(b), "[id="+id+"]") {
		t.Errorf("MEMORY.md missing id %s: %q", id, b)
	}

	if _, err := tool.Run(context.Background(), json.RawMessage(`{"note":"  "}`)); err == nil {
		t.Error("empty note accepted")
	}
}

func TestRememberDedupesExactBody(t *testing.T) {
	ws := testWorkspace(t)
	tool := RememberTool{WS: ws}
	out1, err := tool.Run(context.Background(), json.RawMessage(`{"note":"same fact"}`))
	if err != nil {
		t.Fatal(err)
	}
	id := extractReportedID(t, out1)
	out2, err := tool.Run(context.Background(), json.RawMessage(`{"note":"  same   fact  "}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out2, "already noted") || !strings.Contains(out2, id) {
		t.Errorf("dedupe out = %q, want already noted with id=%s", out2, id)
	}
	body, err := os.ReadFile(ws.MemoryPath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(body), "same fact") != 1 {
		t.Fatalf("expected one note, got:\n%s", body)
	}
}

func TestAppendNormalizesNewlines(t *testing.T) {
	ws := testWorkspace(t)
	if _, err := ws.Append("first line\n- injected\nsecond line"); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(ws.MemoryPath())
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if strings.Count(text, "\n") != 1 {
		t.Fatalf("memory entry spans multiple lines:\n%s", text)
	}
	if strings.Contains(text, "\n- injected") {
		t.Fatalf("memory entry preserved injected bullet:\n%s", text)
	}
	if !strings.Contains(text, "first line - injected second line") {
		t.Fatalf("memory entry not normalized: %s", text)
	}
}

func TestAppendSupersedeForget(t *testing.T) {
	ws := testWorkspace(t)
	tool := RememberTool{WS: ws}
	upd := MemoryUpdateTool{WS: ws, Provenance: Provenance{TrustClass: "owner_stated", SourceID: "test"}}

	out, err := tool.Run(context.Background(), json.RawMessage(`{"note":"old preference"}`))
	if err != nil {
		t.Fatal(err)
	}
	oldID := extractReportedID(t, out)

	// Supersede → archive old, new id with today's date and (supersedes #old).
	supIn, _ := json.Marshal(map[string]string{
		"id": oldID, "action": "supersede", "note": "new preference",
	})
	supOut, err := upd.Run(context.Background(), supIn)
	if err != nil {
		t.Fatalf("supersede: %v", err)
	}
	newID := extractReportedID(t, supOut)
	if newID == oldID {
		t.Fatalf("supersede reused id %s", newID)
	}

	live, err := os.ReadFile(ws.MemoryPath())
	if err != nil {
		t.Fatal(err)
	}
	liveText := string(live)
	if strings.Contains(liveText, "[id="+oldID+"]") {
		t.Fatalf("old id still in MEMORY.md:\n%s", liveText)
	}
	if !strings.Contains(liveText, "[id="+newID+"]") {
		t.Fatalf("new id missing from MEMORY.md:\n%s", liveText)
	}
	today := time.Now().UTC().Format("2006-01-02")
	if !strings.Contains(liveText, today) {
		t.Fatalf("supersede missing today's date %s:\n%s", today, liveText)
	}
	if !strings.Contains(liveText, "(supersedes #"+oldID+")") {
		t.Fatalf("missing supersedes marker:\n%s", liveText)
	}
	if !strings.Contains(liveText, "new preference") {
		t.Fatalf("replacement body missing:\n%s", liveText)
	}

	arch, err := os.ReadFile(ws.ArchivePath())
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	if !strings.Contains(string(arch), "[id="+oldID+"]") || !strings.Contains(string(arch), "old preference") {
		t.Fatalf("archive missing old note:\n%s", arch)
	}

	// Forget → archive replacement, empty live memory.
	forIn, _ := json.Marshal(map[string]string{"id": newID, "action": "forget"})
	if _, err := upd.Run(context.Background(), forIn); err != nil {
		t.Fatalf("forget: %v", err)
	}
	live, err = os.ReadFile(ws.MemoryPath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(live), "[id="+newID+"]") || strings.Contains(string(live), "new preference") {
		t.Fatalf("forgotten note still live:\n%s", live)
	}
	arch, err = os.ReadFile(ws.ArchivePath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(arch), "[id="+newID+"]") {
		t.Fatalf("forget did not archive new note:\n%s", arch)
	}
}

func TestInjectBudgetBoundary(t *testing.T) {
	ws := testWorkspace(t)
	// Two notes; budget fits only the newer one when both are unpinned.
	// Write fixed lines so sizes are deterministic.
	content := "" +
		"- [id=old001] 2020-01-01 [trust=owner_stated source=]: alpha note about the past\n" +
		"- [id=new001] 2026-07-12 [trust=owner_stated source=]: beta note about now\n"
	if err := os.WriteFile(ws.MemoryPath(), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	// Budget large enough for one full line + newline, not both.
	lineNew := "- [id=new001] 2026-07-12 [trust=owner_stated source=]: beta note about now"
	ws.InjectBudget = len(lineNew) + 1 // exactly one line
	got, err := ws.SystemContext()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "beta note about now") {
		t.Fatalf("expected newest note: %q", got)
	}
	if strings.Contains(got, "alpha note about the past") {
		t.Fatalf("expected oldest elided: %q", got)
	}
	if !strings.Contains(got, "1 notes omitted") || !strings.Contains(got, "recall") {
		t.Fatalf("expected omit count pointing at recall: %q", got)
	}

	// Pinned older note wins over newer unpinned when budget is tight.
	content = "" +
		"- [id=old002] 2020-01-01 [pin] [trust=owner_stated source=]: pinned old fact\n" +
		"- [id=new002] 2026-07-12 [trust=owner_stated source=]: unpinned new fact\n"
	if err := os.WriteFile(ws.MemoryPath(), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	pinned := "- [id=old002] 2020-01-01 [pin] [trust=owner_stated source=]: pinned old fact"
	ws.InjectBudget = len(pinned) + 1
	got, err = ws.SystemContext()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "pinned old fact") {
		t.Fatalf("expected pinned note: %q", got)
	}
	if strings.Contains(got, "unpinned new fact") {
		t.Fatalf("expected unpinned elided under budget: %q", got)
	}
}

func TestArchiveNotInjected(t *testing.T) {
	ws := testWorkspace(t)
	if err := os.WriteFile(ws.MemoryPath(), []byte("- [id=live01] 2026-07-12 [trust=owner_stated source=]: live note\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ws.ArchivePath(), []byte("- [id=arch01] 2020-01-01 [trust=owner_stated source=]: archived secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ws.SystemContext()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "live note") {
		t.Fatalf("live note missing: %q", got)
	}
	if strings.Contains(got, "archived secret") || strings.Contains(got, "arch01") {
		t.Fatalf("archive leaked into SystemContext: %q", got)
	}
}

func TestLegacyUnIDdLinesStillRender(t *testing.T) {
	ws := testWorkspace(t)
	legacy := "- 2025-06-01 [trust=owner_stated source=]: legacy preference for spaces\n"
	if err := os.WriteFile(ws.MemoryPath(), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ws.SystemContext()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "legacy preference for spaces") {
		t.Fatalf("legacy line not rendered: %q", got)
	}
	if !strings.Contains(got, "<MEMORY.md>") {
		t.Fatalf("MEMORY section missing: %q", got)
	}
}

func TestMatchingAndRemovingMemoryLines(t *testing.T) {
	ws := Workspace{Dir: t.TempDir()}
	if err := os.WriteFile(ws.MemoryPath(), []byte("keep this\nremove this\nkeep too\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lines, err := ws.MatchingLines("remove")
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "2:") {
		t.Fatalf("matches = %v", lines)
	}
	if err := ws.RemoveLines([]int{2}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(ws.MemoryPath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "remove this") || !strings.Contains(string(body), "keep this") {
		t.Fatalf("memory = %q", body)
	}
}

func TestRecallTool(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	}()
	sessions := session.New(st)

	sess, _ := sessions.Create(ctx, "")
	if err := sessions.AppendTurn(ctx, sess.ID, llm.UserText("remember the zanzibar migration plan")); err != nil {
		t.Fatal(err)
	}

	tool := RecallTool{Sessions: sessions}
	out, err := tool.Run(ctx, json.RawMessage(`{"query":"zanzibar"}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, sess.ID) || !strings.Contains(out, "zanzibar") {
		t.Errorf("recall = %q", out)
	}

	out, err = tool.Run(ctx, json.RawMessage(`{"query":"nonexistent-term-xyz"}`))
	if err != nil || !strings.Contains(out, "no matches") {
		t.Errorf("no-match recall = %q, %v", out, err)
	}

	// scope=turns still works; scope=notes with empty WS is empty.
	out, err = tool.Run(ctx, json.RawMessage(`{"query":"zanzibar","scope":"turns"}`))
	if err != nil || !strings.Contains(out, "[turn]") {
		t.Errorf("scoped recall = %q, %v", out, err)
	}

	// Invalid scope is a tool error, not a crash (#60).
	if _, err := tool.Run(ctx, json.RawMessage(`{"query":"zanzibar","scope":"bogus"}`)); err == nil {
		t.Fatal("expected invalid scope error")
	} else if !strings.Contains(err.Error(), "invalid scope") {
		t.Fatalf("err = %v", err)
	}
}

func TestRecallNotesViaFTSAndArchive(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	}()
	notes := &NotesIndex{DB: st.DB}
	ws := testWorkspace(t)
	ws.Notes = notes

	tool := RememberTool{WS: ws, Notes: notes}
	if _, err := tool.Run(ctx, json.RawMessage(`{"note":"docker bridge networking quirk"}`)); err != nil {
		t.Fatal(err)
	}
	// Forget moves to archive but stays FTS-searchable.
	idOut, err := tool.Run(ctx, json.RawMessage(`{"note":"archived unique token xyzzy"}`))
	if err != nil {
		t.Fatal(err)
	}
	nid := extractReportedID(t, idOut)
	upd := MemoryUpdateTool{WS: ws, Notes: notes, Provenance: Provenance{TrustClass: "owner_stated"}}
	if _, err := upd.Run(ctx, json.RawMessage(`{"id":"`+nid+`","action":"forget"}`)); err != nil {
		t.Fatal(err)
	}

	recall := RecallTool{Notes: notes, WS: ws}
	got, err := recall.Run(ctx, json.RawMessage(`{"query":"docker networking","scope":"notes"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "docker") {
		t.Fatalf("FTS notes miss live note: %q", got)
	}
	got, err = recall.Run(ctx, json.RawMessage(`{"query":"xyzzy","scope":"notes"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "xyzzy") || !strings.Contains(got, "archive") {
		t.Fatalf("FTS notes miss archive: %q", got)
	}
}

func TestRecallFindsDockerNetworkingTurn(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	}()
	sessions := session.New(st)
	sess, err := sessions.Create(ctx, "sandbox networking")
	if err != nil {
		t.Fatal(err)
	}
	if err := sessions.AppendTurn(ctx, sess.ID, llm.UserText("fix docker networking so the container can reach the broker")); err != nil {
		t.Fatal(err)
	}
	out, err := RecallTool{Sessions: sessions}.Run(ctx, json.RawMessage(`{"query":"docker networking"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, sess.ID) || !strings.Contains(strings.ToLower(out), "docker") {
		t.Fatalf("regression recall docker networking: %q", out)
	}
}

func TestRecallSummariesScope(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	}()
	sessions := session.New(st)
	sess, err := sessions.Create(ctx, "ops retros")
	if err != nil {
		t.Fatal(err)
	}
	// Distinctive terms only in the session summary (not in turns).
	if err := sessions.SetSummary(ctx, sess.ID, "diagnosed the flubber deployment rollback after canary"); err != nil {
		t.Fatal(err)
	}
	out, err := RecallTool{Sessions: sessions}.Run(ctx, json.RawMessage(`{"query":"flubber deployment","scope":"summaries"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[summary]") {
		t.Fatalf("expected [summary] hit, got %q", out)
	}
	if !strings.Contains(out, sess.ID) || !strings.Contains(strings.ToLower(out), "flubber") {
		t.Fatalf("summary recall missing session/snippet: %q", out)
	}
	// scope=turns must not surface the summary-only match.
	out, err = RecallTool{Sessions: sessions}.Run(ctx, json.RawMessage(`{"query":"flubber deployment","scope":"turns"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "[summary]") || strings.Contains(out, "flubber") {
		t.Fatalf("turns scope should not return summary-only content: %q", out)
	}
}

func TestRecallSpillsScope(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	}()
	sp := &spill.Store{DB: st.DB}
	// Save requires content past OutputLimit; embed a unique FTS token.
	big := strings.Repeat("x", tool.OutputLimit) + "\nUNIQUE_SPILL_RECALL_TOKEN\n" + strings.Repeat("y", 200)
	sid, _, err := sp.Save(ctx, "sess-spill-recall", "bash", big)
	if err != nil || sid == "" {
		t.Fatalf("spill save: id=%q err=%v", sid, err)
	}
	out, err := RecallTool{Spills: sp}.Run(ctx, json.RawMessage(`{"query":"UNIQUE_SPILL_RECALL_TOKEN","scope":"spills"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[spill]") {
		t.Fatalf("expected [spill] labeled hit, got %q", out)
	}
	if !strings.Contains(out, sid) && !strings.Contains(out, "sess-spill-recall") {
		t.Fatalf("spill hit missing id/session: %q", out)
	}
	if !strings.Contains(out, "UNIQUE_SPILL_RECALL_TOKEN") {
		t.Fatalf("spill snippet missing token: %q", out)
	}
}

func TestRecallPartialMatchLabel(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	}()
	sessions := session.New(st)
	sess, _ := sessions.Create(ctx, "")
	// Terms match but not as the contiguous phrase "alpha omega".
	if err := sessions.AppendTurn(ctx, sess.ID, llm.UserText("alpha sits in the middle before omega arrives")); err != nil {
		t.Fatal(err)
	}
	out, err := RecallTool{Sessions: sessions}.Run(ctx, json.RawMessage(`{"query":"alpha omega","scope":"turns"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "partial:") {
		t.Fatalf("expected partial label: %q", out)
	}
	// Contiguous phrase → match:
	if err := sessions.AppendTurn(ctx, sess.ID, llm.UserText("discussed the alpha omega plan end-to-end")); err != nil {
		t.Fatal(err)
	}
	out, err = RecallTool{Sessions: sessions}.Run(ctx, json.RawMessage(`{"query":"alpha omega","scope":"turns"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "match:") {
		t.Fatalf("expected match label for phrase: %q", out)
	}
}

func TestNotesIndexSyncFromFiles(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	}()
	ws := testWorkspace(t)
	// Pre-change: notes only on disk (no index rows yet).
	if err := os.WriteFile(ws.MemoryPath(), []byte("- [id=pre001] 2026-01-01 [trust=owner_stated source=]: pre-migration fact about widgets\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	idx := &NotesIndex{DB: st.DB}
	if err := idx.SyncWorkspace(ctx, DefaultAgent, ws); err != nil {
		t.Fatal(err)
	}
	hits, err := idx.Search(ctx, "widgets", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || !strings.Contains(hits[0].Body, "widgets") {
		t.Fatalf("hits = %+v", hits)
	}
}

var reportedIDRE = regexp.MustCompile(`id=([a-zA-Z0-9]+)`)

func extractReportedID(t *testing.T, out string) string {
	t.Helper()
	// Prefer the last id= (supersede messages: "superseded #old → id=new")
	matches := reportedIDRE.FindAllStringSubmatch(out, -1)
	if len(matches) == 0 {
		t.Fatalf("no id= in tool result %q", out)
	}
	return matches[len(matches)-1][1]
}
