# waffle

A personal AI agent, written in Go, that runs on your own hardware — one
binary containing the agent loop, a messaging gateway, a terminal UI, and a
provider-agnostic LLM layer.

Status: Phase 1 (the loop) — a working terminal agent: streaming agent loop,
Anthropic + OpenAI-compatible providers behind one canonical message format,
native tools (bash, file read/write/edit, fetch), encrypted secret store.

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
