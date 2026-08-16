---
title: "Chat clients and connection modes"
description: "The terminal client in full: connection modes, keys, commands, and managed-service troubleshooting."
sidebar:
  order: 2
---

*In plain terms: [Meet Waffle](/docs/meet/what-waffle-is/).*

`waffle chat` is Waffle's keyboard-first Focused Conversation interface. An
interactive terminal gets the full-screen TUI. Redirected stdin or stdout, or
an explicit `--plain`, gets stable plain text suitable for scripts, logs,
accessibility tools, and tests.

## First run

On a new install (empty `$WAFFLE_HOME`), run setup once before chat:

```sh
waffle setup
```

`waffle setup` walks through secret-store identity creation (if needed), guided
provider enrollment (credential + model aliases), a minimal
`[agent.profile.main]` block, and an offer to enable [Waffle Desk](/docs/under-the-hood/waffle-desk/),
the loopback browser interface — printing its URL when enabled. Re-running setup
is safe: completed steps print an "already configured" message and are skipped.
After setup succeeds:

```sh
waffle chat
```

If chat is started with no providers configured, it exits with a message
directing you to `waffle setup`.

## Managed-host quick start

On an Infra-managed host in **Ready** state, run:

```sh
waffle chat
```

Do not use `sudo waffle chat`. The managed command connects the ordinary user
to `/run/waffle/chat.sock`; the running service continues to own the agent,
provider calls, tools, sessions, memory, and workspaces. Continue the most
recent session or choose a profile with:

```sh
waffle chat --continue
waffle chat --profile reviewer
```

## Connection and display modes

Standalone `waffle chat` uses direct mode when neither `--socket` nor a
non-empty `WAFFLE_CHAT_SOCKET` is present. Direct mode opens the current
user's Waffle configuration, encrypted secrets, and SQLite state. The client
selection order is:

1. `--socket /absolute/path.sock`
2. a non-empty `WAFFLE_CHAT_SOCKET`
3. direct mode

An explicitly selected socket must be absolute. If it cannot be reached, chat
reports that path and the service/socket units; it does not fall back to direct
mode or open local state. The managed wrapper always selects
`/run/waffle/chat.sock` and rejects attempts to override it.

### Waffle Desk browser companion

Waffle Desk is a browser companion to `waffle chat`, not a replacement for the
terminal interface. Enable the dashboard, run `waffle serve`, and open
`http://127.0.0.1:8422/desk/` on the same machine. The browser and terminal
paths share Waffle's session ownership, so they cannot mutate one session at
the same time.

If another client already owns the session, the Desk reports that the chat
session is active instead of creating a second runtime. Finish or close the
other client, then use the Desk's explicit refresh action. The Desk never
retries a submitted turn automatically.

The Desk model picker uses the existing `/model <alias>` command. Like the
terminal command, it persists the alias for the current session only and does
not change Waffle's global default.

The complete invocation is:

```text
waffle chat [-c|--continue] [--profile name] [--socket absolute-path] [--plain]
```

`-c` is an alias for `--continue`. Use `--plain` to force deterministic line
mode. Non-terminal input or output selects the same mode automatically, even
over a socket. `NO_COLOR=1` disables color in the TUI; text, borders, status
symbols, and focus remain readable without it.

## Focused Conversation

The header shows the session, selected model, profile, and either `local
service` or `direct`. The middle viewport contains user/assistant cards and
compact tool rows. The composer stays at the bottom, with current controls and
usage in the footer. Lists and confirmations open as overlays and return focus
to the composer when dismissed.

This sanitized capture is generated from the accepted Focused Conversation
golden fixture; its messages and identifiers are test data:

```text
 Waffle  Focused deploy · 01SNAPSH                                           gpt · main · direct
────────────────────────────────────────────────────────────────────────────────────────────────
You
Explain the failed deploy.

Waffle
Finding
The service could not parse its database URL.

            ┌──────────────────────────────────────────────────────────────────────┐
            │ Confirm                                                              │
            │ Switch to the selected session and preserve all stored history befo… │
            │                                                                      │
            │ Enter confirm · Esc cancel                                           │
            └──────────────────────────────────────────────────────────────────────┘
  ✓ read logs   2.1 KB
Error
Deploy remains unhealthy.

┌──────────────────────────────────────────────────────────────────────────────────────────────┐
│ Ask Waffle…                                                                                  │
└──────────────────────────────────────────────────────────────────────────────────────────────┘
/help  /model  /sessions                                                  Alt+↵ newline · ↵ send
```

### Keys

- `Enter` submits the composer or accepts the selected overlay item.
- `Alt+Enter` inserts a newline; the composer grows to keep multiline input
  visible.
- Typing `/` opens filtered command completion. `Up`/`Down` selects and `Tab`
  completes the highlighted command.
- `Escape` cancels an active turn. When idle it closes an overlay or command
  palette and returns focus to the composer.
- `Ctrl+C` cancels an active turn. When idle, press it twice to exit.
- `Ctrl+D` exits when the composer is empty.
- `PageUp`/`PageDown` or `Ctrl+Up`/`Ctrl+Down` scrolls the transcript.
- `/exit` closes gracefully. If the service disconnects, acknowledge the
  frozen transcript with `Enter`, `Escape`, or `Ctrl+C`.

While a model turn is already running, plain-text `Enter` does not start a
second concurrent turn. The message is **queued** (composer cleared, a Notice
card acknowledges the queue) and auto-submitted as the next turn once the
active turn finishes. Submitting again while a queue is held **replaces** the
queued draft and shows a replace notice. Recognized slash commands (for
example `/help` or `/status`) still run immediately during an active turn.

## Commands

Command names match the complete first word. For example, `/modelsx` is an
ordinary model message, not `/models`. Invalid local syntax is never sent to
the model.

| Syntax | Purpose |
|---|---|
| `/help` | Open the command and key reference. |
| `/exit` (alias `/quit`) | Finish and close chat. |
| `/model [alias]` | Open the model picker, or validate, persist, and select an alias for this session. |
| `/models` | List configured aliases, provider labels, and the current model without credentials. |
| `/new` (aliases `/reset`, `/clear`) | Preserve stored history and start a new session; in plain mode confirm a prompted reset with `/new confirm`. |
| `/sessions` | List recent sessions with model and summary metadata. |
| `/resume [session]` | Open the session picker, or resume the exact session ID. |
| `/status` | Show session, model/provider, profile, connection, sandbox, and workspace state. |
| `/usage` | Show current and persisted request/token usage. |
| `/permissions` | Show the effective sandbox and tool allow/deny policy. |
| `/skill <name> [args]` | Invoke a configured skill. |
| `/skills [attach <name>\|detach <name>]` | List active/session skills, attach an active skill to this session, or detach it without deactivating it. |
| `/repo <owner/repo>` | Open and bind a repository workspace. |
| `/workset [list\|replace <id> <text>\|drop <id>\|clear]` | Inspect or correct the session working set. |
| `/branch <turn>` | Fork this conversation from a completed exchange (turn sequence from the transcript); an empty boundary branches from the final exchange. |

Outside the table's Markdown escaping, the exact working-set syntax is
`/workset [list|replace <id> <text>|drop <id>|clear]`.
The exact session-skill syntax is
`/skills [attach <name>|detach <name>]`. Attached active skills are restored
with the session and forced into its system context, with a 256 KiB combined
attachment-block limit. An attachment that was removed or deactivated remains
visible as unavailable until it is restored or detached; unavailable skills
are never injected.

The selected `/model` alias is stored on the current session. `/resume
[session]` and `waffle chat --continue` restore that session's model instead
of silently selecting the current default. If an alias was removed from
configuration, chat keeps the unavailable alias visible, blocks turns, and
offers `/models` so the operator can choose a valid replacement.

## Managed service troubleshooting

First inspect both coupled units and the service journal:

```sh
systemctl status waffle.service waffle-chat.socket
journalctl -u waffle.service -u waffle-chat.socket --since '15 minutes ago'
```

The **Ready** lifecycle enables and starts `waffle-chat.socket`, confirms the
socket is active, requires `waffle.service` to be active, verifies `/healthz`,
then enables the service. The provider/rollout flow may already have started
the service; otherwise a socket connection may activate it. **Installed**
stops and disables the service before the socket so a chat connection cannot
reactivate it. Use the Infra lifecycle operation to change those states; do
not work around a failed deployment with `sudo waffle chat`.

Socket access is enforced by ownership and mode on `/run/waffle/chat.sock`
(`root:sudo`, `0660`). A socket client does not read service provider
credentials, the age identity, configuration bodies, database paths, or
service-owned files; those stay inside `waffle.service`. The socket is local
only and has no TCP or public listener.

The binary, wrapper, `waffle.service`, and `waffle-chat.socket` are one
compatible release set. During rollback Infra restores them together. An old
client cannot speak to an incompatible service protocol: it fails with a
concise version error that reports both versions, rather than retrying or
falling back to direct state.
