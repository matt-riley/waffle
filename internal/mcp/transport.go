// Transport is the JSON-RPC framing boundary behind which stdio (the
// default) and streamable HTTP (#249) both sit. The protocol surface
// (initialize, tools/list, tools/call) is defined once; each transport owns
// framing, session state, and teardown.
package mcp

import (
	"context"
	"encoding/json"

	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/tool"
)

// Transport is the JSON-RPC framing boundary between the MCP protocol layer
// and one server connection. StdioTransport (a command over pipes, the
// default) and HTTPTransport (MCP streamable HTTP, #249) implement it.
//
// Implementations must be safe for concurrent use: the agent dispatches
// tools in parallel, so Call may be invoked concurrently on one transport.
type Transport interface {
	// Call performs a JSON-RPC request/response round trip. params may be
	// nil. The returned raw message is the response's result member.
	Call(ctx context.Context, method string, params any) (json.RawMessage, error)
	// Notify sends a JSON-RPC notification (no response expected).
	Notify(ctx context.Context, method string) error
	// Close terminates the connection exactly once. Callers should not use
	// the transport afterwards.
	Close() error
}

// rpcClient is the connection surface the toolbox layer needs. Both
// *Client (stdio) and *HTTPClient satisfy it; keeping it unexported keeps
// the transport boundary honest without widening the package API.
type rpcClient interface {
	call(ctx context.Context, method string, params any) (json.RawMessage, error)
	notify(ctx context.Context, method string) error
	Close() error
}

// newToolbox lists a connection's tools and wraps them as a tool.Toolbox.
// Tool names are prefixed with the server name to avoid collisions. Shared
// by the stdio and HTTP clients so the protocol surface stays identical
// across transports (#249).
func newToolbox(c rpcClient, ctx context.Context, name string) (tool.Toolbox, error) {
	raw, err := c.call(ctx, "tools/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var listed struct {
		Tools []mcpTool `json:"tools"`
	}
	if err := json.Unmarshal(raw, &listed); err != nil {
		return nil, err
	}
	tb := &toolbox{client: c, prefix: name + "__"}
	for _, t := range listed.Tools {
		tb.defs = append(tb.defs, llm.Tool{
			Name:        tb.prefix + t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		})
	}
	return tb, nil
}
