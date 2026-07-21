// Package mcp is a minimal Model Context Protocol client over stdio
// (docs/plan.md, "Tools" — the long tail arrives via MCP). It speaks
// enough of the protocol to list a server's tools and call them, exposing
// each as a waffle tool.Toolbox. One dependency-free JSON-RPC client
// rather than an SDK: the surface waffle needs is small.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/matt-riley/waffle/internal/id"
	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/tool"
)

// Server is one configured MCP server (a command run over stdio).
type Server struct {
	Name    string
	Command string
	Args    []string
	Env     []string // allowlisted parent environment variable names
	// DockerContainer is the docker --name set by WrapDocker. ConnectRestricted
	// copies it onto Client so Close can docker stop/rm the container (#97).
	DockerContainer string
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
	name          string
	containerName string // docker --name when sandbox-wrapped; cleaned up on Close (#97)
	cmd           *exec.Cmd
	cancel        context.CancelFunc // kills the child process; tied to client lifetime, not the handshake ctx
	in            io.WriteCloser
	out           *bufio.Reader

	writeMu sync.Mutex // serializes writes to stdin

	mu      sync.Mutex // guards nextID and pending
	nextID  int
	pending map[int]chan rpcResponse
	readErr error // set once when the reader loop exits
}

// BuildProcessEnv constructs the restricted environment for an MCP child
// process (#79 / #77). Only PATH (from the host) and explicitly allowlisted
// variable names are included — never a copy of os.Environ(). Secret-bearing
// ambient vars (WAFFLE_HOME, tokens, age identity, …) are excluded unless
// named in allowlist (and codeintel config rejects secret-like names entirely).
func BuildProcessEnv(allowlist []string) []string {
	env := make([]string, 0, len(allowlist)+1)
	if path, ok := os.LookupEnv("PATH"); ok {
		env = append(env, "PATH="+path)
	}
	for _, name := range allowlist {
		if name == "" || name == "PATH" {
			continue
		}
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return env
}

// RestrictOpts configures #77-compliant isolation for an MCP child process.
type RestrictOpts struct {
	// Dir is the working directory for the child (workspace root when known).
	Dir string
	// Mode is "restricted" (default) or "sandbox" for audit/docs.
	Mode string
}

// DockerWrapOpts configures wrapping an MCP server as
// `docker run -i --rm --network none` with only allowlisted env (#77 / #79).
type DockerWrapOpts struct {
	// Image is the container image (default: debian:stable-slim).
	Image string
	// Network is the docker network mode (default: "none").
	Network string
	// WorkDir is the host path mounted at /work (optional).
	WorkDir string
}

// WrapDocker transforms s into a docker-run invocation that executes the
// original command inside a network-restricted container. Environment is
// only BuildProcessEnv(s.Env) as docker -e name=value pairs — never ambient
// host secrets. The returned Server.Env is empty so ConnectRestricted only
// passes PATH to the docker client process itself.
//
// The container is given a unique --name (waffle-mcp-<suffix>) so Client.Close
// can stop/rm it if killing the docker CLI leaves the container orphaned (#97).
func WrapDocker(s Server, opts DockerWrapOpts) Server {
	if opts.Image == "" {
		opts.Image = "debian:stable-slim"
	}
	if opts.Network == "" {
		opts.Network = "none"
	}
	suffix, err := id.NewBytes(4)
	if err != nil {
		// crypto/rand is effectively always available; fall back so naming
		// (and Close cleanup) still works if it is not.
		suffix = fmt.Sprintf("%x", time.Now().UnixNano())
	}
	name := "waffle-mcp-" + suffix
	args := []string{"run", "-i", "--rm", "--name", name, "--network", opts.Network}
	if opts.WorkDir != "" {
		args = append(args, "-v", opts.WorkDir+":/work", "-w", "/work")
	}
	for _, e := range BuildProcessEnv(s.Env) {
		args = append(args, "-e", e)
	}
	args = append(args, opts.Image, s.Command)
	args = append(args, s.Args...)
	return Server{
		Name:            s.Name,
		Command:         "docker",
		Args:            args,
		Env:             nil,
		DockerContainer: name,
	}
}

// PlanLaunch decides the restricted launch form for an MCP server (#77 / #79).
//
//   - execution "sandbox" + agentMode "docker" → docker-wrapped command, Mode=sandbox
//   - execution "sandbox" + host agent mode    → ConnectRestricted with Dir=workDir, Mode=restricted
//   - execution "host" (or empty)              → ConnectRestricted without dir, Mode=restricted
//
// The returned Server is what should be passed to ConnectRestricted; env is
// always BuildProcessEnv-only at process start.
func PlanLaunch(s Server, execution, agentMode, workDir, image, network string) (Server, RestrictOpts) {
	if execution == "" {
		execution = "host"
	}
	opts := RestrictOpts{Mode: "restricted"}
	out := s
	if execution == "sandbox" {
		if agentMode == "docker" {
			out = WrapDocker(s, DockerWrapOpts{
				Image:   image,
				Network: network,
				WorkDir: workDir,
			})
			opts.Mode = "sandbox"
			return out, opts
		}
		opts.Dir = workDir
		opts.Mode = "restricted"
	}
	return out, opts
}

// Connect launches the server process and performs the initialize
// handshake. Equivalent to ConnectRestricted with empty Dir — the child
// receives only BuildProcessEnv(s.Env), never ambient secrets (#79 / #77).
func Connect(ctx context.Context, s Server) (*Client, error) {
	return ConnectRestricted(ctx, s, RestrictOpts{})
}

// ConnectRestricted launches the MCP server with #77-compliant isolation:
//   - Environment is ONLY BuildProcessEnv(s.Env) — never os.Environ()
//   - Working directory set to opts.Dir when non-empty
//   - Extra file descriptors are not inherited (os/exec default; no ExtraFiles)
//
// Same handshake as Connect. opts.Mode defaults to "restricted" (audit label).
func ConnectRestricted(ctx context.Context, s Server, opts RestrictOpts) (*Client, error) {
	if opts.Mode == "" {
		opts.Mode = "restricted"
	}
	// procCtx lives until Close, not until the caller's ctx ends: the caller's
	// ctx only bounds the handshake, but the child must still be killable
	// afterward (and must die if Close is invoked).
	procCtx, procCancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(procCtx, s.Command, s.Args...)
	cmd.Env = BuildProcessEnv(s.Env)
	if opts.Dir != "" {
		cmd.Dir = opts.Dir
	}
	// os/exec does not pass ExtraFiles, so only stdin/stdout/stderr are
	// inherited — ambient gateway FDs/secrets cannot leak via open descriptors.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		procCancel()
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		procCancel()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		procCancel()
		return nil, fmt.Errorf("mcp %s: start %q (mode=%s): %w", s.Name, s.Command, opts.Mode, err)
	}
	c := &Client{
		name:          s.Name,
		containerName: s.DockerContainer,
		cmd:           cmd,
		cancel:        procCancel,
		in:            stdin,
		out:           bufio.NewReader(stdout),
		pending:       map[int]chan rpcResponse{},
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

// Close terminates the server process. When the server was docker-wrapped
// (containerName set), it first stops and force-removes the named container
// so killing only the local docker CLI cannot leave an orphaned container
// running (#97). Stop/rm use short timeouts so Close returns promptly.
func (c *Client) Close() error {
	if c.containerName != "" {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = exec.CommandContext(stopCtx, "docker", "stop", "-t", "1", c.containerName).Run()
		cancel()
		rmCtx, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
		_ = exec.CommandContext(rmCtx, "docker", "rm", "-f", c.containerName).Run()
		cancel2()
	}

	var first error
	if err := c.in.Close(); err != nil {
		first = err
	}
	// Cancelling procCtx kills the child even if Process.Kill below would be
	// skipped (e.g. nil Process in edge cases) and releases cmd's resources.
	if c.cancel != nil {
		c.cancel()
	}
	if c.cmd.Process != nil {
		if err := c.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			if first == nil {
				first = err
			}
		}
	}
	if err := c.cmd.Wait(); err != nil {
		// Kill makes Wait report *exec.ExitError (e.g. "signal: killed"); with
		// CommandContext it may instead wrap context.Canceled. Both are the
		// expected outcome of an intentional shutdown, not a Close failure.
		var ee *exec.ExitError
		if !errors.As(err, &ee) && !errors.Is(err, context.Canceled) && first == nil {
			first = err
		}
	}
	return first
}

// readLoop is the sole reader of the server's stdout. It routes each
// response to the channel registered under its id and, on stream error,
// fails every in-flight call.
func (c *Client) readLoop() {
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
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			continue
		}
		var resp rpcResponse
		if err := json.Unmarshal(trimmed, &resp); err != nil {
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
		return nil, c.connClosed()
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
			return nil, c.connClosed()
		}
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	}
}

// connClosed wraps the reader-loop's exit error uniformly, whether it is
// detected up front or when a pending channel is closed mid-call. readErr
// is written under mu by readLoop, so read it under mu here.
func (c *Client) connClosed() error {
	c.mu.Lock()
	err := c.readErr
	c.mu.Unlock()
	return fmt.Errorf("mcp %s: connection closed: %w", c.name, err)
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
	}
	return tb, nil
}

type toolbox struct {
	client *Client
	prefix string
	defs   []llm.Tool
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
