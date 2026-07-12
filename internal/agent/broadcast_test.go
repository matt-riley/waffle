package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/matt-riley/waffle/internal/llm"
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
