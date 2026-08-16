# Waffle Copilot Instructions

## Commands

- `mise install` installs the pinned toolchain — Go, Node, and pnpm. `mise.toml`
  is the authority on versions; `go.mod` declares the Go *language* version,
  which is deliberately a different number from the toolchain pin.
- `mise run build` produces the version-stamped binary at `bin/waffle`.
- `mise run test` regenerates templ components, fails if
  `internal/dashboard/ui/*_templ.go` is dirty, runs the Desk client tests, then
  `go test -race ./...` and the zero-network `waffle eval`.
- `mise run website-check` builds the site and runs its tests; `mise run
  docs-screenshots` regenerates the Desk screenshots used in the docs.
- `mise run vet`, `mise run fmt`, and `mise run lint` run repository checks.
- For focused work, use `go test ./internal/<package> -run '^TestName$' -count=1`.
- Sandbox queue checks are opt-in: `go test -tags=sandbox_stress ./internal/sandbox -run Stress -count=1`; Docker-specific coverage is documented in `website/src/content/docs/docs/under-the-hood/sandbox.md`.

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

## The website (`website/`)

`website/` is an Astro site with two halves that share a brand and almost
nothing else. Review them by different rules; `website/DOCS-PLAN.md` is the
brief they are built to.

- **`/`** — hand-built marketing homepage: Astro components, Tailwind, GSAP.
- **`/docs/`** — Starlight. Content lives in `src/content/docs/docs/`; the extra
  nesting level is what puts the docs under `/docs/` while leaving `/` bespoke.

Conventions that look like defects but are deliberate. Please do not report
these:

- **Docs pages load no Tailwind.** `src/styles/docs.css` intentionally omits it.
  Two CSS resets in one page is how a themed docs site breaks.
- **Brand tokens are duplicated** between `src/styles/global.css` and
  `src/styles/docs.css`. That is deliberate — the two halves share values, not
  machinery — and `tests/site.test.mjs` fails if they ever disagree.
- **`docs.css` mirrors Starlight's own selector lists**, including
  `:root, ::backdrop` and `[data-theme="light"] ::backdrop`, copied from
  `@astrojs/starlight/style/props.css`. Overrides must land on exactly the
  selectors carrying the values they replace; narrowing them leaves backdrops on
  Starlight's stock palette.
- **`tools/dashboard-tests/capture-docs-screenshots.mjs` re-implements a small
  fixture bootstrap** rather than importing it from `tests/desk.spec.mjs`.
  Importing that module would execute its test registrations. Both drive the
  same fixture binary, which is the part that has to agree.

Rules worth enforcing in review:

- **Ginger `#E99A42` must never be text on the paper ground** — it measures
  2.2:1. It is for rules, borders, and fills. On the evening (dark) ground it
  reaches 8.2:1 and may carry text. A test enforces the paper-side ban.
- **At most two cat images per docs page, never two in a row.** Enforced by
  test. It is the guard against mascot fatigue.
- **Every plain-language page ends with a Nerd corner link** into its technical
  counterpart, and every technical page links back up.
- **Tests assert intent, not formatting.** Do not add assertions that pin exact
  whitespace, quote style, or indentation of a source file — a formatter run
  must not fail a test whose subject has not changed.
- **Screenshots are generated, never hand-taken.** They come from the
  deterministic Desk fixture via `mise run docs-screenshots`, and captures are
  reproducible byte-for-byte.
- Cat art comes only from `assets/brand/waffle/`. Never invent a new cat, and
  keep the canon anchors (forehead M, grey-green eyes, pale muzzle, ringed tail).

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
