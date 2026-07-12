# waffle

A personal AI agent, written in Go, that runs on your own hardware — one
binary containing the agent loop, a messaging gateway, a terminal UI, and a
provider-agnostic LLM layer.

Status: **all seven planned phases implemented.** waffle is a working
personal agent — terminal, gateway, isolation, workspaces, automation, and
self-improvement. See [docs/plan.md](docs/plan.md) for the design.

What's here, by capability:

- **Agent loop & providers** — streaming loop over one canonical message
  format; Anthropic and any OpenAI-compatible endpoint (OpenRouter, Ollama,
  a running workweave/router) are config, not code.
- **Terminal** — `waffle chat` (`-c` resumes the last session), with native
  tools (bash, file read/write/edit, fetch).
  Fetch blocks loopback, link-local, unspecified, and private IPv4/IPv6
  destinations by default (including redirects). For deliberate local use,
  allow exact CIDRs or host:port destinations with
  `[tools.fetch] allow_private = ["192.168.1.0/24", "localhost:3000"]`.
- **Memory** — every conversation persisted to SQLite with FTS5 search;
  `remember`/`recall` tools; `AGENT.md`/`USER.md`/`MEMORY.md` workspace
  files injected into the prompt; sessions summarized on exit.
- **Skills** — agentskills.io-compatible `SKILL.md` dirs, invoked with
  `/skill`; `distill_skill` writes new ones from what the agent works out.
- **Gateway** — `waffle serve` with a Telegram adapter. Single-owner:
  unknown senders get a pairing code redeemable only via `waffle pair
  approve` on the host.
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
- **Deployment** — systemd and launchd service examples, headless identity
  setup, and the loopback `/healthz` probe are in [docs/deploy.md](docs/deploy.md).
- **Automation** — `waffle cron` schedules jobs (prompt + cron + delivery
  target) that fire under `waffle serve` and deliver to a channel;
  `spawn_subagent` delegates parallel work; MCP servers add their tools.

Session data is retained forever by default. `waffle session rm <id>` and
`waffle forget <query>` require confirmation, update the FTS index, and run a
SQLite `VACUUM`/incremental vacuum; opt-in
retention runs only under `waffle serve`:

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

MCP processes run as the daemon user and are never made safe by a Docker
agent group. A Docker group must explicitly opt a server into host execution
by setting `execution = "host"` and listing that group; otherwise startup
fails closed. `execution = "sandbox"` is reserved until MCP can be integrated
with the constrained runner and currently fails closed.
- **Trust tiering** — agent groups carry their own sandbox mode and tool
  policy (`[agent.group.<name>]` with `sandbox` and `tools.allow`/`deny`).
  The owner's interactive sessions run on the `main` tier; unattended
  scheduled jobs run on the `cron` tier, which **denies host `bash` by
  default** — set `[agent.group.cron]` to override. The gateway and
  scheduler run as separate agents so a cron prompt can't reach host shell.
- **Self-improvement** — `waffle doctor` self-checks, `waffle upgrade`
  rebuilds from a local checkout, gates on the new binary's own doctor,
  atomically swaps it in, and `waffle rollback` restores the previous one.

```sh
go build ./cmd/waffle
./waffle secret init
printf '%s' sk-ant-... | ./waffle secret set anthropic/api-key
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

Point it elsewhere in `~/.waffle/config.toml` — any OpenAI-compatible
endpoint (OpenRouter, Ollama, a running workweave/router) works:

```toml
[provider]
name = "openai"
base_url = "http://localhost:11434/v1"
model = "qwen3:32b"
```

Start here:

- [docs/plan.md](docs/plan.md) — architecture and phased roadmap
- [docs/research.md](docs/research.md) — research on the projects it draws
  from: [hermes-agent](https://github.com/NousResearch/hermes-agent),
  [nanoclaw](https://github.com/nanocoai/nanoclaw),
  [openclaw](https://github.com/openclaw/openclaw), and
  [workweave/router](https://github.com/workweave/router)

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

`waffle usage` reports totals. `waffle pause` stops new agent calls (including cron and broker traffic); `waffle resume` re-enables them.
