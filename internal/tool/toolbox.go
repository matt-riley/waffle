package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/matt-riley/waffle/internal/llm"
)

// Toolbox is anything that can offer and execute tools: the in-process
// Registry, a sandboxed executor, or compositions of both. The agent loop
// only ever sees this interface.
type Toolbox interface {
	Defs() []llm.Tool
	Run(ctx context.Context, name string, input json.RawMessage) (string, error)
}

// Policy is an allow/deny tool filter. Policy is enforced host-side —
// before a call ever reaches a sandbox — so a compromised sandbox can't
// grant itself tools (docs/plan.md, "Tools").
type Policy struct {
	Allow []string // empty means everything not denied
	Deny  []string // wins over Allow
}

// Permits reports whether the policy allows the named tool.
func (p Policy) Permits(name string) bool {
	if slices.Contains(p.Deny, name) {
		return false
	}
	if len(p.Allow) > 0 && !slices.Contains(p.Allow, name) {
		return false
	}
	return true
}

// IsZero reports whether the policy restricts nothing.
func (p Policy) IsZero() bool { return len(p.Allow) == 0 && len(p.Deny) == 0 }

// Restrict applies a policy to a toolbox: denied tools disappear from Defs
// and refuse to Run.
func Restrict(tb Toolbox, p Policy) Toolbox {
	if p.IsZero() {
		return tb
	}
	return &restricted{tb: tb, policy: p}
}

type restricted struct {
	tb     Toolbox
	policy Policy
}

func (r *restricted) Defs() []llm.Tool {
	var defs []llm.Tool
	for _, d := range r.tb.Defs() {
		if r.policy.Permits(d.Name) {
			defs = append(defs, d)
		}
	}
	return defs
}

func (r *restricted) Run(ctx context.Context, name string, input json.RawMessage) (string, error) {
	if !r.policy.Permits(name) {
		return "", fmt.Errorf("tool %q is not permitted by policy", name)
	}
	return r.tb.Run(ctx, name, input)
}

// Combine merges toolboxes; the first box offering a name wins. Used to
// pair sandboxed builtins with host-side memory tools.
func Combine(boxes ...Toolbox) Toolbox {
	c := &combined{routes: map[string]Toolbox{}}
	for _, b := range boxes {
		for _, d := range b.Defs() {
			if _, taken := c.routes[d.Name]; taken {
				continue
			}
			c.routes[d.Name] = b
			c.defs = append(c.defs, d)
		}
	}
	return c
}

type combined struct {
	defs   []llm.Tool
	routes map[string]Toolbox
}

func (c *combined) Defs() []llm.Tool { return c.defs }

func (c *combined) Run(ctx context.Context, name string, input json.RawMessage) (string, error) {
	tb, ok := c.routes[name]
	if !ok {
		return "", fmt.Errorf("unknown tool %q", name)
	}
	return tb.Run(ctx, name, input)
}
