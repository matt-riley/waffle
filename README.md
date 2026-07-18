# waffle

A personal AI agent, written in Go, that runs on your own hardware — one
binary containing the agent loop, a messaging gateway, a terminal chat REPL,
and a provider-agnostic LLM layer.

Status: **phases 0–4 fully delivered; phases 5–7 landed with deliberate
cuts** (line REPL instead of a full-screen TUI, OpenAI-compatible Gemini,
stdio-only MCP, Telegram only — Discord not shipped). See
[docs/plan.md](docs/plan.md) ([Deviations](docs/plan.md#deviations)) for the
design and what was intentionally left out.

What's here, by capability:

- **Agent loop & providers** — streaming loop over one canonical message
  format; named Anthropic and OpenAI-compatible connections (including
  OpenRouter and Ollama) can coexist, and deterministic model aliases select
  the connection and upstream model for each request.
- **Terminal** — `waffle chat` (`-c` resumes the last session), with native
  tools (bash, file read/write/edit, fetch).
  Fetch blocks loopback, link-local, unspecified, and private IPv4/IPv6
  destinations by default (including redirects). For deliberate local use,
  allow exact CIDRs or host:port destinations with
  `[tools.fetch] allow_private = ["192.168.1.0/24", "localhost:3000"]`.
- **Memory** — three layers: (1) **transcript** (SQLite turns + FTS5
  history/summaries), (2) **working set** (session-local goals/constraints
  via `workspace_update`; not durable knowledge), (3) **MEMORY.md**
  (durable owner notes across sessions). Multi-tier `recall`
  (turns/summaries/notes/spills); `remember` / `memory_update`; tool spills
  with `expand_output` / `expand_context`; `AGENT.md`/`USER.md`/`MEMORY.md`
  workspace files; sessions summarized on exit via the shared reflection
  prompt. Under `serve`, idle reflection runs when
  `[memory] reflect_after` is a positive duration (e.g. `"30m"`;
  `"0"` disables); `reflect_every` is the poll interval and
  `reflect_every_turns` also summarizes active gateway chats every N
  turns. Reflection holds the per-conversation group lock and skips when
  busy. `waffle session ls` shows stored summaries. Long histories are
  summarized with an in-process prefix cache; summary blocks name turn
  ranges for `expand_context`. Optional `[agent] utility_model` selects a
  model alias for summarization/reflection.
- **Skills** — agentskills.io-compatible `SKILL.md` dirs, invoked with
  `/skill`; `distill_skill` writes new ones **inactive** until
  `waffle skills activate <name>`. `waffle learn` (or `skills audit`) mines
  recurring tool failures and proposes constrained skill/memory/config-stub
  edits under a held-in/held-out promotion rule.
- **Gateway** — `waffle serve` with a Telegram adapter. Single-owner:
  unknown senders in **private** chats get a pairing code redeemable only
  via `waffle pair approve` on the host. **Group chats** are quieter and
  more restricted (see [Group chat posture](#group-chat-posture)).
- **Run status** — `waffle status` reads the gateway's local-only status
  endpoint and prints active/recent runs plus token and runtime totals. The
  endpoint follows `[gateway] status_listen`, which defaults to
  `127.0.0.1:8422`; keep it loopback-only. Start `waffle serve` before using
  the command.
- **Isolation** — `[sandbox] mode = "docker"` runs tools inside any image
  via the bind-mounted `waffle runner` over a single-writer SQLite queue
  pair; host-enforced tool allow/deny policy; the credential broker fronts
  provider APIs (and git) with per-session `wk_` tokens so raw keys never
  leave the host. Docker mode bind-mounts a **linux** build of `waffle` as
  the container entrypoint, so on a non-linux host set
  `[sandbox] runner_binary` to an **absolute path** to a static linux build
  whose `GOARCH` matches your container image (which may differ from the
  host's — e.g. an arm64 host running amd64 images). Build it with:

  ```sh
  CGO_ENABLED=0 GOOS=linux GOARCH=<image arch> go build -o /abs/path/waffle-linux ./cmd/waffle
  ```

  `waffle doctor` fails fast if the path is missing or not absolute.
  Queue bind-mount stress and doctor probes: see
  [docs/sandbox-queue.md](docs/sandbox-queue.md)
  (`go test -tags=sandbox_stress ./internal/sandbox -run Stress`; optional
  `-tags=sandbox_docker` when Docker is available). Zero-network agent checks:
  `waffle eval` (also run by `mise run test` and `waffle upgrade` verify).
  Results are recorded in SQLite; `waffle eval --history` lists them. Live
  provider evals are opt-in via `WAFFLE_EVAL_LIVE=1` and are skipped without a
  configured provider.

  Each sandbox and workspace container is limited by default to `2g` memory,
  two CPUs, and 512 processes, with `no-new-privileges`. Override these with
  `[sandbox] memory`, `cpus`, and `pids`. `[sandbox] disk` requests a
  per-container storage size when the Docker storage driver supports it;
  unsupported drivers fall back without a disk quota, so use workspace
  lifecycle cleanup and volume monitoring there.
- **Repo workspaces** — `waffle ws open owner/repo` / `/repo` clones into a
  dedicated container + volume (devcontainer image when present), git auth
  via `waffle git-credential` to the broker, idle keeps the volume, close
  refuses on unpushed work. Egress is deny-by-default; choose
  `[workspace] egress = "none"` (default), `"allowlist"` with
  `allowlist = ["api.github.com"]` (host broker proxy), or `"full"` only when
  unrestricted network access is required. Allowlisted HTTP requests are
  token-authenticated, audited, and rejected if DNS resolves to private
  address space.
  Under `waffle serve`, open workspaces stop after 30 minutes idle and close
  after 168 hours only when clean; set `[workspace] idle_timeout` or
  `close_ttl` to a Go duration, or `"0"` to disable. Dirty/unpushed work is
  retained and reported. Configure `[github.app]` with secret-backed
  `app_id`, `installation_id`, and `private_key` for one-hour,
  repo-scoped installation tokens; without it, the documented PAT fallback is
  used.
- **Data lifecycle** — `waffle session rm <id>` and `waffle forget <query>`
  delete live sessions/turn matches while keeping FTS consistent. Optional
  `[store] retain = "365d"` runs under `serve`; `"0"` (the default) retains
  forever. Deletion cannot remove provider logs, delivered messages, or old
  backups.
- **Deployment** — a managed deployment first reaches **Installed** without a
  provider, then `sudo waffle provider add` validates an on-host connection
  and can move it to **Ready**. The two-state flow, systemd and launchd service
  examples, and loopback `/healthz` probe are in [docs/deploy.md](docs/deploy.md).
- **Releases** — Release Please handles version PRs, `v` tags, and GitHub
  releases independently from the binary deployment flow described below.
- **Automation** — `waffle cron` schedules jobs (prompt + cron + delivery
  target) that fire under `waffle serve` and deliver to a channel;
  `[[intake.github]]` watchers poll labeled issues and dispatch restricted
  workspace runs (board intake); `spawn_subagent` delegates parallel work;
  MCP servers add their tools. Repo `WAFFLE.md` may tighten tool policy and
  declare container lifecycle hooks (`after_create` / `before_run` /
  `after_run` / `before_remove`).

Session data is retained forever by default. `waffle session rm <id>` and
`waffle forget <query>` require confirmation and update the FTS index. Deletion
does not run a foreground SQLite `VACUUM`, so it does not block the active
gateway; free pages are reused by later writes. Opt-in retention runs only
under `waffle serve`:

```toml
[store]
retain = "365d" # "0" means forever (the default)
```

Deletion affects only the live waffle store. It cannot remove provider logs,
already-delivered Telegram messages, or data in existing backups.

MCP servers are configured explicitly in `config.toml`:

```toml
[[mcp]]
name = "example"
command = "/absolute/path/to/server"
execution = "host"
groups = ["main"]       # omit for all host groups
tools = ["echo"]        # optional; enables pre-launch policy filtering
env = ["EXAMPLE_TOKEN"] # only these variables are passed to the child
```

MCP processes never inherit the gateway ambient environment: children get
only `PATH` plus an allowlisted `env` via `mcp.ConnectRestricted` (#77).
`execution = "sandbox"` launches through that restricted executor; when the
agent group is docker mode the command is docker-wrapped
(`docker run -i --rm --network none`, work dir mount, allowlisted `-e` only).
A Docker group that wants host MCP must set `execution = "host"` and list
that group; otherwise host launch fails closed.
- **Trust tiering** — agent groups carry their own sandbox mode and tool
  policy (`[agent.group.<name>]` with `sandbox` and `tools.allow`/`deny`).
  The owner's interactive sessions run on the `main` tier; unattended
  scheduled jobs run on the `cron` tier, issue intake on the `issue` tier,
  and multi-party channel chats on the `group` tier — all three **deny host
  `bash` and memory writes by default**. Override only with an explicit
  `[agent.group.cron]` / `[agent.group.issue]` / `[agent.group.group]` tool
  policy. Action-level `[[policy.rule]]` tables match tool name + optional
  bash prefix/regex with allow/deny/require and guidance; `[sandbox] enforcer`
  is `none` (default) or `feedback` (include guidance in denials). Decisions
  are audited in `policy_audit`. Bash matching is quote-aware but does **not**
  expand shell indirection — see [docs/plan.md](docs/plan.md).
- **Self-improvement** — `waffle doctor` self-checks, `waffle upgrade`
  rebuilds from a local checkout, gates on the new binary's own doctor,
  atomically swaps it in, and `waffle rollback` restores the previous one.

```sh
go build ./cmd/waffle
./waffle secret init
credential-command | ./waffle provider add \
  --name anthropic --type anthropic \
  --model claude=claude-model-id --default claude --api-key-stdin
./waffle chat
```

## Backup and disaster recovery

Back up to a new local directory (the destination must not already exist):

```sh
waffle backup /Volumes/Backup/waffle
waffle secret export-identity --output /Volumes/Backup/waffle.identity
```

The backup contains a consistent SQLite snapshot, `secrets.age`,
`config.toml`, and workspace/skill files. The identity is deliberately kept
separate; use `waffle backup ... --with-identity` only when the destination is
protected, because that directory then contains both ciphertext and its key.
Identity export/import requires confirmation, or `--yes` for scripted recovery.

To recover, import the identity on the new machine, inspect the backup, then
run `waffle restore /Volumes/Backup/waffle`. Restore validates the database
migrations and configuration before replacing live files. Keep old backups in
mind when applying retention or `forget`: deleting data from the live store
does not delete it from prior backups.

Workspace Docker volumes are disposable and are not backed up. Push repository
work before `waffle ws close`; the repository is the source of truth and close
refuses unpushed work.

`waffle serve` is the single owner of background work and in-memory broker
tokens. It holds an OS advisory lock for its lifetime and records its PID and a
heartbeat in `~/.waffle/serve.lock`; a second
`serve` refuses to start, and `waffle ws open` refuses before changing the
database or starting Docker while that owner is live. Use the gateway `/repo`
command instead. A stale lock is reclaimed automatically once its heartbeat is
old and its PID is no longer alive; do not delete a lock owned by a live PID.

Named connections can target different providers at the same time. Model
aliases select one connection and one upstream model deterministically:

```toml
[providers.anthropic]
type = "anthropic"
api_key = "secret://provider/anthropic/api-key"

[providers.local]
type = "openai"
base_url = "http://127.0.0.1:11434/v1"

[models.claude]
provider = "anthropic"
model = "claude-model-id"

[models.local]
provider = "local"
model = "qwen3:32b"

[agent]
default_model = "claude"
utility_model = "local"
```

Use `waffle provider add` to write this configuration and its encrypted API
key transactionally. See [Managed host installation](docs/deploy.md#managed-host-installation)
for interactive and non-interactive examples. The legacy singular
`[provider]` table remains readable and is migrated on the first provider
management write.

### Named agent profiles

Profiles are a **trust boundary** (system prompt, model, tools, sandbox), not
just personality presets. With no `[agent.profile]` section, waffle behaves as
today under the effective `main` profile — no migration required. See
[`config.example.toml`](config.example.toml) for `main` / `researcher` /
`reviewer` samples and [docs/plan.md](docs/plan.md) for composition with
agent-group tiers (#33), repo `WAFFLE.md` (#53), action policy (#66), and
subagent working-set broadcast (#68).

```toml
[agent.profile.reviewer]
system = "You are a strict code reviewer."
model = "default"   # or "utility" / a configured model alias
sandbox = "docker"
[agent.profile.reviewer.tools]
allow = ["read_file", "search", "recall"]
deny = ["bash", "write_file", "edit_file"]
```

Bind surfaces: `waffle session profile <channel:chat> <name>`,
`waffle cron add … --profile name`, `waffle chat --profile name`,
`waffle ws open owner/repo --profile name`. Repo policy can only tighten the
selected profile.

Start here:

- [docs/plan.md](docs/plan.md) — architecture and phased roadmap
- [docs/research.md](docs/research.md) — research on the projects it draws
  from: [hermes-agent](https://github.com/NousResearch/hermes-agent),
  [nanoclaw](https://github.com/nanocoai/nanoclaw),
  [openclaw](https://github.com/openclaw/openclaw), and
  [workweave/router](https://github.com/workweave/router)

## Release automation

Waffle keeps its two main `main`-branch automations independent:

- Release Please runs on every push to `main` (and on manual
  `workflow_dispatch`) to open or update the version bump PR, then creates
  `v`-prefixed tags and GitHub releases when that PR is merged.
- The binary deployment flow runs on successful `main` pushes and uses the
  git-derived version available at that exact commit, whether or not Release
  Please creates a release during the same push.

That separation keeps semantic versioning and GitHub release publication
decoupled from the Linux artifact build/deployment path.

### Usage limits

Usage is persisted per session and period. By default limits are unlimited; for unattended use configure a budget:

```toml
[limits]
tokens_per_day = 100000
requests_per_hour = 100
[limits.group.cron]
tokens_per_day = 20000
requests_per_hour = 20
```

`waffle usage` reports actual and reserved totals. Before a provider-proxy
request is dispatched, Waffle atomically reserves its declared output maximum
plus a conservative text-prompt bound. Missing or invalid maxima, external
image/file inputs, provider-side context references, and unknown request
extensions reserve the remaining daily allowance. Only an explicitly completed
provider stream replaces the reservation with actual usage; aborted, partial,
or unmetered requests keep it charged. Anthropic actual usage includes base,
cache-creation, and cache-read input tokens. `waffle pause` stops new agent
calls (including cron and broker traffic); `waffle resume` re-enables them.

### Group chat posture

Telegram groups, supergroups, and channels are multi-party and untrusted
relative to a 1:1 DM with the owner:

1. **Mention-gated** — the bot only processes a group message when it is
   `@mentioned` (or a `/command@bot` form) or when the message is a reply
   to the bot. The bot username comes from Telegram `getMe` (cached), not
   from config. Mentions are stripped from the text before the agent runs.
2. **Silent strangers** — unknown senders in groups are ignored. No pairing
   codes, no replies. Pairing remains a private-chat flow only.
3. **Restricted tools** — conversations that originate in a group bind to
   the `group` agent tier (same default denies as `cron` / `issue`: no host
   `bash`, `remember`, `memory_update`, or `distill_skill`). Override with
   an explicit `[agent.group.group]` tool policy if you intentionally want
   more power in groups.

Private chats keep the existing single-owner pairing and `main` tier.

## License

MIT — see [LICENSE](LICENSE).
