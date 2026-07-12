# Code intelligence (#79)

waffle exposes six code-intelligence **tool contracts**. Implementations may
come from an optional MCP server (preferred for full LSP accuracy) or from
the in-process `internal/codeintel` Go fallback (`go/parser`, no types).

## Tool contracts

| Tool | Purpose | Key inputs | Output |
|------|---------|------------|--------|
| `code_find_definition` | Jump to declaration | `path`, `line`, `col` (1-based) | list of locations |
| `code_find_references` | Find all references | `path`, `line`, `col` | list of locations |
| `code_hover` | Signature / docs at point | `path`, `line`, `col` | markdown/text |
| `code_workspace_symbols` | Fuzzy project-wide symbols | `query` | list of symbols |
| `code_document_symbols` | Outline one file | `path` | list of symbols |
| `code_diagnostics` | Problems for a file | `path` | list of diagnostics |

### Shared types

```json
{
  "location": {
    "path": "string",
    "line": 1,
    "col": 1,
    "end_line": 1,
    "end_col": 1,
    "name": "string",
    "kind": "func|method|type|var|const|…",
    "detail": "string"
  },
  "symbol": {
    "name": "string",
    "kind": "string",
    "location": { "...": "Location" }
  },
  "diagnostic": {
    "location": { "...": "Location" },
    "severity": "error|warning|info|hint",
    "message": "string",
    "source": "string"
  }
}
```

## Optional MCP server

Add an MCP server that implements the six tool names above:

```toml
[[mcp]]
name = "codeintel"
command = "/path/to/codeintel-mcp"
args = ["serve"]
execution = "host"
groups = ["main"]
tools = [
  "code_find_definition",
  "code_find_references",
  "code_hover",
  "code_workspace_symbols",
  "code_document_symbols",
  "code_diagnostics",
]
```

Suggested backends: a thin bridge over `gopls` (stdio LSP), scip indexes, or
any language server that can answer definition/references/hover/symbols.

When both MCP and the Go fallback would register the same name, `tool.Combine`
keeps the **first** registration. Register MCP before the fallback so LSP wins.

## In-process fallback

`internal/codeintel.GoFallback` walks a root directory with `go/parser`:

- **Workspace / document symbols** — funcs, methods, types, consts, vars
- **Find definition** — same-file declaration by identifier name
- **Find references** — name match under Root (not type-aware)
- **Hover** — kind + name + location
- **Diagnostics** — parse errors only

This is intentionally small: enough for local fixture tests and offline use,
not a replacement for gopls.
