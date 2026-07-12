// Package codeintel defines the six MCP tool contracts for code intelligence
// (#79) and ships a tiny text-fallback Go symbol finder (go/parser) so local
// development works without an external language server.
//
// Optional MCP path: configure an MCP server that implements the tool names
// in ToolNames (for example a gopls or scip-based bridge). When MCP is
// present, host tools with the same names take precedence in tool.Combine
// order — register MCP before the fallback finder if you want LSP results.
package codeintel

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

// ToolNames are the six code-intelligence MCP contracts (#79).
var ToolNames = []string{
	"code_find_definition",
	"code_find_references",
	"code_hover",
	"code_workspace_symbols",
	"code_document_symbols",
	"code_diagnostics",
}

// Location is a file span.
type Location struct {
	Path   string `json:"path"`
	Line   int    `json:"line"` // 1-based
	Col    int    `json:"col"`  // 1-based
	EndLn  int    `json:"end_line,omitempty"`
	EndCol int    `json:"end_col,omitempty"`
	Name   string `json:"name,omitempty"`
	Kind   string `json:"kind,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// Symbol is a named code entity.
type Symbol struct {
	Name     string   `json:"name"`
	Kind     string   `json:"kind"`
	Location Location `json:"location"`
}

// Diagnostic is a problem report.
type Diagnostic struct {
	Location Location `json:"location"`
	Severity string   `json:"severity"` // error | warning | info | hint
	Message  string   `json:"message"`
	Source   string   `json:"source,omitempty"`
}

// Finder is the minimal code-intelligence surface.
type Finder interface {
	FindDefinition(ctx context.Context, path string, line, col int) ([]Location, error)
	FindReferences(ctx context.Context, path string, line, col int) ([]Location, error)
	Hover(ctx context.Context, path string, line, col int) (string, error)
	WorkspaceSymbols(ctx context.Context, query string) ([]Symbol, error)
	DocumentSymbols(ctx context.Context, path string) ([]Symbol, error)
	Diagnostics(ctx context.Context, path string) ([]Diagnostic, error)
}

// GoFallback is a directory-scoped go/parser finder (no type-checking).
// Suitable as a last-resort when no MCP language server is configured.
type GoFallback struct {
	// Root is the directory to walk for .go files.
	Root string
}

// WorkspaceSymbols returns funcs/types/vars whose names contain query
// (case-insensitive). Empty query returns nothing.
func (g *GoFallback) WorkspaceSymbols(ctx context.Context, query string) ([]Symbol, error) {
	query = strings.TrimSpace(strings.ToLower(query))
	if query == "" || g.Root == "" {
		return nil, nil
	}
	var out []Symbol
	err := filepath.WalkDir(g.Root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			base := d.Name()
			if base == "vendor" || base == ".git" || base == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		syms, err := parseFileSymbols(path)
		if err != nil {
			return nil // skip unparsable files
		}
		for _, s := range syms {
			if strings.Contains(strings.ToLower(s.Name), query) {
				out = append(out, s)
			}
		}
		return nil
	})
	return out, err
}

// DocumentSymbols lists top-level declarations in one Go file.
func (g *GoFallback) DocumentSymbols(ctx context.Context, path string) ([]Symbol, error) {
	_ = ctx
	return parseFileSymbols(path)
}

// FindDefinition looks up the identifier at line/col and returns same-file
// declarations with that name (best-effort; not type-aware).
func (g *GoFallback) FindDefinition(ctx context.Context, path string, line, col int) ([]Location, error) {
	_ = ctx
	name, err := identAt(path, line, col)
	if err != nil || name == "" {
		return nil, err
	}
	syms, err := parseFileSymbols(path)
	if err != nil {
		return nil, err
	}
	var locs []Location
	for _, s := range syms {
		if s.Name == name {
			locs = append(locs, s.Location)
		}
	}
	return locs, nil
}

// FindReferences walks Root for the identifier under the cursor.
func (g *GoFallback) FindReferences(ctx context.Context, path string, line, col int) ([]Location, error) {
	name, err := identAt(path, line, col)
	if err != nil || name == "" {
		return nil, err
	}
	var locs []Location
	err = filepath.WalkDir(g.Root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "vendor" || d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			return nil
		}
		ast.Inspect(f, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok || id.Name != name {
				return true
			}
			pos := fset.Position(id.Pos())
			locs = append(locs, Location{Path: p, Line: pos.Line, Col: pos.Column, Name: name})
			return true
		})
		return nil
	})
	return locs, err
}

// Hover returns a short declaration snippet for the identifier at line/col.
func (g *GoFallback) Hover(ctx context.Context, path string, line, col int) (string, error) {
	_ = ctx
	name, err := identAt(path, line, col)
	if err != nil {
		return "", err
	}
	if name == "" {
		return "", nil
	}
	syms, err := parseFileSymbols(path)
	if err != nil {
		return "", err
	}
	for _, s := range syms {
		if s.Name == name {
			return fmt.Sprintf("%s %s at %s:%d", s.Kind, s.Name, s.Location.Path, s.Location.Line), nil
		}
	}
	return "identifier " + name, nil
}

// Diagnostics returns parse errors for a Go file (fallback only).
func (g *GoFallback) Diagnostics(ctx context.Context, path string) ([]Diagnostic, error) {
	_ = ctx
	fset := token.NewFileSet()
	_, err := parser.ParseFile(fset, path, nil, parser.AllErrors)
	if err == nil {
		return nil, nil
	}
	return []Diagnostic{{
		Location: Location{Path: path, Line: 1, Col: 1},
		Severity: "error",
		Message:  err.Error(),
		Source:   "go/parser",
	}}, nil
}

func parseFileSymbols(path string) ([]Symbol, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return nil, err
	}
	var out []Symbol
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Name == nil {
				continue
			}
			pos := fset.Position(d.Name.Pos())
			kind := "func"
			if d.Recv != nil {
				kind = "method"
			}
			out = append(out, Symbol{
				Name:     d.Name.Name,
				Kind:     kind,
				Location: Location{Path: path, Line: pos.Line, Col: pos.Column, Name: d.Name.Name, Kind: kind},
			})
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch sp := spec.(type) {
				case *ast.TypeSpec:
					pos := fset.Position(sp.Name.Pos())
					out = append(out, Symbol{
						Name:     sp.Name.Name,
						Kind:     "type",
						Location: Location{Path: path, Line: pos.Line, Col: pos.Column, Name: sp.Name.Name, Kind: "type"},
					})
				case *ast.ValueSpec:
					for _, n := range sp.Names {
						pos := fset.Position(n.Pos())
						kind := "var"
						if d.Tok == token.CONST {
							kind = "const"
						}
						out = append(out, Symbol{
							Name:     n.Name,
							Kind:     kind,
							Location: Location{Path: path, Line: pos.Line, Col: pos.Column, Name: n.Name, Kind: kind},
						})
					}
				}
			}
		}
	}
	return out, nil
}

func identAt(path string, line, col int) (string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return "", err
	}
	var found string
	ast.Inspect(f, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		pos := fset.Position(id.Pos())
		if pos.Line == line && pos.Column <= col && col <= pos.Column+len(id.Name) {
			found = id.Name
			return false
		}
		return true
	})
	return found, nil
}
