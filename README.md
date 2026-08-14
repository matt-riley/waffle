# waffle

A personal AI agent, written in Go, that you run on your own hardware. One
static binary holds the agent loop, a Telegram gateway, terminal chat, Waffle
Desk, and a provider-agnostic LLM layer. It serves exactly one owner. Sandbox
and network policy are deny-by-default. Named for the cat.

## Status

Developer preview. Tagged releases are on
[Releases](https://github.com/matt-riley/waffle/releases) and in
[CHANGELOG.md](CHANGELOG.md). They do not promise v1 compatibility.
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
starter `[agent.profile.main]`. After that, talk in the TUI. Telegram uses
the same loop over `waffle serve` once a bot token is configured: message the
bot, then `waffle pair approve` on the host.

## Where it runs

On a laptop, `waffle chat` is direct: it opens your config, secrets, and
SQLite store in-process. On a managed host, `waffle serve` owns the agent and
`waffle chat` attaches to the service socket without `sudo`. First run and the
socket path are in [docs/chat.md](docs/chat.md). Installed vs Ready, provider
enrollment, and host rollout are in [docs/deploy.md](docs/deploy.md).

## What it is not

Waffle is not multi-tenant and not a guest or group chatbot platform.
Pairing binds the owner's own accounts; anyone else gets a code and nothing
more. Discord is not shipped. There is no plugin marketplace, no in-tree
smart routing, and no Postgres. Voice, companion apps, and extra chat
channels stay out of scope.

Keys never leave the host. Sandboxes authenticate to a host-side broker with
scoped tokens. An agent profile cannot widen its group's tool or sandbox
policy.

## Until v1

Phases 0–7 from [docs/plan.md](docs/plan.md) are in daily use. What is still
open there is a cut we chose (OpenAI-compatible Gemini, Telegram only, no
marketplace), not unfinished work.

v1 will mean the owner loops above stay documented from this README,
`waffle upgrade` / `waffle rollback` stays the compatibility contract, and
breaking config or schema changes are called out in the changelog. Until
then, treat tags as preview.

## Docs

- [Architecture and trust model](docs/plan.md)
- [First run and chat](docs/chat.md)
- [Skills, profiles, cron, learning](docs/usage-guide.md)
- [Waffle Desk](docs/waffle-desk.md)
- [Managed host install](docs/deploy.md)
- [Prior art](docs/research.md)
- [Configuration contract](config.example.toml)

## Feedback

Bug reports are welcome if you actually ran it. This is a personal agent,
not a feature-intake queue. Report vulnerabilities privately through
[SECURITY.md](SECURITY.md), never as a public issue.
