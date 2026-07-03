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
7. **Config is for trust boundaries; everything else is code.** nanoclaw's
   minimal-config philosophy, adapted to Go: `config.toml` names listen
   addresses, policies, and `secret://` references — and rejects unknown
   keys. Behavioral customization happens by changing waffle's own source.
   Your instance is your fork; upstream is a remote you merge. Which leads
   to:
8. **waffle works on waffle.** The agent can improve its own code through
   the same repo-workspace machinery it uses for any repo, with git as the
   audit trail and tests + self-check as the gate (see "Self-development
   loop").

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

The agent loop always runs on the host — that keeps one loop implementation
and keeps memory, skills, and session history host-side. What varies per
session is the **executor**: where its `bash`/`read`/`write`/`edit` tool
calls actually run.

- `host` executor: in-process, owner-primary sessions only.
- `docker` executor: a container per session. Because Go builds a static
  binary, the *same* `waffle` binary is bind-mounted read-only into any
  image and started as `waffle runner`.

Host and runner communicate nanoclaw-style, through a pair of SQLite files
per session on a shared mount: `inbound.db` (host writes exec requests,
runner reads) and `outbound.db` (runner writes results/output, host reads).
One writer per file — no sockets, no exec-attach fragility, results survive
container or gateway restarts, and a stopped container resumes exactly where
it left off. Containers see only their workspace volume, the queue mount,
and the host's proxy endpoints — never secrets, never the host filesystem.

### Repo workspaces ("work on this repo")

Saying *"work on matt-riley/foo"* (or `/repo matt-riley/foo`) turns into a
first-class object, the **workspace**: a container + named volume dedicated
to one repository, with a session bound to it.

Lifecycle:

1. **Open.** The gateway picks the image — if the repo has a
   `.devcontainer/devcontainer.json`, use that image; else a per-repo or
   global default dev image (git + language toolchains). It creates a named
   volume, starts the container with the `waffle runner` bind-mount, and
   clones the repo using a broker-minted short-lived token (see secret
   management). The token is used once by the runner and never stored.
2. **Work.** The session's tools execute in the container. Git pushes use
   the `waffle` binary itself as the repo's `credential.helper`: on demand it
   asks the host broker for a fresh token scoped to that one repository, so
   the container never holds a durable credential. Network egress is
   policy-controlled per workspace (none / allowlist via host egress proxy /
   full).
3. **Idle.** After a configurable idle timeout the container *stops* but the
   volume persists — "pick up where we left off tomorrow" is a container
   start, not a re-clone.
4. **Close.** On explicit close or TTL expiry, waffle verifies the branch is
   pushed (warns if not), then removes container + volume.

Workspaces are just sessions, so everything composes: several can run in
parallel, a subagent can be pointed at one, and a cron job can open one
("every Monday, update deps in repo X and open a PR"). `waffle ws list`
shows open workspaces, their repos, branches, and dirty state.

### Secret management

Layered, with one rule throughout: **raw secrets exist only in the host
store; everything else gets short-lived, scoped derivatives.**

- **Store.** `internal/secret` defines a `Store` interface. Default backend:
  an age-encrypted file `~/.waffle/secrets.age` whose key lives in the OS
  keychain (Keychain / libsecret via `go-keyring`), with a passphrase
  fallback for headless boxes. Env-var backend for CI; external backends
  (`op` / Vault) can come later behind the same interface. Managed with
  `waffle secret set|rm|ls` (values never echoed).
- **References, not values.** Config carries `secret://` URIs
  (`token = "secret://telegram/bot-token"`); resolution happens at use, in
  the gateway. Secrets never appear in config, SQLite, or logs.
- **Broker.** A host-side credential broker (part of the gateway, sibling of
  the provider proxy) is the only component that reads the store on behalf
  of sessions. It authenticates callers with per-session `wk_` tokens and
  applies the session's policy. It has three faces:
  - *LLM:* the provider proxy — injects the real API key upstream
    (unchanged from the base plan).
  - *Git:* mints short-lived, least-privilege repo credentials — a GitHub
    App installation token or fine-grained PAT scoped to the single repo,
    ~1 h TTL — for workspace clone/push.
  - *HTTP:* an egress proxy that injects `Authorization` headers for
    allowlisted hosts, so a sandboxed tool can call an API it is entitled
    to without ever seeing the key (nanoclaw's Agent Vault pattern).
- **Redaction.** The gateway keeps a digest set of all stored secret values
  and scrubs matches from tool output, model context, logs, and traces
  (`[redacted:github/pat]`) — protects against the "cat ~/.netrc into the
  transcript" class of leak even on the host executor.
- **Audit.** Every broker grant is a SQLite row: session, secret name,
  scope, TTL, timestamp.

Threat model in one line: a fully compromised session (prompt injection,
malicious repo code) can spend its own scoped tokens until they expire, but
cannot read another repo, another session's secrets, or any raw key.

### Skills & memory (from hermes-agent)

- Workspace layout: `~/.waffle/workspace/<agent>/` with `AGENT.md` (persona +
  standing instructions), `MEMORY.md`, `USER.md`, and `skills/<name>/SKILL.md`
  (agentskills.io-compatible so hermes/openclaw skills port over).
- Memory recall: every turn is indexed in SQLite FTS5; a `remember` tool lets
  the agent curate `MEMORY.md`; a periodic reflection pass nudges curation
  and writes session summaries for cross-session recall.
- Learning loop (later phase): after a complex task completes, the agent is
  prompted to distill the procedure into a new or improved skill file.

### Self-development loop (waffle works on waffle)

hermes-agent improves itself at the *prompt* level (skills). waffle goes one
level down: because it is a compiled single binary and its source is just a
git repo, code-level self-improvement is repo-workspace work where the repo
happens to be waffle's own.

The pipeline, using only machinery that already exists by Phase 5:

1. **Propose.** "Fix that timeout you keep hitting" (or the agent notices a
   recurring papercut during reflection) opens a workspace on the waffle
   repo — sandboxed container, scoped git credentials, like any other repo.
2. **Change.** The agent edits, then must get `go build`, `go vet`,
   `go test -race`, and `golangci-lint` green *inside the workspace*. The
   running gateway is never edited in place.
3. **Land.** The change is pushed as a branch. The approval gate is
   config-per-instance: review every diff, review PRs with CI green, or
   auto-land patch-level changes. Git is the audit trail either way — every
   self-modification is a commit that can be read, reverted, and merged
   with upstream.
4. **Deploy.** `waffle upgrade` builds the new binary from the approved
   ref, runs `waffle doctor` against it (self-check: config parses, DB
   migrates on a copy, secret store round-trips, providers reachable), then
   atomically swaps and re-execs the gateway. The previous binary is kept;
   `waffle rollback` is one command and no thought.

Two loops, one ladder: when reflection notices a *skill* shelling out the
same fragile pipeline for the third time, the distillation target stops
being SKILL.md and becomes a native Go tool submitted as a self-PR. Skills
are how waffle learns behavior; self-PRs are how learned behavior hardens
into code.

Safety properties worth stating: the self-workspace is sandboxed like any
other (a bad self-change can't touch the running host); the gate is
mechanical (tests + doctor) plus configurable human review; and because
customization-by-code replaces config sprawl (principle 7), the diff *is*
the config change — there is no second system to keep consistent.

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
cmd/waffle/            main; subcommands: serve, chat, runner, ws, secret, cron
internal/gateway/      control plane: wiring, broker (proxy/git/egress), admin
internal/entity/       user/channel-group/agent-group/session model
internal/channel/      Adapter interface; cli/, telegram/, discord/ ...
internal/agent/        the loop: context assembly, streaming, tool dispatch
internal/llm/          canonical types; anthropic/, openai/, gemini/
internal/tool/         Tool interface, builtins, MCP client, policy
internal/sandbox/      executors: host, docker; runner; sqlite queue IPC
internal/workspace/    repo workspaces: lifecycle, devcontainer, git helper
internal/secret/       Store iface; age+keyring backend; redaction; audit
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
`golangci-lint`), OTel wiring; `internal/secret` Store interface with the
age+keyring backend and `waffle secret` CLI (needed before the first
provider key is configured).

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

**Phase 4 — Isolation & the broker.** Docker executor + `waffle runner`,
SQLite queue-pair IPC, per-session tool policies, credential broker
(provider proxy + `wk_` session tokens), secret redaction filter, group-chat
support. *Milestone: safe to add a group chat or a second user.*

**Phase 5 — Repo workspaces.** `internal/workspace` lifecycle
(open/idle/close), devcontainer image selection, broker-minted git
credentials + `waffle` as in-container credential helper, egress policy,
`waffle ws` CLI and `/repo` command. *Milestone: "work on repo X" from any
channel spins up a container and ends in a pushed branch.*

**Phase 6 — Automation.** Cron scheduler with channel delivery; subagents
(parallel sandboxed sessions reporting back to a parent); MCP client.
*Milestone: unattended recurring jobs, including scheduled repo work.*

**Phase 7 — The learning loops.** Post-task skill distillation, in-use
skill refinement, memory-curation nudges; the self-development loop
(`waffle upgrade`, `waffle doctor`, `waffle rollback`, skill→Go-tool
promotion via self-PRs); optional weave-router deployment docs for smart
model routing; second channel (Discord) if wanted.

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
- **Loop on host, tools in sandbox:** waffle deliberately diverges from
  nanoclaw here (nanoclaw runs the whole agent in the container). One loop
  implementation, memory/skills stay host-side, and containers need nothing
  but the bind-mounted runner — at the cost of tool traffic transiting the
  host, which is acceptable for a single-user system.
- **GitHub App over PAT for repo credentials:** installation tokens are
  natively short-lived and repo-scoped, which is exactly the broker's
  contract. A fine-grained PAT is the fallback for the first iteration.

## Explicitly out of scope (for now)

Voice/wake-word, canvas/GUI surfaces, companion mobile apps, >2 chat
channels, multi-tenant operation, Postgres, plugin marketplaces, and
in-tree smart routing. Each is a place the studied projects grew large;
waffle can add any of them later without architectural change.
