package tool

import (
	"context"
	"encoding/json"
	"fmt"
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
		// Deny overrides allow=["*"] (#71).
		{Policy{Allow: []string{"*"}, Deny: []string{"bash"}}, "bash", false},
		{Policy{Allow: []string{"*"}, Deny: []string{"bash"}}, "read_file", true},
		{Policy{Allow: []string{"*"}}, "anything", true},
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

func TestDenyPrefixes(t *testing.T) {
	tb := NewRegistry(Bash{})
	r := Restrict(tb, Policy{
		DenyPrefixes: []string{"rm -rf", "curl "},
		Guidance:     "use safer alternatives or request an explicit allow",
	})
	_, err := r.Run(context.Background(), "bash", json.RawMessage(`{"command":"rm -rf /tmp/x"}`))
	if err == nil || !strings.Contains(err.Error(), "rm -rf") || !strings.Contains(err.Error(), "safer") {
		t.Fatalf("want prefix denial with guidance, got %v", err)
	}
	// Quote-aware prefix match.
	_, err = r.Run(context.Background(), "bash", json.RawMessage(`{"command":"rm -rf \"/tmp/foo bar\""}`))
	if err == nil || !strings.Contains(err.Error(), "rm -rf") {
		t.Fatalf("want quoted path denial, got %v", err)
	}
	// Non-matching command is allowed (may still fail for other reasons).
	out, err := r.Run(context.Background(), "bash", json.RawMessage(`{"command":"echo ok"}`))
	if err != nil || !strings.Contains(out, "ok") {
		t.Fatalf("echo = %q %v", out, err)
	}
}

func TestCheckActionDeny(t *testing.T) {
	tb := NewRegistry(Bash{})
	r := Restrict(tb, Policy{
		CheckAction: func(ctx context.Context, name string, input json.RawMessage) error {
			return fmt.Errorf("bash call denied by policy rule %q — use safer cleanup", "no-rm")
		},
	})
	_, err := r.Run(context.Background(), "bash", json.RawMessage(`{"command":"rm -rf /tmp/x"}`))
	if err == nil || !strings.Contains(err.Error(), "no-rm") || !strings.Contains(err.Error(), "safer") {
		t.Fatalf("want CheckAction denial with guidance, got %v", err)
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
