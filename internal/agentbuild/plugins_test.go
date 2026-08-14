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
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/store"
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
