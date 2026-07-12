package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/matt-riley/waffle/internal/llm"
)

// Toolbox is anything that can offer and execute tools: the in-process
// Registry, a sandboxed executor, or compositions of both. The agent loop
// only ever sees this interface.
type Toolbox interface {
	Defs() []llm.Tool
	Run(ctx context.Context, name string, input json.RawMessage) (string, error)
}

// CallerToolbox optionally accepts the provider's stable tool-call identity.
// Toolboxes that do not implement it retain the legacy Run behavior.
type CallerToolbox interface {
	Toolbox
	RunWithID(context.Context, string, string, json.RawMessage) (string, error)
}

// Policy is an allow/deny tool filter. Policy is enforced host-side —
// before a call ever reaches a sandbox — so a compromised sandbox can't
// grant itself tools (docs/plan.md, "Tools").
type Policy struct {
	Allow []string // empty means everything not denied
	Deny  []string // wins over Allow
	// DenyPrefixes denies bash (or other shell) commands whose text starts
	// with any of these prefixes after optional leading whitespace (#66).
	// Denial messages include Guidance when set.
	DenyPrefixes []string
	// Guidance is appended to action-level denial messages.
	Guidance string
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
func (p Policy) IsZero() bool {
	return len(p.Allow) == 0 && len(p.Deny) == 0 && len(p.DenyPrefixes) == 0
}

// Restrict applies a policy to a toolbox: denied tools disappear from Defs
// and refuse to Run. Action-level DenyPrefixes are enforced at Run time for
// bash only (tool remains listed).
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
	if err := r.policy.checkCommand(name, input); err != nil {
		return "", err
	}
	return r.tb.Run(ctx, name, input)
}

func (r *restricted) RunWithID(ctx context.Context, id, name string, input json.RawMessage) (string, error) {
	if !r.policy.Permits(name) {
		return "", fmt.Errorf("tool %q is not permitted by policy", name)
	}
	if err := r.policy.checkCommand(name, input); err != nil {
		return "", err
	}
	if tb, ok := r.tb.(CallerToolbox); ok {
		return tb.RunWithID(ctx, id, name, input)
	}
	return r.tb.Run(ctx, name, input)
}

// checkCommand enforces DenyPrefixes for bash (#66).
func (p Policy) checkCommand(name string, input json.RawMessage) error {
	if name != "bash" || len(p.DenyPrefixes) == 0 {
		return nil
	}
	var in struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return nil // let the tool report bad input
	}
	cmd := strings.TrimSpace(in.Command)
	for _, pref := range p.DenyPrefixes {
		pref = strings.TrimSpace(pref)
		if pref == "" {
			continue
		}
		if strings.HasPrefix(cmd, pref) {
			msg := fmt.Sprintf("bash command denied by policy: prefix %q is not allowed", pref)
			if p.Guidance != "" {
				msg += " — " + p.Guidance
			}
			return fmt.Errorf("%s", msg)
		}
	}
	return nil
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

func (c *combined) RunWithID(ctx context.Context, id, name string, input json.RawMessage) (string, error) {
	tb, ok := c.routes[name]
	if !ok {
		return "", fmt.Errorf("unknown tool %q", name)
	}
	if caller, ok := tb.(CallerToolbox); ok {
		return caller.RunWithID(ctx, id, name, input)
	}
	return tb.Run(ctx, name, input)
}
