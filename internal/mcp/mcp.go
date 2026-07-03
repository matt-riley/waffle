// Package mcp is a minimal Model Context Protocol client over stdio
// (docs/plan.md, "Tools" — the long tail arrives via MCP). It speaks
// enough of the protocol to list a server's tools and call them, exposing
// each as a waffle tool.Toolbox. One dependency-free JSON-RPC client
// rather than an SDK: the surface waffle needs is small.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/tool"
)

// Server is one configured MCP server (a command run over stdio).
type Server struct {
	Name    string
	Command string
	Args    []string
}

// rpcRequest / rpcResponse are JSON-RPC 2.0 envelopes.
type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("mcp error %d: %s", e.Code, e.Message) }

// Client is a live connection to one MCP server. A single reader goroutine
// demultiplexes responses by request id, so concurrent calls (the agent
// dispatches tools in parallel) never race on the stdout stream.
type Client struct {
	name string
	cmd  *exec.Cmd
	in   io.WriteCloser
	out  *bufio.Reader

	writeMu sync.Mutex // serializes writes to stdin

	mu      sync.Mutex // guards nextID and pending
	nextID  int
	pending map[int]chan rpcResponse
	readErr error // set once when the reader loop exits
	closed  chan struct{}
}

// Connect launches the server process and performs the initialize
// handshake.
func Connect(ctx context.Context, s Server) (*Client, error) {
	cmd := exec.Command(s.Command, s.Args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("mcp %s: start %q: %w", s.Name, s.Command, err)
	}
	c := &Client{
		name:    s.Name,
		cmd:     cmd,
		in:      stdin,
		out:     bufio.NewReader(stdout),
		pending: map[int]chan rpcResponse{},
		closed:  make(chan struct{}),
	}
	go c.readLoop()

	if _, err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "waffle", "version": "0"},
	}); err != nil {
		_ = c.Close()
		return nil, err
	}
	if err := c.notify("notifications/initialized"); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

// Close terminates the server process.
func (c *Client) Close() error {
	_ = c.in.Close()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	return c.cmd.Wait()
}

// readLoop is the sole reader of the server's stdout. It routes each
// response to the channel registered under its id and, on stream error,
// fails every in-flight call.
func (c *Client) readLoop() {
	defer close(c.closed)
	for {
		line, err := c.out.ReadBytes('\n')
		if err != nil {
			c.mu.Lock()
			c.readErr = err
			for id, ch := range c.pending {
				close(ch)
				delete(c.pending, id)
			}
			c.mu.Unlock()
			return
		}
		trimmed := strings.TrimSpace(string(line))
		if trimmed == "" {
			continue
		}
		var resp rpcResponse
		if err := json.Unmarshal([]byte(trimmed), &resp); err != nil {
			continue // skip non-JSON log lines the server may emit
		}
		c.mu.Lock()
		ch, ok := c.pending[resp.ID]
		if ok {
			delete(c.pending, resp.ID)
		}
		c.mu.Unlock()
		if ok {
			ch <- resp
		}
	}
}

func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	if c.readErr != nil {
		c.mu.Unlock()
		return nil, c.readErr
	}
	c.nextID++
	id := c.nextID
	ch := make(chan rpcResponse, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	unregister := func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}

	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		unregister()
		return nil, err
	}
	c.writeMu.Lock()
	_, err = fmt.Fprintf(c.in, "%s\n", body)
	c.writeMu.Unlock()
	if err != nil {
		unregister()
		return nil, err
	}

	select {
	case <-ctx.Done():
		unregister()
		return nil, ctx.Err()
	case resp, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("mcp %s: connection closed: %w", c.name, c.readErr)
		}
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	}
}

func (c *Client) notify(method string) error {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method})
	c.writeMu.Lock()
	_, err := fmt.Fprintf(c.in, "%s\n", body)
	c.writeMu.Unlock()
	return err
}

type mcpTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// Toolbox lists the server's tools and returns a tool.Toolbox exposing
// them. Tool names are prefixed with the server name to avoid collisions.
func (c *Client) Toolbox(ctx context.Context) (tool.Toolbox, error) {
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
	tb := &toolbox{client: c, prefix: c.name + "__"}
	for _, t := range listed.Tools {
		tb.defs = append(tb.defs, llm.Tool{
			Name:        tb.prefix + t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		})
		tb.remote = append(tb.remote, t.Name)
	}
	return tb, nil
}

type toolbox struct {
	client *Client
	prefix string
	defs   []llm.Tool
	remote []string
}

func (t *toolbox) Defs() []llm.Tool { return t.defs }

func (t *toolbox) Run(ctx context.Context, name string, input json.RawMessage) (string, error) {
	remote := strings.TrimPrefix(name, t.prefix)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	raw, err := t.client.call(ctx, "tools/call", map[string]any{
		"name":      remote,
		"arguments": input,
	})
	if err != nil {
		return "", err
	}
	return renderToolResult(raw), nil
}

// renderToolResult flattens MCP's content-block result into text.
func renderToolResult(raw json.RawMessage) string {
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return string(raw)
	}
	var parts []string
	for _, c := range res.Content {
		if c.Text != "" {
			parts = append(parts, c.Text)
		}
	}
	out := strings.Join(parts, "\n")
	if res.IsError {
		return "error: " + out
	}
	return out
}
