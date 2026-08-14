# waffle

A personal AI agent, written in Go, that you run on your own hardware. One
static binary holds the agent loop, a Telegram gateway, terminal chat, Waffle
Desk, and a provider-agnostic LLM layer. It serves exactly one owner, with
deny-by-default sandbox and network policy. Named for the cat.

## Status

Developer preview. Tagged releases exist — see
[Releases](https://github.com/matt-riley/waffle/releases) and
[CHANGELOG.md](CHANGELOG.md) — but they are not a v1 compatibility promise.
Config keys, flags, and SQLite migrations can still change. Sharp edges are
expected.

## First hour

```sh
git clone https://github.com/matt-riley/waffle.git
cd waffle
mise install
mise run build
./bin/waffle setup
./bin/waffle chat
```

`mise install` pins Go (see `mise.toml`). If you already have that toolchain,
`go install github.com/matt-riley/waffle/cmd/waffle@latest` then `waffle setup`
and `waffle chat` is enough.

`waffle setup` creates the secret identity, enrolls a provider, and writes a
starter `[agent.profile.main]`. After that, talk in the TUI. Telegram is the
same loop over `waffle serve` once a bot token is configured: message the
bot, then `waffle pair approve` on the host.

## Where it runs

On a laptop, `waffle chat` is direct: it opens your config, secrets, and
SQLite store in-process. On a managed host, `waffle serve` owns the agent and
`waffle chat` attaches to the service socket — no `sudo`. First run and the
socket path are in [docs/chat.md](docs/chat.md). Installed vs Ready, provider
enrollment, and host rollout are in [docs/deploy.md](docs/deploy.md).

## What it is not

Not multi-tenant. Not a guest or group chatbot platform — pairing binds the
owner's own accounts; anyone else gets a code and nothing more. Discord is
not shipped. There is no plugin marketplace, no in-tree smart routing, and
no Postgres. Voice, companion apps, and extra chat channels are out of
scope.

Keys never leave the host. Sandboxes authenticate to a host-side broker with
scoped tokens. An agent profile cannot widen its group's tool or sandbox
policy.

## Roadmap to v1

Phases 0–7 from [docs/plan.md](docs/plan.md) are in daily use. Remaining
gaps there are deliberate cuts (OpenAI-compatible Gemini, Telegram only,
no marketplace), not unfinished work.

v1 is a promise, not a backlog: the owner loops above stay documented from
this README, `waffle upgrade` / `waffle rollback` remains the compatibility
contract, and breaking config or schema changes are called out in the
changelog. Until then, treat tags as preview stamps.

## Docs

- [docs/plan.md](docs/plan.md) — architecture, trust model, deviations, phases
- [docs/chat.md](docs/chat.md) — first run, TUI, managed chat socket
- [docs/usage-guide.md](docs/usage-guide.md) — skills, profiles, cron, learning
- [docs/waffle-desk.md](docs/waffle-desk.md) — loopback personal cockpit
- [docs/deploy.md](docs/deploy.md) — managed host install and rollout
- [docs/research.md](docs/research.md) — prior art and why these choices
- [config.example.toml](config.example.toml) — configuration contract

## Feedback

Bug reports are welcome if you actually ran it. This is a personal agent,
not a feature-intake queue. Report vulnerabilities privately through
[SECURITY.md](SECURITY.md), never as a public issue.
