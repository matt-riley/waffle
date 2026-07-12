package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

// TestBuildProcessEnvNoAmbientSecrets asserts Connect/BuildProcessEnv never
// copies ambient process env (WAFFLE_HOME, tokens, age identity) into the child (#79).
func TestBuildProcessEnvNoAmbientSecrets(t *testing.T) {
	t.Setenv("WAFFLE_HOME", "/secret/waffle-home")
	t.Setenv("WAFFLE_AGE_IDENTITY", "AGE-SECRET-KEY-leak")
	t.Setenv("GITHUB_TOKEN", "ghp_should_not_leak")
	t.Setenv("SAFE_VAR", "ok-value")

	env := BuildProcessEnv([]string{"SAFE_VAR"})
	joined := fmt.Sprintf("%q", env)
	for _, leak := range []string{"WAFFLE_HOME", "WAFFLE_AGE_IDENTITY", "GITHUB_TOKEN", "/secret/waffle-home", "AGE-SECRET", "ghp_should"} {
		if strings.Contains(joined, leak) {
			t.Fatalf("ambient secret %q leaked into child env: %v", leak, env)
		}
	}
	foundSafe := false
	for _, e := range env {
		if e == "SAFE_VAR=ok-value" {
			foundSafe = true
		}
		if strings.HasPrefix(e, "PATH=") {
			continue
		}
		if e != "SAFE_VAR=ok-value" {
			t.Fatalf("unexpected env entry %q (only allowlist + PATH permitted)", e)
		}
	}
	if !foundSafe {
		t.Fatalf("allowlisted SAFE_VAR missing: %v", env)
	}

	// Empty allowlist: only PATH (if set).
	env2 := BuildProcessEnv(nil)
	for _, e := range env2 {
		if !strings.HasPrefix(e, "PATH=") {
			t.Fatalf("empty allowlist must not pass %q", e)
		}
	}
}

// envDumpServer is a helper MCP process that writes its environment to
// $ENV_DUMP_FILE before speaking the fake protocol. Used to prove
// ConnectRestricted never inherits ambient gateway secrets (#77 / #79).
const envDumpServer = `#!/usr/bin/env bash
if [[ -n "${ENV_DUMP_FILE:-}" ]]; then
  env | sort > "$ENV_DUMP_FILE"
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

// TestConnectRestrictedNoAmbientSecrets launches a real child via
// ConnectRestricted and asserts WAFFLE_HOME / GITHUB_TOKEN / age identity
// are absent from the child environment even when set on the parent.
func TestConnectRestrictedNoAmbientSecrets(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash helper is unix-only")
	}
	dir := t.TempDir()
	dump := filepath.Join(dir, "child.env")
	path := filepath.Join(dir, "env-server.sh")
	if err := os.WriteFile(path, []byte(envDumpServer), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("WAFFLE_HOME", "/secret/waffle-home")
	t.Setenv("WAFFLE_AGE_IDENTITY", "AGE-SECRET-KEY-leak")
	t.Setenv("GITHUB_TOKEN", "ghp_should_not_leak")
	t.Setenv("SAFE_VAR", "ok-value")
	t.Setenv("ENV_DUMP_FILE", dump)

	ctx := context.Background()
	client, err := ConnectRestricted(ctx, Server{
		Name:    "envdump",
		Command: "bash",
		Args:    []string{path},
		Env:     []string{"SAFE_VAR", "ENV_DUMP_FILE"},
	}, RestrictOpts{Dir: dir, Mode: "restricted"})
	if err != nil {
		t.Fatalf("ConnectRestricted: %v", err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	}()

	raw, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("read child env dump: %v", err)
	}
	body := string(raw)
	for _, leak := range []string{
		"WAFFLE_HOME=",
		"WAFFLE_AGE_IDENTITY=",
		"GITHUB_TOKEN=",
		"/secret/waffle-home",
		"AGE-SECRET-KEY-leak",
		"ghp_should_not_leak",
	} {
		if strings.Contains(body, leak) {
			t.Fatalf("child inherited ambient secret material %q:\n%s", leak, body)
		}
	}
	if !strings.Contains(body, "SAFE_VAR=ok-value") {
		t.Fatalf("allowlisted SAFE_VAR missing from child env:\n%s", body)
	}
	if !strings.Contains(body, "ENV_DUMP_FILE=") {
		t.Fatalf("allowlisted ENV_DUMP_FILE missing from child env:\n%s", body)
	}
}

// TestSandboxMCPUsesRestrictedExecutor is table-driven construction of the
// launch command for sandbox vs host agent modes (#77 / #79).
func TestSandboxMCPUsesRestrictedExecutor(t *testing.T) {
	base := Server{
		Name:    "codeintel",
		Command: "gopls",
		Args:    []string{"mcp"},
		Env:     []string{"SAFE_VAR"},
	}
	t.Setenv("SAFE_VAR", "ok")
	t.Setenv("GITHUB_TOKEN", "must-not-appear-in-docker-e")

	cases := []struct {
		name       string
		execution  string
		agentMode  string
		workDir    string
		image      string
		network    string
		wantCmd    string
		wantMode   string
		wantDir    string
		wantSubstr []string // must appear in joined Args
		denySubstr []string // must not appear
	}{
		{
			name:      "sandbox+docker wraps docker run",
			execution: "sandbox",
			agentMode: "docker",
			workDir:   "/ws/root",
			image:     "waffle-sb:test",
			network:   "none",
			wantCmd:   "docker",
			wantMode:  "sandbox",
			wantDir:   "",
			wantSubstr: []string{
				"run", "-i", "--rm",
				"--network", "none",
				"-v", "/ws/root:/work", "-w", "/work",
				"waffle-sb:test", "gopls", "mcp",
				"-e", "SAFE_VAR=ok",
			},
			denySubstr: []string{"GITHUB_TOKEN", "must-not-appear"},
		},
		{
			name:       "sandbox+host uses restricted dir",
			execution:  "sandbox",
			agentMode:  "host",
			workDir:    "/ws/host",
			wantCmd:    "gopls",
			wantMode:   "restricted",
			wantDir:    "/ws/host",
			wantSubstr: []string{"mcp"},
		},
		{
			name:      "host execution stays restricted env path",
			execution: "host",
			agentMode: "docker",
			workDir:   "/ignored",
			wantCmd:   "gopls",
			wantMode:  "restricted",
			wantDir:   "",
		},
		{
			name:      "empty execution defaults host-restricted",
			execution: "",
			agentMode: "host",
			wantCmd:   "gopls",
			wantMode:  "restricted",
		},
		{
			name:      "sandbox docker default image and network",
			execution: "sandbox",
			agentMode: "docker",
			wantCmd:   "docker",
			wantMode:  "sandbox",
			wantSubstr: []string{
				"--network", "none",
				"debian:stable-slim", "gopls",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, opts := PlanLaunch(base, tc.execution, tc.agentMode, tc.workDir, tc.image, tc.network)
			if got.Command != tc.wantCmd {
				t.Fatalf("Command = %q, want %q (args=%v)", got.Command, tc.wantCmd, got.Args)
			}
			if opts.Mode != tc.wantMode {
				t.Fatalf("Mode = %q, want %q", opts.Mode, tc.wantMode)
			}
			if opts.Dir != tc.wantDir {
				t.Fatalf("Dir = %q, want %q", opts.Dir, tc.wantDir)
			}
			joined := strings.Join(append([]string{got.Command}, got.Args...), "\x00")
			for _, sub := range tc.wantSubstr {
				if !strings.Contains(joined, sub) {
					t.Errorf("launch missing %q; args=%v", sub, got.Args)
				}
			}
			for _, sub := range tc.denySubstr {
				if strings.Contains(joined, sub) {
					t.Errorf("launch must not contain %q; args=%v", sub, got.Args)
				}
			}
			// Docker wrap must not leave original allowlist on the client Server.Env
			// (env is baked into -e flags); host path keeps allowlist for BuildProcessEnv.
			if got.Command == "docker" && len(got.Env) != 0 {
				t.Fatalf("docker-wrapped Server.Env = %v, want nil/empty", got.Env)
			}
		})
	}
}

func TestWrapDockerOnlyAllowlistedEnv(t *testing.T) {
	t.Setenv("SAFE_VAR", "yes")
	t.Setenv("WAFFLE_HOME", "/nope")
	t.Setenv("GITHUB_TOKEN", "nope")
	s := WrapDocker(Server{
		Name:    "x",
		Command: "my-mcp",
		Args:    []string{"--stdio"},
		Env:     []string{"SAFE_VAR"},
	}, DockerWrapOpts{Image: "img:1", WorkDir: "/w"})
	if s.Command != "docker" {
		t.Fatalf("Command = %q", s.Command)
	}
	joined := strings.Join(s.Args, " ")
	if !strings.Contains(joined, "-e SAFE_VAR=yes") {
		t.Fatalf("missing allowlisted -e: %v", s.Args)
	}
	if strings.Contains(joined, "WAFFLE_HOME") || strings.Contains(joined, "GITHUB_TOKEN") || strings.Contains(joined, "/nope") {
		t.Fatalf("secret leaked into docker args: %v", s.Args)
	}
}
