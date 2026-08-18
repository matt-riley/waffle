# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project overview

Waffle is a personal AI agent written in Go: one binary (`waffle`) containing the agent loop, a messaging gateway (Telegram), a terminal chat TUI, Waffle Desk (a loopback web cockpit), and a provider-agnostic LLM layer. It serves exactly one owner, runs on that owner's own hardware, and keeps sandbox/network/tool policy deny-by-default throughout. Status is developer preview: config keys, flags, and SQLite migrations can still change.

`AGENTS.md` carries the same conventions in the vendor-neutral format; keep the two consistent when either changes. `CONTEXT.md` is the operations vocabulary (First Deployment, Managed Setup, Installed vs Ready, Provider Connection, Model Alias) — use those terms in operator-facing text.

## Commands

`mise` owns the pinned toolchain (Go, Node, pnpm — the versions live in `mise.toml`); `go.mod` declares the Go language version separately, and the two Go numbers are deliberately not the same. `mise install` installs it; `mise tasks` lists everything. The tasks that matter:

- `mise run build` — version-stamped `bin/waffle`.
- `mise run test` — regenerates templ components, fails if `internal/dashboard/ui/*_templ.go` is dirty, runs the Desk client tests, then `go test -race ./...` and the zero-network `waffle eval`.
- `mise run fmt`, `mise run vet`, `mise run lint` — gofmt check, `go vet`, `golangci-lint`.
- `mise run dashboard-generate` — `templ generate` for `internal/dashboard/ui`. Generated `_templ.go` files are committed; regenerate and commit them with any `.templ` change or CI fails.
- `mise run dashboard-client-test` — `node --test` over the dependency-free Desk browser client (`*_client_test.mjs`).
- `mise run dashboard-check` — full browser gate (`dashboard-install` + Playwright/Chrome tests in `tools/dashboard-tests`).
- `mise run brand-check`, `mise run brand-rig-check` — brand raster/rig manifest validation.

Focused iteration: `go test ./internal/agent -run TestName`. CI shards the race suite five ways via `scripts/ci-test-shard.sh` (`internal/workspace` alone is ~half the race time and gets two runners); the weights live in `scripts/ci-test-*-weights.tsv` and should be refreshed when package timings shift materially.

Run locally: `go run ./cmd/waffle setup` (secret identity, provider enrollment, `[agent.profile.main]`, optional Desk), then `go run ./cmd/waffle chat`. Manual alternative: `./waffle secret init`, then `waffle provider add` (or `printf '%s' sk-ant-... | ./waffle secret set anthropic/api-key`), then chat.

Opt-in test tags, documented in `website/src/content/docs/docs/under-the-hood/sandbox.md`:
`go test -tags=sandbox_stress ./internal/sandbox -run Stress -count=1` (add `-tags=sandbox_docker` when Docker is available). Live provider evals require `WAFFLE_EVAL_LIVE=1` and a configured provider; they skip otherwise.

## Architecture

`cmd/waffle` is a single binary with subcommands: `setup`, `chat`, `serve`, `status`, `pair`, `ws`, `cron`, `session`, `forget`, `usage`, `pause`/`resume`, `secret`, `mcp`, `provider`, `candidates`, `backup`/`restore`, `doctor`, `eval`, `skills`, `learn`, `upgrade`/`rollback`, `completion`, `runner` (the in-container entrypoint), `version`. State lives in `$WAFFLE_HOME` (default `~/.waffle`).

`serve` is the sole owner of background processing, the gateway, and in-memory broker tokens. It holds the serve-owner lock (`~/.waffle/serve.lock`, PID + heartbeat via `internal/instance`, 5s heartbeat / 30s stale) and starts the lifecycle sweepers. A second `serve` refuses to start; a stale lock is reclaimed once its PID is dead.

**Inbound path**: channel adapter (`internal/channel`, `internal/channel/telegram`) → `internal/gateway` → `internal/entity` (`user → channel group → agent group → session`) → `internal/agent`. Gateway handlers run concurrently across conversations but are serialized per channel group.

**Agent assembly**: `internal/agentbuild` is the single composition root — group policy → action policy engine → host tools (memory/workset/spill/PR) → sandbox start → MCP `ConnectRestricted` → codeintel → subagent wrapper → `agent.Agent`. Add new agent surfaces through the Builder; `cmd` keeps flags, open/close, and wiring only.

**Trust boundaries**: agent profiles (`internal/agent`, `[agent.profile.*]`) and agent groups (`[agent.group.*]`) are trust boundaries — system prompt, model, tools, and sandbox mode. Profile selection must never widen a group's tool or sandbox policy; repo policy (`WAFFLE.md` or `AGENT.md`, via `internal/repopolicy`) can only tighten it further. Four tiers exist: `main` (owner interactive), `cron` (unattended scheduled), `issue` (board intake), `group` (multi-party chat) — the latter three deny host `bash` and memory writes by default.

**LLM layer**: `internal/llm` owns the canonical provider-neutral message and tool types. Provider packages (`internal/llm/anthropicp`, `internal/llm/openaip` for OpenRouter/Ollama/OpenAI-compatible) translate those types at the boundary — put provider-specific behavior in the translator, never in the agent loop. `internal/llmtest` supplies fake providers for offline tests. `internal/providerconfig` enrolls a Provider Connection as one transaction across `config.toml`, the encrypted secret store, and service activation; `internal/modelcatalog` holds provider-neutral catalogue data (a derived, disposable 24h cache — selected Model Aliases stay authoritative). The agent itself always runs on the host; only tool *execution* varies between host and Docker executors.

**Storage**: all durable state is SQLite via `internal/store`, opened in WAL mode with a **single connection** — avoid long transactions, foreground maintenance, or per-row commits on normal request paths. Schema changes are ordered, embedded SQL migrations in `internal/store/migrations/`; version numbers must stay contiguous and every migration must remain safe to apply to existing databases.

**Sandboxing**: Docker mode uses paired, single-writer SQLite queues between the host and `waffle runner` (bind-mounted into the container as the entrypoint — must be a static **linux** build matching the container's `GOARCH`, set via `[sandbox] runner_binary` as an absolute path on non-linux hosts). Sandboxes never receive raw provider or Git credentials: `internal/broker` issues scoped `wk_` session tokens with a **24h TTL** (`broker.DefaultTokenTTL`); expired tokens stop authorizing proxy, git-credential, and API faces and are swept from memory — long-running work renews via re-mint/resume. `internal/secret` holds real values (age-encrypted), `internal/gitcred` fronts git auth to the broker, `internal/netlock` makes the container drop its default route (fail-closed) so only `waffle-host` remains reachable, and `internal/redact` scrubs secrets from anything rendered or logged. Preserve deny-by-default network/tool/secret policy in any change touching gateway, broker, MCP, sandbox, or workspace code.

**Tools**: `internal/tool` defines the native tools (`bash`, `read_file`, `write_file`, `edit_file`, `list_files`, `search`, `fetch`, `web_search`) and the registry. Memory/session tools (`remember`, `recall`, `memory_update`, `distill_skill`, `workspace_update`, `expand_output`, `notify`) come from their owning packages. `internal/apiface` generates one narrow `api_<name>` tool per configured broker API face rather than a generic call tool, so tool policy can grant or deny a face by name and the credential stays host-side. `internal/codeintel` adds the optional structural-code tools (`code_find_symbol`, `code_references`, `code_callers`, `code_structure`, `code_blast_radius`, `code_suggest_tests`) over MCP with an in-process `go/parser` fallback; cached answers never silently override a live file read.

**Memory** (`internal/memory`, `internal/workset`, `internal/spill`) spans three layers: (1) transcript sessions (SQLite turns + FTS5 history/summaries via `internal/session`), (2) a session-local working set (goals/constraints via `workspace_update`, not durable), (3) durable `MEMORY.md` workspace notes maintained through the memory tools and indexed in SQLite. Keep working-set state session-scoped; only promote to durable notes through the memory tools. `[memory] write_gate` (`auto`/`notify`/`review`) routes durable writes through the `waffle candidates` review queue; untrusted-derived writes (e.g. fetched web content) are always queued regardless of the setting.

**Skills, learning, plugins**: `internal/skill` loads `SKILL.md` skills and implements the mine→propose→validate learning loop behind `waffle learn` (`/learn` is the only reserved internal cron action; arbitrary CLI commands are never dispatched from job prompts). `internal/skillinstall` fetches and stages external skills — it rejects symlinks outright in untrusted trees and records provenance. `internal/plugin` loads Agent Plugins 1.0.0 packages with hand-rolled, entirely local schema validation (the spec forbids fetching a schema at load time; `TestNoNetworkingImports` enforces that the package imports no networking); `internal/pluginmcp` maps a validated portable `mcp.json` entry onto an MCP server config. Approved skills are written inactive — activation stays explicit (`waffle skills activate <name>`).

**Repo workspaces** (`internal/workspace`, `cmd/waffle/ws_cmd.go`): `waffle ws open owner/repo` clones into a dedicated container + volume; git auth flows through `waffle git-credential` to the broker, backed by the scoped GitHub App in `[github.app]`. Egress is deny-by-default (`none` / `allowlist` via host broker proxy / `full`). Under `serve`, idle workspaces stop after 30 min and close after 168h only when clean — dirty or unpushed work is retained, never discarded. `internal/hooks` runs workspace lifecycle shell hooks inside the sandbox.

**Self-development** (`internal/selfdev`): `doctor` self-checks a build, `upgrade` builds from an approved ref and atomically swaps the binary in after verification, `rollback` restores the previous one. `[selfdev] approval` is `manual` (default), `ci`, or `auto-patch`; `approval = "ci"` verifies every `required_checks` entry completed successfully **for the exact candidate SHA** and fails closed on missing/pending/stale checks or API errors. Every upgrade resolves one commit SHA and builds it in an isolated detached worktree, so the configured checkout is never modified and uncommitted local edits can never reach the binary. `selfdev-upgrades.jsonl` binds base SHA, candidate SHA, tree hash, and the artifact's SHA-256.

**Waffle Desk** (`internal/dashboard`, `internal/dashboard/ui`): the personal cockpit, disabled by default and enabled via `[dashboard] enabled = true` or `waffle setup`. It is **not** a separate listener — it shares the loopback `gateway.status_listen` address (default `127.0.0.1:8422`). Server-side rendering is templ (`.templ` → committed `_templ.go`); the browser client is dependency-free ES modules tested with `node --test`, plus a Playwright/Chrome gate in `tools/dashboard-tests`. `[dashboard.tailnet]` may authorize requests proxied by a same-host `tailscale serve`, authenticated by allowlisted Tailscale identity headers; those requests may address only `/desk/` and `/api/v1/desk/*`, and Funnel requests are always rejected.

**Chat surfaces**: `internal/chat` is the presentation-neutral contract; `internal/chatui` is the Bubble Tea TUI; `internal/chatwire` is the bounded, versioned local protocol; `internal/localsocket` owns the filesystem-authorized local listener (ownership and mode checks on every ancestor, 0600 socket). `waffle chat` runs in direct mode (opens config, secrets, and the store in-process) unless `--socket` or `WAFFLE_CHAT_SOCKET` selects a service socket; `--plain` selects the non-TUI renderer.

**Policy**: action-level `[[policy.rule]]` tables (`internal/policy`, `internal/repopolicy`) match tool name plus optional bash prefix/regex with allow/deny/require and guidance. Bash matching is quote-aware but does not expand shell indirection. All decisions are audited in the `policy_audit` table.

**MCP** (`internal/mcp`): servers are declared explicitly in `config.toml`. Child processes never inherit the gateway's ambient environment — they get only `PATH` plus an explicit `env` allowlist via `mcp.ConnectRestricted`. `execution = "sandbox"` docker-wraps the command (`--network none`, allowlisted `-e` only) when the agent group is docker mode; a docker group needing host MCP must explicitly set `execution = "host"`.

**Supporting packages**: `internal/schedule` (cron), `internal/intake` (issue-tracker work dispatch), `internal/observability` + `internal/telemetry` (run tracking, OpenTelemetry tracing), `internal/usage` (persisted token/request usage and cost), `internal/notify` (session-scoped outbound sender), `internal/backup` (local state backup/restore), `internal/lifecycle`/`internal/flock`/`internal/filecommit`/`internal/id`/`internal/textcut` (shared primitives), `internal/eval` (zero-network eval harness).

## Repository layout

- `cmd/waffle/` — command wiring, one `*_cmd.go` per subcommand.
- `internal/` — all reusable behavior; tests colocated as `*_test.go`.
- `internal/store/migrations/` — ordered, embedded SQL migrations.
- `evals/` — zero-network evaluation scenarios (TOML), run by `waffle eval`.
- `docs/` — `plan.md` (architecture, trust model, phased roadmap incl. `#Deviations`), `chat.md`, `usage-guide.md`, `waffle-desk.md` / `waffle-desk-htmx.md`, `deploy.md`, `sandbox-queue.md`, `code-intelligence.md`, `research.md` (prior art), plus audit notes under `acceptance-audit/` and `issues/`.
- `scripts/` — CI shard runner and weights, Linux artifact build and reproducibility checks.
- `tools/brand-assets/`, `assets/brand/` — brand sources and their pnpm validation/build scripts.
- `tools/dashboard-tests/` — Playwright browser gate for Desk.
- `website/` — Astro marketing/docs site with its own `CLAUDE.md` and `AGENTS.md`; follow those when working in that tree.
- `config.example.toml` — the configuration contract/reference.

## Coding style

- Wrap errors with context using `%w`; document any `nolint` suppression (required by `nolintlint`).
- Format with `gofmt`; `goimports` and `gofmt` run as golangci-lint formatters, with `misspell` and `unconvert` enabled on top of the standard set.
- Short lowercase package names, `PascalCase` exported, `camelCase` unexported. Keep package boundaries aligned with responsibilities: command wiring in `cmd/waffle`, reusable behavior in `internal`.
- Package doc comments carry the design rationale here — when adding a package, write a `// Package x ...` header explaining the boundary and the decision behind it, and reference the driving issue or `docs/plan.md` section.
- Table-driven tests named `TestBehavior`, colocated beside the implementation. Add regression coverage for failure, cancellation, persistence, and concurrency paths.

## Engineering principles

Work in the lazy senior dev persona: lazy means efficient, not careless, and the best code is the code never written. Avoid overengineering and unnecessary complexity — if a senior engineer would call it overcomplicated, simplify. A date picker request is `<input type="date">`, not flatpickr, a wrapper component, a stylesheet, and a timezone debate.

Climb the ladder after understanding the problem, not instead of it. Read the task and the code it touches, trace the real flow end to end, then stop at the first rung that holds:

1. Does this need to be built at all? No? Skip it. (YAGNI)
2. Does it already exist in this codebase? Reuse the helper, util, or pattern.
3. Does the standard library do it? Use it.
4. Does a native platform feature cover it? Use it.
5. Does an already-installed dependency solve it? Use it.
6. Can this be one line? Do it.
7. Only then: write the minimum code that works.

Fix the root cause, not the symptom. A report names a symptom; before editing, grep every caller of the function you are about to touch. One guard in the shared function is smaller than one guard per caller, and patching only the path the ticket names leaves sibling callers broken.

Rules:

- No unrequested abstractions.
- No avoidable dependencies.
- No speculative scaffolding.
- Prefer deletion over addition.
- Boring over clever.
- Fewest files possible.
- Shortest working diff wins once you understand the problem.
- Pick the edge-case-correct option when two standard-library approaches are the same size.

Complex request? Ship the lazy version and question it in the same response: "Did X. Y covers it. Need full X? Say so." Always tell the user what you skipped. If the user insists on the full version, build it, no re-arguing.

Do not be lazy about validation, error handling, security, accessibility, data-loss protection, or real edge cases. Do not skip understanding: a small diff you do not understand is laziness dressed up as efficiency. Non-trivial logic leaves one runnable check behind; trivial one-liners need no test.

## Security & configuration

- Never commit `config.toml`, generated databases, identities, or secrets. `config.toml` stores `secret://` references only, never secret values.
- Configuration is strict: use `config.example.toml` as the contract; don't add permissive fallback behavior for unknown or invalid policy/configuration.
- Fail closed. Every credential, network, and verification path in this repo prefers a hard error over a degraded default — preserve that when editing broker, netlock, selfdev, skillinstall, or policy code.
- Treat migrations, secret-store changes, and sandbox/network-policy changes as compatibility- and security-sensitive — document rollback/compatibility and any required operational changes in the PR description.
- The gateway status endpoint (`[gateway] status_listen`, default `127.0.0.1:8422`) must stay loopback-only. `[dashboard.tailnet]` may authorize Desk requests proxied to that listener by a same-host `tailscale serve`; it must never move the bind address, and `/status` and `/healthz` must stay loopback-only in every configuration.

## Commit & PR conventions

Conventional Commits with focused, imperative subjects: `feat: add workspace cleanup`, `fix: handle cancelled runs`, `docs: clarify deployment`, `build(deps): bump sqlite`. Issue-scoped forms like `fix(#68): ...` are also accepted; release-please consumes these for versioning and `CHANGELOG.md`. PRs should explain behavior changes, link issues, list verification commands run, and call out configuration or migration effects. Include screenshots only for user-visible output changes.
