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

// TestExpandPlaceholders pins the exact §9.2 semantics: single-pass,
// non-recursive, exact-token matching.
func TestExpandPlaceholders(t *testing.T) {
	root := "/plugins/acme"
	data := "/plugins-data/acme"
	cases := []struct {
		in   string
		want string
	}{
		{"${PLUGIN_ROOT}", root},
		{"${PLUGIN_DATA}", data},
		{"${PLUGIN_ROOT}/config.json", root + "/config.json"},
		{"x${PLUGIN_ROOT}y", "x" + root + "y"},
		{"${PLUGIN_ROOT}${PLUGIN_DATA}", root + data},
		{"${PLUGIN_DATA}${PLUGIN_ROOT}${PLUGIN_DATA}", data + root + data},
		{"$PLUGIN_ROOT", "$PLUGIN_ROOT"},
		{"PLUGIN_ROOT", "PLUGIN_ROOT"},
		{"${PLUGIN_ROOTX}", "${PLUGIN_ROOTX}"},
		{"${PLUGIN_ROOT", "${PLUGIN_ROOT"},
		{"${FOO}", "${FOO}"},
		{"plain", "plain"},
		{"", ""},
		// Text introduced by a replacement is never re-scanned: root
		// containing the other placeholder stays literal.
		{"${PLUGIN_ROOT}", root},
	}
	for _, tc := range cases {
		got := ExpandPlaceholders(tc.in, root, data)
		if got != tc.want {
			t.Errorf("ExpandPlaceholders(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// Non-recursion: a root that itself contains ${PLUGIN_DATA} is inserted
	// verbatim, never re-expanded.
	craftedRoot := "/x/${PLUGIN_DATA}"
	if got := ExpandPlaceholders("${PLUGIN_ROOT}", craftedRoot, "/data"); got != craftedRoot {
		t.Errorf("non-recursive insertion violated: %q", got)
	}
}

// pluginEnvDumpServer writes its environment, args, and cwd to a file and
// answers the MCP handshake.
const pluginEnvDumpServer = `#!/usr/bin/env bash
if [[ -n "${PLUGIN_DUMP_FILE:-}" ]]; then
  {
    echo "ENV:"; env | sort
    echo "ARGS:"; for a in "$@"; do echo "  $a"; done
    echo "PWD: $(pwd)"
  } > "$PLUGIN_DUMP_FILE"
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

// TestPluginServerReceivesRootAndData launches a plugin-sourced stdio server
// and asserts the §9 contract end to end: PLUGIN_ROOT/PLUGIN_DATA in the
// child env, args/env/cwd placeholder expansion, and no ambient secrets.
func TestPluginServerReceivesRootAndData(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash helper is unix-only")
	}
	dir := t.TempDir()
	dump := filepath.Join(dir, "child.txt")
	path := filepath.Join(dir, "plugin-server.sh")
	if err := os.WriteFile(path, []byte(pluginEnvDumpServer), 0o755); err != nil {
		t.Fatal(err)
	}

	pluginRoot := t.TempDir()
	pluginData := filepath.Join(t.TempDir(), "data")
	t.Setenv("PLUGIN_DUMP_FILE", dump)
	t.Setenv("GITHUB_TOKEN", "ghp_should_not_leak")

	ctx := context.Background()
	client, err := ConnectRestricted(ctx, Server{
		Name:    "plugin-srv",
		Command: "bash",
		Args:    []string{path, "--config", "${PLUGIN_ROOT}/config.json"},
		Env:     []string{"PLUGIN_DUMP_FILE"},
		EnvVars: map[string]string{
			"CONFIG_PATH": "${PLUGIN_ROOT}/config.json",
			"STATE_DIR":   "${PLUGIN_DATA}/state",
		},
		Cwd:        "${PLUGIN_ROOT}",
		PluginRoot: pluginRoot,
		PluginData: pluginData,
	}, RestrictOpts{Mode: "restricted"})
	if err != nil {
		t.Fatalf("ConnectRestricted: %v", err)
	}
	defer func() { _ = client.Close() }()

	raw, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("read child dump: %v", err)
	}
	body := string(raw)
	for _, want := range []string{
		"PLUGIN_ROOT=" + pluginRoot,
		"PLUGIN_DATA=" + pluginData,
		"CONFIG_PATH=" + pluginRoot + "/config.json",
		"STATE_DIR=" + pluginData + "/state",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("child env missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "ghp_should_not_leak") {
		t.Errorf("ambient secret leaked:\n%s", body)
	}
	if !strings.Contains(body, pluginRoot+"/config.json") {
		t.Errorf("args not expanded:\n%s", body)
	}
	resolvedRoot, err := filepath.EvalSymlinks(pluginRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "PWD: "+resolvedRoot) {
		t.Errorf("cwd not the plugin root:\n%s", body)
	}
	info, err := os.Stat(pluginData)
	if err != nil {
		t.Fatalf("plugin data dir not created: %v", err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("plugin data mode = %o, want 0700", info.Mode().Perm())
	}
}

// TestPluginDataPersistsAcrossLaunches writes into PLUGIN_DATA from one
// launch and asserts a second launch sees it (spec §9.1: preserved across
// launches and plugin updates).
func TestPluginDataPersistsAcrossLaunches(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash helper is unix-only")
	}
	dir := t.TempDir()
	dump := filepath.Join(dir, "child.txt")
	path := filepath.Join(dir, "plugin-server.sh")
	if err := os.WriteFile(path, []byte(pluginEnvDumpServer), 0o755); err != nil {
		t.Fatal(err)
	}
	pluginRoot := t.TempDir()
	pluginData := filepath.Join(t.TempDir(), "data")
	t.Setenv("PLUGIN_DUMP_FILE", dump)

	launch := func() *Client {
		t.Helper()
		client, err := ConnectRestricted(context.Background(), Server{
			Name:       "plugin-srv",
			Command:    "bash",
			Args:       []string{path, "--state", "${PLUGIN_DATA}/state"},
			Env:        []string{"PLUGIN_DUMP_FILE"},
			EnvVars:    map[string]string{"STATE": "${PLUGIN_DATA}/state"},
			PluginRoot: pluginRoot,
			PluginData: pluginData,
		}, RestrictOpts{Mode: "restricted"})
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		return client
	}
	first := launch()
	if err := os.MkdirAll(filepath.Join(pluginData, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginData, "state", "marker"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second := launch()
	defer func() { _ = second.Close() }()
	if _, err := os.Stat(filepath.Join(pluginData, "state", "marker")); err != nil {
		t.Errorf("PLUGIN_DATA not preserved across launches: %v", err)
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
			"X-Tenant": {"public-tenant"},
			// A lowercase-only custom header must be canonicalized (and read
			// back canonically) rather than appearing as a second key.
			"x-custom-case":  {"lowercase-kept"},
			"User-Agent":     {"plugin-tries"},
			"Content-Type":   {"plugin-tries"},
			"Mcp-Session-Id": {"plugin-tries"},
			// Case variants of reserved headers must never survive (#400).
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
	if gotHeader.Get("X-Tenant") != "public-tenant" {
		t.Errorf("plugin X-Tenant missing: %v", gotHeader)
	}
	if gotHeader.Get("X-Custom-Case") != "lowercase-kept" {
		t.Errorf("lowercase-only plugin header not canonicalized: %v", gotHeader)
	}
	if len(gotHeader.Values("X-Tenant")) != 1 {
		t.Errorf("X-Tenant duplicated on the wire: %v", gotHeader.Values("X-Tenant"))
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
