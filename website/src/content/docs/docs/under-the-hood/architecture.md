---
title: How it fits together
description: The binary, the request path, the trust boundaries, and where state lands.
sidebar:
  order: 1
---

*In plain terms: [What Waffle is](/docs/meet/what-waffle-is/) and
[Keeping her safe](/docs/meet/keeping-her-safe/).*

Waffle is one Go binary. The agent loop, the messaging gateway, the terminal
chat client, the browser cockpit, and the provider-agnostic LLM layer all ship
inside it, and subcommands select which of them runs.

```sh
waffle setup      # first deployment: identity, provider, starter profile
waffle chat       # terminal chat client
waffle serve      # background processing, gateway, brokers
waffle status     # what is running
waffle doctor     # self-check the installation
```

All durable state lives under `$WAFFLE_HOME`, which defaults to `~/.waffle`.

## One owner of background work

`waffle serve` is the sole owner of background processing, the messaging
gateway, and in-memory broker tokens. It holds a lock file with a PID and a
heartbeat, so a second `serve` refuses to start rather than racing the first.
A lock left behind by a dead process is reclaimed once that PID is confirmed
gone.

On a laptop you can skip it: `waffle chat` runs in direct mode and opens the
config, secrets, and store in-process. On a managed host, `serve` owns
everything and `chat` attaches over a local socket.

## The inbound path

A message from a chat channel takes a fixed route:

```
channel adapter → gateway → entity resolution → agent
                             (user → channel group
                              → agent group → session)
```

Handlers run concurrently across conversations but are serialised per channel
group, so two messages in the same conversation cannot interleave.

## Trust boundaries

Two things are trust boundaries, and they are the ones to reason about:

**Agent groups** define the ceiling — which tools exist, which sandbox mode
applies, what the network policy is.

**Agent profiles** select a system prompt, a model, and a narrower set of tools
*within* that ceiling. A profile can tighten a group's policy. It can never
widen it. Repository-level policy, if present, can tighten things further still.

Four tiers ship:

| Tier | Used for | Default posture |
| --- | --- | --- |
| `main` | Owner, interactive | Most permissive |
| `cron` | Unattended scheduled work | No host shell, no memory writes |
| `issue` | Tracker/board intake | No host shell, no memory writes |
| `group` | Multi-party conversation | No host shell, no memory writes |

The three restricted tiers exist because unattended and multi-party contexts are
where a prompt-injection attempt is most likely to arrive and least likely to be
noticed.

## Credentials and the sandbox

The agent loop always runs on the host. Only *tool execution* varies: it happens
either directly on the host or inside a Docker container.

Sandboxes never receive raw provider or Git credentials. Instead:

- Real secrets live host-side in an age-encrypted store.
- A host-side broker issues scoped `wk_` session tokens with a 24-hour TTL.
- Expired tokens stop authorising the proxy, git-credential, and API faces, and
  are swept from memory. Long-running work re-mints rather than holding one
  token open.
- The container drops its default route, so only the host gateway is reachable.
  This fails closed: a misconfiguration removes access rather than granting it.
- Secrets are scrubbed from anything rendered or logged.

Rather than exposing one generic "call an API" tool, each configured broker API
face becomes its own narrow tool. Policy can then grant or deny a specific face
by name, and the credential behind it never crosses the boundary.

## Storage

All durable state is SQLite, opened in WAL mode with a **single connection**.
That constraint shapes the code: no long transactions, no foreground
maintenance, and no per-row commits on request paths.

Schema changes are ordered, embedded SQL migrations. Version numbers stay
contiguous, and every migration must remain safe to apply to a database that
already exists in the wild.

## Memory, in three layers

| Layer | Lifetime | Written by |
| --- | --- | --- |
| Transcript sessions | Per session, searchable | Automatic |
| Working set (goals, constraints) | Session-scoped, not durable | `workspace_update` |
| Durable workspace notes | Permanent until deleted | Memory tools only |

Working-set state stays session-scoped by design; promotion to durable notes
happens only through the memory tools. A write gate can route those durable
writes through a review queue, and writes derived from untrusted content — a
fetched web page, for instance — are queued regardless of the setting.

## The LLM layer

Canonical message and tool types are provider-neutral. Provider packages
translate at the boundary, which is the rule that keeps the agent loop free of
per-vendor special cases: provider-specific behaviour belongs in the translator,
never in the loop.

A **Provider Connection** is one configured endpoint. A **Model Alias** is a
stable local name that selects one upstream model through exactly one Provider
Connection. Aliases are what you actually reference; the catalogue of available
models is a disposable cache and never authoritative over them.

## The browser cockpit

Waffle Desk is disabled by default. When enabled it is **not** a separate
listener — it shares the loopback status address, `127.0.0.1:8422` by default.
That address stays loopback-only in every configuration. A tailnet integration
can authorise requests proxied by a same-host `tailscale serve`, but it never
moves the bind address, and the raw status and health endpoints stay local-only
regardless.
