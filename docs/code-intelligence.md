# Code intelligence (#79)

Structural code tools belong **behind MCP / workspace tooling**, not in
waffle's core storage or runtime. This document is the contract.

## Six tool contracts

| Tool | Purpose |
|------|---------|
| `code_find_symbol` | Definitions matching a name/kind |
| `code_references` | References to a symbol or location |
| `code_callers` | Direct callers where the backend can establish them (may be uncertain) |
| `code_structure` | Symbols in a file or package |
| `code_blast_radius` | Conservative affected files/packages/tests for a change |
| `code_suggest_tests` | Focused verification targets with evidence |

### Result shape (`CodeLocation`)

```json
{
  "repo": "owner/name",
  "ref": "main",
  "path": "pkg/foo.go",
  "start_line": 10,
  "end_line": 20,
  "symbol": "Foo",
  "kind": "func",
  "source": "live-lsp|cached-index|text-fallback",
  "indexed_at": "2026-07-12T00:00:00Z",
  "stale": false,
  "uncertain": false,
  "evidence": "optional note"
}
```

### Source authority and staleness

1. **Live checked-out source is always authoritative** over any index or cache.
2. `source` must be set on every result: `live-lsp`, `cached-index`, or `text-fallback`.
3. `cached-index` results **must** include `indexed_at`. If the file content hash
   no longer matches, set `stale: true` or re-parse live (`text-fallback` / live-lsp).
4. Agents must **read the current file spans** before editing; never trust
   indexed snippets alone.
5. Uncertain analyses (callers, blast radius without types) set `uncertain: true`
   and an `evidence` string — never fabricate certainty.

## Agent discovery

When enabled (`[codeintel] enabled = true`, default on for host builds with a
workspace root or always as the in-process fallback), waffle registers the six
tools on the agent toolbox. Capabilities are explicit via tool defs; partial
MCP implementations are fine — only declared tools appear.

If code intelligence is absent or fails, the agent continues with `search` /
`read_file` / `bash`. Workspaces do **not** require codeintel unless the host
opts in with a required MCP server.

## Sandbox / isolation

Code-intelligence MCP servers read arbitrary repo content and often run
language tooling. They must **not** inherit the gateway's ambient environment
or secrets (#77 / #79):

- Prefer `execution = "sandbox"` (workspace-scoped restricted executor).
- `env` is an allowlist of variable **names**; MCP children receive only
  `PATH` plus allowlisted values via `mcp.BuildProcessEnv` — never
  `os.Environ()`. Secret-like names (`TOKEN`, `SECRET`, `API_KEY`,
  `WAFFLE_AGE_IDENTITY`, …) are rejected for codeintel tool providers.
- **Every MCP launch** goes through `mcp.ConnectRestricted` (restricted
  executor): allowlisted env only, optional working directory, no ambient
  FD inheritance beyond stdio.
- When `execution = "sandbox"`:
  - **Host agent groups:** process runs on the host under
    `ConnectRestricted` with `Dir` set to the workspace/work dir
    (`Mode=restricted`).
  - **Docker agent groups:** command is rewritten to
    `docker run -i --rm --network none` (or `[sandbox] network`), with
    `-v workDir:/work -w /work` when known, only allowlisted `-e` pairs
    from `BuildProcessEnv`, and the sandbox image (`Mode=sandbox`).
- Host execution (`execution = "host"`) still uses the restricted env path
  but requires explicit `[codeintel] allow_host_mcp = true` for codeintel
  servers. Docker agent groups additionally require `groups` to list that
  group (explicit host opt-in).
- If a codeintel MCP connect fails and `[codeintel] required` is false, the
  agent keeps the in-process `go/parser` text-fallback tools. When required,
  agent build fails.

Repo policy (`WAFFLE.md`) may only **select host-approved capability IDs**
from this list (`FilterCodeIntelCaps` + `ApprovedCapability`); it cannot
name arbitrary executables or widen launch posture. When
`code_intel_caps` is set, non-selected codeintel tools are denied at
workspace open / issue dispatch.

## Reference implementation

`internal/codeintel.Service` is the Go-first **text-fallback** backend
(`go/parser`, not type-aware):

- definitions / references / structure
- conservative callers + blast radius (package/file level, `uncertain`)
- test suggestions from `*_test.go` name hits
- optional content-hash cache for staleness demos

**Supported language: Go only** (state kept in sync with tool descriptions
and `CapabilitiesJSON`). Every `code_*` tool asked about a repo or path in
another language returns an explicit limitation naming the language instead
of an empty result: a Go-only repo returns the plain result array, a
mixed-language repo lists which files were analysed and which were skipped,
and a repo with no supported files says so. Coverage for other languages is
deferred (see #255); a tool that cannot answer a question says so.

Full accuracy: configure an external MCP bridge over `gopls` (or similar)
under the isolation rules above.

## Agent usage convention

For non-trivial edits when tools are present:

1. `code_find_symbol` → 2. `code_references` / `code_callers` / `code_blast_radius`
→ 3. read live source spans → 4. edit → 5. `code_suggest_tests` + focused verify
→ 6. fall back to broader `search`/tests when structural data is absent.
