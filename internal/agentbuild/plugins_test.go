package agentbuild

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/memory"
	"github.com/matt-riley/waffle/internal/plugin"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/store"
	"github.com/matt-riley/waffle/internal/tool"
)

// captureLog redirects slog to a text handler over a buffer so tests can
// assert skip reports carry the server name and reason attrs.
type captureLog struct {
	mu sync.Mutex
	b  strings.Builder
}

func (c *captureLog) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.b.String()
}

func (c *captureLog) contains(sub string) bool {
	return strings.Contains(c.String(), sub)
}

func newCaptureLog(t *testing.T) *captureLog {
	t.Helper()
	c := &captureLog{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&lockedWriter{c: c}, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return c
}

type lockedWriter struct{ c *captureLog }

func (w lockedWriter) Write(p []byte) (int, error) {
	w.c.mu.Lock()
	defer w.c.mu.Unlock()
	return w.c.b.Write(p)
}

// writeTestPlugin writes a plugin under <home>/plugins/<name>.
func writeTestPlugin(t *testing.T, home, name string, files map[string]string) {
	t.Helper()
	root := filepath.Join(home, "plugins", name)
	for rel, content := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if strings.HasSuffix(rel, "/") {
			if err := os.MkdirAll(path, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func newPluginTestBuilder(t *testing.T) *Builder {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := config.Default()
	cfg.Provider.APIKey = "test-key"
	cfg.Agent.Subagents = false
	cfg.Agent.Learn = false
	return &Builder{
		Config:    cfg,
		Sessions:  session.New(st),
		Workspace: memory.Workspace{Dir: t.TempDir()},
		Runtime:   &buildTestRuntime{},
	}
}

// TestBuildSkipsFailedPluginMCPServer: a plugin stdio server that dies
// during the handshake is skipped with a log line naming server and reason;
// the build succeeds with the remaining tools (§7.2.2 rule 5, §11.3).
func TestBuildSkipsFailedPluginMCPServer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WAFFLE_HOME", home)
	writeTestPlugin(t, home, "dying", map[string]string{
		"plugin.json": `{"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json","name":"dying"}`,
		"mcp.json":    `{"$schema":"https://agent-plugins.org/schemas/1.0.0/mcp.schema.json","mcpServers":{"boom":{"type":"stdio","command":"./server.sh"}}}`,
		"server.sh":   "#!/bin/sh\nexit 1\n",
	})
	capture := newCaptureLog(t)

	b := newPluginTestBuilder(t)
	agent, cleanup, err := b.Build(context.Background(), config.GroupMain, "")
	if err != nil {
		t.Fatalf("build with dying plugin server must succeed: %v", err)
	}
	defer func() { _ = cleanup.Stop() }()
	if agent == nil {
		t.Fatal("agent is nil")
	}
	if !capture.contains("plugin mcp server failed") {
		t.Errorf("no skip log recorded:\n%s", capture.String())
	}
	if !capture.contains("boom") {
		t.Errorf("skip log does not name the server:\n%s", capture.String())
	}
}

// TestBuildIndexesPluginSkills: a plugin's conforming skills are merged into
// the system prompt alongside workspace skills.
func TestBuildIndexesPluginSkills(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WAFFLE_HOME", home)
	writeTestPlugin(t, home, "skills-plugin", map[string]string{
		"plugin.json":            `{"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json","name":"skills-plugin"}`,
		"skills/deploy/SKILL.md": "---\nname: deploy\ndescription: Deploys a build.\n---\n\nBody.\n",
	})

	b := newPluginTestBuilder(t)
	agent, cleanup, err := b.Build(context.Background(), config.GroupMain, "")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	defer func() { _ = cleanup.Stop() }()
	if !strings.Contains(agent.System, "deploy") || !strings.Contains(agent.System, "Deploys a build") {
		t.Errorf("plugin skill not indexed in system prompt:\n%s", agent.System)
	}
}

// TestBuildRefusesPluginRemoteAndDockerStdio: portable plugin servers have
// no credentials/egress and host-executed binaries, so remote servers and
// docker-mode stdio are refused with a report, not connected.
func TestBuildRefusesPluginRemoteAndDockerStdio(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WAFFLE_HOME", home)
	writeTestPlugin(t, home, "remote", map[string]string{
		"plugin.json": `{"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json","name":"remote"}`,
		"mcp.json":    `{"$schema":"https://agent-plugins.org/schemas/1.0.0/mcp.schema.json","mcpServers":{"api":{"type":"streamable-http","url":"https://example.com/mcp"}}}`,
	})
	capture := newCaptureLog(t)

	b := newPluginTestBuilder(t)
	agent, cleanup, err := b.Build(context.Background(), config.GroupMain, "")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	defer func() { _ = cleanup.Stop() }()
	if agent == nil {
		t.Fatal("agent is nil")
	}
	if !capture.contains("plugin remote mcp server refused") {
		t.Errorf("remote plugin server not refused with report:\n%s", capture.String())
	}
}

// TestBuildExtensionSkillActivation: the waffle extension's per-skill status
// override is respected when indexing plugin skills — an extension-inactive
// skill is left out of the system prompt.
func TestBuildExtensionSkillActivation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WAFFLE_HOME", home)
	writeTestPlugin(t, home, "gated", map[string]string{
		"plugin.json":            `{"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json","name":"gated","extensions":{"dev.mattriley.waffle":{"skills":{"deploy":{"status":"inactive"},"lint":{"status":"active"}}}}}`,
		"skills/deploy/SKILL.md": "---\nname: deploy\ndescription: Deploys a build.\n---\n\nBody.\n",
		"skills/lint/SKILL.md":   "---\nname: lint\ndescription: Lints the code.\n---\n\nBody.\n",
	})

	b := newPluginTestBuilder(t)
	agent, cleanup, err := b.Build(context.Background(), config.GroupMain, "")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	defer func() { _ = cleanup.Stop() }()
	if strings.Contains(agent.System, "Deploys a build") {
		t.Errorf("extension-inactive plugin skill indexed:\n%s", agent.System)
	}
	if !strings.Contains(agent.System, "Lints the code") {
		t.Errorf("extension-active plugin skill missing:\n%s", agent.System)
	}
}

// TestExtensionMCPPolicyCannotBypassPosture: extension-declared egress still
// cannot bypass the #249 posture — a docker-mode group refuses egress=direct
// with a report, without dialing anything; a stdio server whose extension
// groups exclude the build's group is skipped.
func TestExtensionMCPPolicyCannotBypassPosture(t *testing.T) {
	capture := newCaptureLog(t)
	b := newPluginTestBuilder(t)
	result := plugin.LoadResult{
		Plugin: plugin.Plugin{Root: t.TempDir(), Manifest: plugin.Manifest{Name: "policy"}},
		MCP: plugin.MCPResult{Servers: []plugin.MCPServer{
			{Name: "api", Type: plugin.MCPTypeStreamableHTTP, URL: "https://example.com/mcp"},
		}},
	}
	var boxes []tool.Toolbox
	var closers []Cleanup
	var redactors []func(string) string
	// A plugin remote server without an explicit secret:// token is refused
	// outright — connectRemoteMCP would otherwise fall back to OAuth tokens
	// keyed by server name, which a malicious plugin could exfiltrate by
	// choosing a matching name and URL (#403 security review).
	b.wirePluginMCPServer(context.Background(), &boxes, &closers, &redactors, result, result.MCP.Servers[0],
		plugin.WaffleMCPPolicy{Egress: "broker"}, t.TempDir(), "docker", config.GroupMain)
	if len(boxes) != 0 {
		t.Fatalf("boxes = %d, want none", len(boxes))
	}
	if !capture.contains("require an explicit secret:// token") {
		t.Errorf("tokenless plugin remote server not refused:\n%s", capture.String())
	}
	if len(closers) != 0 {
		t.Errorf("closers = %d, want none (nothing connected)", len(closers))
	}

	// Even with a token, egress=direct in docker mode is refused by
	// connectRemoteMCP before any dial (#249 posture cannot be bypassed).
	capture2 := newCaptureLog(t)
	b.wirePluginMCPServer(context.Background(), &boxes, &closers, &redactors, result, result.MCP.Servers[0],
		plugin.WaffleMCPPolicy{Egress: "direct", Token: "secret://mcp/api/access-token"}, t.TempDir(), "docker", config.GroupMain)
	if !capture2.contains("egress=direct is refused") {
		t.Errorf("extension egress=direct with token not refused in docker mode:\n%s", capture2.String())
	}
}

// TestBuildNativeMCPFailureStillFails: native [[mcp]] servers keep fail-fast;
// a server that cannot start fails the whole build (regression for #393's
// plugin/native split).
func TestBuildNativeMCPFailureStillFails(t *testing.T) {
	b := newPluginTestBuilder(t)
	b.Config.MCP = []config.MCPServer{
		{Name: "broken-native", Command: "/nonexistent/waffle-binary"},
	}
	_, cleanup, err := b.Build(context.Background(), config.GroupMain, "")
	defer func() { _ = cleanup.Stop() }()
	if err == nil || !strings.Contains(err.Error(), "broken-native") {
		t.Errorf("native MCP failure error = %v, want error naming the server", err)
	}
}
