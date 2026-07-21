### Title
feat: add `waffle setup` to chain first-run secret init, provider add, and starter profile

### Labels
enhancement, cli, ux, onboarding

### Body

**Problem**

First run today requires the operator to already know about, and manually run in order: `waffle secret init`, then `waffle secret set <provider>/api-key` (piping a key in), then `waffle provider add` for guided model discovery, then hand-edit `config.toml` to add or confirm an `[agent.profile.main]` block per `config.example.toml`. Each step individually is solid (the provider flow in particular probes the upstream and commits credential+config transactionally), but there's no single entry point that walks a new install through all of them, and nothing in `waffle --help` / `waffle chat`'s error output points a first-time user at this sequence.

**Context**

`docs/tui-ux-comparison.md` compares this to `openclaw onboard` and `hermes setup --portal` — both single guided commands that provision credentials, discover/confirm a model, and bootstrap workspace files in one pass.

**Proposed fix**

Add a `waffle setup` subcommand (in a new `cmd/waffle/setup_cmd.go`) that runs, interactively:

1. `secret init` if `~/.waffle` has no identity yet (skip with a message if already initialized).
2. Prompt for a provider preset (`openai` | `anthropic` | `openrouter` | `openai-compatible`), then delegate to the existing `providerAdd` flow in `cmd/waffle/provider_cmd.go` for credential entry and model discovery.
3. Confirm or write a minimal `[agent.profile.main]` block to `config.toml` if one doesn't already exist, using the `main` example from `config.example.toml` as the template (asking only for the `system` prompt line; sandbox/tools stay at documented safe defaults).
4. Print a closing summary: the model alias now active, and the exact `waffle chat` command to run next.

Additionally: `waffle chat` run against a config with no `[providers]`/`[models]` should detect that condition and print `run 'waffle setup' to get started` instead of (or in addition to) whatever its current unconfigured-state message is.

**Acceptance criteria**

- AC1: `waffle setup` run against a completely fresh `$WAFFLE_HOME` (no identity, no config) completes all four steps above without requiring the operator to invoke any other `waffle` subcommand directly, and exits 0.
- AC2: `waffle setup` run a second time against an already-initialized install detects each already-complete step (identity exists, a provider connection exists, `agent.profile.main` exists) and skips it with an explicit "already configured" message rather than erroring or duplicating state.
- AC3: Credentials are still read the same way the existing `provider add` flow requires (stdin or a root-owned 0600 file) — `waffle setup` must not introduce a new way to pass secrets via command-line arguments or environment variables.
- AC4: After `waffle setup` completes successfully, `waffle chat` opens with a working model selected with no further configuration.
- AC5: Running `waffle chat` against an unconfigured install (no `[providers]`) prints a message directing the operator to `waffle setup`.
- AC6: New tests in `cmd/waffle/setup_cmd_test.go` cover: fresh install end-to-end, re-run against a fully-configured install (idempotent/skip path), and re-run against a partially-configured install (e.g. identity exists but no provider).
- AC7: `docs/chat.md` or a new `docs/getting-started.md` documents `waffle setup` as the first command a new install should run, and `CLAUDE.md`'s "Run locally" section is updated to reference it instead of (or alongside) the three-step manual sequence.
- AC8: `gofmt` clean; `go test ./cmd/waffle/... -run Setup -count=1` passes.
