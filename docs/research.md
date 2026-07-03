# Research: prior art for waffle

Research date: 2026-07-03. Four projects were studied as inputs to waffle's design:
[hermes-agent](https://github.com/NousResearch/hermes-agent),
[nanoclaw](https://github.com/nanocoai/nanoclaw),
[openclaw](https://github.com/openclaw/openclaw), and
[workweave/router](https://github.com/workweave/router).

The short version: hermes-agent shows what a *capable* personal agent looks like
(learning loop, memory, skills, scheduling), nanoclaw shows how to keep the
implementation *small and trustworthy* (single-writer SQLite IPC, container
isolation, skills-install-code instead of config), openclaw shows a mature
*gateway/channel* architecture and its security posture, and workweave/router
shows how to build the *provider layer* in Go (protocol translation + smart
model routing).

---

## 1. hermes-agent (Nous Research)

**What it is.** A self-improving personal agent, Python (~82%) + TypeScript.
MIT licensed, very large community (~208k stars). Marketed as "the agent that
grows with you" — the successor to workflows people ran on OpenClaw (it ships
an OpenClaw migration path).

**Architecture.**
- *Agent core* — the decision-making loop.
- *Gateway* — one process that fronts Telegram, Discord, Slack, WhatsApp,
  Signal, Email, and the terminal, with cross-platform conversation
  continuity and shared slash commands.
- *Skills system* — procedural memory stored in `~/.hermes/skills/`,
  compatible with the agentskills.io open standard, invoked as `/<skill>`.
- *Tools subsystem* — 40+ built-in tools plus MCP servers for extension.
- *Terminal backends* — six execution environments (local, Docker, SSH,
  Singularity, Modal, Daytona), so "where code runs" is pluggable.
- *Memory layer* — persistent agent-curated memory (MEMORY.md / USER.md
  style files) plus SQLite FTS5 full-text search over past sessions with
  LLM summarization for recall; Honcho dialectic user modeling on top.

**The learning loop (its signature feature).**
- Skills are created *autonomously* after the agent completes a complex task.
- Skills self-improve during use (the agent edits its own skill files).
- Periodic "persistence nudges" prompt the agent to curate memory.

**Other notable capabilities.**
- Built-in cron scheduler: natural-language recurring jobs with delivery to
  any connected platform.
- Distributed subagents for parallel workstreams; tools callable from Python
  scripts via RPC to collapse multi-step pipelines.
- Provider-agnostic: Nous Portal (300+ models), OpenRouter, OpenAI, custom
  endpoints; model switching is a single command.

**What waffle should take.**
- The skills-as-procedural-memory model and the *learning loop* (post-task
  skill creation, in-use skill refinement).
- SQLite FTS5 session search + LLM summarization as the memory recall design.
- Cron scheduling with results delivered to any channel.
- The "no provider lock-in" stance.

**What waffle should not take.** The sheer surface area (six terminal
backends, 40+ tools, voice transcription, trajectory generation for model
training). That breadth is why it needs a big community to maintain.

---

## 2. nanoclaw

**What it is.** A deliberately tiny personal-agent framework in TypeScript
(~92%), positioned as the understandable alternative to OpenClaw: "one
process and a handful of files." Claude-first (built on the Claude Agent
SDK), with other providers installable.

**Architecture — the interesting part.**
- *Entity model:* `user → messaging group → agent group → session`. Every
  inbound message resolves through that chain to exactly one session.
- *SQLite as IPC:* two SQLite files per session — `inbound.db` (host writes,
  container reads) and `outbound.db` (container writes, host reads). Each
  file has **exactly one writer**, so there is no cross-mount contention, no
  socket protocol, no stdin piping. The host router writes an inbound row;
  the container wakes, runs the agent, writes outbound rows; a delivery
  poller on the host ships them to the channel.
- *Isolation:* each agent group runs in its own Docker container (optionally
  micro-VM or Apple Container) and can only see explicitly mounted
  directories. Security comes from OS-level isolation, not app-level
  permission checks — an explicit critique of OpenClaw.
- *Credential security:* raw API keys never enter a container. Outbound LLM
  requests route through a host-side vault/proxy that injects credentials at
  request time and can enforce rate limits and policies.

**Philosophy.**
- *Skills install code, not config.* Channel adapters and alternative
  providers live on side branches; `/add-telegram` etc. copies the module
  into the tree, wires registration, pins deps. No plugin API, no config
  sprawl — you own and read your fork.
- *AI-native maintenance:* setup and debugging hand off to Claude Code when
  judgment is needed.

**What waffle should take.**
- The entity model, near-verbatim — it is the cleanest routing formalism of
  the four.
- Single-writer SQLite queues as the host↔sandbox IPC mechanism.
- Keys-never-in-the-sandbox: credential injection at a host-side proxy.
- The size discipline: every subsystem must stay readable by one person.

**What waffle should not take.** The "no config file, edit your fork"
extreme. Go is compiled; a single binary with a small config file is the more
natural equivalent, with code-generation-style skills reserved for genuinely
custom extensions.

---

## 3. openclaw

**What it is.** The most established of the three assistants: a local-first
personal AI you run on your own devices, Node/TypeScript, 600+ contributors,
huge channel matrix (WhatsApp, Telegram, Slack, Discord, Signal, iMessage,
Matrix, Teams, IRC, LINE, and a dozen more, plus native macOS/iOS/Android
nodes, wake-word voice, and a Live Canvas).

**Architecture.**
- *Gateway control plane:* one local process owns sessions, channels, tools,
  and events. Companion apps and device nodes pair over WebSocket.
- *Multi-agent routing:* inbound channels/accounts/peers can be routed to
  isolated agents, each with its own workspace and sessions.
- *Sandboxing policy:* the `main` session runs tools directly on the host
  (full access); non-main sessions (groups, unknown peers) default to
  sandboxes (Docker default; SSH and OpenShell backends) with an
  allow/deny tool policy (e.g. allow bash/read/write/edit, deny
  browser/canvas/gateway).
- *Workspace:* `~/.openclaw/workspace` holds agent state; prompt files
  (`AGENTS.md`, `SOUL.md`, `TOOLS.md`) are injected into context; skills are
  `skills/<skill>/SKILL.md` directories discoverable via the ClawHub
  registry.
- *Security posture:* inbound DMs are untrusted by default — unknown senders
  get a pairing code and must be explicitly approved; remote exposure of the
  gateway requires deliberate configuration.

**What waffle should take.**
- The gateway-as-control-plane framing and the session-routing model.
- The tiered trust policy: owner's primary session gets the host; everyone
  and everything else gets a sandbox and a restricted tool policy.
- Pairing codes for unknown senders.
- Workspace layout with injected prompt files and `SKILL.md`-per-directory
  skills (this converged with hermes and the agentskills.io standard).

**What waffle should not take.** The channel matrix and companion-app
ecosystem. nanoclaw exists precisely because OpenClaw's surface area became
hard to audit; waffle should start with 1–2 channels done well.

---

## 4. workweave/router ("Weave Router")

**What it is.** A Go (~84%) drop-in LLM proxy that routes each request to the
best model for the prompt. Directly relevant twice over: it solves waffle's
provider-abstraction problem *and* it is a reference for idiomatic Go in this
domain.

**Architecture.**
- *Protocol translation:* natively speaks Anthropic Messages
  (`/v1/messages`), OpenAI Chat Completions (`/v1/chat/completions`), and
  Gemini `generateContent` — any client dialect to any upstream provider,
  with streaming, tool use, and vision preserved across translations.
- *Smart routing:* a cluster scorer derived from Avengers-Pro
  ("Performance–Efficiency Optimized Routing", arXiv:2508.12631) picks the
  model per request in <50 ms using a small on-box embedding model — no
  external calls, no hand-written heuristics. `/v1/route` returns the routing
  decision without calling upstream. Claims 40–70% cost reduction.
- *BYOK key model:* upstream provider keys stay on your box in `.env.local`,
  encrypted at rest; clients authenticate with separate `rk_...` router
  tokens. The router is the only thing that ever holds real keys.
- *Ops:* Postgres for state, OTLP traces out of the box, web dashboard,
  integrations for Claude Code / Codex / opencode.

**What waffle should take.**
- The canonical-message-format + per-provider translation design for the
  provider layer.
- The two-tier key model (`rk_` tokens vs. upstream keys) — it composes
  perfectly with nanoclaw's keys-never-in-the-sandbox rule: sandboxes get a
  scoped router token, never a provider key.
- OTLP tracing from day one.
- Pragmatically: waffle can simply *point at* a running Weave Router as an
  OpenAI/Anthropic-compatible base URL and get multi-provider + smart routing
  for free, before (or instead of) building routing in-house.

**What waffle should not take.** Postgres and the dashboard. waffle is
single-user; SQLite is the right store everywhere.

---

## Synthesis — the design waffle inherits

| Concern | Chosen approach | Borrowed from |
|---|---|---|
| Overall shape | One Go binary, gateway as control plane | openclaw, nanoclaw |
| Routing formalism | user → channel group → agent group → session | nanoclaw |
| Host↔sandbox IPC | Single-writer SQLite queue pairs | nanoclaw |
| Trust model | Owner session on host; others sandboxed w/ tool policy | openclaw |
| Sender auth | Pairing codes for unknown DMs | openclaw |
| Credentials | Host-side proxy injects keys; sandboxes get scoped tokens | nanoclaw + router |
| Provider layer | Canonical message type + translators; optional smart routing | router |
| Skills | `SKILL.md` dirs, agentskills.io-compatible, learning loop later | hermes, openclaw |
| Memory | Curated memory files + SQLite FTS5 recall + summarization | hermes |
| Scheduling | Built-in cron with delivery to any channel | hermes |
| Extensibility | MCP client for third-party tools | hermes, nanoclaw |
| Size discipline | Every subsystem readable by one person | nanoclaw |

The full architecture and phased roadmap are in [plan.md](./plan.md).
