package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/policy"
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

// PolicyDenial carries safe, structured audit metadata for a denied tool
// call. Message is returned to the model; profile, source, and rule are
// configuration identifiers and must never contain tool input.
type PolicyDenial struct {
	Profile string
	Source  string
	Rule    string
	Message string
}

func (d *PolicyDenial) Error() string {
	msg := d.Message
	if msg == "" {
		msg = "tool call denied by policy"
	}
	if d.Profile != "" {
		msg += fmt.Sprintf(" (profile %q)", d.Profile)
	}
	if d.Source != "" {
		msg += fmt.Sprintf(" [policy source %q", d.Source)
		if d.Rule != "" {
			msg += fmt.Sprintf("; rule %q", d.Rule)
		}
		msg += "]"
	}
	return msg
}

// NewPolicyDenial constructs a denial with audit-safe policy provenance.
func NewPolicyDenial(profile, source, rule, message string) error {
	return &PolicyDenial{Profile: profile, Source: source, Rule: rule, Message: message}
}

// PolicyDenialDetails extracts structured policy provenance from err.
func PolicyDenialDetails(err error) (profile, source, rule string, ok bool) {
	var denial *PolicyDenial
	if !errors.As(err, &denial) {
		return "", "", "", false
	}
	return denial.Profile, denial.Source, denial.Rule, true
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
	// Profile is the named agent profile enforcing this policy (#71).
	// Included in tool-policy denial messages when set.
	Profile string
	// CheckAction, when set, runs after tool allow/deny and DenyPrefixes
	// for finer-grained [[policy.rule]] evaluation (#66). Return a non-nil
	// error to deny the call (message is shown to the model).
	CheckAction func(ctx context.Context, name string, input json.RawMessage) error
	// ObserveSuccess, when set, runs after a successful tool Run (#66).
	// Used to record write/predicate events for action=require rules.
	ObserveSuccess func(ctx context.Context, name string, input json.RawMessage)
}

// Permits reports whether the policy allows the named tool.
// Deny always wins over Allow. Allow entry "*" means all tools not denied.
func (p Policy) Permits(name string) bool {
	if slices.Contains(p.Deny, name) {
		return false
	}
	if len(p.Allow) == 0 {
		return true
	}
	if slices.Contains(p.Allow, "*") {
		return true
	}
	return slices.Contains(p.Allow, name)
}

// IsZero reports whether the policy restricts nothing.
// Profile alone does not make a policy non-zero (it only annotates denials).
func (p Policy) IsZero() bool {
	return len(p.Allow) == 0 && len(p.Deny) == 0 && len(p.DenyPrefixes) == 0 &&
		p.CheckAction == nil && p.ObserveSuccess == nil
}

// Restrict applies a policy to a toolbox: denied tools disappear from Defs
// and refuse to Run. Action-level DenyPrefixes are enforced at Run time for
// bash only (tool remains listed).
func Restrict(tb Toolbox, p Policy) Toolbox {
	if p.IsZero() {
		return tb
	}
	r := &restricted{tb: tb, policy: p}
	for _, d := range tb.Defs() {
		if p.Permits(d.Name) {
			r.defs = append(r.defs, d)
		}
	}
	return r
}

type restricted struct {
	tb     Toolbox
	policy Policy
	// defs is computed once in Restrict since policy never changes for the
	// lifetime of a restricted toolbox.
	defs []llm.Tool
}

func (r *restricted) Defs() []llm.Tool { return r.defs }

// checkPolicy runs the shared allow/deny + action checks common to Run and
// RunWithID. Returns a non-nil error if the call should be denied.
func (r *restricted) checkPolicy(ctx context.Context, name string, input json.RawMessage) error {
	if !r.policy.Permits(name) {
		return r.policy.denyTool(name)
	}
	if err := r.policy.checkCommand(name, input); err != nil {
		return err
	}
	if r.policy.CheckAction != nil {
		if err := r.policy.CheckAction(ctx, name, input); err != nil {
			return err
		}
	}
	return nil
}

// observe runs ObserveSuccess after a successful dispatch, if set.
func (r *restricted) observe(ctx context.Context, name string, input json.RawMessage, err error) {
	if err == nil && r.policy.ObserveSuccess != nil {
		r.policy.ObserveSuccess(ctx, name, input)
	}
}

func (r *restricted) Run(ctx context.Context, name string, input json.RawMessage) (string, error) {
	if err := r.checkPolicy(ctx, name, input); err != nil {
		return "", err
	}
	out, err := r.tb.Run(ctx, name, input)
	r.observe(ctx, name, input, err)
	return out, err
}

func (r *restricted) RunWithID(ctx context.Context, id, name string, input json.RawMessage) (string, error) {
	if err := r.checkPolicy(ctx, name, input); err != nil {
		return "", err
	}
	var (
		out string
		err error
	)
	if tb, ok := r.tb.(CallerToolbox); ok {
		out, err = tb.RunWithID(ctx, id, name, input)
	} else {
		out, err = r.tb.Run(ctx, name, input)
	}
	r.observe(ctx, name, input, err)
	return out, err
}

// denyTool formats a tool-policy denial, including profile when set (#71).
func (p Policy) denyTool(name string) error {
	msg := fmt.Sprintf("tool %q is not permitted by policy", name)
	if p.Guidance != "" {
		msg += " — " + p.Guidance
	}
	rule := "allow"
	if slices.Contains(p.Deny, name) {
		rule = "deny"
	}
	return NewPolicyDenial(p.Profile, "tool_policy", rule, msg)
}

// checkCommand enforces DenyPrefixes for bash (#66).
// Matching uses quote-aware token split so `rm -rf "/tmp/x"` matches prefix
// `rm -rf`. Shell indirection (eval, variables, $(), aliases) is not
// expanded — do not rely on prefix policy alone for high-assurance isolation.
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
		if policy.MatchBashPrefix(cmd, pref) {
			msg := fmt.Sprintf("bash command denied by policy: prefix %q is not allowed", pref)
			if p.Guidance != "" {
				msg += " — " + p.Guidance
			}
			return NewPolicyDenial(p.Profile, "tool_policy", "deny_prefix", msg)
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
