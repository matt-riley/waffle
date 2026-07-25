package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
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

// TestDockerSandboxMCPExecution is the gated #77 integration proof. Run with:
// WAFFLE_TEST_DOCKER=1 go test ./internal/mcp -run TestDockerSandboxMCPExecution -count=1 -v
func TestDockerSandboxMCPExecution(t *testing.T) {
	if os.Getenv("WAFFLE_TEST_DOCKER") != "1" {
		t.Skip("set WAFFLE_TEST_DOCKER=1 to run Docker MCP sandbox integration")
	}
	server := Server{
		Name:    "gated",
		Command: "sh",
		Args:    []string{"-c", `while IFS= read -r line; do id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p'); case "$line" in *'"initialize"'*) printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2024-11-05","capabilities":{}}}\n' "$id";; *'"tools/list"'*) printf '{"jsonrpc":"2.0","id":%s,"result":{"tools":[]}}\n' "$id";; esac; done`},
	}
	planned, opts := PlanLaunch(server, "sandbox", "docker", "", "alpine:3.20", "none")
	client, err := ConnectRestricted(context.Background(), planned, opts)
	if err != nil {
		t.Fatalf("sandboxed MCP connect: %v", err)
	}
	defer func() { _ = client.Close() }()
	box, err := client.Toolbox(context.Background())
	if err != nil {
		t.Fatalf("sandboxed MCP tools/list: %v", err)
	}
	if len(box.Defs()) != 0 {
		t.Fatalf("unexpected tools: %v", box.Defs())
	}
}

// TestDockerCloseRemovesContainer is the gated #97 proof: after Close on a
// docker-wrapped MCP client, docker inspect must not find the named container.
// Run with: WAFFLE_TEST_DOCKER=1 go test ./internal/mcp -run TestDockerCloseRemovesContainer -count=1 -v
func TestDockerCloseRemovesContainer(t *testing.T) {
	if os.Getenv("WAFFLE_TEST_DOCKER") != "1" {
		t.Skip("set WAFFLE_TEST_DOCKER=1 to run Docker MCP container cleanup integration")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH")
	}

	server := Server{
		Name:    "close-cleanup",
		Command: "sh",
		Args: []string{"-c", `while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  case "$line" in
    *'"initialize"'*) printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2024-11-05","capabilities":{}}}\n' "$id";;
    *'"tools/list"'*) printf '{"jsonrpc":"2.0","id":%s,"result":{"tools":[]}}\n' "$id";;
  esac
done`},
	}
	planned, opts := PlanLaunch(server, "sandbox", "docker", "", "alpine:3.20", "none")
	if planned.DockerContainer == "" {
		t.Fatal("PlanLaunch sandbox+docker must set DockerContainer name")
	}
	name := planned.DockerContainer

	client, err := ConnectRestricted(context.Background(), planned, opts)
	if err != nil {
		t.Fatalf("ConnectRestricted: %v", err)
	}
	if client.containerName != name {
		t.Fatalf("client.containerName = %q, want %q", client.containerName, name)
	}

	// Container must exist while the session is live.
	if !dockerContainerExists(t, name) {
		t.Fatalf("container %q not running after connect (docker inspect failed)", name)
	}

	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// After Close, stop/rm must have removed it — inspect must fail.
	deadline := time.Now().Add(10 * time.Second)
	for dockerContainerExists(t, name) {
		if time.Now().After(deadline) {
			t.Fatalf("container %q still present after Close (docker inspect succeeded)", name)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// dockerContainerExists reports whether docker inspect finds name (any state).
func dockerContainerExists(t *testing.T, name string) bool {
	t.Helper()
	cmd := exec.Command("docker", "inspect", name)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return true
	}
	// Missing container: docker returns non-zero; surface other failures.
	msg := string(out) + err.Error()
	if strings.Contains(msg, "No such object") || strings.Contains(msg, "no such object") {
		return false
	}
	// Also accept empty-name / not found patterns from docker CLI variants.
	if strings.Contains(strings.ToLower(msg), "no such") {
		return false
	}
	t.Logf("docker inspect %s: %v\n%s", name, err, out)
	return false
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
			// Docker-wrapped launches must name the container for Close cleanup (#97).
			if got.Command == "docker" {
				if got.DockerContainer == "" {
					t.Fatalf("docker-wrapped Server.DockerContainer empty; args=%v", got.Args)
				}
				if !strings.HasPrefix(got.DockerContainer, "waffle-mcp-") {
					t.Fatalf("DockerContainer = %q, want waffle-mcp- prefix", got.DockerContainer)
				}
				if !strings.Contains(joined, "--name\x00"+got.DockerContainer) {
					t.Errorf("docker args missing --name %s; args=%v", got.DockerContainer, got.Args)
				}
			} else if got.DockerContainer != "" {
				t.Fatalf("non-docker Server.DockerContainer = %q, want empty", got.DockerContainer)
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

// TestWrapDockerNamesContainer asserts docker-wrapped MCP servers get a unique
// --name and keep --network none so Close can stop/rm the container (#97).
func TestWrapDockerNamesContainer(t *testing.T) {
	s := WrapDocker(Server{
		Name:    "x",
		Command: "my-mcp",
		Args:    []string{"--stdio"},
	}, DockerWrapOpts{Image: "img:1"})
	if s.Command != "docker" {
		t.Fatalf("Command = %q", s.Command)
	}
	if s.DockerContainer == "" {
		t.Fatal("DockerContainer empty")
	}
	if !strings.HasPrefix(s.DockerContainer, "waffle-mcp-") {
		t.Fatalf("DockerContainer = %q, want waffle-mcp- prefix", s.DockerContainer)
	}
	// Suffix is id.NewBytes(4) → 8 hex chars.
	suffix := strings.TrimPrefix(s.DockerContainer, "waffle-mcp-")
	if len(suffix) != 8 {
		t.Fatalf("name suffix %q length = %d, want 8 hex chars", suffix, len(suffix))
	}

	// --name <container> and --network none must both be present as tokens.
	var nameIdx, netIdx = -1, -1
	for i, a := range s.Args {
		switch a {
		case "--name":
			nameIdx = i
		case "--network":
			netIdx = i
		}
	}
	if nameIdx < 0 || nameIdx+1 >= len(s.Args) {
		t.Fatalf("missing --name in args: %v", s.Args)
	}
	if s.Args[nameIdx+1] != s.DockerContainer {
		t.Fatalf("--name value = %q, want DockerContainer %q", s.Args[nameIdx+1], s.DockerContainer)
	}
	if s.Args[nameIdx+1] == "" {
		t.Fatal("--name value empty")
	}
	if netIdx < 0 || netIdx+1 >= len(s.Args) || s.Args[netIdx+1] != "none" {
		t.Fatalf("want --network none in args: %v", s.Args)
	}
	// Still keep --rm for the normal docker lifecycle path.
	foundRM := false
	for _, a := range s.Args {
		if a == "--rm" {
			foundRM = true
			break
		}
	}
	if !foundRM {
		t.Fatalf("missing --rm in args: %v", s.Args)
	}

	// Two wraps get distinct names.
	s2 := WrapDocker(Server{Name: "y", Command: "other"}, DockerWrapOpts{})
	if s2.DockerContainer == s.DockerContainer {
		t.Fatalf("two WrapDocker calls produced same name %q", s.DockerContainer)
	}
}

// TestCloseStopsNamedContainer puts a fake docker binary on PATH that records
// stop/rm invocations. Client.Close must issue docker stop/rm for the named
// container before killing the local process (#97).
func TestCloseStopsNamedContainer(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash fake docker is unix-only")
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "docker.log")
	fakeDocker := filepath.Join(dir, "docker")
	// Record argv; succeed immediately. Used only for stop/rm during Close.
	script := fmt.Sprintf("#!/usr/bin/env bash\nprintf '%%s\\n' \"$*\" >> %q\nexit 0\n", logPath)
	if err := os.WriteFile(fakeDocker, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	const containerName = "waffle-mcp-testclose"
	path := writeFakeServer(t)
	ctx := context.Background()
	client, err := ConnectRestricted(ctx, Server{
		Name:            "fake",
		Command:         "bash",
		Args:            []string{path},
		DockerContainer: containerName,
	}, RestrictOpts{Mode: "sandbox"})
	if err != nil {
		t.Fatalf("ConnectRestricted: %v", err)
	}
	if client.containerName != containerName {
		t.Fatalf("containerName = %q, want %q", client.containerName, containerName)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read docker log: %v", err)
	}
	body := string(raw)
	wantStop := "stop -t 1 " + containerName
	wantRM := "rm -f " + containerName
	if !strings.Contains(body, wantStop) {
		t.Errorf("docker log missing %q:\n%s", wantStop, body)
	}
	if !strings.Contains(body, wantRM) {
		t.Errorf("docker log missing %q:\n%s", wantRM, body)
	}
}

func TestCloseContextPreservesCallerDeadlineForContainerCleanup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash fake docker is unix-only")
	}
	dir := t.TempDir()
	fakeDocker := filepath.Join(dir, "docker")
	// Single long sleep: CommandContext can SIGKILL one process cleanly.
	// A tight sleep loop leaves short-lived children and adds kill latency.
	script := "#!/usr/bin/env bash\nexec sleep 3600\n"
	if err := os.WriteFile(fakeDocker, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	path := writeFakeServer(t)
	client, err := ConnectRestricted(context.Background(), Server{
		Name:            "fake",
		Command:         "bash",
		Args:            []string{path},
		DockerContainer: "waffle-mcp-context",
	}, RestrictOpts{Mode: "sandbox"})
	if err != nil {
		t.Fatalf("ConnectRestricted: %v", err)
	}
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer closeCancel()
	started := time.Now()
	err = client.CloseContext(closeCtx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("CloseContext error = %v, want deadline exceeded", err)
	}
	// Caller deadline is 30ms; allow process-kill/teardown overhead under -race
	// and busy CI hosts, but stay far below Close()'s 8s default and the prior
	// fixed 5s+3s docker cleanup budgets that this regression guards against.
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("CloseContext replaced caller deadline: %v", elapsed)
	}
}

func TestCloseContextRetainsFailedContainerCleanupForRetry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash fake docker is unix-only")
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "docker.log")
	releasePath := filepath.Join(dir, "release")
	fakeDocker := filepath.Join(dir, "docker")
	script := `#!/usr/bin/env bash
printf '%s\n' "$*" >> "$WAFFLE_TEST_DOCKER_LOG"
if [ ! -f "$WAFFLE_TEST_DOCKER_RELEASE" ]; then
  echo "docker cleanup unavailable" >&2
  exit 1
fi
exit 0
`
	if err := os.WriteFile(fakeDocker, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("WAFFLE_TEST_DOCKER_LOG", logPath)
	t.Setenv("WAFFLE_TEST_DOCKER_RELEASE", releasePath)

	const containerName = "waffle-mcp-retry"
	path := writeFakeServer(t)
	client, err := ConnectRestricted(context.Background(), Server{
		Name:            "fake",
		Command:         "bash",
		Args:            []string{path},
		DockerContainer: containerName,
	}, RestrictOpts{Mode: "sandbox"})
	if err != nil {
		t.Fatalf("ConnectRestricted: %v", err)
	}
	t.Cleanup(func() {
		_ = os.WriteFile(releasePath, []byte("release"), 0o600)
		_ = client.Close()
	})

	if err := client.CloseContext(context.Background()); err == nil {
		t.Fatal("CloseContext succeeded after docker stop/rm failures")
	}
	if client.containerName != containerName {
		t.Fatalf("failed cleanup containerName = %q, want retained %q", client.containerName, containerName)
	}
	if err := os.WriteFile(releasePath, []byte("release"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := client.CloseContext(context.Background()); err != nil {
		t.Fatalf("CloseContext retry: %v", err)
	}
	if client.containerName != "" {
		t.Fatalf("successful retry retained containerName %q", client.containerName)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if stops := strings.Count(string(raw), "stop -t 1 "+containerName); stops != 2 {
		t.Fatalf("docker stop attempts = %d, want 2\n%s", stops, raw)
	}
	if removals := strings.Count(string(raw), "rm -f "+containerName); removals != 2 {
		t.Fatalf("docker rm attempts = %d, want 2\n%s", removals, raw)
	}
}

func TestCloseContextDoesNotReplayCompletedProcessErrorForever(t *testing.T) {
	path := writeFakeServer(t)
	client, err := Connect(context.Background(), Server{Name: "fake", Command: "bash", Args: []string{path}})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	closeErr := errors.New("stdin close warning")
	client.in = &errorWriteCloser{WriteCloser: client.in, err: closeErr}

	if err := client.CloseContext(context.Background()); !errors.Is(err, closeErr) {
		t.Fatalf("first CloseContext error = %v, want %v", err, closeErr)
	}
	if !client.processClosed {
		t.Fatal("first CloseContext did not finish local process teardown")
	}
	if err := client.CloseContext(context.Background()); err != nil {
		t.Fatalf("completed process error was replayed on retry: %v", err)
	}
}

// TestCloseWithoutContainerSkipsDocker ensures host MCP Close does not
// invoke docker when no container was named.
func TestCloseWithoutContainerSkipsDocker(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash fake server is unix-only")
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "docker.log")
	fakeDocker := filepath.Join(dir, "docker")
	script := fmt.Sprintf("#!/usr/bin/env bash\nprintf '%%s\\n' \"$*\" >> %q\nexit 0\n", logPath)
	if err := os.WriteFile(fakeDocker, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	path := writeFakeServer(t)
	client, err := Connect(context.Background(), Server{Name: "fake", Command: "bash", Args: []string{path}})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if client.containerName != "" {
		t.Fatalf("containerName = %q, want empty", client.containerName)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if raw, err := os.ReadFile(logPath); err == nil && len(raw) > 0 {
		t.Fatalf("host MCP Close must not invoke docker; log:\n%s", raw)
	}
}

type errorWriteCloser struct {
	io.WriteCloser
	err error
}

func (c *errorWriteCloser) Close() error {
	return errors.Join(c.WriteCloser.Close(), c.err)
}
