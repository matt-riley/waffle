package pluginmcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matt-riley/waffle/internal/plugin"
)

func TestMapStdio(t *testing.T) {
	root := t.TempDir()
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "server"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Bare command, no cwd: cwd defaults to the resolved plugin root.
	bare := plugin.MCPServer{Name: "npx-srv", Type: plugin.MCPTypeStdio, Command: "npx", Args: []string{"mcp"}}
	got, err := MapStdio(bare, root)
	if err != nil {
		t.Fatalf("MapStdio(bare): %v", err)
	}
	if got.Command != "npx" || got.Cwd != resolvedRoot || len(got.Args) != 1 {
		t.Errorf("bare = %+v", got)
	}

	// ./-relative command and cwd resolve within the root.
	rel := plugin.MCPServer{
		Name: "rel", Type: plugin.MCPTypeStdio,
		Command: "./bin/server", Cwd: "./bin",
		Env: map[string]string{"CONFIG": "${PLUGIN_ROOT}/config.json"},
	}
	got, err = MapStdio(rel, root)
	if err != nil {
		t.Fatalf("MapStdio(rel): %v", err)
	}
	if got.Command != filepath.Join(resolvedRoot, "bin", "server") {
		t.Errorf("command = %q, want resolved %q", got.Command, filepath.Join(resolvedRoot, "bin", "server"))
	}
	if got.Cwd != filepath.Join(resolvedRoot, "bin") {
		t.Errorf("cwd = %q", got.Cwd)
	}
	if got.EnvVars["CONFIG"] != "${PLUGIN_ROOT}/config.json" {
		t.Errorf("env = %v", got.EnvVars)
	}

	// ${PLUGIN_ROOT} cwd passes through for #392 expansion.
	placeholder := plugin.MCPServer{Name: "p", Type: plugin.MCPTypeStdio, Command: "npx", Cwd: "${PLUGIN_DATA}/state"}
	got, err = MapStdio(placeholder, root)
	if err != nil {
		t.Fatalf("MapStdio(placeholder): %v", err)
	}
	if got.Cwd != "${PLUGIN_DATA}/state" {
		t.Errorf("cwd = %q, want placeholder untouched", got.Cwd)
	}

	// Non-./ relative and absolute commands are rejected defensively.
	escape := plugin.MCPServer{Name: "e", Type: plugin.MCPTypeStdio, Command: "../bin/server"}
	if _, err := MapStdio(escape, root); err == nil || !strings.Contains(err.Error(), "bare name") {
		t.Errorf("escaping command error = %v, want bare-name rejection", err)
	}
	absl := plugin.MCPServer{Name: "a", Type: plugin.MCPTypeStdio, Command: "/bin/server"}
	if _, err := MapStdio(absl, root); err == nil {
		t.Error("absolute command accepted")
	}

	// A ./-relative command whose symlink escapes the root is rejected by
	// ResolveWithin containment.
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "server"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "server"), filepath.Join(root, "bin", "escape-link")); err != nil {
		t.Fatal(err)
	}
	symlinkEscape := plugin.MCPServer{Name: "s", Type: plugin.MCPTypeStdio, Command: "./bin/escape-link"}
	if _, err := MapStdio(symlinkEscape, root); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Errorf("symlink escape error = %v, want containment rejection", err)
	}
}

func TestMapHTTP(t *testing.T) {
	remote := plugin.MCPServer{
		Name: "remote", Type: plugin.MCPTypeStreamableHTTP,
		URL:     "https://deploy.example.com/mcp",
		Headers: map[string]string{"X-Tenant": "public-tenant"},
	}
	url, opts, err := MapHTTP(remote)
	if err != nil {
		t.Fatalf("MapHTTP: %v", err)
	}
	if url != "https://deploy.example.com/mcp" {
		t.Errorf("url = %q", url)
	}
	if opts.Headers.Get("X-Tenant") != "public-tenant" {
		t.Errorf("headers = %v", opts.Headers)
	}

	sse := plugin.MCPServer{Name: "legacy", Type: plugin.MCPTypeSSE, URL: "https://x/sse"}
	if _, _, err := MapHTTP(sse); err == nil {
		t.Error("sse entry mapped, want error (unsupported transport)")
	}
}

func TestMapStdioProducesRuntimeServer(t *testing.T) {
	root := t.TempDir()
	got, err := MapStdio(plugin.MCPServer{Name: "s", Type: plugin.MCPTypeStdio, Command: "npx"}, root)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "s" || got.Command != "npx" || got.Cwd == "" {
		t.Errorf("mapped = %+v", got)
	}
}
