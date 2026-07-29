// Package tool defines waffle's native tools and their registry. Native Go
// tools cover the basics; the long tail arrives via MCP in a later phase
// (docs/plan.md, "Tools").
//
// Action-level bash denials (DenyPrefixes / Policy.CheckAction) use
// quote-aware token matching. Shell indirection (eval, variables, $(),
// aliases) is not expanded — prefix policy is not high-assurance isolation
// (#66); combine with sandboxing.
package tool

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/textcut"
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

// BuiltinOptions configures the standard host toolset.
type BuiltinOptions struct {
	// FetchAllowPrivate lists CIDRs / host:port entries fetch may reach in
	// otherwise-protected address ranges.
	FetchAllowPrivate []string
	// FileRoots confines the file tools to a set of directory trees (#269).
	// The zero value imposes no boundary.
	FileRoots FileRoots
}

// Builtins returns the standard host toolset.
func Builtins() *Registry {
	return BuiltinsWith(BuiltinOptions{})
}

// BuiltinsWithFetch returns the standard toolset with fetch policy applied.
func BuiltinsWithFetch(allowPrivate []string) *Registry {
	return BuiltinsWith(BuiltinOptions{FetchAllowPrivate: allowPrivate})
}

// BuiltinsWith returns the standard toolset under opts.
func BuiltinsWith(opts BuiltinOptions) *Registry {
	return NewRegistry(
		Bash{},
		ReadFile{Roots: opts.FileRoots},
		WriteFile{Roots: opts.FileRoots},
		EditFile{Roots: opts.FileRoots},
		Fetch{AllowPrivate: opts.FetchAllowPrivate},
		Search{Roots: opts.FileRoots},
	)
}

// OutputLimit is the maximum size (bytes) for tool output presented to the
// model after Agent.runOne spills (when configured) and truncates. Sandbox
// outbound rows also use this limit. Truncation keeps head+tail so the
// interesting parts are preserved.
const OutputLimit = 48 * 1024

// HostReturnCap is the maximum bytes a host-executed builtin may return so
// Agent.runOne can spill full content before truncating to OutputLimit.
// Matches spill.SpillCap (512KiB); avoids unbounded memory while keeping
// enough payload for mid-run expand_output / FTS (#69).
const HostReturnCap = 512 * 1024

// CapHostReturn bounds tool output to HostReturnCap without applying
// OutputLimit head+tail truncation (that happens in Agent.runOne after spill).
// Exported for out-of-package transports that return tool output to the agent
// (MCP, #286) so they hit the same boundary as the host builtins. The cut
// lands on a UTF-8 rune boundary (#107).
func CapHostReturn(s string) string {
	if len(s) <= HostReturnCap {
		return s
	}
	return textcut.Cut(s, HostReturnCap)
}

// Truncate caps tool output so a chatty command can't blow out the context
// window or bloat the queue DB; it keeps the head and tail, which is where
// the signal usually is. Head and tail cuts land on UTF-8 rune boundaries so
// the result is always valid UTF-8 with len <= limit (#107).
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
			return textcut.Cut(s, limit)
		}
		nextHead := available / 2
		nextTail := available - nextHead
		if nextHead == head && nextTail == tail {
			break
		}
		head, tail = nextHead, nextTail
	}
	// Snap both ends down to rune boundaries; lengths only shrink, so
	// len(head)+len(marker)+len(tail) stays <= limit.
	return textcut.Cut(s, head) + marker + textcut.CutSuffix(s, tail)
}
