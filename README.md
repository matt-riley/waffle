# waffle

A personal AI agent, written in Go, that runs on your own hardware — one
binary containing the agent loop, a messaging gateway, a terminal UI, and a
provider-agnostic LLM layer.

Status: Phase 0 (skeleton) — module layout, config loading, SQLite store
with migrations, encrypted secret store (`waffle secret`), OTel wiring, CI.

```sh
go build ./cmd/waffle
./waffle help
./waffle secret init   # then: printf 'sk-...' | ./waffle secret set anthropic/api-key
```

Start here:

- [docs/plan.md](docs/plan.md) — architecture and phased roadmap
- [docs/research.md](docs/research.md) — research on the projects it draws
  from: [hermes-agent](https://github.com/NousResearch/hermes-agent),
  [nanoclaw](https://github.com/nanocoai/nanoclaw),
  [openclaw](https://github.com/openclaw/openclaw), and
  [workweave/router](https://github.com/workweave/router)
