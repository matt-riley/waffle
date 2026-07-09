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
- **Memory** — every conversation persisted to SQLite with FTS5 search;
  `remember`/`recall` tools; `AGENT.md`/`USER.md`/`MEMORY.md` workspace
  files injected into the prompt; sessions summarized on exit.
- **Skills** — agentskills.io-compatible `SKILL.md` dirs, invoked with
  `/skill`; `distill_skill` writes new ones from what the agent works out.
- **Gateway** — `waffle serve` with a Telegram adapter. Single-owner:
  unknown senders get a pairing code redeemable only via `waffle pair
  approve` on the host.
- **Isolation** — `[sandbox] mode = "docker"` runs tools inside any image
  via the bind-mounted `waffle runner` over a single-writer SQLite queue
  pair; host-enforced tool allow/deny policy; the credential broker fronts
  provider APIs (and git) with per-session `wk_` tokens so raw keys never
  leave the host. Docker mode bind-mounts a **linux** build of `waffle` as
  the container entrypoint, so on a non-linux host set `[sandbox]
  runner_binary` to a linux build (`GOOS=linux go build -o waffle-linux
  ./cmd/waffle`); `waffle doctor` fails fast if it's missing.
- **Repo workspaces** — `waffle ws open owner/repo` / `/repo` clones into a
  dedicated container + volume (devcontainer image when present), git auth
  via `waffle git-credential` to the broker, idle keeps the volume, close
  refuses on unpushed work.
- **Automation** — `waffle cron` schedules jobs (prompt + cron + delivery
  target) that fire under `waffle serve` and deliver to a channel;
  `spawn_subagent` delegates parallel work; MCP servers add their tools.
- **Self-improvement** — `waffle doctor` self-checks, `waffle upgrade`
  rebuilds from a local checkout, gates on the new binary's own doctor,
  atomically swaps it in, and `waffle rollback` restores the previous one.

```sh
go build ./cmd/waffle
./waffle secret init
printf '%s' sk-ant-... | ./waffle secret set anthropic/api-key
./waffle chat
```

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
