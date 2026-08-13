package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadComponentsIsolatesMCPFromSkills: an invalid mcp.json disables
// only MCP for its plugin; the same plugin's skills still load (§7.2.2 rule
// 2, §11.3).
func TestLoadComponentsIsolatesMCPFromSkills(t *testing.T) {
	root := t.TempDir()
	writePlugin(t, root, map[string]string{
		"plugin.json":        manifestJSON(t, baseFields()),
		"skills/ok/SKILL.md": "---\nname: ok\ndescription: A good skill.\n---\n\nBody.\n",
		"mcp.json":           `{"$schema":"` + MCPSchemaID + `","mcpServers":{},"bogus":1}`,
	})
	result, err := LoadComponents(root)
	if err != nil {
		t.Fatalf("LoadComponents: %v", err)
	}
	if len(result.Skills) != 1 || result.Skills[0].Name != "ok" {
		t.Errorf("skills = %+v, want ok still loaded", result.Skills)
	}
	if result.MCP.Disabled == "" || !strings.Contains(result.MCP.Disabled, "bogus") {
		t.Errorf("MCP.Disabled = %q, want unknown-field reason", result.MCP.Disabled)
	}
}

// TestLoadComponentsSkipsInvalidServerKeepsValid: a mcp.json with one valid
// and one invalid server loads the valid one and reports the invalid one.
func TestLoadComponentsSkipsInvalidServerKeepsValid(t *testing.T) {
	root := t.TempDir()
	writePlugin(t, root, map[string]string{
		"plugin.json": manifestJSON(t, baseFields()),
		"mcp.json": mcpServersBody(t, map[string]any{
			"good": map[string]any{"type": "stdio", "command": "npx"},
			"bad":  map[string]any{"type": "websocket", "command": "npx"},
		}),
	})
	result, err := LoadComponents(root)
	if err != nil {
		t.Fatalf("LoadComponents: %v", err)
	}
	if len(result.MCP.Servers) != 1 || result.MCP.Servers[0].Name != "good" {
		t.Errorf("servers = %+v, want only good", result.MCP.Servers)
	}
	if len(result.MCP.Skips) != 1 || !strings.Contains(result.MCP.Skips[0].Reason, "websocket") {
		t.Errorf("skips = %+v, want the invalid entry reported", result.MCP.Skips)
	}
}

// TestLoadComponentsUnsupportedTransportSkipped: sse is skipped while the
// other server and any skills still load (§7.2.2 rule 4).
func TestLoadComponentsUnsupportedTransportSkipped(t *testing.T) {
	root := t.TempDir()
	writePlugin(t, root, map[string]string{
		"plugin.json": manifestJSON(t, baseFields()),
		"mcp.json": mcpServersBody(t, map[string]any{
			"legacy": map[string]any{"type": "sse", "url": "https://example.com/sse"},
			"local":  map[string]any{"type": "stdio", "command": "npx"},
		}),
	})
	result, err := LoadComponents(root)
	if err != nil {
		t.Fatalf("LoadComponents: %v", err)
	}
	if len(result.MCP.Servers) != 1 || result.MCP.Servers[0].Name != "local" {
		t.Errorf("servers = %+v, want only local", result.MCP.Servers)
	}
	if len(result.MCP.Skips) != 1 || !strings.Contains(result.MCP.Skips[0].Reason, "not supported") {
		t.Errorf("skips = %+v, want sse reported", result.MCP.Skips)
	}
}

// TestLoadComponentsComponentTypeInvalidOnly: skills/ as a file invalidates
// only the skills component; mcp.json as a directory invalidates only MCP.
func TestLoadComponentsComponentTypeInvalidOnly(t *testing.T) {
	root := t.TempDir()
	writePlugin(t, root, map[string]string{
		"plugin.json": manifestJSON(t, baseFields()),
		"skills":      "not a directory",
		"mcp.json":    `{"$schema":"` + MCPSchemaID + `","mcpServers":{}}`,
	})
	result, err := LoadComponents(root)
	if err != nil {
		t.Fatalf("LoadComponents (skills as file): %v", err)
	}
	if len(result.Skills) != 0 || len(result.SkillSkips) != 1 {
		t.Errorf("skills=%v skips=%v, want skills component invalid only", result.Skills, result.SkillSkips)
	}
	if result.MCP.Disabled != "" || len(result.MCP.Servers) != 0 {
		t.Errorf("MCP = %+v, want unaffected", result.MCP)
	}

	root2 := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root2, "mcp.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	writePlugin(t, root2, map[string]string{
		"plugin.json":        manifestJSON(t, baseFields()),
		"skills/ok/SKILL.md": "---\nname: ok\ndescription: d\n---\n\nBody.\n",
	})
	result2, err := LoadComponents(root2)
	if err != nil {
		t.Fatalf("LoadComponents (mcp.json as dir): %v", err)
	}
	if result2.MCP.Disabled == "" {
		t.Errorf("MCP.Disabled empty, want component-type-invalid report")
	}
	if len(result2.Skills) != 1 {
		t.Errorf("skills = %v, want unaffected by the broken mcp.json", result2.Skills)
	}
}

// TestInstalledLoadsPartialAndReportsRejected: Installed aggregates every
// plugin under <home>/plugins, rejecting whole-broken ones and keeping
// partially-loaded ones.
func TestInstalledLoadsPartialAndReportsRejected(t *testing.T) {
	home := t.TempDir()
	writePlugin(t, filepath.Join(home, "plugins", "good"), map[string]string{
		"plugin.json":       manifestJSON(t, map[string]any{"$schema": SchemaID, "name": "good"}),
		"skills/a/SKILL.md": "---\nname: a\ndescription: d\n---\n\nBody.\n",
	})
	writePlugin(t, filepath.Join(home, "plugins", "broken"), map[string]string{
		"plugin.json": `{"name": "broken"}`,
	})
	writePlugin(t, filepath.Join(home, "plugins", "partial"), map[string]string{
		"plugin.json": manifestJSON(t, map[string]any{"$schema": SchemaID, "name": "partial"}),
		"mcp.json":    `{"$schema":"` + MCPSchemaID + `","mcpServers":{},"junk":1}`,
	})

	results, rejects, err := Installed(home)
	if err != nil {
		t.Fatalf("Installed: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want good + partial: %+v", len(results), results)
	}
	if results[0].Plugin.Manifest.Name != "good" || results[1].Plugin.Manifest.Name != "partial" {
		t.Errorf("results sorted wrong: %+v", results)
	}
	if len(results[0].Skills) != 1 || results[1].MCP.Disabled == "" {
		t.Errorf("component contents wrong: %+v", results)
	}
	if len(rejects) != 1 || !strings.Contains(rejects[0].Reason, `"$schema"`) {
		t.Errorf("rejects = %+v, want broken reported", rejects)
	}

	// A missing plugins dir is not an error.
	empty, rejects, err := Installed(t.TempDir())
	if err != nil || len(empty) != 0 || len(rejects) != 0 {
		t.Errorf("missing plugins dir = %v, %v, %v; want none", empty, rejects, err)
	}
}
