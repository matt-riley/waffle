---
title: Configuration reference
description: Every table in the configuration contract, what it governs, and the rules that apply to it.
sidebar:
  order: 2
---

*In plain terms: [Meet Waffle](/docs/meet/what-waffle-is/).*

`config.example.toml` in the repository is the configuration contract. This page
describes each table in it. A test (`configuration reference covers every
section of the contract`) fails if the contract grows a table this page does not
describe, so the two cannot drift apart.

Three rules apply throughout, and none of them have exceptions:

- **Configuration is strict.** Unknown or invalid keys are an error. There is no
  permissive fallback, and adding one would be a defect.
- **Secrets are never values.** `config.toml` holds `secret://` references only;
  real values live in the encrypted store.
- **Profiles are trust boundaries**, not personality presets. A profile fixes
  the system prompt, model, tools, and sandbox together.

Providers are deliberately absent from the example: a fresh install may omit
provider, model, and default-model configuration entirely until
`waffle provider add` validates the first connection on the host. Prefer that
command over hand-editing — it probes upstream and commits the encrypted
credential and the config change as one transaction.

## `[chat]`

Optional standalone local chat service (`socket`). Managed deployments normally
receive `/run/waffle/chat.sock` from systemd socket activation instead. Setting
this does not change client behaviour: `waffle chat` stays in direct mode unless
`--socket` or `WAFFLE_CHAT_SOCKET` selects a socket explicitly.

Every ancestor of the socket path is checked for ownership and mode; the socket
itself is `0600`.

## `[dashboard]`

Waffle Desk (`enabled`, plus skill-import roots and permitted Git hosts).
Disabled by default, and a disabled Desk cannot enable itself. Desk shares the
loopback `gateway.status_listen` address rather than adding a listener.

See [Waffle Desk](/docs/under-the-hood/waffle-desk/).

## `[dashboard.tailnet]`

Authorises Desk requests proxied by a same-host `tailscale serve`, using
allowlisted Tailscale identity headers. Those requests may address only `/desk/`
and `/api/v1/desk/*`. Funnel requests are always rejected, and this never moves
the bind address — `/status` and `/healthz` stay loopback-only in every
configuration.

## `[memory]`

Durable-memory behaviour, principally `write_gate` (`auto`, `notify`, or
`review`) and the injection budget. Under `review`, durable writes queue for
approval through `waffle candidates`.

Writes derived from untrusted content are queued regardless of this setting.

## `[selfdev]`

Self-development: `approval` (`manual`, `ci`, or `auto-patch`), verification
steps, protected paths, and `required_checks`. Under `ci`, every required check
must have completed successfully **for the exact candidate SHA**; missing,
pending, or stale checks fail closed.

Every upgrade resolves one commit SHA and builds it in an isolated detached
worktree, so the configured checkout is never modified.

## `[github.app]`

The scoped GitHub App backing Git credentials for workspaces: `app_id`,
`installation_id`, `private_key` (a `secret://` reference), and `base_url` for
GitHub Enterprise. Register it with the narrowest permissions the tools you use
require.

## `[broker]`

The host-side broker the sandbox authenticates to (`listen`). It issues scoped
`wk_` session tokens with a 24-hour TTL; expired tokens stop authorising the
proxy, git-credential, and API faces and are swept from memory. Real
credentials never cross into the sandbox.

## `[agent]`

Top-level agent settings: `default_profile` and the base `system` prompt.

## `[agent.profile.main]`

The owner-interactive profile: system prompt, model, sandbox mode (`host` or
`docker`), and which child profiles it may spawn. This is the profile
`waffle chat` uses unless `--profile` selects another.

## `[agent.profile.main.tools]`

Tool policy for `main` — `allow` and `deny` lists. A profile can tighten its
group's policy and can never widen it.

## `[agent.profile.researcher]`

Example restricted profile: docker sandbox, read and fetch only.

## `[agent.profile.researcher.tools]`

Tool policy for `researcher`, including the filesystem roots its file tools may
reach.

## `[agent.profile.reviewer]`

Example restricted profile: docker sandbox, read-only critique.

## `[agent.profile.reviewer.tools]`

Tool policy for `reviewer`.

## `[channel.telegram]`

The Telegram gateway: `enabled`, a `secret://` bot token, and the maximum
attachment size. Pairing binds your own account; anyone else receives a code and
nothing more.

## `[sandbox]`

Sandbox execution: working directory, the filesystem roots the built-in file
tools may reach, PID limits, and the enforcer. On a non-Linux host running
Docker mode, `runner_binary` must be an absolute path to a static **linux**
build matching the container's `GOARCH`.

Tool policy can deny a tool by name but cannot widen the filesystem boundary.

See [The sandbox queue](/docs/under-the-hood/sandbox/).

## `[search.brave]`

A named search connection: `type`, an `api_key` reference, and result limits.

## `[search.default]`

The search connection used when none is named.

## `[limits]`

Spend and throughput ceilings: tokens per day, requests per hour, and tunnel
bytes per session.
