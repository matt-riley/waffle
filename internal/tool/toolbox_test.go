package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/matt-riley/waffle/internal/llm"
)

type namedTool struct{ name string }

func (n namedTool) Def() llm.Tool {
	return llm.Tool{Name: n.name, InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func (n namedTool) Run(ctx context.Context, _ json.RawMessage) (string, error) {
	return "ran:" + n.name, nil
}

func names(defs []llm.Tool) []string {
	out := make([]string, len(defs))
	for i, d := range defs {
		out[i] = d.Name
	}
	return out
}

func TestPolicy(t *testing.T) {
	cases := []struct {
		p    Policy
		name string
		want bool
	}{
		{Policy{}, "bash", true},
		{Policy{Deny: []string{"bash"}}, "bash", false},
		{Policy{Allow: []string{"fetch"}}, "bash", false},
		{Policy{Allow: []string{"bash"}}, "bash", true},
		{Policy{Allow: []string{"bash"}, Deny: []string{"bash"}}, "bash", false}, // deny wins
	}
	for _, c := range cases {
		if got := c.p.Permits(c.name); got != c.want {
			t.Errorf("%+v Permits(%s) = %v, want %v", c.p, c.name, got, c.want)
		}
	}
}

func TestRestrict(t *testing.T) {
	tb := NewRegistry(namedTool{"bash"}, namedTool{"fetch"})
	r := Restrict(tb, Policy{Deny: []string{"bash"}})

	got := names(r.Defs())
	if len(got) != 1 || got[0] != "fetch" {
		t.Errorf("defs = %v", got)
	}
	if _, err := r.Run(context.Background(), "bash", nil); err == nil || !strings.Contains(err.Error(), "not permitted") {
		t.Errorf("denied tool ran: %v", err)
	}
	if out, err := r.Run(context.Background(), "fetch", nil); err != nil || out != "ran:fetch" {
		t.Errorf("allowed tool = %q, %v", out, err)
	}

	// Zero policy is a no-op passthrough.
	if Restrict(tb, Policy{}) != Toolbox(tb) {
		t.Error("zero policy wrapped the toolbox")
	}
}

func TestCombine(t *testing.T) {
	a := NewRegistry(namedTool{"bash"}, namedTool{"fetch"})
	b := NewRegistry(namedTool{"remember"}, namedTool{"bash"}) // duplicate bash: first wins

	c := Combine(a, b)
	got := names(c.Defs())
	if len(got) != 3 {
		t.Fatalf("defs = %v", got)
	}
	if out, _ := c.Run(context.Background(), "remember", nil); out != "ran:remember" {
		t.Errorf("remember = %q", out)
	}
	if _, err := c.Run(context.Background(), "nope", nil); err == nil {
		t.Error("unknown tool ran")
	}
}
