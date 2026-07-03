# waffle — build plan

A personal AI agent, written in Go, that you run on your own hardware. It
combines hermes-agent's learning loop and memory, nanoclaw's minimal
single-writer architecture and isolation model, openclaw's gateway/trust
design, and workweave/router's provider layer. Background and rationale for
each borrowed idea are in [research.md](./research.md).

## Design principles

1. **One binary.** `waffle` compiles to a single static binary containing the
   gateway, the agent runtime, the TUI, and all channel adapters. No Node, no
   Python, no service mesh. Subcommands select the role.
2. **Small enough to read.** nanoclaw's discipline: any subsystem should be
   reviewable by one person in one sitting. Features that threaten this get
   cut or become optional modules.
3. **SQLite for everything.** State, memory, message queues, schedules — all
   SQLite (pure-Go driver, no cgo). No Postgres, no Redis. Single-user scale
   makes this correct, not just convenient.
4. **Trust is tiered.** The owner's primary session runs tools on the host.
   Every other session — group chats, unknown senders, scheduled jobs —
   runs in a sandbox with an explicit tool policy. Unknown DM senders get a
   pairing code, not an agent.
5. **Keys never leave the host.** Sandboxes and subagents authenticate to a
   host-side provider proxy with scoped tokens. Real provider keys exist in
   exactly one place.
6. **Provider-agnostic core.** The agent loop speaks one canonical message
   format. Providers are translators. Swapping Anthropic for OpenRouter for a
   local Ollama model is config, not code.

## Architecture

```
                                   ┌──────────────────────────────────────┐
  Telegram ──┐                     │            waffle gateway            │
  Discord ───┤   channel           │                                      │
  CLI/TUI ───┤   adapters ───► router (entity model) ───► session manager │
  Webhook ───┘                     │        │                    │        │
                                   │   cron scheduler       agent runtime │
                                   │                             │        │
                                   │                        tool dispatch │
                                   │                         │       │    │
                                   │                     host tools  MCP  │
                                   │                                      │
                                   │  provider proxy ──► Anthropic/OpenAI/│
                                   │  (only key holder)   Gemini/Ollama/  │
                                   │                      weave-router    │
                                   └──────────┬───────────────────────────┘
                                              │ single-writer SQLite queues
                                   ┌──────────┴───────────┐
                                   │  sandboxed sessions  │
                                   │  (Docker containers) │
                                   └──────────────────────┘
```

### Entity model (from nanoclaw)

Every inbound message resolves `user → channel group → agent group →
session`. A *channel group* is "this Telegram chat" or "the CLI". An *agent
group* is a configured agent (workspace, persona, tool policy, model
preferences). A *session* is one conversation thread with history. This one
chain answers routing, isolation, and storage questions uniformly.

### Agent loop

Plain and synchronous per session, streaming to the channel:

1. Assemble context: system prompt (workspace prompt files + skills index +
   relevant memory), then session history.
2. Call the provider (streaming).
3. If the response contains tool calls, dispatch them (independent calls in
   parallel), append results, go to 2.
4. Otherwise deliver the reply, persist the turn, and (periodically) run the
   memory/skill reflection pass.

Context overflow is handled by summarize-and-truncate; full history stays in
SQLite and remains searchable via FTS5.

### Provider layer (from workweave/router)

`internal/llm` defines the canonical types (`Message`, `ToolCall`,
`ContentBlock`, streaming events) and a `Provider` interface. Translators
implement it for Anthropic Messages, OpenAI Chat Completions
(covers OpenRouter, Ollama, and most local servers), and Gemini. A
`baseURL`-only "openai-compatible" provider means a running
[workweave/router](https://github.com/workweave/router) instance slots in as
just another endpoint — that is the recommended way to get smart
multi-model routing rather than reimplementing cluster scoring in-tree.

The *provider proxy* is a thin HTTP listener inside the gateway that
sandboxed sessions call with scoped `wk_...` tokens; it injects the real key,
enforces per-session policy/limits, and forwards upstream (nanoclaw's Agent
Vault + router's two-tier key model).

### Tools

Native Go tools first: `bash` (policy-gated), `read`/`write`/`edit`, `fetch`,
`search`. Everything else arrives via MCP — waffle embeds an MCP client
(official `modelcontextprotocol/go-sdk`) so third-party servers provide the
long tail instead of a 40-tool builtin matrix. Tool availability is decided
by the session's policy (openclaw-style allow/deny), evaluated in the
gateway, not trusted to the sandbox.

### Sandboxing & IPC

Owner-primary sessions execute tools in-process on the host. All other
sessions get a Docker container whose only shared state is a pair of SQLite
files per session: `inbound.db` (host writes, agent reads) and `outbound.db`
(agent writes, host reads). One writer per file — no sockets, no races, and
the delivery poller survives container crashes. Containers see only their
mounted workspace and the provider proxy; never keys, never the host FS.

### Skills & memory (from hermes-agent)

- Workspace layout: `~/.waffle/workspace/<agent>/` with `AGENT.md` (persona +
  standing instructions), `MEMORY.md`, `USER.md`, and `skills/<name>/SKILL.md`
  (agentskills.io-compatible so hermes/openclaw skills port over).
- Memory recall: every turn is indexed in SQLite FTS5; a `remember` tool lets
  the agent curate `MEMORY.md`; a periodic reflection pass nudges curation
  and writes session summaries for cross-session recall.
- Learning loop (later phase): after a complex task completes, the agent is
  prompted to distill the procedure into a new or improved skill file.

### Scheduling

`robfig/cron`-style scheduler persisted in SQLite. A job is: cron expression +
prompt + agent group + delivery target. Jobs run as normal (sandboxed)
sessions, so "email me a Monday summary of my starred repos" is one row.

### Security posture (from openclaw)

- Unknown senders: pairing-code handshake before any agent access.
- Group chats: sandboxed, restricted tool policy, mention-gated.
- Gateway binds loopback by default; remote access is an explicit opt-in.
- All external content (messages, fetched pages, tool output) is treated as
  untrusted input, never as instructions.

## Repository layout

```
cmd/waffle/            main; subcommands: serve, chat, agent, skill, cron
internal/gateway/      control plane: wiring, provider proxy, HTTP admin
internal/entity/       user/channel-group/agent-group/session model
internal/channel/      Adapter interface; cli/, telegram/, discord/ ...
internal/agent/        the loop: context assembly, streaming, tool dispatch
internal/llm/          canonical types; anthropic/, openai/, gemini/
internal/tool/         Tool interface, builtins, MCP client, policy
internal/sandbox/      exec backends: host, docker; sqlite queue IPC
internal/skill/        SKILL.md discovery, indexing, learning loop
internal/memory/       FTS5 store, curation, reflection/summarization
internal/schedule/     cron persistence + runner
internal/store/        sqlite open/migrations (modernc.org/sqlite)
docs/                  this plan, research notes, ADRs
```

Key dependencies (all pure Go where possible): `modernc.org/sqlite`,
`charmbracelet/bubbletea` (TUI), `modelcontextprotocol/go-sdk` (MCP),
`robfig/cron/v3`, `go-telegram/bot`, `bwmarrin/discordgo`, stdlib
`net/http` + `encoding/json/v2` for providers, OTel SDK for tracing.

## Roadmap

Each phase ends with something you actually use daily; nothing depends on a
later phase to be useful.

**Phase 0 — Skeleton (small).** Go module, `cmd/waffle`, config loading
(`~/.waffle/config.toml`), SQLite store + migrations, CI (build, test,
`golangci-lint`), OTel wiring.

**Phase 1 — The loop (the heart).** `internal/llm` canonical types +
Anthropic and openai-compatible providers; agent loop with streaming; host
tools (`bash`, file ops, `fetch`); `waffle chat` TUI. *Milestone: a useful
Claude-backed terminal agent.*

**Phase 2 — Persistence, skills, memory.** Sessions/turns in SQLite; FTS5
recall + `remember` tool; workspace prompt files; `SKILL.md` loading and
`/skill` invocation; reflection pass writing session summaries. *Milestone:
it remembers you between sessions.*

**Phase 3 — Gateway + first channel.** Entity model, session manager,
channel `Adapter` interface, Telegram adapter, pairing codes,
`waffle serve`. *Milestone: message your agent from your phone.*

**Phase 4 — Isolation.** Docker sandbox backend, SQLite queue-pair IPC,
per-session tool policies, provider proxy with scoped tokens, group-chat
support. *Milestone: safe to add a group chat or a second user.*

**Phase 5 — Automation.** Cron scheduler with channel delivery; subagents
(parallel sandboxed sessions reporting back to a parent); MCP client.
*Milestone: unattended recurring jobs.*

**Phase 6 — The learning loop.** Post-task skill distillation, in-use skill
refinement, memory-curation nudges; optional weave-router deployment docs
for smart model routing; second channel (Discord) if wanted.

## Decisions to make now

- **Name the trust boundary in config, not code:** `agent groups` carry
  `sandbox: host|docker` and a tool allow/deny list from day one, even while
  Docker support is unimplemented — retrofitting policy is much harder.
- **Anthropic-first, never Anthropic-only:** Phase 1 ships both the Anthropic
  and openai-compatible translators so the no-lock-in property is real from
  the first release.
- **Buy routing, build the loop:** smart model routing stays out of tree
  (use workweave/router as an endpoint); the agent loop, memory, and skills
  are the parts worth owning.

## Explicitly out of scope (for now)

Voice/wake-word, canvas/GUI surfaces, companion mobile apps, >2 chat
channels, multi-tenant operation, Postgres, plugin marketplaces, and
in-tree smart routing. Each is a place the studied projects grew large;
waffle can add any of them later without architectural change.
