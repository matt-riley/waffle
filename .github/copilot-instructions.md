# Waffle Copilot Instructions

## Commands

- `mise install` installs Go 1.25.12.
- `mise run build` produces the version-stamped binary at `bin/waffle`.
- `mise run test` runs `go test -race ./...` and zero-network evaluations.
- `mise run vet`, `mise run fmt`, and `mise run lint` run repository checks.
- For focused work, use `go test ./internal/<package> -run '^TestName$' -count=1`.
- Sandbox queue checks are opt-in: `go test -tags=sandbox_stress ./internal/sandbox -run Stress -count=1`; Docker-specific coverage is documented in `docs/sandbox-queue.md`.

## Architecture

`cmd/waffle` is one binary with subcommands for chat, `serve`, sandbox runner,
workspaces, scheduling, and lifecycle operations. `serve` is the sole owner of
background processing, the gateway, and in-memory broker tokens; it holds the
serve-owner lock and starts lifecycle sweepers.

The inbound path is channel adapter -> `internal/gateway` -> `internal/entity`
(`user -> channel group -> agent group -> session`) -> `internal/agent`.
Gateway handlers are concurrent across conversations but serialized per channel
group. Agent profiles and agent groups are trust boundaries: profile selection
must not widen the group’s tool or sandbox policy.

`internal/llm` owns the canonical provider-neutral message and tool types.
Provider packages translate those types; add provider-specific behavior at the
translator boundary, not in the agent loop. The agent itself runs on the host;
only tool execution varies between host and Docker executors.

All durable application state uses SQLite through `internal/store`. The store
opens WAL mode with **one connection**, so avoid long transactions, foreground
maintenance, and per-row commits on normal request paths. Schema changes are
embedded ordered SQL migrations in `internal/store/migrations/`; versions must
be contiguous and migrations must remain safe for existing databases.

Docker sandboxing uses paired, single-writer SQLite queues between host and
`waffle runner`. Sandboxes never receive raw provider or Git credentials:
`internal/broker` provides scoped, short-lived tokens and `internal/secret`
holds the real values. Preserve deny-by-default network, tool, and secret
policies when changing gateway, broker, MCP, sandbox, or workspace code.

Memory spans transcript/FTS sessions, a session-local working set, and
workspace `MEMORY.md` notes. Keep the working set session-scoped; durable notes
are maintained through the memory tools and indexed in SQLite.

## Repository Conventions

- Keep command wiring in `cmd/waffle` and reusable behavior in focused
  `internal` packages. Test files sit beside their package.
- Run `gofmt` on changed Go files. Wrap errors with context using `%w`; avoid
  undocumented `nolint` suppressions.
- Configuration is strict and represents security/trust boundaries. Use
  `config.example.toml` as the configuration contract; do not add permissive
  fallback behavior for unknown or invalid policy/configuration.
- `config.toml`, generated databases, identities, and secrets must not be
  committed. Config stores `secret://` references, never secret values.
- Treat migrations, secret-store changes, and sandbox/network-policy changes as
  compatibility- and security-sensitive. Include migration effects and any
  required operational changes in the PR description.
- Use Conventional Commit-style subjects (`feat:`, `fix:`, `docs:`,
  `build(deps):`). PRs state behavior changes, linked issues, verification, and
  configuration or migration effects.
