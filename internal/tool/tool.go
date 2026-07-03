// Package tool defines waffle's native tools and their registry. Native Go
// tools cover the basics; the long tail arrives via MCP in a later phase
// (docs/plan.md, "Tools").
package tool

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/matt-riley/waffle/internal/llm"
)

// Tool is one callable capability.
//
// Tools are independent by contract: Run must be safe to call concurrently
// with other tool invocations (or the same tool), must not assume sequential
// execution or exclusive access to shared resources, and must respect
// context cancellation for cooperative cancellation. A tool that blocks
// indefinitely is subject to outer timeouts and bounded execution pools
// (regular tools via toolSem, subagents via subagentSem in agent package).
// SubagentTool is a special case with its own depth guard.
type Tool interface {
	Def() llm.Tool
	Run(ctx context.Context, input json.RawMessage) (string, error)
}

// Registry holds the tools available to a session, in a stable order.
type Registry struct {
	order  []Tool
	byName map[string]Tool
}

// NewRegistry builds a registry; duplicate names panic (programmer error).
func NewRegistry(tools ...Tool) *Registry {
	r := &Registry{byName: make(map[string]Tool, len(tools))}
	for _, t := range tools {
		name := t.Def().Name
		if _, dup := r.byName[name]; dup {
			panic(fmt.Sprintf("tool: duplicate tool %q", name))
		}
		r.byName[name] = t
		r.order = append(r.order, t)
	}
	return r
}

// Defs returns the tool definitions in registration order.
func (r *Registry) Defs() []llm.Tool {
	defs := make([]llm.Tool, len(r.order))
	for i, t := range r.order {
		defs[i] = t.Def()
	}
	return defs
}

// Run executes the named tool. An unknown name is an error result, not a
// crash — the model sees it and can correct itself.
func (r *Registry) Run(ctx context.Context, name string, input json.RawMessage) (string, error) {
	t, ok := r.byName[name]
	if !ok {
		return "", fmt.Errorf("unknown tool %q", name)
	}
	return t.Run(ctx, input)
}

// Builtins returns the standard host toolset.
func Builtins() *Registry {
	return NewRegistry(Bash{}, ReadFile{}, WriteFile{}, EditFile{}, Fetch{})
}

// OutputLimit is the maximum size (bytes) for tool output returned to the
// caller or written into the sandbox queue outbound row. Truncation keeps
// head+tail so the interesting parts are preserved.
const OutputLimit = 48 * 1024

// Truncate caps tool output so a chatty command can't blow out the context
// window or bloat the queue DB; it keeps the head and tail, which is where
// the signal usually is.
func Truncate(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(s) <= limit {
		return s
	}
	marker := ""
	head, tail := 0, 0
	for {
		marker = fmt.Sprintf("\n... [%d bytes truncated] ...\n", len(s)-head-tail)
		available := limit - len(marker)
		if available <= 0 {
			return s[:limit]
		}
		nextHead := available / 2
		nextTail := available - nextHead
		if nextHead == head && nextTail == tail {
			break
		}
		head, tail = nextHead, nextTail
	}
	return s[:head] + marker + s[len(s)-tail:]
}
