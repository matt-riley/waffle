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

type orderedHandoffProvider struct {
	entered chan<- struct{}
	release <-chan struct{}
	file    string
}

func (p orderedHandoffProvider) Complete(context.Context, llm.Request, llm.StreamFunc) (*llm.Response, error) {
	p.entered <- struct{}{}
	<-p.release
	return &llm.Response{StopReason: llm.StopEndTurn, Message: llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "```json\n{\"status\":\"done\",\"summary\":\"ok\",\"files_changed\":[\"" + p.file + "\"]}\n```"}}}}, nil
}

func TestParseAndNormalizeHandoff(t *testing.T) {
	text := "done\n```json\n{\"status\":\"done\",\"summary\":\"ok\",\"verification\":[]}\n```\n"
	h, err := ParseHandoff(text)
	if err != nil {
		t.Fatal(err)
	}
	p := WorkPacket{Task: "t", VerifyCommands: []string{"go test"}}
	h = NormalizeHandoff(h, p)
	if h.Status != "partial" {
		t.Fatalf("status = %s", h.Status)
	}

	h2 := Handoff{Status: "done", Summary: "x", FilesChanged: []string{"secret.txt"}}
	h2 = NormalizeHandoff(h2, WorkPacket{Task: "t", ReadOnly: true})
	if h2.Status != "blocked" {
		t.Fatalf("readonly = %s", h2.Status)
	}

	h3 := Handoff{Status: "done", Summary: "x", FilesChanged: []string{"other/x.go"},
		Proposals: []workset.Proposal{{Op: "add", Kind: "nope", Body: "z"}}}
	h3 = NormalizeHandoff(h3, WorkPacket{Task: "t", OwnedPaths: []string{"pkg/"}})
	if h3.Status != "partial" || len(h3.Proposals) != 0 {
		t.Fatalf("%+v", h3)
	}
	out := FormatHandoffResult(Handoff{Status: "partial", Summary: "s", Proposals: []workset.Proposal{{Op: "add", Kind: workset.KindFact, Body: "a"}}})
	if !strings.Contains(out, "WORKING_SET_PROPOSALS — not applied") {
		t.Fatal(out)
	}
}

func TestFramePacketLegacyCompatible(t *testing.T) {
	p := WorkPacket{Task: "research X"}
	f := FramePacket(p)
	if !strings.Contains(f, "research X") || !strings.Contains(f, "<work_packet>") {
		t.Fatal(f)
	}
}

func TestParseHandoffRequiresFencedJSON(t *testing.T) {
	if _, err := ParseHandoff(`{"status":"done","summary":"bare"}`); err == nil {
		t.Fatal("bare JSON must not satisfy the strict handoff contract")
	}
}

func TestNormalizeHandoffDowngradesMissingRequestedCommand(t *testing.T) {
	h := Handoff{
		Status:  "done",
		Summary: "only one requested check was reported",
		Verification: []VerificationResult{{
			Command: "go test ./...",
			Status:  "pass",
		}},
	}
	h = NormalizeHandoff(h, WorkPacket{
		Task:           "t",
		VerifyCommands: []string{"go test ./...", "go vet ./..."},
	})
	if h.Status != "partial" {
		t.Fatalf("status = %q, want partial", h.Status)
	}
	if !strings.Contains(strings.Join(h.Reasons, "\n"), "go vet ./...") {
		t.Fatalf("reasons = %v, want missing command", h.Reasons)
	}
}

func TestParseHandoffRejectsDuplicateChangedPaths(t *testing.T) {
	text := "```json\n{\"status\":\"done\",\"summary\":\"x\",\"files_changed\":[\"a.go\",\"a.go\"]}\n```"
	if _, err := ParseHandoff(text); err == nil {
		t.Fatal("duplicate changed paths must fail handoff validation")
	}
}

func TestParseHandoffStrictSchema(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"unknown top-level field", `{"status":"done","summary":"x","surprise":true}`},
		{"unknown nested finding field", `{"status":"done","summary":"x","findings":[{"title":"t","surprise":true}]}`},
		{"unknown nested verification field", `{"status":"done","summary":"x","verification":[{"command":"go test","status":"pass","surprise":true}]}`},
		{"unknown nested proposal field", `{"status":"done","summary":"x","proposals":[{"op":"add","kind":"fact","body":"x","surprise":true}]}`},
		{"unknown verification status", `{"status":"done","summary":"x","verification":[{"command":"go test","status":"maybe"}]}`},
		{"unknown handoff status", `{"status":"complete","summary":"x"}`},
		{"normalized duplicate path", `{"status":"done","summary":"x","files_changed":["./a.go","a.go"]}`},
		{"oversized summary", `{"status":"done","summary":"` + strings.Repeat("x", MaxHandoffTextBytes+1) + `"}`},
		{"oversized path", `{"status":"done","summary":"x","files_changed":["` + strings.Repeat("p", MaxHandoffPathBytes+1) + `"]}`},
		{"oversized verification output", `{"status":"done","summary":"x","verification":[{"command":"go test","status":"pass","output":"` + strings.Repeat("x", MaxHandoffTextBytes+1) + `"}]}`},
		{"oversized findings collection", `{"status":"done","summary":"x","findings":` + repeatedFindings(MaxHandoffItems+1) + `}`},
		{"trailing JSON", `{"status":"done","summary":"x"} {"status":"done","summary":"y"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseHandoff("```json\n" + tc.body + "\n```"); err == nil {
				t.Fatal("expected strict schema rejection")
			}
		})
	}
}

func repeatedFindings(n int) string {
	var b strings.Builder
	b.WriteByte('[')
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"title":"x"}`)
	}
	b.WriteByte(']')
	return b.String()
}

func TestParseWorkPacketRejectsUnknownAndOversizedFields(t *testing.T) {
	for _, raw := range []string{
		`{"task":"x","surprise":true}`,
		`{"task":"` + strings.Repeat("x", MaxHandoffTextBytes+1) + `"}`,
	} {
		if _, err := ParseWorkPacket([]byte(raw)); err == nil {
			t.Fatalf("expected packet rejection for %.40q", raw)
		}
	}
	if p, err := ParseWorkPacket([]byte(`{"task":"legacy"}`)); err != nil || p.Task != "legacy" {
		t.Fatalf("legacy packet: %+v %v", p, err)
	}
}

func TestPacketRawLimitsAndOwnedPathValidation(t *testing.T) {
	if _, err := ParseWorkPacket([]byte(`{"task":"` + strings.Repeat("x", MaxPacketRawBytes) + `"}`)); err == nil {
		t.Fatal("aggregate work packet limit not enforced before decode")
	}
	for _, raw := range []string{
		`{"task":"x","owned_paths":["../escape"]}`,
		`{"task":"x","owned_paths":["/absolute"]}`,
		`{"task":"x","owned_paths":["a/../../escape"]}`,
		`{"task":"x","owned_paths":["C:\\\\repo\\\\file.go"]}`,
		`{"task":"x","owned_paths":["pkg\\\\..\\\\secret.go"]}`,
		`{"task":"x","owned_paths":["` + strings.Repeat("p", MaxHandoffPathBytes+1) + `"]}`,
		`{"task":"x","context_refs":["` + strings.Repeat("r", MaxHandoffPathBytes+1) + `"]}`,
	} {
		if _, err := ParseWorkPacket([]byte(raw)); err == nil {
			t.Fatalf("unsafe/oversized packet path accepted: %.80q", raw)
		}
	}
	if _, err := ParseHandoff("```json\n" + strings.Repeat(" ", MaxPacketRawBytes) + "\n```"); err == nil {
		t.Fatal("aggregate handoff limit not enforced before decode")
	}
}

func TestNormalizeHandoffRejectsOwnedPathTraversalBypass(t *testing.T) {
	tests := []string{"pkg/../secret.txt", "../pkg/file.go", "/pkg/file.go"}
	for _, changed := range tests {
		h := NormalizeHandoff(Handoff{Status: "done", Summary: "x", FilesChanged: []string{changed}}, WorkPacket{Task: "x", OwnedPaths: []string{"pkg"}})
		if h.Status == "done" || !strings.Contains(strings.Join(h.Reasons, "\n"), "needs_supervisor_review") {
			t.Fatalf("changed path %q bypassed owned paths: %+v", changed, h)
		}
	}
	h := NormalizeHandoff(Handoff{Status: "done", Summary: "x", FilesChanged: []string{"./pkg/a.go"}}, WorkPacket{Task: "x", OwnedPaths: []string{"pkg/./"}})
	if h.Status != "done" {
		t.Fatalf("normalized in-scope path rejected: %+v", h)
	}
}

func TestParallelSubagentsDisjointOwnedPathsRemainAssociatedOutOfOrder(t *testing.T) {
	entered := make(chan struct{}, 2)
	releaseA, releaseB := make(chan struct{}), make(chan struct{})
	tasks := []struct {
		provider llm.Provider
		input    string
	}{
		{orderedHandoffProvider{entered, releaseA, "a/a.go"}, `{"task":"A","owned_paths":["a"]}`},
		// B deliberately violates its b/ ownership; its partial result must not
		// contaminate A when B completes first.
		{orderedHandoffProvider{entered, releaseB, "a/intruder.go"}, `{"task":"B","owned_paths":["b"]}`},
	}
	results := make([]string, 2)
	var wg sync.WaitGroup
	for i := range tasks {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], _ = (SubagentTool{Provider: tasks[i].provider, Tools: tool.NewRegistry(), Model: "m"}).Run(context.Background(), json.RawMessage(tasks[i].input))
		}(i)
	}
	<-entered
	<-entered
	close(releaseB) // reverse completion order
	close(releaseA)
	wg.Wait()
	if !strings.Contains(results[0], `"status": "done"`) || !strings.Contains(results[0], "a/a.go") || strings.Contains(results[0], "intruder") {
		t.Fatalf("child A result misassociated: %s", results[0])
	}
	if !strings.Contains(results[1], `"status": "partial"`) || !strings.Contains(results[1], "needs_supervisor_review: a/intruder.go") || strings.Contains(results[1], "a/a.go") {
		t.Fatalf("child B result misassociated: %s", results[1])
	}
}
