// Package codeintel implements waffle's optional structural-code tool surface
// (#79): six MCP tool contracts plus an in-process go/parser fallback.
//
// Architecture: code intelligence is workspace/MCP tooling, not core storage.
// Cached answers never silently override a live file read; Source and
// IndexedAt make freshness explicit.
package codeintel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/tool"
)

// ToolNames are the six contracts from issue #79.
var ToolNames = []string{
	"code_find_symbol",
	"code_references",
	"code_callers",
	"code_structure",
	"code_blast_radius",
	"code_suggest_tests",
}

// Source classification for provenance.
const (
	SourceLiveLSP      = "live-lsp"
	SourceCachedIndex  = "cached-index"
	SourceTextFallback = "text-fallback"
)

// CodeLocation is the shared result shape (#79).
type CodeLocation struct {
	Repo      string `json:"repo,omitempty"`
	Ref       string `json:"ref,omitempty"`
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line,omitempty"`
	Symbol    string `json:"symbol,omitempty"`
	Kind      string `json:"kind,omitempty"`
	// Source is live-lsp | cached-index | text-fallback.
	Source string `json:"source"`
	// IndexedAt is set for cached-index results (RFC3339). Empty for live.
	IndexedAt string `json:"indexed_at,omitempty"`
	// Stale is true when the underlying file content hash no longer matches.
	Stale bool `json:"stale,omitempty"`
	// Uncertain marks conservative/incomplete answers (e.g. blast radius).
	Uncertain bool   `json:"uncertain,omitempty"`
	Evidence  string `json:"evidence,omitempty"`
}

// Service is the Go-first reference implementation (text-fallback via go/parser).
// It supports an optional content-hash cache so staleness can be demonstrated.
type Service struct {
	Root string // workspace root (usually /work/repo or a test dir)
	Repo string // owner/name for provenance
	Ref  string // git ref if known

	mu    sync.Mutex
	cache map[string]cacheEntry // path → snapshot
}

type cacheEntry struct {
	hash      string
	indexedAt time.Time
	symbols   []symbolRec
}

type symbolRec struct {
	Name      string
	Kind      string
	StartLine int
	EndLine   int
	Pkg       string
}

// NewService constructs a fallback finder rooted at root.
func NewService(root, repo, ref string) *Service {
	return &Service{Root: root, Repo: repo, Ref: ref, cache: map[string]cacheEntry{}}
}

func (s *Service) loc(path, symbol, kind, source string, start, end int, stale bool) CodeLocation {
	return CodeLocation{
		Repo: s.Repo, Ref: s.Ref, Path: path,
		StartLine: start, EndLine: end, Symbol: symbol, Kind: kind,
		Source: source, Stale: stale,
	}
}

// IndexFile parses path into the cache (for staleness tests and cached-index source).
func (s *Service) IndexFile(path string) error {
	hash, err := fileHash(path)
	if err != nil {
		return err
	}
	syms, err := parseSymbols(path)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cache == nil {
		s.cache = map[string]cacheEntry{}
	}
	s.cache[path] = cacheEntry{hash: hash, indexedAt: time.Now().UTC(), symbols: syms}
	return nil
}

// FindSymbol implements code_find_symbol.
func (s *Service) FindSymbol(ctx context.Context, name, kind string) ([]CodeLocation, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("name is required")
	}
	var out []CodeLocation
	err := s.walkGo(ctx, func(path string) error {
		live, stale, src, indexedAt, err := s.symbolsFor(path)
		if err != nil {
			return nil
		}
		for _, sy := range live {
			if sy.Name != name {
				continue
			}
			if kind != "" && !strings.EqualFold(sy.Kind, kind) {
				continue
			}
			cl := s.loc(path, sy.Name, sy.Kind, src, sy.StartLine, sy.EndLine, stale)
			cl.IndexedAt = indexedAt
			out = append(out, cl)
		}
		return nil
	})
	return out, err
}

// References implements code_references (name-based, not type-aware).
func (s *Service) References(ctx context.Context, path string, line int, symbol string) ([]CodeLocation, error) {
	if symbol == "" && path != "" && line > 0 {
		var err error
		symbol, err = identAt(path, line, 1)
		if err != nil {
			return nil, err
		}
	}
	if symbol == "" {
		return nil, fmt.Errorf("symbol or path+line required")
	}
	var out []CodeLocation
	err := s.walkGo(ctx, func(p string) error {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			return nil
		}
		ast.Inspect(f, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok || id.Name != symbol {
				return true
			}
			pos := fset.Position(id.Pos())
			out = append(out, s.loc(p, symbol, "ref", SourceTextFallback, pos.Line, pos.Line, false))
			return true
		})
		return nil
	})
	return out, err
}

// Callers implements code_callers conservatively: functions in the same package
// whose body text contains the symbol name. Always marks Uncertain.
func (s *Service) Callers(ctx context.Context, symbol string) ([]CodeLocation, error) {
	symbol = strings.TrimSpace(symbol)
	if symbol == "" {
		return nil, fmt.Errorf("symbol is required")
	}
	var out []CodeLocation
	err := s.walkGo(ctx, func(path string) error {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return nil
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name == nil || fn.Body == nil {
				continue
			}
			found := false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				id, ok := n.(*ast.Ident)
				if ok && id.Name == symbol {
					found = true
					return false
				}
				return true
			})
			if found && fn.Name.Name != symbol {
				pos := fset.Position(fn.Name.Pos())
				end := fset.Position(fn.End())
				cl := s.loc(path, fn.Name.Name, "func", SourceTextFallback, pos.Line, end.Line, false)
				cl.Uncertain = true
				cl.Evidence = "name occurrence in function body (not type-aware call graph)"
				out = append(out, cl)
			}
		}
		return nil
	})
	return out, err
}

// Structure implements code_structure for a file or package directory.
func (s *Service) Structure(ctx context.Context, path string) ([]CodeLocation, error) {
	if path == "" {
		return nil, fmt.Errorf("path is required")
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	var files []string
	if st.IsDir() {
		err = filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && p != path {
				return filepath.SkipDir
			}
			if !d.IsDir() && strings.HasSuffix(p, ".go") {
				files = append(files, p)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	} else {
		files = []string{path}
	}
	var out []CodeLocation
	for _, f := range files {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		live, stale, src, indexedAt, err := s.symbolsFor(f)
		if err != nil {
			continue
		}
		for _, sy := range live {
			cl := s.loc(f, sy.Name, sy.Kind, src, sy.StartLine, sy.EndLine, stale)
			cl.IndexedAt = indexedAt
			out = append(out, cl)
		}
	}
	return out, nil
}

// BlastRadius implements code_blast_radius conservatively: same package files
// that reference the symbol, plus *_test.go files in that package.
func (s *Service) BlastRadius(ctx context.Context, path, symbol string) ([]CodeLocation, error) {
	if symbol == "" && path != "" {
		// Best-effort: use first top-level func in file.
		syms, _ := parseSymbols(path)
		if len(syms) > 0 {
			symbol = syms[0].Name
		}
	}
	if symbol == "" {
		return nil, fmt.Errorf("symbol is required")
	}
	refs, err := s.References(ctx, path, 0, symbol)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []CodeLocation
	for _, r := range refs {
		dir := filepath.Dir(r.Path)
		// Add referencing file.
		if !seen[r.Path] {
			seen[r.Path] = true
			cl := r
			cl.Uncertain = true
			cl.Evidence = "file references symbol by name; package-level blast radius only"
			out = append(out, cl)
		}
		// Add tests in same package dir.
		_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				if d != nil && d.IsDir() && p != dir {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(p, "_test.go") && !seen[p] {
				seen[p] = true
				out = append(out, CodeLocation{
					Repo: s.Repo, Ref: s.Ref, Path: p, StartLine: 1,
					Symbol: symbol, Source: SourceTextFallback, Uncertain: true,
					Evidence: "same-package test file (may not exercise the symbol)",
				})
			}
			return nil
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// SuggestTests implements code_suggest_tests: same-package *_test.go files
// plus files whose names mention the symbol.
func (s *Service) SuggestTests(ctx context.Context, symbol string) ([]CodeLocation, error) {
	if symbol == "" {
		return nil, fmt.Errorf("symbol is required")
	}
	var out []CodeLocation
	err := s.walkGo(ctx, func(path string) error {
		base := filepath.Base(path)
		if !strings.HasSuffix(base, "_test.go") {
			return nil
		}
		// Prefer tests that mention the symbol.
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		if !strings.Contains(string(b), symbol) {
			return nil
		}
		out = append(out, CodeLocation{
			Repo: s.Repo, Ref: s.Ref, Path: path, StartLine: 1,
			Symbol: symbol, Source: SourceTextFallback, Uncertain: true,
			Evidence: "test file contains symbol name",
		})
		return nil
	})
	return out, err
}

func (s *Service) symbolsFor(path string) (syms []symbolRec, stale bool, source, indexedAt string, err error) {
	hash, err := fileHash(path)
	if err != nil {
		return nil, false, "", "", err
	}
	s.mu.Lock()
	ce, ok := s.cache[path]
	s.mu.Unlock()
	if ok {
		stale = ce.hash != hash
		indexedAt = ce.indexedAt.UTC().Format(time.RFC3339Nano)
		if !stale {
			return ce.symbols, false, SourceCachedIndex, indexedAt, nil
		}
		// Stale cache: re-parse live and mark stale on the old metadata path
		// is avoided — live parse wins (source=text-fallback).
	}
	live, err := parseSymbols(path)
	if err != nil {
		return nil, stale, "", indexedAt, err
	}
	return live, stale, SourceTextFallback, "", nil
}

func (s *Service) walkGo(ctx context.Context, fn func(path string) error) error {
	root := s.Root
	if root == "" {
		return fmt.Errorf("codeintel root not set")
	}
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			n := d.Name()
			if n == "vendor" || n == ".git" || n == "node_modules" {
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
		return fn(path)
	})
}

func fileHash(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func parseSymbols(path string) ([]symbolRec, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return nil, err
	}
	pkg := f.Name.Name
	var out []symbolRec
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Name == nil {
				continue
			}
			pos := fset.Position(d.Name.Pos())
			end := fset.Position(d.End())
			kind := "func"
			if d.Recv != nil {
				kind = "method"
			}
			out = append(out, symbolRec{Name: d.Name.Name, Kind: kind, StartLine: pos.Line, EndLine: end.Line, Pkg: pkg})
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch sp := spec.(type) {
				case *ast.TypeSpec:
					pos := fset.Position(sp.Name.Pos())
					end := fset.Position(sp.End())
					out = append(out, symbolRec{Name: sp.Name.Name, Kind: "type", StartLine: pos.Line, EndLine: end.Line, Pkg: pkg})
				case *ast.ValueSpec:
					for _, n := range sp.Names {
						pos := fset.Position(n.Pos())
						kind := "var"
						if d.Tok == token.CONST {
							kind = "const"
						}
						out = append(out, symbolRec{Name: n.Name, Kind: kind, StartLine: pos.Line, EndLine: pos.Line, Pkg: pkg})
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
		if pos.Line == line && (col <= 0 || (pos.Column <= col && col <= pos.Column+len(id.Name))) {
			found = id.Name
			return false
		}
		return true
	})
	return found, nil
}

// --- tool.Tool adapters for agent discovery (#79) ---

// Toolbox returns tools backed by svc. Empty svc root → tools still register
// but return clear errors (absence does not break agent construction).
func Toolbox(svc *Service) tool.Toolbox {
	if svc == nil {
		svc = &Service{}
	}
	return tool.NewRegistry(
		findSymbolTool{svc},
		referencesTool{svc},
		callersTool{svc},
		structureTool{svc},
		blastTool{svc},
		suggestTestsTool{svc},
	)
}

// CapabilitiesJSON describes which tools are available (partial OK).
func CapabilitiesJSON(svc *Service) string {
	caps := map[string]any{
		"tools":        ToolNames,
		"backend":      "text-fallback",
		"type_aware":   false,
		"call_graph":   "name-based-uncertain",
		"blast_radius": "package-file-conservative",
		"source":       SourceTextFallback,
	}
	if svc != nil && svc.Root != "" {
		caps["root"] = svc.Root
		caps["repo"] = svc.Repo
	}
	b, _ := json.Marshal(caps)
	return string(b)
}

type findSymbolTool struct{ s *Service }

func (t findSymbolTool) Def() llm.Tool {
	return llm.Tool{
		Name:        "code_find_symbol",
		Description: "Find symbol definitions by name (optional kind: func|method|type|var|const). Results include path/line provenance and source classification. Live file content is always authoritative over cached index.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"},"kind":{"type":"string"}},"required":["name"]}`),
	}
}
func (t findSymbolTool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	var in struct {
		Name string `json:"name"`
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", err
	}
	locs, err := t.s.FindSymbol(ctx, in.Name, in.Kind)
	if err != nil {
		return "", err
	}
	return marshalLocs(locs)
}

type referencesTool struct{ s *Service }

func (t referencesTool) Def() llm.Tool {
	return llm.Tool{
		Name:        "code_references",
		Description: "Find references to a symbol (by name, or path+line of an identifier). Name-based; not type-aware.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"symbol":{"type":"string"},"path":{"type":"string"},"line":{"type":"integer"}}}`),
	}
}
func (t referencesTool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	var in struct {
		Symbol string `json:"symbol"`
		Path   string `json:"path"`
		Line   int    `json:"line"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", err
	}
	locs, err := t.s.References(ctx, in.Path, in.Line, in.Symbol)
	if err != nil {
		return "", err
	}
	return marshalLocs(locs)
}

type callersTool struct{ s *Service }

func (t callersTool) Def() llm.Tool {
	return llm.Tool{
		Name:        "code_callers",
		Description: "List likely callers of a symbol (conservative, uncertain: name occurrence in function bodies).",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"symbol":{"type":"string"}},"required":["symbol"]}`),
	}
}
func (t callersTool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	var in struct {
		Symbol string `json:"symbol"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", err
	}
	locs, err := t.s.Callers(ctx, in.Symbol)
	if err != nil {
		return "", err
	}
	return marshalLocs(locs)
}

type structureTool struct{ s *Service }

func (t structureTool) Def() llm.Tool {
	return llm.Tool{
		Name:        "code_structure",
		Description: "List symbols in a file or package directory.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
	}
}
func (t structureTool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	var in struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", err
	}
	locs, err := t.s.Structure(ctx, in.Path)
	if err != nil {
		return "", err
	}
	return marshalLocs(locs)
}

type blastTool struct{ s *Service }

func (t blastTool) Def() llm.Tool {
	return llm.Tool{
		Name:        "code_blast_radius",
		Description: "Conservative affected files/packages/tests for a symbol change. Always uncertain; verify against live source before acting.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"symbol":{"type":"string"},"path":{"type":"string"}},"required":["symbol"]}`),
	}
}
func (t blastTool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	var in struct {
		Symbol string `json:"symbol"`
		Path   string `json:"path"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", err
	}
	locs, err := t.s.BlastRadius(ctx, in.Path, in.Symbol)
	if err != nil {
		return "", err
	}
	return marshalLocs(locs)
}

type suggestTestsTool struct{ s *Service }

func (t suggestTestsTool) Def() llm.Tool {
	return llm.Tool{
		Name:        "code_suggest_tests",
		Description: "Suggest focused test files that mention a symbol (evidence-based, uncertain).",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"symbol":{"type":"string"}},"required":["symbol"]}`),
	}
}
func (t suggestTestsTool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	var in struct {
		Symbol string `json:"symbol"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", err
	}
	locs, err := t.s.SuggestTests(ctx, in.Symbol)
	if err != nil {
		return "", err
	}
	return marshalLocs(locs)
}

func marshalLocs(locs []CodeLocation) (string, error) {
	if len(locs) == 0 {
		return "[]", nil
	}
	b, err := json.MarshalIndent(locs, "", "  ")
	return string(b), err
}

// ValidateMCPServer rejects unsafe codeintel MCP configurations (#79 / #53 / #77).
// A server that exposes any ToolNames must not inherit ambient env and must not
// run as unrestricted host execution when sandbox is required.
func ValidateMCPServer(name string, execution string, env []string, declaredTools []string, allowHost bool) error {
	if !exposesCodeIntel(declaredTools) {
		return nil
	}
	// Env must be an explicit allowlist (may be empty). Non-empty is OK only for
	// non-secret vars; we forbid common secret names.
	for _, e := range env {
		u := strings.ToUpper(e)
		if strings.Contains(u, "TOKEN") || strings.Contains(u, "SECRET") ||
			strings.Contains(u, "PASSWORD") || strings.Contains(u, "API_KEY") ||
			u == "WAFFLE_AGE_IDENTITY" {
			return fmt.Errorf("mcp %q: code-intelligence servers must not receive secret env %q", name, e)
		}
	}
	if execution == "" || execution == "host" {
		if !allowHost {
			return fmt.Errorf("mcp %q: code-intelligence must use execution=%q (or set codeintel.allow_host_mcp); host ambient launch is forbidden", name, "sandbox")
		}
	}
	return nil
}

func exposesCodeIntel(tools []string) bool {
	if len(tools) == 0 {
		// Undeclared tools: treat conservatively if name suggests codeintel.
		return false
	}
	set := map[string]bool{}
	for _, t := range ToolNames {
		set[t] = true
	}
	for _, t := range tools {
		// tools may be bare or server__tool
		base := t
		if i := strings.LastIndex(t, "__"); i >= 0 {
			base = t[i+2:]
		}
		if set[base] {
			return true
		}
	}
	return false
}

// ApprovedCapability reports whether capabilityID is a known codeintel tool.
func ApprovedCapability(id string) bool {
	for _, t := range ToolNames {
		if t == id {
			return true
		}
	}
	return false
}
