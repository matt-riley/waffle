package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mcpServersBody(t *testing.T, servers any) string {
	t.Helper()
	encoded, err := json.Marshal(servers)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf(`{"$schema": %q, "mcpServers": %s}`, MCPSchemaID, encoded)
}

func writeMCP(t *testing.T, root, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "mcp.json"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func loadMCP(t *testing.T, root, content string) MCPResult {
	t.Helper()
	writeMCP(t, root, content)
	result, err := LoadMCP(root, "1.0.0")
	if err != nil {
		t.Fatalf("LoadMCP: %v", err)
	}
	return result
}

func TestLoadMCPParsesAllVariants(t *testing.T) {
	root := t.TempDir()
	result := loadMCP(t, root, mcpServersBody(t, map[string]any{
		"local": map[string]any{
			"type":    "stdio",
			"command": "./bin/server",
			"args":    []string{"--data", "${PLUGIN_DATA}/x"},
			"env":     map[string]any{"CONFIG": "${PLUGIN_ROOT}/config.json"},
			"cwd":     "${PLUGIN_ROOT}",
		},
		"remote": map[string]any{
			"type": "streamable-http",
			"url":  "https://deploy.example.com/mcp",
			"headers": map[string]any{
				"X-Tenant": "public-tenant",
			},
		},
		"loopback": map[string]any{
			"type": "streamable-http",
			"url":  "http://localhost:8080/mcp",
		},
	}))
	if result.Disabled != "" {
		t.Fatalf("Disabled = %q, want empty", result.Disabled)
	}
	if len(result.Skips) != 0 {
		t.Fatalf("Skips = %v, want none (sse is unsupported, these are all supported)", result.Skips)
	}
	if len(result.Servers) != 3 {
		t.Fatalf("Servers = %d, want 3", len(result.Servers))
	}
	byName := map[string]MCPServer{}
	for _, s := range result.Servers {
		byName[s.Name] = s
	}
	local := byName["local"]
	if local.Type != MCPTypeStdio || local.Command != "./bin/server" ||
		len(local.Args) != 2 || local.Env["CONFIG"] != "${PLUGIN_ROOT}/config.json" ||
		local.Cwd != "${PLUGIN_ROOT}" {
		t.Errorf("local = %+v", local)
	}
	remote := byName["remote"]
	if remote.Type != MCPTypeStreamableHTTP || remote.URL != "https://deploy.example.com/mcp" ||
		remote.Headers["X-Tenant"] != "public-tenant" {
		t.Errorf("remote = %+v", remote)
	}
	if byName["loopback"].URL != "http://localhost:8080/mcp" {
		t.Errorf("loopback = %+v", byName["loopback"])
	}
}

func TestLoadMCPSSEUnsupportedTransport(t *testing.T) {
	root := t.TempDir()
	result := loadMCP(t, root, mcpServersBody(t, map[string]any{
		"legacy": map[string]any{"type": "sse", "url": "https://example.com/sse"},
	}))
	if len(result.Servers) != 0 || len(result.Skips) != 1 {
		t.Fatalf("Servers=%v Skips=%v, want only one skip", result.Servers, result.Skips)
	}
	if !strings.Contains(result.Skips[0].Reason, "not supported") {
		t.Errorf("reason = %q, want unsupported-transport report", result.Skips[0].Reason)
	}
}

func TestLoadMCPMissingFile(t *testing.T) {
	result, err := LoadMCP(t.TempDir(), "1.0.0")
	if err != nil {
		t.Fatalf("LoadMCP: %v", err)
	}
	if len(result.Servers) != 0 || result.Disabled != "" || len(result.Skips) != 0 {
		t.Errorf("missing mcp.json = %+v, want all zero", result)
	}
}

func TestLoadMCPDisabledTopLevel(t *testing.T) {
	cases := []struct {
		name    string
		content string
		wantSub string
	}{
		{name: "not json", content: "not json", wantSub: "not a JSON object"},
		{name: "array", content: `[1]`, wantSub: "not a JSON object"},
		{name: "null", content: `null`, wantSub: "not a JSON object"},
		{name: "unknown field", content: `{"$schema":"` + MCPSchemaID + `","mcpServers":{},"bogus":1}`, wantSub: `"bogus"`},
		{name: "missing schema", content: `{"mcpServers":{}}`, wantSub: "$schema"},
		{name: "schema wrong type", content: `{"$schema":1,"mcpServers":{}}`, wantSub: "$schema"},
		{name: "missing mcpServers", content: `{"$schema":"` + MCPSchemaID + `"}`, wantSub: "mcpServers"},
		{name: "mcpServers not object", content: `{"$schema":"` + MCPSchemaID + `","mcpServers":[]}`, wantSub: "mcpServers"},
		{name: "trailing data", content: `{"$schema":"` + MCPSchemaID + `","mcpServers":{}} {}`, wantSub: "trailing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			result := loadMCP(t, root, tc.content)
			if result.Disabled == "" || !strings.Contains(result.Disabled, tc.wantSub) {
				t.Errorf("Disabled = %q, want non-empty naming %q", result.Disabled, tc.wantSub)
			}
			if len(result.Servers) != 0 {
				t.Errorf("Servers = %v, want none when MCP is disabled", result.Servers)
			}
		})
	}
}

func TestLoadMCPVersionMismatch(t *testing.T) {
	root := t.TempDir()
	result := loadMCP(t, root, `{"$schema":"`+MCPSchemaID+`","mcpServers":{}}`)
	_ = result

	root2 := t.TempDir()
	writeMCP(t, root2, `{"$schema":"`+MCPSchemaID+`","mcpServers":{}}`)
	mismatched, err := LoadMCP(root2, "2.0.0")
	if err != nil {
		t.Fatalf("LoadMCP: %v", err)
	}
	if !strings.Contains(mismatched.Disabled, "2.0.0") || !strings.Contains(mismatched.Disabled, "1.0.0") {
		t.Errorf("Disabled = %q, want mismatch naming both versions", mismatched.Disabled)
	}

	root3 := t.TempDir()
	writeMCP(t, root3, `{"$schema":"https://agent-plugins.org/schemas/2.0.0/mcp.schema.json","mcpServers":{}}`)
	unsupported, err := LoadMCP(root3, "1.0.0")
	if err != nil {
		t.Fatalf("LoadMCP: %v", err)
	}
	if !strings.Contains(unsupported.Disabled, "2.0.0") {
		t.Errorf("Disabled = %q, want unsupported version named", unsupported.Disabled)
	}
}

func TestLoadMCPEmptyServersValid(t *testing.T) {
	root := t.TempDir()
	result := loadMCP(t, root, `{"$schema":"`+MCPSchemaID+`","mcpServers":{}}`)
	if result.Disabled != "" || len(result.Servers) != 0 || len(result.Skips) != 0 {
		t.Errorf("empty mcpServers = %+v, want valid with no servers", result)
	}
}

func TestLoadMCPNotRegularFile(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "mcp.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	result, err := LoadMCP(root, "1.0.0")
	if err != nil {
		t.Fatalf("LoadMCP: %v", err)
	}
	if result.Disabled == "" || !strings.Contains(result.Disabled, "not a regular file") {
		t.Errorf("directory mcp.json = %q, want component-type-invalid", result.Disabled)
	}
}

func TestLoadMCPReservedEnvRejected(t *testing.T) {
	root := t.TempDir()
	result := loadMCP(t, root, mcpServersBody(t, map[string]any{
		"bad-root": map[string]any{"type": "stdio", "command": "npx", "env": map[string]any{"PLUGIN_ROOT": "x"}},
		"bad-data": map[string]any{"type": "stdio", "command": "npx", "env": map[string]any{"PLUGIN_DATA": "x"}},
		"good":     map[string]any{"type": "stdio", "command": "npx"},
	}))
	if len(result.Servers) != 1 || result.Servers[0].Name != "good" {
		t.Errorf("Servers = %+v, want only good", result.Servers)
	}
	if len(result.Skips) != 2 {
		t.Fatalf("Skips = %+v, want 2 (reserved env keys invalidate the entry)", result.Skips)
	}
	for _, skip := range result.Skips {
		if !strings.Contains(skip.Reason, "reserved") {
			t.Errorf("skip reason = %q, want reserved-variable report", skip.Reason)
		}
	}
}

func TestPluginDataDirConvention(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".waffle")
	got, err := PluginDataDir(home, "acme-tools")
	if err != nil {
		t.Fatalf("PluginDataDir: %v", err)
	}
	if want := filepath.Join(home, "plugins-data", "acme-tools"); got != want {
		t.Errorf("PluginDataDir = %q, want %q", got, want)
	}
	if PluginsDataDir(home) != filepath.Join(home, "plugins-data") {
		t.Errorf("PluginsDataDir = %q", PluginsDataDir(home))
	}
	if _, err := PluginDataDir(home, "Bad-Name"); !errors.Is(err, ErrInvalidName) {
		t.Errorf("invalid name error = %v", err)
	}
}

func TestLoadMCPSkipsInvalidServers(t *testing.T) {
	root := t.TempDir()
	result := loadMCP(t, root, mcpServersBody(t, map[string]any{
		"good":     map[string]any{"type": "stdio", "command": "npx"},
		"no-type":  map[string]any{"command": "npx"},
		"bad-type": map[string]any{"type": "websocket", "command": "npx"},
		// Variant-field violations.
		"stdio-with-url":  map[string]any{"type": "stdio", "command": "npx", "url": "https://x"},
		"remote-with-cmd": map[string]any{"type": "streamable-http", "url": "https://x", "command": "npx"},
		// Bad commands.
		"absolute-cmd": map[string]any{"type": "stdio", "command": "/bin/server"},
		"parent-cmd":   map[string]any{"type": "stdio", "command": "../bin/server"},
		"multi-cmd":    map[string]any{"type": "stdio", "command": "node server.js"},
		"rel-cmd":      map[string]any{"type": "stdio", "command": "bin/server"},
		"win-abs-cmd":  map[string]any{"type": "stdio", "command": "C:\\bin\\server"},
		"win-rel-cmd":  map[string]any{"type": "stdio", "command": "..\\bin\\server"},
		// Bad cwd.
		"bad-cwd": map[string]any{"type": "stdio", "command": "npx", "cwd": "/etc"},
		// Bad urls.
		"no-scheme":   map[string]any{"type": "streamable-http", "url": "example.com/mcp"},
		"http-remote": map[string]any{"type": "streamable-http", "url": "http://example.com/mcp"},
		"userinfo":    map[string]any{"type": "streamable-http", "url": "https://user:pass@example.com/mcp"},
		"fragment":    map[string]any{"type": "streamable-http", "url": "https://example.com/mcp#frag"},
		// Bad headers / env.
		"bad-header-name": map[string]any{"type": "streamable-http", "url": "https://x", "headers": map[string]any{"bad name": "v"}},
		"dup-header":      map[string]any{"type": "streamable-http", "url": "https://x", "headers": map[string]any{"X-Tenant": "a", "x-tenant": "b"}},
		"crlf-header":     map[string]any{"type": "streamable-http", "url": "https://x", "headers": map[string]any{"X-Tenant": "a\r\nb"}},
		"env-non-string":  map[string]any{"type": "stdio", "command": "npx", "env": map[string]any{"X": 1}},
	}))
	if result.Disabled != "" {
		t.Fatalf("Disabled = %q, want empty (entry skips only)", result.Disabled)
	}
	if len(result.Servers) != 1 || result.Servers[0].Name != "good" {
		t.Errorf("Servers = %+v, want only good", result.Servers)
	}
	if len(result.Skips) != 19 {
		t.Fatalf("Skips = %d, want 19: %+v", len(result.Skips), result.Skips)
	}
	for _, skip := range result.Skips {
		if skip.Name == "good" {
			t.Errorf("good was skipped: %+v", skip)
		}
	}
}

func TestLoadMCPLoopbackURLValidation(t *testing.T) {
	root := t.TempDir()
	result := loadMCP(t, root, mcpServersBody(t, map[string]any{
		"ok-v4":        map[string]any{"type": "streamable-http", "url": "http://127.0.0.1:8080/mcp"},
		"ok-localhost": map[string]any{"type": "streamable-http", "url": "http://localhost/mcp"},
		"bad-v4":       map[string]any{"type": "streamable-http", "url": "http://127.999.999.999/mcp"},
		"bad-octet":    map[string]any{"type": "streamable-http", "url": "http://127.0.0.256/mcp"},
	}))
	if len(result.Servers) != 2 || len(result.Skips) != 2 {
		t.Fatalf("Servers=%+v Skips=%+v, want 2 ok / 2 invalid", result.Servers, result.Skips)
	}
	for _, skip := range result.Skips {
		if !strings.Contains(skip.Reason, "url") {
			t.Errorf("skip reason = %q, want url validation report", skip.Reason)
		}
	}
}
