package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/store"
	"github.com/matt-riley/waffle/internal/tool"
	"github.com/matt-riley/waffle/internal/workset"
)

func TestParallelSubagentsShareBroadcastSnapshot(t *testing.T) {
	const snap = "<working_set>\n- [goal id=g1 source=user] fixed snapshot\n</working_set>\n"
	var mu sync.Mutex
	var systems []string
	p := &recordingProvider{onComplete: func(req llm.Request) llm.Response {
		mu.Lock()
		systems = append(systems, req.System)
		mu.Unlock()
		return llm.Response{
			StopReason: llm.StopEndTurn,
			Message: llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
				{Type: llm.BlockText, Text: "```json\n{\"status\":\"done\",\"summary\":\"ok\"}\n```"},
			}},
		}
	}}
	tl := SubagentTool{
		Provider:            p,
		Tools:               tool.NewRegistry(),
		Model:               "m",
		BroadcastWorkingSet: true,
		WorkingSetBroadcast: snap,
	}
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = tl.Run(context.Background(), json.RawMessage(`{"task":"t"}`))
		}()
	}
	wg.Wait()
	if len(systems) != 3 {
		t.Fatalf("systems=%d", len(systems))
	}
	for _, s := range systems {
		if !strings.Contains(s, "fixed snapshot") {
			t.Fatalf("missing snapshot: %q", s)
		}
	}
}

// TestParallelSubagentsFrozenSnapshotDespiteParentMutation freezes the broadcast
// before three concurrent Runs, mutates the parent store mid-flight, and asserts
// every child still sees the initial snapshot (#68).
func TestParallelSubagentsFrozenSnapshotDespiteParentMutation(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "snap.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ws := &workset.Store{DB: st.DB}
	const sid = "parent-parallel"
	if _, err := ws.Add(ctx, sid, workset.KindGoal, "initial-goal", workset.SourceUser, true); err != nil {
		t.Fatal(err)
	}
	before, err := ws.List(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	// Freeze snapshot at dispatch (as of tool batch start).
	frozen := workset.Render(before)
	if !strings.Contains(frozen, "initial-goal") {
		t.Fatalf("frozen=%q", frozen)
	}

	var mu sync.Mutex
	var systems []string
	var started sync.WaitGroup
	started.Add(3)
	release := make(chan struct{})
	p := &recordingProvider{onComplete: func(req llm.Request) llm.Response {
		mu.Lock()
		systems = append(systems, req.System)
		mu.Unlock()
		started.Done()
		<-release // hold until parent mutates
		return llm.Response{
			StopReason: llm.StopEndTurn,
			Message: llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
				{Type: llm.BlockText, Text: "```json\n{\"status\":\"done\",\"summary\":\"ok\"}\n```"},
			}},
		}
	}}
	tl := SubagentTool{
		Provider:            p,
		Tools:               tool.NewRegistry(),
		Model:               "m",
		BroadcastWorkingSet: true,
		WorkingSetBroadcast: frozen, // shared frozen string for all parallel Runs
	}
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = tl.Run(ctx, json.RawMessage(`{"task":"t"}`))
		}()
	}
	// Wait until all three children have captured system prompts, then mutate parent.
	started.Wait()
	if _, err := ws.Add(ctx, sid, workset.KindFact, "mutated-after-dispatch", workset.SourceUser, false); err != nil {
		t.Fatal(err)
	}
	close(release)
	wg.Wait()
	if len(systems) != 3 {
		t.Fatalf("systems=%d", len(systems))
	}
	for i, s := range systems {
		if !strings.Contains(s, "initial-goal") {
			t.Fatalf("child %d missing frozen snapshot: %q", i, s)
		}
		if strings.Contains(s, "mutated-after-dispatch") {
			t.Fatalf("child %d saw post-dispatch parent mutation: %q", i, s)
		}
	}
}

func TestSubagentProposalsDoNotMutateParentSet(t *testing.T) {
	// Structural: FormatHandoffResult marks proposals not applied; Normalize drops invalid.
	h := NormalizeHandoff(Handoff{
		Status:  "done",
		Summary: "x",
		Proposals: []workset.Proposal{
			{Op: "add", Kind: workset.KindFact, Body: "suggested"},
		},
	}, WorkPacket{Task: "t"})
	out := FormatHandoffResult(h)
	if !strings.Contains(out, "WORKING_SET_PROPOSALS — not applied") {
		t.Fatal(out)
	}
	// Parent set mutation would require workspace_update which is denied in child tools.
	tl := SubagentTool{Tools: tool.NewRegistry()}
	child := tool.Restrict(tl.Tools, tool.Policy{Deny: []string{"workspace_update", "spawn_subagent"}})
	if child != nil {
		for _, d := range child.Defs() {
			if d.Name == "workspace_update" {
				t.Fatal("child must not expose workspace_update")
			}
		}
	}
}

func TestEmptyBroadcastSystemByteIdentical(t *testing.T) {
	// empty set / BroadcastWorkingSet false → system prompt without working_set is identical.
	pFalse := &captureSystemProvider{}
	tlFalse := SubagentTool{
		Provider:            pFalse,
		Tools:               tool.NewRegistry(),
		Model:               "m",
		BroadcastWorkingSet: false,
		WorkingSetBroadcast: "",
	}
	if _, err := tlFalse.Run(context.Background(), json.RawMessage(`{"task":"same"}`)); err != nil {
		t.Fatal(err)
	}
	pEmpty := &captureSystemProvider{}
	tlEmpty := SubagentTool{
		Provider:            pEmpty,
		Tools:               tool.NewRegistry(),
		Model:               "m",
		BroadcastWorkingSet: true,
		WorkingSetBroadcast: "", // empty set still no injection
	}
	if _, err := tlEmpty.Run(context.Background(), json.RawMessage(`{"task":"same"}`)); err != nil {
		t.Fatal(err)
	}
	if pFalse.system != pEmpty.system {
		t.Fatalf("systems differ:\n%q\n%q", pFalse.system, pEmpty.system)
	}
	if strings.Contains(pFalse.system, "working_set") {
		t.Fatal("empty broadcast must not inject working_set")
	}
}

func TestParentWorksetUnchangedAfterSubagentProposals(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ws := &workset.Store{DB: st.DB}
	const sid = "parent-sess"
	e, err := ws.Add(ctx, sid, workset.KindGoal, "ship it", workset.SourceUser, true)
	if err != nil {
		t.Fatal(err)
	}
	before, err := ws.List(ctx, sid)
	if err != nil || len(before) != 1 {
		t.Fatalf("before: %+v %v", before, err)
	}

	// Subagent returns proposals; FormatHandoffResult does not apply them.
	p := &recordingProvider{onComplete: func(req llm.Request) llm.Response {
		return llm.Response{
			StopReason: llm.StopEndTurn,
			Message: llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
				{Type: llm.BlockText, Text: "```json\n{\"status\":\"done\",\"summary\":\"ok\",\"proposals\":[{\"op\":\"add\",\"kind\":\"fact\",\"body\":\"child suggestion\"}]}\n```"},
			}},
		}
	}}
	tl := SubagentTool{
		Provider:            p,
		Tools:               tool.NewRegistry(),
		Model:               "m",
		BroadcastWorkingSet: true,
		WorkingSetBroadcast: workset.Render(before),
	}
	out, err := tl.Run(ctx, json.RawMessage(`{"task":"t"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "WORKING_SET_PROPOSALS") {
		t.Fatalf("out=%q", out)
	}
	after, err := ws.List(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || after[0].ID != e.ID || after[0].Body != e.Body {
		t.Fatalf("parent set mutated: before=%+v after=%+v", before, after)
	}
}
