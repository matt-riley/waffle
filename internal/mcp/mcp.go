// Package mcp is a minimal Model Context Protocol client (docs/plan.md,
// "Tools" — the long tail arrives via MCP). It speaks enough of the
// protocol to list a server's tools and call them, exposing each as a
// waffle tool.Toolbox. Two transports sit behind one JSON-RPC surface:
// stdio (a command over pipes, the default) and streamable HTTP for
// remote servers (#249). One dependency-free client rather than an SDK:
// the surface waffle needs is small.
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

// MaxFrameBytes bounds one physical NDJSON line read from a server's stdout.
// MCP servers are untrusted child processes, and bufio grows a single buffer
// until it sees a newline, so an unbounded read is a process-level DoS against
// the host (#286). Sized well above a legitimate tool result (which is capped
// at tool.HostReturnCap before the agent ever sees it) so only a pathological
// or hostile server trips it.
const MaxFrameBytes = 8 << 20

// ErrFrameTooLarge reports a server line exceeding MaxFrameBytes. It is fatal
// to the connection: the stream cannot be resynchronised without reading (and
// therefore buffering) the rest of the oversized line.
var ErrFrameTooLarge = errors.New("mcp: server line exceeds max frame size")

// Server is one configured MCP server: either a local command run over
// stdio (the default), or a remote streamable-HTTP endpoint (#249). Exactly
// one of Command or URL is set; config validation enforces the contract
// before this struct is ever built from user input.
type Server struct {
	Name    string
	Command string
	Args    []string
	Env     []string // allowlisted parent environment variable names
	// EnvVars are explicit name→value environment pairs overlaid on the
	// BuildProcessEnv base at launch. They come from portable plugin
	// mcp.json env objects (internal/pluginmcp); native [[mcp]] config does
	// not set them. Values replace same-name allowlisted entries (POSIX
	// last-wins); PLUGIN_ROOT/PLUGIN_DATA are reserved by the Agent Plugins
	// spec and added after this overlay (#392).
	EnvVars map[string]string
	// Cwd is the child working directory, used when RestrictOpts.Dir is
	// empty. Portable plugin servers default it to the plugin root;
	// native [[mcp]] servers leave it empty (caller supplies opts.Dir).
	Cwd string
	// PluginRoot, when non-empty, marks this server as plugin-sourced
	// (Agent Plugins §9): at launch, args, env values, and cwd undergo
	// ${PLUGIN_ROOT}/${PLUGIN_DATA} expansion, and the child receives
	// PLUGIN_ROOT/PLUGIN_DATA after the configured env overlay (the
	// client's values always win). Native [[mcp]] servers leave it empty
	// and keep verbatim behavior.
	PluginRoot string
	// PluginData is the client-managed per-plugin writable data directory
	// (spec §9.1): created 0700 before launch, preserved across plugin
	// updates. Empty when the server is not plugin-sourced.
	PluginData string
	// URL is a remote MCP streamable HTTP endpoint. Mutually exclusive with
	// Command. Remote servers have no process to restrict; their network
	// posture (broker egress vs direct) and credential handling are decided
	// by the caller at connect time (#249).
	URL string
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

	closeMu       sync.Mutex
	processClosed bool
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

// ExpandPlaceholders performs the Agent Plugins §9.2 expansion on s:
// every exact occurrence of ${PLUGIN_ROOT} and ${PLUGIN_DATA} is replaced
// once, in a single left-to-right pass, and text introduced by a
// replacement is never scanned again (non-recursive). Unrecognized
// placeholder-like text — $PLUGIN_ROOT, ${PLUGIN_ROOTX}, ${FOO}, an
// unclosed ${PLUGIN_ROOT — stays literal.
func ExpandPlaceholders(s, root, data string) string {
	const (
		rootToken = "${PLUGIN_ROOT}"
		dataToken = "${PLUGIN_DATA}"
	)
	var b strings.Builder
	for {
		rootAt := strings.Index(s, rootToken)
		dataAt := strings.Index(s, dataToken)
		if rootAt < 0 && dataAt < 0 {
			b.WriteString(s)
			return b.String()
		}
		if dataAt < 0 || rootAt >= 0 && rootAt < dataAt {
			b.WriteString(s[:rootAt])
			b.WriteString(root)
			s = s[rootAt+len(rootToken):]
		} else {
			b.WriteString(s[:dataAt])
			b.WriteString(data)
			s = s[dataAt+len(dataToken):]
		}
	}
}

// buildChildEnv assembles the child environment per the #79/#77 posture
// and the Agent Plugins §9 runtime contract: the BuildProcessEnv
// allowlisted base, the explicit EnvVars overlay (expanded for
// plugin-sourced servers), and then — plugin-sourced only — the reserved
// PLUGIN_ROOT/PLUGIN_DATA variables appended last so the client's values
// always win over any same-name entry (spec §9.1).
// buildChildEnv assembles the child environment per the #79/#77 posture
// and the Agent Plugins §9 runtime contract: the BuildProcessEnv
// allowlisted base, the explicit EnvVars overlay (expanded for
// plugin-sourced servers), and then — plugin-sourced only — the reserved
// PLUGIN_ROOT/PLUGIN_DATA variables appended last so the client's values
// always win over any same-name entry (spec §9.1). Same-name allowlisted
// entries are replaced, never duplicated (#400).
func (s Server) buildChildEnv() []string {
	env := BuildProcessEnv(s.Env)
	overlay := make(map[string]string, len(s.EnvVars))
	for name, value := range s.EnvVars {
		if s.PluginRoot != "" {
			value = ExpandPlaceholders(value, s.PluginRoot, s.PluginData)
		}
		overlay[name] = value
	}
	env = overlayEnv(env, overlay)
	if s.PluginRoot != "" {
		env = append(env, "PLUGIN_ROOT="+s.PluginRoot, "PLUGIN_DATA="+s.PluginData)
	}
	return env
}

// overlayEnv replaces same-name entries in base with the explicit name→value
// pairs, deduplicating so no NAME= entry appears twice (duplicate entries
// have unspecified precedence across platforms). Unknown names are appended.
func overlayEnv(base []string, pairs map[string]string) []string {
	if len(pairs) == 0 {
		return base
	}
	out := make([]string, 0, len(base)+len(pairs))
	seen := make(map[string]bool, len(base)+len(pairs))
	for _, item := range base {
		name, _, _ := strings.Cut(item, "=")
		if _, replaced := pairs[name]; replaced {
			continue // dropped; the explicit pair below wins
		}
		seen[name] = true
		out = append(out, item)
	}
	for name, value := range pairs {
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name+"="+value)
	}
	return out
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
	// The container receives the same restricted child environment the host
	// path would: allowlisted base + EnvVars overlay + reserved plugin vars.
	for _, e := range s.buildChildEnv() {
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
//   - Environment is buildChildEnv(s) — the BuildProcessEnv allowlisted
//     base plus explicit EnvVars, never os.Environ()
//   - Working directory set to opts.Dir when non-empty, else s.Cwd
//     (expanded for plugin-sourced servers)
//   - Extra file descriptors are not inherited (os/exec default; no ExtraFiles)
//
// Same handshake as Connect. opts.Mode defaults to "restricted" (audit label).
func ConnectRestricted(ctx context.Context, s Server, opts RestrictOpts) (*Client, error) {
	if opts.Mode == "" {
		opts.Mode = "restricted"
	}
	// Plugin-sourced servers (#392): ensure the client-managed PLUGIN_DATA
	// directory exists (0700, writable) before launch (spec §9.1), and
	// expand ${PLUGIN_ROOT}/${PLUGIN_DATA} in args, env values, and cwd
	// (§9.2). command and env keys are never expanded.
	if s.PluginRoot != "" && s.PluginData != "" {
		if err := os.MkdirAll(s.PluginData, 0o700); err != nil {
			return nil, fmt.Errorf("mcp %s: create plugin data dir: %w", s.Name, err)
		}
		if err := os.Chmod(s.PluginData, 0o700); err != nil {
			return nil, fmt.Errorf("mcp %s: secure plugin data dir: %w", s.Name, err)
		}
	}
	// procCtx lives until Close, not until the caller's ctx ends: the caller's
	// ctx only bounds the handshake, but the child must still be killable
	// afterward (and must die if Close is invoked).
	procCtx, procCancel := context.WithCancel(context.Background())
	args := s.Args
	dir := s.Cwd
	if s.PluginRoot != "" {
		args = make([]string, len(s.Args))
		for i, a := range s.Args {
			args[i] = ExpandPlaceholders(a, s.PluginRoot, s.PluginData)
		}
		dir = ExpandPlaceholders(s.Cwd, s.PluginRoot, s.PluginData)
	}
	cmd := exec.CommandContext(procCtx, s.Command, args...)
	cmd.Env = s.buildChildEnv()
	if opts.Dir != "" {
		cmd.Dir = opts.Dir
	} else if dir != "" {
		cmd.Dir = dir
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
	if err := c.notify(ctx, "notifications/initialized"); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

// Close terminates the server process. When the server was docker-wrapped
// (containerName set), it first stops and force-removes the named container
// so killing only the local docker CLI cannot leave an orphaned container
// running (#97). The compatibility wrapper retains the prior bounded default.
func (c *Client) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	return c.CloseContext(ctx)
}

// CloseContext terminates the server process and any wrapper container under
// the caller's deadline. Timed-out container cleanup stays retryable while the
// local process teardown remains exact-once.
func (c *Client) CloseContext(ctx context.Context) error {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()

	var containerErr error
	if c.containerName != "" {
		stopErr := runDockerCleanup(ctx, "stop", "-t", "1", c.containerName)
		removeErr := runDockerCleanup(ctx, "rm", "-f", c.containerName)
		containerErr = errors.Join(stopErr, removeErr)
		if removeErr == nil {
			c.containerName = ""
		}
	}
	if c.processClosed {
		return containerErr
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
	c.processClosed = true
	return errors.Join(containerErr, first)
}

func runDockerCleanup(ctx context.Context, args ...string) error {
	// Fail fast when the caller's deadline is already exhausted so sequential
	// stop/rm cleanup cannot pay another process spawn under a spent context.
	if err := ctx.Err(); err != nil {
		return err
	}
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err == nil || strings.Contains(string(out), "No such container:") {
		return nil
	}
	return fmt.Errorf("mcp: docker %s: %w\n%s", args[0], err, strings.TrimSpace(string(out)))
}

// readLoop is the sole reader of the server's stdout. It routes each
// response to the channel registered under its id and, on stream error,
// fails every in-flight call.
func (c *Client) readLoop() {
	for {
		line, err := c.readFrame()
		if err != nil {
			c.failPending(err)
			if errors.Is(err, ErrFrameTooLarge) {
				// The stream cannot be resynchronised, and the child is
				// producing unbounded output: stop reading it entirely.
				_ = c.Close()
			}
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

// failPending records the reader-loop's exit error and fails every in-flight
// call so no caller waits on a stream nobody is reading any more.
func (c *Client) failPending(err error) {
	c.mu.Lock()
	c.readErr = err
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	c.mu.Unlock()
}

// readFrame reads one newline-terminated line, refusing to buffer more than
// MaxFrameBytes of payload (the terminator itself does not count). ReadSlice
// keeps the reader's own buffer fixed, so the only growth is the accumulator
// this function bounds itself; the returned slice is valid until the next
// read, which is all readLoop needs.
func (c *Client) readFrame() ([]byte, error) {
	var buf []byte
	for {
		chunk, err := c.out.ReadSlice('\n')
		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			if len(buf)+len(chunk) > MaxFrameBytes {
				return nil, ErrFrameTooLarge
			}
			buf = append(buf, chunk...)
		case err != nil:
			return nil, err
		default:
			// A complete line: the trailing newline is not payload.
			if len(buf)+len(chunk)-1 > MaxFrameBytes {
				return nil, ErrFrameTooLarge
			}
			if buf == nil {
				return chunk, nil
			}
			return append(buf, chunk...), nil
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

func (c *Client) notify(ctx context.Context, method string) error {
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
	return newToolbox(c, ctx, c.name)
}

type toolbox struct {
	client rpcClient
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
	// Cap at the same boundary as the host builtins (#286): MCP servers are
	// untrusted children, and Agent.runOne spills before truncating to
	// OutputLimit, so a plain cap (not head+tail Truncate) is what belongs here.
	return tool.CapHostReturn(renderToolResult(raw)), nil
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
