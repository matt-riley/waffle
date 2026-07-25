# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project overview

Waffle is a personal AI agent written in Go: one binary (`waffle`) containing the agent loop, a messaging gateway (Telegram), a terminal chat REPL, and a provider-agnostic LLM layer. It's designed to run on the owner's own hardware with deny-by-default sandbox/network/tool policies throughout.

## Commands

Use `mise` to install the pinned toolchain (Go 1.25.12) and run tasks:

See `mise.toml` for the full task list (`mise tasks`); `mise install` installs the pinned toolchain.

Focused test iteration: `go test ./internal/<package> -run '^TestName$' -count=1`.

Run locally: `go run ./cmd/waffle setup` (first-run: secret identity, provider, and `[agent.profile.main]`), then `go run ./cmd/waffle chat`. Manual alternative: `./waffle secret init`, configure a provider (`waffle provider add` or `printf '%s' sk-ant-... | ./waffle secret set anthropic/api-key`), then chat.

Sandbox-specific tests are opt-in and documented in `docs/sandbox-queue.md`:
`go test -tags=sandbox_stress ./internal/sandbox -run Stress -count=1` (add `-tags=sandbox_docker` when Docker is available).

Live provider evals require `WAFFLE_EVAL_LIVE=1` and a configured provider; they're skipped otherwise.

## Architecture

`cmd/waffle` is a single binary with subcommands (`setup`, `chat`, `serve`, sandbox runner, `ws`, `cron`, `session`, `secret`, `skills`, `learn`, `pair`, `usage`, `status`, etc.). `serve` is the sole owner of background processing, the gateway, and in-memory broker tokens — it holds the serve-owner lock (`~/.waffle/serve.lock`, PID + heartbeat) and starts lifecycle sweepers. A second `serve` refuses to start; a stale lock is reclaimed automatically once its PID is dead.

**Inbound path**: channel adapter → `internal/gateway` → `internal/entity` (`user → channel group → agent group → session`) → `internal/agent`. Gateway handlers run concurrently across conversations but are serialized per channel group.

**Trust boundaries**: agent profiles (`internal/agent`, `[agent.profile.*]`) and agent groups (`[agent.group.*]`) are trust boundaries — system prompt, model, tools, and sandbox mode. Profile selection must never widen a group's tool or sandbox policy; repo policy (`WAFFLE.md`) can only tighten it further. Four tiers exist: `main` (owner interactive), `cron` (unattended scheduled), `issue` (board intake), `group` (multi-party chat) — the latter three deny host `bash` and memory writes by default.

**LLM layer**: `internal/llm` owns the canonical provider-neutral message and tool types. Provider packages (Anthropic, OpenAI-compatible for OpenRouter/Ollama/etc.) translate those types at the boundary — add provider-specific behavior in the translator, never in the agent loop. The agent itself always runs on the host; only tool *execution* varies between host and Docker executors.

**Storage**: all durable state is SQLite via `internal/store`, opened in WAL mode with a **single connection** — avoid long transactions, foreground maintenance, or per-row commits on normal request paths. Schema changes are ordered, embedded SQL migrations in `internal/store/migrations/`; version numbers must stay contiguous and every migration must remain safe to apply to existing databases.

**Sandboxing**: Docker mode uses paired, single-writer SQLite queues between the host and `waffle runner` (bind-mounted into the container as the entrypoint — must be a static **linux** build matching the container's `GOARCH`, set via `[sandbox] runner_binary` as an absolute path on non-linux hosts). Sandboxes never receive raw provider or Git credentials: `internal/broker` issues scoped `wk_` session tokens with a **24h TTL** (`broker.DefaultTokenTTL`); expired tokens stop authorizing proxy and git-credential faces and are swept from memory — long-running work renews via re-mint/resume. `internal/secret` holds real values (age-encrypted). `internal/gitcred` fronts git auth to the broker. Preserve deny-by-default network/tool/secret policy in any change touching gateway, broker, MCP, sandbox, or workspace code.

**Memory** (`internal/memory`, `internal/workset`, `internal/spill`) spans three layers: (1) transcript sessions (SQLite turns + FTS5 history/summaries), (2) a session-local working set (goals/constraints via `workspace_update`, not durable), (3) durable `MEMORY.md` workspace notes maintained through memory tools and indexed in SQLite. Keep working-set state session-scoped; only promote to durable notes through the memory tools.

**Repo workspaces** (`internal/workspace`, `cmd/waffle/ws_cmd.go`): `waffle ws open owner/repo` clones into a dedicated container + volume; git auth flows through `waffle git-credential` to the broker. Egress is deny-by-default (`none` / `allowlist` via host broker proxy / `full`). Under `serve`, idle workspaces stop after 30 min and close after 168h only when clean (dirty/unpushed work is retained, never discarded).

**Policy**: action-level `[[policy.rule]]` tables (`internal/policy`, `internal/repopolicy`) match tool name plus optional bash prefix/regex with allow/deny/require and guidance. Bash matching is quote-aware but does not expand shell indirection. All decisions are audited in the `policy_audit` table.

**MCP** (`internal/mcp`): servers are declared explicitly in `config.toml`. Child processes never inherit the gateway's ambient environment — they get only `PATH` plus an explicit `env` allowlist via `mcp.ConnectRestricted`. `execution = "sandbox"` docker-wraps the command (`--network none`, allowlisted `-e` only) when the agent group is docker mode; a docker group needing host MCP must explicitly set `execution = "host"`.

## Repository layout

- `cmd/waffle/` — the executable and command handlers.
- `internal/` — focused packages by responsibility (see directory listing).
- `evals/` — zero-network evaluation scenarios (TOML), run by `waffle eval`.
- `docs/` — `plan.md` (architecture and phased roadmap, incl. `#Deviations` from the original design), `research.md` (prior art: hermes-agent, nanoclaw, openclaw, workweave/router), `deploy.md`, `sandbox-queue.md`, `code-intelligence.md`.
- `assets/brand/`, `tools/brand-assets/` — brand source assets and their validation/build scripts (pnpm).
- `config.example.toml` — the configuration contract/reference.

## Coding style

- Format every Go change with `gofmt`; accept its tab indentation.
- Wrap errors with context using `%w`; document any `nolint` suppression (required by `nolintlint`).
- Keep package boundaries aligned with responsibilities; keep command wiring in `cmd/waffle`, reusable behavior in `internal`.
- Table-driven tests named `TestBehavior`, colocated as `*_test.go` beside the implementation. Add regression coverage for failure, cancellation, persistence, and concurrency paths.

## Security & configuration

- Never commit `config.toml`, generated databases, identities, or secrets. `config.toml` stores `secret://` references only, never secret values.
- Configuration is strict: use `config.example.toml` as the contract; don't add permissive fallback behavior for unknown or invalid policy/configuration.
- Treat migrations, secret-store changes, and sandbox/network-policy changes as compatibility- and security-sensitive — document rollback/compatibility and any required operational changes in the PR description.
- The gateway status endpoint (`[gateway] status_listen`, default `127.0.0.1:8422`) must stay loopback-only.

## Commit & PR conventions

Conventional Commits with focused, imperative subjects: `feat: add workspace cleanup`, `fix: handle cancelled runs`, `docs: clarify deployment`, `build(deps): bump sqlite`. Issue-scoped forms like `fix(#68): ...` are also accepted. PRs should explain behavior changes, link issues, list verification commands run, and call out configuration or migration effects.
