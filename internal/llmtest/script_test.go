package llmtest

import (
	"context"
	"testing"

	"github.com/matt-riley/waffle/internal/llm"
)

func TestScriptConsumesInOrder(t *testing.T) {
	p := &Script{Responses: []llm.Response{Text("a"), Text("b")}}
	r1, err := p.Complete(context.Background(), llm.Request{Model: "m1"}, nil)
	if err != nil || r1.Message.Text() != "a" {
		t.Fatalf("r1=%v err=%v", r1, err)
	}
	r2, err := p.Complete(context.Background(), llm.Request{Model: "m2"}, nil)
	if err != nil || r2.Message.Text() != "b" {
		t.Fatalf("r2=%v err=%v", r2, err)
	}
	if _, err := p.Complete(context.Background(), llm.Request{}, nil); err == nil {
		t.Fatal("expected exhaustion")
	}
	if p.Calls != 3 || len(p.Models) != 3 || p.Models[0] != "m1" {
		t.Fatalf("calls=%d models=%v", p.Calls, p.Models)
	}
}
