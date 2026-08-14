package mcp

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// cwdEchoServer answers the MCP handshake and prints its working directory
// to a file, so a test can assert the child cwd.
const cwdEchoServer = `#!/usr/bin/env bash
if [[ -n "${CWD_DUMP_FILE:-}" ]]; then
  pwd > "$CWD_DUMP_FILE"
fi
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  case "$line" in
    *'"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2024-11-05","capabilities":{}}}\n' "$id" ;;
    *'"tools/list"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"tools":[]}}\n' "$id" ;;
    *'"notifications/initialized"'*) : ;;
  esac
done
`

// TestConnectEnvVarsOverlay verifies the portable-plugin env-object overlay:
// explicit name→value pairs reach the child, same-name allowlisted entries
// are replaced (POSIX last-wins), and ambient secrets still never leak.
func TestConnectEnvVarsOverlay(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash helper is unix-only")
	}
	dir := t.TempDir()
	dump := filepath.Join(dir, "child.env")
	path := filepath.Join(dir, "env-server.sh")
	if err := os.WriteFile(path, []byte(envDumpServer), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GITHUB_TOKEN", "ghp_should_not_leak")
	t.Setenv("SAFE_VAR", "host-value")
	t.Setenv("OVERRIDDEN", "host-value")
	t.Setenv("ENV_DUMP_FILE", dump)

	ctx := context.Background()
	client, err := ConnectRestricted(ctx, Server{
		Name:    "portable",
		Command: "bash",
		Args:    []string{path},
		Env:     []string{"SAFE_VAR", "ENV_DUMP_FILE"},
		EnvVars: map[string]string{
			"OVERRIDDEN":  "plugin-value",
			"PLUGIN_ONLY": "from-plugin",
		},
	}, RestrictOpts{Dir: dir, Mode: "restricted"})
	if err != nil {
		t.Fatalf("ConnectRestricted: %v", err)
	}
	defer func() { _ = client.Close() }()

	raw, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("read child env dump: %v", err)
	}
	body := string(raw)
	if strings.Contains(body, "ghp_should_not_leak") {
		t.Fatalf("child inherited ambient secret:\n%s", body)
	}
	if !strings.Contains(body, "OVERRIDDEN=plugin-value") {
		t.Errorf("EnvVars overlay missing (want plugin-value to win):\n%s", body)
	}
	if strings.Contains(body, "OVERRIDDEN=host-value") {
		// Duplicate NAME= entries have unspecified precedence; the overlay
		// must replace the allowlisted entry, not append beside it (#400).
		t.Errorf("allowlisted OVERRIDDEN=host-value survived the overlay:\n%s", body)
	}
	if !strings.Contains(body, "PLUGIN_ONLY=from-plugin") {
		t.Errorf("EnvVars pair missing:\n%s", body)
	}
	if !strings.Contains(body, "SAFE_VAR=host-value") {
		t.Errorf("allowlisted base var missing:\n%s", body)
	}
}

// TestConnectCwd verifies per-server Cwd is applied when RestrictOpts.Dir is
// empty, and that a caller-supplied Dir still wins.
func TestConnectCwd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash helper is unix-only")
	}
	dir := t.TempDir()
	childDir := t.TempDir()
	dump := filepath.Join(dir, "cwd.txt")
	path := filepath.Join(dir, "cwd-server.sh")
	if err := os.WriteFile(path, []byte(cwdEchoServer), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CWD_DUMP_FILE", dump)

	ctx := context.Background()
	client, err := ConnectRestricted(ctx, Server{
		Name:    "cwd",
		Command: "bash",
		Args:    []string{path},
		Env:     []string{"CWD_DUMP_FILE"},
		Cwd:     childDir,
	}, RestrictOpts{Mode: "restricted"})
	if err != nil {
		t.Fatalf("ConnectRestricted: %v", err)
	}
	defer func() { _ = client.Close() }()

	raw, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("read cwd dump: %v", err)
	}
	got := strings.TrimSpace(string(raw))
	resolvedChild, err := filepath.EvalSymlinks(childDir)
	if err != nil {
		t.Fatal(err)
	}
	if got != resolvedChild {
		t.Errorf("child cwd = %q, want %q", got, resolvedChild)
	}
}

// TestHTTPHeadersApplied verifies portable-plugin fixed headers reach the
// server and that client-generated headers win on name collision.
func TestHTTPHeadersApplied(t *testing.T) {
	var gotHeader http.Header
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotHeader = r.Header.Clone()
		mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		id := 1
		if bytes.Contains(body, []byte("initialize")) {
			id = 0
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + fmt.Sprint(id) + `,"result":{"protocolVersion":"2025-03-26","capabilities":{}}}`))
	}))
	defer srv.Close()

	opts := HTTPOpts{
		Headers: http.Header{
			"X-Tenant":       {"public-tenant"},
			"X-Custom":       {"kept"},
			"User-Agent":     {"plugin-tries"},
			"Content-Type":   {"plugin-tries"},
			"Mcp-Session-Id": {"plugin-tries"},
			// Case variants must be canonicalized and never survive as a
			// second wire header (#400).
			"x-tenant":       {"lowercase-dup"},
			"mcp-session-id": {"lowercase-spoof"},
			"authorization":  {"bearer spoof"},
		},
	}
	client, err := ConnectHTTP(context.Background(), "remote", srv.URL, opts)
	if err != nil {
		t.Fatalf("ConnectHTTP: %v", err)
	}
	defer func() { _ = client.Close() }()
	if _, err := client.Call(context.Background(), "tools/list", nil); err != nil {
		t.Fatalf("Call: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotHeader.Get("X-Tenant") != "public-tenant" || gotHeader.Get("X-Custom") != "kept" {
		t.Errorf("plugin headers missing: %v", gotHeader)
	}
	if len(gotHeader.Values("X-Tenant")) != 1 {
		t.Errorf("case-variant X-Tenant duplicated on the wire: %v", gotHeader.Values("X-Tenant"))
	}
	if gotHeader.Get("Authorization") != "" {
		t.Errorf("plugin Authorization header survived (case-insensitive strip failed): %v", gotHeader)
	}
	if gotHeader.Get("User-Agent") != "waffle-mcp/0" {
		t.Errorf("client User-Agent should win, got %q", gotHeader.Get("User-Agent"))
	}
	if gotHeader.Get("Content-Type") != "application/json" {
		t.Errorf("client Content-Type should win, got %q", gotHeader.Get("Content-Type"))
	}
	if gotHeader.Get("Mcp-Session-Id") != "" {
		t.Errorf("plugin Mcp-Session-Id should never be honored, got %q", gotHeader.Get("Mcp-Session-Id"))
	}
}
