package plugin

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// LoadResult aggregates a plugin's component outcomes (spec §11.3): the
// manifest plus each component's result, every one with its own failure
// boundary. Only the manifest can reject the whole plugin; skills and MCP
// failures are contained here.
type LoadResult struct {
	Plugin     Plugin
	Skills     []Skill
	SkillSkips []SkillSkip
	MCP        MCPResult
}

// LoadComponents loads a plugin package end to end: plugin.json (a failure
// rejects the whole plugin — nothing is discovered or executed, §5.2/§11.3),
// skills/ (§6.2/§7.1 skips), and mcp.json (§7.2.2 disables/skips). A
// returned error means the whole plugin is rejected; component problems are
// reported inside the result.
func LoadComponents(root string) (LoadResult, error) {
	p, err := Load(root)
	if err != nil {
		return LoadResult{}, err
	}
	version := supportedSchemas[p.Manifest.Schema]
	// Component failures are contained per §11.3: only the manifest rejects
	// the whole plugin. A component-level read failure disables that
	// component (reported) instead of taking down the other components.
	skills, skillSkips, err := DiscoverSkills(root)
	if err != nil {
		skillSkips = append(skillSkips, SkillSkip{Dir: "skills", Reason: err.Error()})
	}
	mcpResult, err := LoadMCP(root, version)
	if err != nil {
		mcpResult = MCPResult{Disabled: err.Error()}
	}
	return LoadResult{Plugin: p, Skills: skills, SkillSkips: skillSkips, MCP: mcpResult}, nil
}

// PluginReject reports an installed plugin directory that was rejected as a
// whole (invalid plugin.json): per §5.2/§11.3 nothing from it is
// discovered or executed.
type PluginReject struct {
	Dir    string
	Reason string
}

// Installed loads every plugin under <home>/plugins (the #389 convention)
// with per-plugin boundaries: a rejected plugin is reported and skipped;
// a partially-loaded plugin still yields its valid components. A missing
// plugins dir is not an error. Results are sorted by manifest name for
// deterministic wiring.
func Installed(home string) ([]LoadResult, []PluginReject, error) {
	root := PluginsDir(home)
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read plugins dir: %w", err)
	}
	var results []LoadResult
	var rejects []PluginReject
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		result, err := LoadComponents(dir)
		if err != nil {
			rejects = append(rejects, PluginReject{Dir: dir, Reason: err.Error()})
			continue
		}
		results = append(results, result)
	}
	slices.SortFunc(results, func(a, b LoadResult) int {
		return strings.Compare(a.Plugin.Manifest.Name, b.Plugin.Manifest.Name)
	})
	return results, rejects, nil
}
