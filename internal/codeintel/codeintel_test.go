package codeintel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

type honestyResponse struct {
	Results  []CodeLocation `json:"results"`
	Analysis struct {
		SupportedLanguages []string `json:"supported_languages"`
		AnalysedFiles      []string `json:"analysed_files"`
		SkippedFiles       []struct {
			Path     string `json:"path"`
			Language string `json:"language"`
		} `json:"skipped_files"`
		Limitation string `json:"limitation"`
	} `json:"analysis"`
}

func marshalToolInput(t *testing.T, input map[string]any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func codeToolCases(t *testing.T, root string) []struct {
	name  string
	input json.RawMessage
} {
	t.Helper()
	return []struct {
		name  string
		input json.RawMessage
	}{
		{name: "code_find_symbol", input: marshalToolInput(t, map[string]any{"name": "Hello"})},
		{name: "code_references", input: marshalToolInput(t, map[string]any{"symbol": "Hello"})},
		{name: "code_callers", input: marshalToolInput(t, map[string]any{"symbol": "Hello"})},
		{name: "code_structure", input: marshalToolInput(t, map[string]any{"path": root})},
		{name: "code_blast_radius", input: marshalToolInput(t, map[string]any{"symbol": "Hello"})},
		{name: "code_suggest_tests", input: marshalToolInput(t, map[string]any{"symbol": "Hello"})},
	}
}

func writeTypeScriptFixture(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "app.ts")
	if err := os.WriteFile(path, []byte("export function Hello(): string { return 'hi' }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestUnsupportedLanguagesAreReportedAcrossCodeTools(t *testing.T) {
	dir := t.TempDir()
	tsPath := writeTypeScriptFixture(t, dir)
	tb := Toolbox(NewService(dir, "acme/demo", "main"))

	for _, tc := range codeToolCases(t, dir) {
		t.Run(tc.name, func(t *testing.T) {
			out, err := tb.Run(context.Background(), tc.name, tc.input)
			if err != nil {
				t.Fatal(err)
			}
			var got honestyResponse
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("output must be an honesty response, got %s: %v", out, err)
			}
			if len(got.Results) != 0 {
				t.Fatalf("results=%v want none", got.Results)
			}
			if len(got.Analysis.SupportedLanguages) != 1 || got.Analysis.SupportedLanguages[0] != "Go" {
				t.Fatalf("supported_languages=%v", got.Analysis.SupportedLanguages)
			}
			if !strings.Contains(got.Analysis.Limitation, "Go only") ||
				!strings.Contains(got.Analysis.Limitation, "TypeScript") {
				t.Fatalf("limitation=%q", got.Analysis.Limitation)
			}
			if len(got.Analysis.AnalysedFiles) != 0 {
				t.Fatalf("analysed_files=%v want none", got.Analysis.AnalysedFiles)
			}
			if len(got.Analysis.SkippedFiles) != 1 ||
				got.Analysis.SkippedFiles[0].Path != tsPath ||
				got.Analysis.SkippedFiles[0].Language != "TypeScript" {
				t.Fatalf("skipped_files=%v", got.Analysis.SkippedFiles)
			}
		})
	}
}

func TestMixedLanguageAnalysisIsReportedAcrossCodeTools(t *testing.T) {
	dir, mainPath := writeFixture(t)
	testPath := filepath.Join(dir, "hello_test.go")
	tsPath := writeTypeScriptFixture(t, dir)
	tb := Toolbox(NewService(dir, "acme/demo", "main"))

	for _, tc := range codeToolCases(t, dir) {
		t.Run(tc.name, func(t *testing.T) {
			out, err := tb.Run(context.Background(), tc.name, tc.input)
			if err != nil {
				t.Fatal(err)
			}
			var got honestyResponse
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("output must report partial analysis, got %s: %v", out, err)
			}
			if len(got.Results) == 0 {
				t.Fatal("mixed repository must retain Go results")
			}
			analysed := make(map[string]bool, len(got.Analysis.AnalysedFiles))
			for _, path := range got.Analysis.AnalysedFiles {
				analysed[path] = true
			}
			if len(analysed) != 2 || !analysed[mainPath] || !analysed[testPath] {
				t.Fatalf("analysed_files=%v", got.Analysis.AnalysedFiles)
			}
			if len(got.Analysis.SkippedFiles) != 1 ||
				got.Analysis.SkippedFiles[0].Path != tsPath ||
				got.Analysis.SkippedFiles[0].Language != "TypeScript" {
				t.Fatalf("skipped_files=%v", got.Analysis.SkippedFiles)
			}
			if !strings.Contains(got.Analysis.Limitation, "TypeScript") {
				t.Fatalf("limitation=%q", got.Analysis.Limitation)
			}
		})
	}
}

func TestUnsupportedPathIsReported(t *testing.T) {
	dir := t.TempDir()
	tsPath := writeTypeScriptFixture(t, dir)
	tb := Toolbox(NewService(dir, "acme/demo", "main"))
	tests := []struct {
		name  string
		input map[string]any
	}{
		{name: "code_references", input: map[string]any{"path": tsPath, "line": 1}},
		{name: "code_structure", input: map[string]any{"path": tsPath}},
		{name: "code_blast_radius", input: map[string]any{"path": tsPath, "symbol": "Hello"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := tb.Run(context.Background(), tt.name, marshalToolInput(t, tt.input))
			if err != nil {
				t.Fatalf("unsupported path returned an error instead of a limitation: %v", err)
			}
			var got honestyResponse
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("output=%s: %v", out, err)
			}
			if !strings.Contains(got.Analysis.Limitation, "TypeScript") {
				t.Fatalf("limitation=%q", got.Analysis.Limitation)
			}
		})
	}
}

func TestUnsupportedPathWithSymbolRetainsGoResults(t *testing.T) {
	dir, _ := writeFixture(t)
	tsPath := writeTypeScriptFixture(t, dir)
	tb := Toolbox(NewService(dir, "acme/demo", "main"))
	tests := []struct {
		name  string
		input map[string]any
	}{
		{name: "code_references", input: map[string]any{"path": tsPath, "symbol": "Hello"}},
		{name: "code_blast_radius", input: map[string]any{"path": tsPath, "symbol": "Hello"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := tb.Run(context.Background(), tt.name, marshalToolInput(t, tt.input))
			if err != nil {
				t.Fatal(err)
			}
			var got honestyResponse
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("output=%s: %v", out, err)
			}
			if len(got.Results) == 0 {
				t.Fatal("symbol-based search discarded available Go results")
			}
			if len(got.Analysis.SkippedFiles) != 1 ||
				got.Analysis.SkippedFiles[0].Path != tsPath ||
				got.Analysis.SkippedFiles[0].Language != "TypeScript" {
				t.Fatalf("skipped_files=%v", got.Analysis.SkippedFiles)
			}
		})
	}
}

func TestHeaderFilesAreReported(t *testing.T) {
	tests := []struct {
		extension string
		language  string
	}{
		{extension: ".h", language: "C/C++ header"},
		{extension: ".hh", language: "C++"},
		{extension: ".hpp", language: "C++"},
		{extension: ".hxx", language: "C++"},
	}

	for _, tt := range tests {
		t.Run(tt.extension, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "header"+tt.extension)
			if err := os.WriteFile(path, []byte("void Hello();\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			tb := Toolbox(NewService(dir, "acme/demo", "main"))
			out, err := tb.Run(
				context.Background(),
				"code_find_symbol",
				marshalToolInput(t, map[string]any{"name": "Hello"}),
			)
			if err != nil {
				t.Fatal(err)
			}
			var got honestyResponse
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("output=%s: %v", out, err)
			}
			if len(got.Analysis.SkippedFiles) != 1 ||
				got.Analysis.SkippedFiles[0].Path != path ||
				got.Analysis.SkippedFiles[0].Language != tt.language {
				t.Fatalf("skipped_files=%v", got.Analysis.SkippedFiles)
			}
			if !strings.Contains(got.Analysis.Limitation, tt.language) {
				t.Fatalf("limitation=%q", got.Analysis.Limitation)
			}
		})
	}
}

func TestToolDescriptionsStateSupportedLanguage(t *testing.T) {
	for _, def := range Toolbox(NewService(t.TempDir(), "", "")).Defs() {
		t.Run(def.Name, func(t *testing.T) {
			if !strings.Contains(def.Description, "Supports Go only") {
				t.Fatalf("description=%q", def.Description)
			}
		})
	}
}

func TestCapabilitiesStateSupportedLanguage(t *testing.T) {
	var got struct {
		SupportedLanguages []string `json:"supported_languages"`
	}
	if err := json.Unmarshal([]byte(CapabilitiesJSON(nil)), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.SupportedLanguages) != 1 || got.SupportedLanguages[0] != "Go" {
		t.Fatalf("supported_languages=%v", got.SupportedLanguages)
	}
}

func TestGoOnlyToolOutputRemainsArray(t *testing.T) {
	dir, _ := writeFixture(t)
	tb := Toolbox(NewService(dir, "acme/demo", "main"))

	for _, tc := range codeToolCases(t, dir) {
		t.Run(tc.name, func(t *testing.T) {
			out, err := tb.Run(context.Background(), tc.name, tc.input)
			if err != nil {
				t.Fatal(err)
			}
			var got []CodeLocation
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("Go-only output changed from the existing array: %s: %v", out, err)
			}
			if len(got) == 0 {
				t.Fatal("expected existing Go result")
			}
		})
	}
}

func TestNoSupportedFilesReportsLimitation(t *testing.T) {
	// A repository with no supported source files must say so explicitly
	// rather than reporting zero symbols (#255).
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# demo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	emptyDir := t.TempDir()
	tb := Toolbox(NewService(dir, "acme/demo", "main"))
	emptyTB := Toolbox(NewService(emptyDir, "acme/demo", "main"))

	for _, tc := range codeToolCases(t, dir) {
		t.Run("non-source-only/"+tc.name, func(t *testing.T) {
			out, err := tb.Run(context.Background(), tc.name, tc.input)
			if err != nil {
				t.Fatal(err)
			}
			var got honestyResponse
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("output must be an honesty response, got %s: %v", out, err)
			}
			if len(got.Results) != 0 {
				t.Fatalf("results=%v want none", got.Results)
			}
			if !strings.Contains(got.Analysis.Limitation, "no Go files were found") {
				t.Fatalf("limitation=%q", got.Analysis.Limitation)
			}
			if len(got.Analysis.AnalysedFiles) != 0 || len(got.Analysis.SkippedFiles) != 0 {
				t.Fatalf("analysed=%v skipped=%v want both empty", got.Analysis.AnalysedFiles, got.Analysis.SkippedFiles)
			}
			if !strings.Contains(out, `"results": []`) {
				t.Fatalf("honesty response must render results as an empty array, not null: %s", out)
			}
		})
	}
	for _, tc := range codeToolCases(t, emptyDir) {
		t.Run("empty/"+tc.name, func(t *testing.T) {
			out, err := emptyTB.Run(context.Background(), tc.name, tc.input)
			if err != nil {
				t.Fatal(err)
			}
			var got honestyResponse
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("output must be an honesty response, got %s: %v", out, err)
			}
			if len(got.Results) != 0 {
				t.Fatalf("results=%v want none", got.Results)
			}
			if !strings.Contains(got.Analysis.Limitation, "no Go files were found") {
				t.Fatalf("limitation=%q", got.Analysis.Limitation)
			}
		})
	}
}

func TestCancelledIndexWalkAbortsWithoutPartialResults(t *testing.T) {
	dir, _ := writeFixture(t)

	// Pre-cancelled: every tool aborts promptly with the cancellation error
	// and never emits a partial result as complete (#255).
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tb := Toolbox(NewService(dir, "acme/demo", "main"))
	for _, tc := range codeToolCases(t, dir) {
		t.Run("pre-cancelled/"+tc.name, func(t *testing.T) {
			out, err := tb.Run(ctx, tc.name, tc.input)
			if err == nil {
				t.Fatalf("cancelled %s returned nil error; output=%s", tc.name, out)
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled %s err=%v want context.Canceled", tc.name, err)
			}
			if out != "" {
				t.Fatalf("cancelled %s returned partial output %q; a cancelled build must not read as complete", tc.name, out)
			}
		})
	}

	// In-flight: cancel while the walk is running; the walk must abort with
	// context.Canceled rather than returning a partial result set.
	big := t.TempDir()
	const fileCount = 3000
	for i := 0; i < fileCount; i++ {
		name := fmt.Sprintf("f%04d", i)
		if err := os.WriteFile(filepath.Join(big, name+".go"), []byte("package p\nfunc "+name+"() {}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	svc := NewService(big, "acme/demo", "main")
	walkCtx, walkCancel := context.WithCancel(context.Background())
	type walkResult struct {
		out string
		err error
	}
	done := make(chan walkResult, 1)
	go func() {
		out, err := Toolbox(svc).Run(walkCtx, "code_find_symbol", json.RawMessage(`{"name":"f0000"}`))
		done <- walkResult{out: out, err: err}
	}()
	time.Sleep(2 * time.Millisecond)
	walkCancel()
	r := <-done
	if r.err == nil {
		// The walk won the race and finished before cancellation; it must
		// then be complete, never partial.
		if !strings.Contains(r.out, "f0000") {
			t.Fatalf("completed walk returned partial output: %s", r.out)
		}
		return
	}
	if !errors.Is(r.err, context.Canceled) {
		t.Fatalf("in-flight walk err=%v want context.Canceled", r.err)
	}
	if r.out != "" {
		t.Fatalf("cancelled in-flight build emitted partial output %q; a cancelled build must not read as complete", r.out)
	}
}

func TestConcurrentToolCallsShareServiceSafely(t *testing.T) {
	dir := t.TempDir()
	paths := make([]string, 0, 8)
	for i := 0; i < 8; i++ {
		name := fmt.Sprintf("F%d", i)
		p := filepath.Join(dir, fmt.Sprintf("f%d.go", i))
		if err := os.WriteFile(p, []byte("package p\nfunc "+name+"() {}\nfunc call"+name+"() { "+name+"() }\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	testPath := filepath.Join(dir, "f0_test.go")
	if err := os.WriteFile(testPath, []byte("package p\nfunc TestF0(t *testing.T) { _ = F0() }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tsPath := writeTypeScriptFixture(t, dir)

	svc := NewService(dir, "acme/demo", "main")
	ctx := context.Background()
	var mu sync.Mutex
	var failures []string
	report := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		failures = append(failures, fmt.Sprintf(format, args...))
	}

	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 5; i++ {
				n := (g + i) % 8
				name := fmt.Sprintf("F%d", n)
				path := paths[n]
				if locs, err := svc.FindSymbol(ctx, name, ""); err != nil {
					report("find %s: %v", name, err)
				} else if len(locs) != 1 || locs[0].Symbol != name {
					report("find %s: %v", name, locs)
				}
				if locs, err := svc.References(ctx, path, 0, name); err != nil {
					report("refs %s: %v", name, err)
				} else if len(locs) < 2 {
					report("refs %s: %v want >=2", name, locs)
				}
				if locs, err := svc.Structure(ctx, path); err != nil {
					report("structure %s: %v", path, err)
				} else if len(locs) != 2 {
					report("structure %s: %v want 2 symbols", path, locs)
				}
				if locs, err := svc.BlastRadius(ctx, path, name); err != nil {
					report("blast %s: %v", name, err)
				} else if len(locs) == 0 {
					report("blast %s: empty", name)
				}
				if locs, err := svc.SuggestTests(ctx, name); err != nil {
					report("suggest %s: %v", name, err)
				} else if len(locs) != 0 && locs[0].Symbol != name {
					report("suggest %s: %v", name, locs)
				}
				// Concurrent cache writes on the same path while reads run.
				if err := svc.IndexFile(path); err != nil {
					report("index %s: %v", path, err)
				}
			}
		}(g)
	}
	wg.Wait()
	if len(failures) > 0 {
		t.Fatalf("concurrent shared-service use failed:\n%s", strings.Join(failures, "\n"))
	}
	// The unsupported file must still be reported in a mixed repo under load.
	tb := Toolbox(svc)
	out, err := tb.Run(ctx, "code_find_symbol", marshalToolInput(t, map[string]any{"name": "F0"}))
	if err != nil {
		t.Fatal(err)
	}
	var got honestyResponse
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output=%s: %v", out, err)
	}
	if len(got.Analysis.SkippedFiles) != 1 || got.Analysis.SkippedFiles[0].Path != tsPath {
		t.Fatalf("skipped_files=%v", got.Analysis.SkippedFiles)
	}
}

func TestGoResultsAreByteIdenticalToPinnedFixture(t *testing.T) {
	// Pinned fixture repo: Go output is byte-identical to the checked-in
	// baseline. Any change to Go parsing or result shape fails here (#255).
	svc := NewService("testdata/gorepo", "acme/demo", "main")
	tb := Toolbox(svc)
	cases := []struct {
		tool  string
		input string
		want  string
	}{
		{
			tool:  "code_find_symbol",
			input: `{"name":"Hello"}`,
			want: `[
  {
    "repo": "acme/demo",
    "ref": "main",
    "path": "testdata/gorepo/hello.go",
    "start_line": 4,
    "end_line": 4,
    "symbol": "Hello",
    "kind": "func",
    "source": "text-fallback"
  }
]`,
		},
		{
			tool:  "code_find_symbol",
			input: `{"name":"Answer","kind":"const"}`,
			want: `[
  {
    "repo": "acme/demo",
    "ref": "main",
    "path": "testdata/gorepo/hello.go",
    "start_line": 6,
    "end_line": 6,
    "symbol": "Answer",
    "kind": "const",
    "source": "text-fallback"
  }
]`,
		},
		{
			tool:  "code_structure",
			input: `{"path":"testdata/gorepo/hello.go"}`,
			want: `[
  {
    "repo": "acme/demo",
    "ref": "main",
    "path": "testdata/gorepo/hello.go",
    "start_line": 4,
    "end_line": 4,
    "symbol": "Hello",
    "kind": "func",
    "source": "text-fallback"
  },
  {
    "repo": "acme/demo",
    "ref": "main",
    "path": "testdata/gorepo/hello.go",
    "start_line": 6,
    "end_line": 6,
    "symbol": "Answer",
    "kind": "const",
    "source": "text-fallback"
  },
  {
    "repo": "acme/demo",
    "ref": "main",
    "path": "testdata/gorepo/hello.go",
    "start_line": 8,
    "end_line": 8,
    "symbol": "Greeter",
    "kind": "type",
    "source": "text-fallback"
  },
  {
    "repo": "acme/demo",
    "ref": "main",
    "path": "testdata/gorepo/hello.go",
    "start_line": 10,
    "end_line": 10,
    "symbol": "Greet",
    "kind": "method",
    "source": "text-fallback"
  },
  {
    "repo": "acme/demo",
    "ref": "main",
    "path": "testdata/gorepo/hello.go",
    "start_line": 12,
    "end_line": 12,
    "symbol": "CallHello",
    "kind": "func",
    "source": "text-fallback"
  }
]`,
		},
		{
			tool:  "code_references",
			input: `{"symbol":"Hello"}`,
			want: `[
  {
    "repo": "acme/demo",
    "ref": "main",
    "path": "testdata/gorepo/hello.go",
    "start_line": 4,
    "end_line": 4,
    "symbol": "Hello",
    "kind": "ref",
    "source": "text-fallback"
  },
  {
    "repo": "acme/demo",
    "ref": "main",
    "path": "testdata/gorepo/hello.go",
    "start_line": 10,
    "end_line": 10,
    "symbol": "Hello",
    "kind": "ref",
    "source": "text-fallback"
  },
  {
    "repo": "acme/demo",
    "ref": "main",
    "path": "testdata/gorepo/hello.go",
    "start_line": 12,
    "end_line": 12,
    "symbol": "Hello",
    "kind": "ref",
    "source": "text-fallback"
  },
  {
    "repo": "acme/demo",
    "ref": "main",
    "path": "testdata/gorepo/hello_test.go",
    "start_line": 3,
    "end_line": 3,
    "symbol": "Hello",
    "kind": "ref",
    "source": "text-fallback"
  }
]`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			out, err := tb.Run(context.Background(), tc.tool, json.RawMessage(tc.input))
			if err != nil {
				t.Fatal(err)
			}
			if out != tc.want {
				t.Fatalf("Go output changed:\ngot:\n%s\nwant:\n%s", out, tc.want)
			}
		})
	}
}
