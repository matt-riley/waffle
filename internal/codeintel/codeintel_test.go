package codeintel

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGoFallbackSymbols(t *testing.T) {
	dir := t.TempDir()
	src := `package sample

type Widget struct{ N int }

func MakeWidget(n int) Widget { return Widget{N: n} }

const Max = 3
`
	path := filepath.Join(dir, "sample.go")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &GoFallback{Root: dir}
	ctx := context.Background()

	doc, err := f.DocumentSymbols(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	for _, s := range doc {
		names[s.Name] = s.Kind
	}
	if names["Widget"] != "type" || names["MakeWidget"] != "func" || names["Max"] != "const" {
		t.Fatalf("symbols = %+v", names)
	}

	ws, err := f.WorkspaceSymbols(ctx, "widget")
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) < 2 {
		t.Fatalf("workspace symbols = %+v", ws)
	}

	// MakeWidget is on line 5 in the fixture.
	defs, err := f.FindDefinition(ctx, path, 5, 6)
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 1 || defs[0].Name != "MakeWidget" {
		t.Fatalf("defs = %+v", defs)
	}

	hov, err := f.Hover(ctx, path, 5, 6)
	if err != nil || !strings.Contains(hov, "MakeWidget") {
		t.Fatalf("hover = %q %v", hov, err)
	}

	refs, err := f.FindReferences(ctx, path, 5, 6)
	if err != nil || len(refs) < 1 {
		t.Fatalf("refs = %+v %v", refs, err)
	}
}

func TestToolNames(t *testing.T) {
	if len(ToolNames) != 6 {
		t.Fatalf("want 6 tools, got %d", len(ToolNames))
	}
}
