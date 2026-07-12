package codeintel

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFixture(t *testing.T) (dir, mainPath string) {
	t.Helper()
	dir = t.TempDir()
	mainPath = filepath.Join(dir, "hello.go")
	src := `package demo

func Hello() string { return "hi" }

func CallHello() string { return Hello() }
`
	if err := os.WriteFile(mainPath, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	testPath := filepath.Join(dir, "hello_test.go")
	if err := os.WriteFile(testPath, []byte("package demo\nfunc TestHello(t *testing.T) { _ = Hello() }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, mainPath
}

func TestToolNamesMatchIssue79(t *testing.T) {
	want := []string{
		"code_find_symbol", "code_references", "code_callers",
		"code_structure", "code_blast_radius", "code_suggest_tests",
	}
	if len(ToolNames) != 6 {
		t.Fatalf("len=%d", len(ToolNames))
	}
	for i, n := range want {
		if ToolNames[i] != n {
			t.Fatalf("ToolNames[%d]=%q want %q", i, ToolNames[i], n)
		}
	}
}

func TestFindSymbolStructureReferences(t *testing.T) {
	dir, mainPath := writeFixture(t)
	svc := NewService(dir, "acme/demo", "main")
	ctx := context.Background()

	syms, err := svc.FindSymbol(ctx, "Hello", "func")
	if err != nil || len(syms) == 0 {
		t.Fatalf("find: %v %v", syms, err)
	}
	if syms[0].Source != SourceTextFallback || syms[0].Path == "" || syms[0].StartLine < 1 {
		t.Fatalf("%+v", syms[0])
	}
	if syms[0].Repo != "acme/demo" || syms[0].Ref != "main" {
		t.Fatalf("provenance %+v", syms[0])
	}

	structLocs, err := svc.Structure(ctx, mainPath)
	if err != nil || len(structLocs) < 2 {
		t.Fatalf("structure: %v %v", structLocs, err)
	}

	refs, err := svc.References(ctx, mainPath, 0, "Hello")
	if err != nil || len(refs) < 1 {
		t.Fatalf("refs: %v %v", refs, err)
	}
}

func TestBlastRadiusAndSuggestTestsUncertain(t *testing.T) {
	dir, _ := writeFixture(t)
	svc := NewService(dir, "acme/demo", "")
	ctx := context.Background()
	blast, err := svc.BlastRadius(ctx, "", "Hello")
	if err != nil || len(blast) == 0 {
		t.Fatalf("blast: %v %v", blast, err)
	}
	uncertain := false
	for _, b := range blast {
		if b.Uncertain && b.Evidence != "" {
			uncertain = true
		}
		if b.Source == "" {
			t.Fatalf("missing source: %+v", b)
		}
	}
	if !uncertain {
		t.Fatal("blast radius must mark uncertainty")
	}
	tests, err := svc.SuggestTests(ctx, "Hello")
	if err != nil || len(tests) == 0 {
		t.Fatalf("suggest: %v %v", tests, err)
	}
}

func TestCacheStaleAfterEdit(t *testing.T) {
	dir, mainPath := writeFixture(t)
	svc := NewService(dir, "acme/demo", "main")
	if err := svc.IndexFile(mainPath); err != nil {
		t.Fatal(err)
	}
	// Query from cache (same hash).
	syms, err := svc.FindSymbol(context.Background(), "Hello", "")
	if err != nil || len(syms) == 0 {
		t.Fatal(err)
	}
	if syms[0].Source != SourceCachedIndex || syms[0].IndexedAt == "" {
		t.Fatalf("want cached-index with indexed_at: %+v", syms[0])
	}
	time.Sleep(5 * time.Millisecond)
	// Modify file — live content is authoritative; re-parse must not serve stale
	// as authoritative without stale flag.
	if err := os.WriteFile(mainPath, []byte("package demo\nfunc Hello() string { return \"bye\" }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	syms2, err := svc.FindSymbol(context.Background(), "Hello", "")
	if err != nil || len(syms2) == 0 {
		t.Fatal(err)
	}
	// Live re-parse uses text-fallback (not silent stale cache).
	if syms2[0].Source != SourceTextFallback {
		t.Fatalf("after edit source=%q want text-fallback", syms2[0].Source)
	}
	raw, _ := os.ReadFile(mainPath)
	if !strings.Contains(string(raw), "bye") {
		t.Fatal("live file authoritative")
	}
}

func TestToolboxDiscovery(t *testing.T) {
	svc := NewService(t.TempDir(), "", "")
	tb := Toolbox(svc)
	defs := tb.Defs()
	if len(defs) != 6 {
		t.Fatalf("defs=%d", len(defs))
	}
	names := map[string]bool{}
	for _, d := range defs {
		names[d.Name] = true
	}
	for _, n := range ToolNames {
		if !names[n] {
			t.Fatalf("missing tool %s", n)
		}
	}
	// Partial failure: empty root returns error string, does not panic.
	out, err := tb.Run(context.Background(), "code_find_symbol", json.RawMessage(`{"name":"X"}`))
	if err != nil {
		// ok — root empty may error
		t.Log(err)
	} else if out != "[]" && out != "" {
		t.Log(out)
	}
	_ = CapabilitiesJSON(svc)
}

func TestValidateMCPServerRejectsSecretsAndHost(t *testing.T) {
	err := ValidateMCPServer("codeintel", "host", []string{"GITHUB_TOKEN"}, ToolNames, false)
	if err == nil {
		t.Fatal("expected secret env rejection")
	}
	err = ValidateMCPServer("codeintel", "host", nil, ToolNames, false)
	if err == nil {
		t.Fatal("expected host rejection without allow")
	}
	if err := ValidateMCPServer("codeintel", "sandbox", nil, ToolNames, false); err != nil {
		t.Fatal(err)
	}
	if err := ValidateMCPServer("codeintel", "host", nil, ToolNames, true); err != nil {
		t.Fatal(err)
	}
	// Non-codeintel server unrestricted.
	if err := ValidateMCPServer("other", "host", []string{"TOKEN"}, []string{"echo"}, false); err != nil {
		t.Fatal(err)
	}
}

func TestApprovedCapability(t *testing.T) {
	if !ApprovedCapability("code_blast_radius") {
		t.Fatal()
	}
	if ApprovedCapability("/bin/evil") {
		t.Fatal("repo must not approve arbitrary executables")
	}
}

func TestToolboxWithCapsFilters(t *testing.T) {
	svc := NewService(t.TempDir(), "", "")
	// Repo requests include an unapproved executable-looking ID — ignored.
	tb := ToolboxWithCaps(svc, []string{"code_find_symbol", "/bin/evil", "code_blast_radius", "nope"})
	names := map[string]bool{}
	for _, d := range tb.Defs() {
		names[d.Name] = true
	}
	if !names["code_find_symbol"] || !names["code_blast_radius"] {
		t.Fatalf("missing approved caps: %v", names)
	}
	if names["code_references"] || names["/bin/evil"] {
		t.Fatalf("unapproved tools registered: %v", names)
	}
	if len(names) != 2 {
		t.Fatalf("want 2 tools, got %v", names)
	}
}
