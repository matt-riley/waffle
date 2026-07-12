package agent

import (
	"strings"
	"testing"

	"github.com/matt-riley/waffle/internal/workset"
)

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
