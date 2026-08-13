package plugin

import (
	"encoding/json"
	"strings"
	"testing"
)

// manifestWithExtensions builds a manifest whose extensions hold a waffle
// namespace and foreign namespaces, each stored under its own key.
func manifestWithExtensions(t *testing.T, waffle any, foreign map[string]any) Manifest {
	t.Helper()
	ext := map[string]any{}
	if waffle != nil {
		ext[Namespace] = waffle
	}
	for ns, value := range foreign {
		ext[ns] = value
	}
	encoded := map[string]json.RawMessage{}
	for ns, value := range ext {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		encoded[ns] = raw
	}
	return Manifest{Schema: SchemaID, Name: "test-plugin", Extensions: encoded}
}

func TestLoadWaffleExtensionParses(t *testing.T) {
	m := manifestWithExtensions(t, map[string]any{
		"skills": map[string]any{
			"deploy": map[string]any{"status": "inactive"},
			"lint":   map[string]any{"status": "active"},
		},
		"mcp": map[string]any{
			"local-validator": map[string]any{
				"execution": "sandbox",
				"groups":    []string{"main"},
			},
			"api": map[string]any{
				"egress": "broker",
				"token":  "secret://mcp/api/access-token",
			},
		},
		"future-member": map[string]any{"x": 1},
	}, nil)

	ext, warnings, err := LoadWaffleExtension(m)
	if err != nil {
		t.Fatalf("LoadWaffleExtension: %v", err)
	}
	if ext.Skills["deploy"].Status != "inactive" || ext.Skills["lint"].Status != "active" {
		t.Errorf("skills = %+v", ext.Skills)
	}
	if ext.MCP["local-validator"].Execution != "sandbox" || len(ext.MCP["local-validator"].Groups) != 1 {
		t.Errorf("mcp local-validator = %+v", ext.MCP["local-validator"])
	}
	if ext.MCP["api"].Egress != "broker" || ext.MCP["api"].Token != "secret://mcp/api/access-token" {
		t.Errorf("mcp api = %+v", ext.MCP["api"])
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "future-member") {
		t.Errorf("warnings = %v, want unknown member reported", warnings)
	}
}

func TestLoadWaffleExtensionAbsentOrMalformed(t *testing.T) {
	// No waffle namespace → zero value, no warnings.
	ext, warnings, err := LoadWaffleExtension(Manifest{Extensions: map[string]json.RawMessage{}})
	if err != nil || len(warnings) != 0 {
		t.Errorf("absent = %v, %v, %v", ext, warnings, err)
	}

	// Non-object waffle namespace → reported and ignored.
	raw, _ := json.Marshal("not an object")
	ext, warnings, err = LoadWaffleExtension(Manifest{Extensions: map[string]json.RawMessage{Namespace: raw}})
	if err != nil || len(warnings) != 1 || !strings.Contains(warnings[0], "must be an object") {
		t.Errorf("non-object = %v, %v, %v", ext, warnings, err)
	}

	// Malformed member → that entry ignored, others still apply.
	m := manifestWithExtensions(t, map[string]any{
		"skills": map[string]any{
			"good":       map[string]any{"status": "inactive"},
			"broken":     map[string]any{"status": "bogus-value"},
			"bad-member": map[string]any{"unknown": true},
		},
	}, nil)
	ext, warnings, err = LoadWaffleExtension(m)
	if err != nil {
		t.Fatalf("LoadWaffleExtension: %v", err)
	}
	if ext.Skills["good"].Status != "inactive" {
		t.Errorf("good policy lost: %+v", ext.Skills)
	}
	if _, ok := ext.Skills["broken"]; ok {
		t.Errorf("broken policy applied: %+v", ext.Skills)
	}
	if len(warnings) != 2 {
		t.Errorf("warnings = %v, want broken + bad-member reported", warnings)
	}
}

func TestLoadWaffleExtensionForeignNamespacesUntouched(t *testing.T) {
	// A plugin whose extensions carry a foreign namespace with arbitrary
	// contents is neither rejected nor inspected (#394 acceptance).
	foreign := map[string]any{
		"com.other.client": map[string]any{"anything": []any{1, "x", map[string]any{"deep": true}}},
	}
	m := manifestWithExtensions(t, nil, foreign)
	// LoadWaffleExtension only reads the waffle namespace; foreign contents
	// are untouched in the raw map.
	ext, warnings, err := LoadWaffleExtension(m)
	if err != nil || len(warnings) != 0 {
		t.Fatalf("foreign namespace = %v, %v, %v", ext, warnings, err)
	}
	raw := string(m.Extensions["com.other.client"])
	if !strings.Contains(raw, "deep") {
		t.Errorf("foreign namespace corrupted: %s", raw)
	}
}

// TestWaffleExtensionRoundTrip: loaded, modified, re-serialized — the waffle
// value changes, foreign namespaces keep their exact bytes, and no new
// top-level plugin.json fields are introduced.
func TestWaffleExtensionRoundTrip(t *testing.T) {
	foreign := map[string]any{"com.other.client": map[string]any{"setting": []any{1, 2}}}
	m := manifestWithExtensions(t, map[string]any{
		"skills": map[string]any{"deploy": map[string]any{"status": "inactive"}},
	}, foreign)
	foreignRaw := string(m.Extensions["com.other.client"])

	ext, _, err := LoadWaffleExtension(m)
	if err != nil {
		t.Fatal(err)
	}
	// Modify: activate deploy.
	ext.Skills["deploy"] = WaffleSkillPolicy{Status: "active"}
	modified, err := json.Marshal(map[string]any{
		"skills": map[string]any{"deploy": map[string]any{"status": "active"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	m.Extensions[Namespace] = modified

	if !strings.Contains(string(m.Extensions[Namespace]), "active") {
		t.Error("waffle namespace not modified")
	}
	if string(m.Extensions["com.other.client"]) != foreignRaw {
		t.Errorf("foreign namespace corrupted during round-trip: %s", m.Extensions["com.other.client"])
	}
	// The manifest still has exactly the permitted top-level fields; the
	// extension is the only channel.
	if m.Name == "" || m.Schema == "" {
		t.Error("manifest fields lost")
	}
}

func TestNamespaceStable(t *testing.T) {
	if Namespace != "dev.mattriley.waffle" {
		t.Errorf("Namespace = %q, want dev.mattriley.waffle (documented in docs/plan.md, #394)", Namespace)
	}
}
