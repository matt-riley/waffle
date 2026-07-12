package memory

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/store"
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
	if err := ws.Append("user prefers tabs"); err != nil {
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
}

func TestRememberTool(t *testing.T) {
	ws := testWorkspace(t)
	tool := RememberTool{WS: ws}

	out, err := tool.Run(context.Background(), json.RawMessage(`{"note":"deploys only from CI"}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "MEMORY.md") {
		t.Errorf("out = %q", out)
	}
	b, err := os.ReadFile(ws.MemoryPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "deploys only from CI") {
		t.Errorf("MEMORY.md = %q", b)
	}

	if _, err := tool.Run(context.Background(), json.RawMessage(`{"note":"  "}`)); err == nil {
		t.Error("empty note accepted")
	}
}

func TestAppendNormalizesNewlines(t *testing.T) {
	ws := testWorkspace(t)
	if err := ws.Append("first line\n- injected\nsecond line"); err != nil {
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
}
