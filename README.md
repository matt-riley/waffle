# waffle

A personal AI agent, written in Go, that runs on your own hardware — one
binary containing the agent loop, a messaging gateway, a terminal UI, and a
provider-agnostic LLM layer.

Status: Phase 3 (gateway) — message waffle from your phone: `waffle serve`
runs the gateway with a Telegram adapter, routing every message through the
entity model (identity → channel group → session). waffle is single-owner:
unknown senders get a pairing code redeemable only via `waffle pair
approve` on the host. Also: a terminal agent that remembers you — streaming
agent loop, Anthropic + OpenAI-compatible providers, native tools plus
`remember`/`recall`, every conversation persisted to SQLite with FTS5
search, workspace prompt files (`AGENT.md`/`USER.md`/`MEMORY.md`), and
agentskills.io-compatible `SKILL.md` skills invoked with `/skill`.
`waffle chat -c` resumes where you left off; sessions are summarized on
exit for future recall.

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
