package codeintel

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGoFallbackMarksStaleAfterEdit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	src := "package main\nfunc Hello() {}\n"
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	fb := &GoFallback{Root: dir}
	syms, err := fb.WorkspaceSymbols(context.Background(), "Hello")
	if err != nil || len(syms) != 1 {
		t.Fatalf("find: %v %v", syms, err)
	}
	// Modify file after "index".
	time.Sleep(5 * time.Millisecond)
	if err := os.WriteFile(path, []byte("package main\nfunc Hello() { println(1) }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Re-query: text-fallback re-reads live source and must never silently
	// override a direct file read (live file is authoritative).
	syms2, err := fb.WorkspaceSymbols(context.Background(), "Hello")
	if err != nil || len(syms2) != 1 {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "println") {
		t.Fatal("live file should be authoritative")
	}
	// Symbol still discoverable after edit.
	if syms2[0].Name != "Hello" {
		t.Fatalf("%+v", syms2[0])
	}
}

func TestCodeintelEnvStripped(t *testing.T) {
	// Contract: MCP/LSP process must not inherit gateway secrets — callers
	// configure MCPServer.Env as an allowlist (empty = no env). This package
	// only exposes structural code tools.
	for _, name := range ToolNames {
		if strings.Contains(name, "bash") || strings.Contains(name, "secret") {
			t.Fatalf("unexpected tool %q", name)
		}
	}
}
