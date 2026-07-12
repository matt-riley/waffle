package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// A tiny MCP server written as a shell script: reads JSON-RPC lines,
// answers initialize / tools/list / tools/call. Enough to exercise the
// client without a dependency.
const fakeServer = `#!/usr/bin/env bash
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  case "$line" in
    *'"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2024-11-05","capabilities":{}}}\n' "$id" ;;
    *'"tools/list"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":"echo","description":"echo text","inputSchema":{"type":"object","properties":{"text":{"type":"string"}}}}]}}\n' "$id" ;;
    *'"tools/call"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"pong"}],"isError":false}}\n' "$id" ;;
    *'"notifications/initialized"'*) : ;;
  esac
done
`

func writeFakeServer(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("bash fake server is unix-only")
	}
	path := filepath.Join(t.TempDir(), "server.sh")
	if err := os.WriteFile(path, []byte(fakeServer), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConnectListAndCall(t *testing.T) {
	path := writeFakeServer(t)
	ctx := context.Background()

	client, err := Connect(ctx, Server{Name: "fake", Command: "bash", Args: []string{path}})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			t.Errorf("close client: %v", err)
		}
	}()

	tb, err := client.Toolbox(ctx)
	if err != nil {
		t.Fatalf("Toolbox: %v", err)
	}
	defs := tb.Defs()
	if len(defs) != 1 || defs[0].Name != "fake__echo" {
		t.Fatalf("defs = %+v", defs)
	}
	if defs[0].Description != "echo text" {
		t.Errorf("description = %q", defs[0].Description)
	}

	out, err := tb.Run(ctx, "fake__echo", json.RawMessage(`{"text":"ping"}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "pong" {
		t.Errorf("out = %q", out)
	}
}

// TestConcurrentCalls exercises the demux reader: many parallel tool calls
// on one connection must each get their own response, none dropped.
func TestConcurrentCalls(t *testing.T) {
	path := writeFakeServer(t)
	ctx := context.Background()
	client, err := Connect(ctx, Server{Name: "fake", Command: "bash", Args: []string{path}})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			t.Errorf("close client: %v", err)
		}
	}()
	tb, err := client.Toolbox(ctx)
	if err != nil {
		t.Fatalf("Toolbox: %v", err)
	}

	const n = 20
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			out, err := tb.Run(ctx, "fake__echo", json.RawMessage(`{"text":"ping"}`))
			if err == nil && out != "pong" {
				err = fmt.Errorf("got %q", out)
			}
			errs <- err
		}()
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent call %d: %v", i, err)
		}
	}
}

func TestRenderToolResult(t *testing.T) {
	ok := renderToolResult(json.RawMessage(`{"content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}`))
	if ok != "a\nb" {
		t.Errorf("ok = %q", ok)
	}
	errOut := renderToolResult(json.RawMessage(`{"content":[{"type":"text","text":"boom"}],"isError":true}`))
	if errOut != "error: boom" {
		t.Errorf("errOut = %q", errOut)
	}
}
