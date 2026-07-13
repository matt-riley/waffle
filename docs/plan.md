# waffle — build plan

A personal AI agent, written in Go, that you run on your own hardware. It
combines hermes-agent's learning loop and memory, nanoclaw's minimal
single-writer architecture and isolation model, openclaw's gateway/trust
design, and workweave/router's provider layer. Background and rationale for
each borrowed idea are in [research.md](./research.md).

## Design principles

1. **One binary.** `waffle` compiles to a single static binary containing the
   gateway, the agent runtime, the terminal chat REPL, and all channel
   adapters. No Node, no Python, no service mesh. Subcommands select the role.
   (A full-screen TUI was deliberately cut — see [Deviations](#deviations).)
2. **Small enough to read.** nanoclaw's discipline: any subsystem should be
   reviewable by one person in one sitting. Features that threaten this get
   cut or become optional modules.
3. **SQLite for everything.** State, memory, message queues, schedules — all
   SQLite (pure-Go driver, no cgo). No Postgres, no Redis. Single-user scale
   makes this correct, not just convenient.
4. **Single-owner, tiered trust.** waffle serves exactly one person. There
   is no guest tier: pairing exists to bind the *owner's own* accounts on
   new channels (approval happens on the host — shell access is the
   ownership proof), and anyone else gets a pairing code and nothing more.
   Trust still tiers by *session*: the owner's interactive sessions run
   tools on the host; risky contexts — repo workspaces, scheduled jobs —
   run in sandboxes with explicit tool policies.
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
  CLI/chat ──┤   channel           │                                      │
             │   adapters ───► router (entity model) ───► session manager │
             │                     │        │                    │        │
             │                     │   cron scheduler       agent runtime │
             │                     │                             │        │
             │                     │                        tool dispatch │
             │                     │                         │       │    │
             │                     │                     host tools  MCP  │
             │                     │                      (stdio only)    │
             │                     │  provider proxy ──► Anthropic /      │
             │                     │  (only key holder)   OpenAI-compat   │
             │                     │                   (Ollama/router/    │
             │                     │                    Gemini endpoint)  │
                                   └──────────┬───────────────────────────┘
                                              │ single-writer SQLite queues
                                   ┌──────────┴───────────┐
                                   │  sandboxed sessions  │
                                   │  (Docker containers) │
                                   └──────────────────────┘
```

Discord is optional and not shipped (see [Deviations](#deviations)).

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

### Scheduled job retries

Unattended cron jobs use the optional `[jobs]` policy in `config.toml`.
`max_attempts` defaults to `1` (the legacy fire-once behavior); retries use
exponential `base_backoff` capped by `max_backoff` and a `stall_timeout`
watchdog. Attempt and next-retry state is persisted in SQLite and shown by
`waffle cron ls`. Retry prompts include their attempt number, and only the
final exhausted attempt sends a failure notification.

Context overflow is handled by summarize-and-truncate with an in-process
summary cache (one summarize per prefix segment / session); summaries
name turn ranges for `expand_context`. Full history stays in SQLite and
remains searchable via FTS5. Optional `[provider] utility_model` is used
for summarization and reflection.

### Provider layer (from workweave/router)

`internal/llm` defines the canonical types (`Message`, `ToolCall`,
`ContentBlock`, streaming events) and a `Provider` interface. Translators
implement it for Anthropic Messages and OpenAI Chat Completions
(covers OpenRouter, Ollama, Gemini's OpenAI-compatible endpoint, and most
local servers). There is **no** first-class `gemini/` package — use
`name = "openai"` with Gemini's compatible `base_url` (see
[Deviations](#deviations)). A `baseURL`-only "openai-compatible" provider
means a running [workweave/router](https://github.com/workweave/router)
instance slots in as just another endpoint — that is the recommended way to
get smart multi-model routing rather than reimplementing cluster scoring
in-tree.

The *provider proxy* is a thin HTTP listener inside the gateway that
sandboxed sessions call with scoped `wk_...` tokens; it injects the real key,
enforces per-session policy/limits, and forwards upstream (nanoclaw's Agent
Vault + router's two-tier key model). Provider dispatch atomically reserves a
declared output maximum plus a text-prompt byte upper bound. Missing/invalid
maxima, external image/file inputs, provider-side context handles, and unknown
request extensions reserve the remaining allowance because their token cost
cannot be bounded locally. Only explicitly completed streams reconcile
trustworthy final usage; aborted or partial streams retain their reservation.
Anthropic reconciliation sums base, cache-creation, and cache-read input token
fields. SSE usage is observed incrementally without retaining the bounded JSON
response prefix or tail.

### Tools

Native Go tools first: `bash` (policy-gated), `read`/`write`/`edit`, `fetch`,
`search`. Everything else arrives via MCP — waffle ships a **hand-rolled
stdio JSON-RPC client** in `internal/mcp` (not the official go-sdk; no
HTTP/SSE transport — see [Deviations](#deviations)) so third-party servers
provide the long tail instead of a 40-tool builtin matrix. Tool availability
is decided by the session's policy (openclaw-style allow/deny), evaluated in
the gateway, not trusted to the sandbox.

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
it left off. Requests carry the model's durable `tool_use_id`; duplicate
delivery is absorbed and completed results can be reclaimed after a host
restart. Containers see only their workspace volume, the queue mount,
and the host's proxy endpoints — never secrets, never the host filesystem.

**Bind-mount / queue stress (#29).** Concurrent queue load is covered by
`go test -tags=sandbox_stress ./internal/sandbox -run Stress` (optional env
`WAFFLE_SANDBOX_STRESS=1`). That exercises the same SQLite inbound/outbound
pair docker bind-mounts; it does not require Docker. With Docker available,
`go test -tags=sandbox_docker ./internal/sandbox -run BindMount` runs the
queue on a host path that is also bind-mounted into a container (skips if
no daemon). See [docs/sandbox-queue.md](sandbox-queue.md). `waffle doctor`
when any tier uses docker mode checks: linux `runner_binary`, host-FS queue
round-trip, and (when the daemon is up) container + bind-mount write/read.
MCP servers report execution authority (`host` / `sandbox|restricted`) as doctor checks.

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
  the agent curate `MEMORY.md` (stable note IDs, exact-body dedupe); a
  `memory_update` tool supersedes or forgets by ID, archiving old lines to
  `MEMORY.archive.md` via localized line edits (never whole-file rewrites
  through the model). A shared reflection prompt (`session.Reflect`) writes
  session summaries for cross-session recall: chat finish, gateway
  `reflect_every_turns`, and idle reflection under `serve` when
  `[memory] reflect_after` is set (use `"0"` to disable idle). Idle
  reflection serializes on the same per-conversation group lock as message
  handling and does not re-reflect when a summary is already present.
- System injection: `MEMORY.md` notes are selected under
  `[memory] inject_budget` (default 8KiB) — pinned first, then newest;
  elided notes report a count and point at `recall`. Archive is never
  injected. Legacy un-ID'd lines still render.
- Learning loop: `distill_skill` writes inactive skills; `waffle learn`
  mines sessions → proposes constrained edits → validates held-in/out
  (see Phase 7 mine→propose→validate below).

Prompt-level self-modification is gated like code self-modification. Memory
and skill candidates carry provenance; `[memory] write_gate` accepts `auto`,
`notify`, or `review`. Review writes remain pending until host approval, and
untrusted-derived candidates never enter the live prompt automatically.
Rendered `MEMORY.md` is explicitly observational data, not instructions.

### Self-development loop (waffle works on waffle)

hermes-agent improves itself at the *prompt* level (skills). waffle goes one
level down: because it is a compiled single binary and its source is just a
git repo, code-level self-improvement is repo-workspace work where the repo
happens to be waffle's own.

The pipeline, using only machinery that already exists by Phase 5:

1. **Propose.** "Fix that timeout you keep hitting" (or the agent notices a
   recurring papercut during reflection) opens a workspace on the waffle
   repo — sandboxed container, scoped git credentials, like any other repo.
2. **Change.** The agent edits, then must get `go build`, `go vet`, and
   `go test -race` green *inside the workspace*. `golangci-lint` is run when
   installed (otherwise the gate reports a warning). The zero-network
   `waffle eval` harness is part of the same ladder; a broken eval blocks
   upgrade. Live provider evals are opt-in (`WAFFLE_EVAL_LIVE=1`) and
   skipped without a provider. `internal/eval` and `evals/` are protected
   auto-patch paths. The running gateway is never edited in place.
3. **Land.** The change is pushed as a branch. The approval policy is
   configured under `[selfdev]`: `approval = "manual"` (the default),
   `"ci"`, or `"auto-patch"`, with `verify = true` by default and optional
   `protected` path prefixes. Auto-patch refuses protected paths and the
   self-development gate/config/doctor code. Git is the audit trail either
   way. Before checkout, a structured reviewer examines the candidate diff
   for task fit and weakened gates. It uses `[provider].utility_model` when
   configured (falling back to the primary model), and writes findings plus
   the reviewed commit SHA to `$WAFFLE_HOME/selfdev-reviews.jsonl`. A
   `blocker` finding stops both manual and auto-patch upgrades. Every
   self-modification is therefore a commit that can be read, reverted, and
   merged with upstream. `waffle upgrade --no-verify` is an explicitly unsafe
   escape hatch for emergency recovery; it does not bypass review.
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
Optional `profile` on a job (#71) selects a named agent posture for that firing.

### Named agent profiles (#71)

Profiles are a **trust boundary in config**, not personality presets. Each
`[agent.profile.<slug>]` names system prompt, model class, sandbox mode, tool
allow/deny, and optional delegation allowlist for a posture (e.g. `reviewer`,
`researcher`). Slugs are `[a-z0-9-]` up to 64 characters. With no profile
section the effective posture is `main` (historical defaults — **no config
migration required**; existing installs keep today's agent construction).
Deny always wins over allow, including `allow = ["*"]`. Unknown tool names in
profile policies are rejected at load. Prompt files (`system = "@path.md"`)
must resolve under `$WAFFLE_HOME`; missing/unreadable/escaped paths are config
errors at agent build. Explicit `system = ""` is allowed.

Model selection on a profile:

- `model = "default"` or omitted → `[provider].model`
- `model = "utility"` → `[provider].utility_model` (error if unset)
- any other value → explicit model id on the same provider

`spawn_subagent` may pass `profile`; children can only **tighten** the parent
toolbox. `allowed_children` on a parent profile limits which child profiles may
be delegated. Surface binds:

| Surface | Bind |
|---|---|
| Channel groups | `waffle session profile <channel:chat> <name>` (audited) |
| Cron jobs | `waffle cron add … --profile name` |
| Chat REPL | `waffle chat --profile name` |
| Repo workspaces | `waffle ws open owner/repo --profile name` (stored on workspace) |

New channel groups default to empty profile → effective `main` via the profile
registry. Runs record `profile` on `run_metrics`; tool-policy denials name the
profile; channel profile rebinds write `profile_audit` (old, new, channel,
chat, source, timestamp).

**Relation to adjacent issues:**

- **#33 (agent-group trust tiers):** groups (`main` / `group` / `cron` / `issue`)
  are the *surface* trust tier (sandbox + baseline tool policy). Profiles are
  *named postures* layered on top — a channel can be group-tier and still bind
  `reviewer`. Profile tools cannot widen past the group ceiling when both apply.
- **#53 (repo WAFFLE.md / AGENT.md):** repo policy is untrusted overlay. It may
  only **tighten** the selected profile (allow-lists intersect; host/profile
  deny always wins). A read-only profile cannot be escalated to host bash by
  a malicious repo file.
- **#66 (action-level policy):** `[[policy.rule]]` and deny prefixes compose
  after profile tool allow/deny; denials can include profile name.
- **#68 (working-set broadcast):** profile-targeted subagents still receive only
  the read-only parent working-set snapshot; they never get `workspace_update`
  or nested spawn.

See `config.example.toml` for `main` / `researcher` / `reviewer` samples.

### Working set & three memory/state layers (#67 / #70)

**Three memory/state layers:**

1. **Transcript** (session turns / SQLite history) — durable conversation
   history and FTS recall across past turns and session summaries.
2. **Working set** — active task state (goals, constraints, decisions,
   assumptions, open questions) for the *current* session; maintained and
   dropped with the session, not durable knowledge. Survives summarization
   via `workspace_update` / pinned entries; idle maintenance may drop
   unpinned model assumptions.
3. **MEMORY.md** — durable owner knowledge across sessions (curated notes
   with stable IDs, budgeted injection, `remember` / `memory_update` /
   `recall` scope `notes`).

Do not put transient task state in MEMORY.md, or durable preferences only
in the working set.

### Subagent working-set broadcast (#68)

When a parent session has a non-empty working set, `spawn_subagent` injects a
**read-only snapshot** into the child system prompt at dispatch time. Parallel
spawns share the same snapshot (captured once per spawn call). Empty set or
`BroadcastWorkingSet=false` leaves the system prompt without a working-set
block (byte-identical to no broadcast). Child handoffs may include
`proposals`; they are **never applied automatically** — the parent set is
unchanged until the owner accepts via `workspace_update`. Children never
receive `workspace_update` or nested `spawn_subagent`.

### Typed subagent packets and handoffs (#78)

`spawn_subagent` accepts a strict `WorkPacket` while retaining the legacy
task-only shape. The child receives that framed packet, not the parent
transcript, and returns one fenced JSON `Handoff`. Unknown fields (including
nested finding, verification, and proposal fields), unknown statuses,
normalized duplicate paths, and trailing JSON are rejected. Each repeated
collection is capped at 128 items; text fields are capped at 16 KiB and paths
at 4 KiB. Repository paths use POSIX `/` separators only; backslashes,
absolute paths, and cleaned paths that escape via `..` are rejected. Malformed
output receives at most one repair attempt.

The parent normalizes evidence before rendering or persistence: every
requested verification command must have a matching result, Waffle-run checks
are `observed` while child claims remain reported, out-of-scope paths require
supervisor review, and read-only changes block the handoff. Proposals remain
unapplied. This typed boundary deliberately does not introduce an in-tree
workflow graph, planner/critic framework, or other workflow engine.

### Tool-output spill (#69)

Tool results larger than `tool.OutputLimit` (48KiB) are **redacted first**,
then spilled to SQLite (`tool_spills` + FTS) up to `spill.SpillCap` (512KiB).
The model sees a truncated head/tail plus a marker with spill id for
`expand_output` (range or grep). Partial spills (over cap) mark
`partial spill` in the notice. Session delete removes spills. Secrets that
pass through `Agent.Redact` never land on disk in spill content. Sandbox
tools that truncate inside the container never deliver full bytes to the
host, so spill applies to host/MCP large strings only.

### Security posture (from openclaw)

- Single-owner: only paired identities reach the agent at all; unknown
  senders get a pairing code that is redeemable only via the host CLI.
- Group chats: sandboxed, restricted tool policy, mention-gated.
- Gateway binds loopback by default; remote access is an explicit opt-in.
- All external content (messages, fetched pages, tool output) is treated as
  untrusted input, never as instructions.

### Extension surfaces (decision record, issue #41)

waffle already has three extension tiers that cover most "plugin" requests.
**Do not invent a fourth in-process tool API.**

| Need | Mechanism | Trust |
|---|---|---|
| Tools | **MCP servers** (`internal/mcp`) | out-of-process, policy-gated |
| Prompt behavior | **Skills** (`SKILL.md`) | data, not code |
| Code / adapters / providers | **Fork + `waffle upgrade`** | fully trusted, review-gated |
| Workspace setup/teardown | **Container lifecycle hooks** (`[workspace.hooks]`, repo `WAFFLE.md`) | untrusted commands run *inside* the sandbox only (#54) |
| Host-side message/tool transforms | *deferred* | would be in-process; see below |

**Policy answers (decision checklist):**

1. **≥2 concrete host-hook use cases today?** No — needs are speculative. Embedded runtime deferred.
2. **Who would write hooks?** Owner-only if/when built (not shared/marketplace).
3. **Initial hook points if built:** inbound message filter, tool-result transform only.
4. **Failure policy:** filters fail *closed*; cosmetic transforms fail *open*.
5. **Scope vs agent groups (#33):** per-group when introduced, never a global bypass of host policy.

**Terminal state A (adopted):** tier map documented above; **no** Lua/JS/WASM engine dependency. Reopen only when ≥2 real owner-authored host-hook needs exist; then implement `hooks.Runner` with gopher-lua behind a waffle-defined interface (not a plugin marketplace). Distributable third-party plugins would require wazero/Extism and are explicitly *not* chosen now.

Repo-versioned `WAFFLE.md`/`AGENT.md` (#53) and container lifecycle hooks (#54) are the supported repo extensibility path; issue-tracker intake (#51) is the third intake surface (with cron and chat).

### Code intelligence (issue #79)

Structural code tools (symbol find, references, callers, structure, blast
radius, test suggestions) belong **behind MCP / workspace tooling**, not in
waffle's SQLite core or an in-tree graph engine. The six-tool contract,
provenance (`source` / `indexed_at` / `stale`), and isolation rules live in
[docs/code-intelligence.md](code-intelligence.md). An in-process
`go/parser` fallback (`internal/codeintel`) proves the shape offline;
full accuracy is an optional sandboxed MCP bridge (e.g. gopls). Live source
always wins over any cache. Absence degrades to `search` / `read_file`.

## Repository layout

```
cmd/waffle/            main; subcommands: serve, chat, status, pair, runner,
                       ws, cron, session, forget, usage, pause/resume,
                       secret, backup/restore, doctor, upgrade, rollback, version
internal/gateway/      control plane: wiring, pairing, serve loop
internal/entity/       user/channel-group/agent-group/session model
internal/channel/      Adapter interface; telegram/ (hand-rolled Bot API HTTP)
internal/agent/        the loop: context assembly, streaming, tool dispatch,
                       subagents
internal/llm/          canonical types; anthropicp/, openaip/
                       (no gemini/ — OpenAI-compatible endpoint instead)
internal/tool/         Tool interface, builtins, policy
internal/mcp/          hand-rolled stdio JSON-RPC MCP client (no HTTP/SSE)
internal/codeintel/    structural code tools (#79) + go/parser fallback
internal/sandbox/      executors: host, docker; runner; sqlite queue IPC
internal/workspace/    repo workspaces: lifecycle, devcontainer, git helper
internal/broker/       credential broker (provider proxy, git, egress)
internal/secret/       Store iface; age+keyring backend; redaction; audit
internal/skill/        SKILL.md discovery, indexing, learning loop
internal/memory/       FTS5 store, curation, distill_skill, reflection
internal/schedule/     cron persistence + runner
internal/intake/       issue-tracker board intake (#51)
internal/repopolicy/   repo WAFFLE.md tighten-only policy (#53)
internal/hooks/        workspace lifecycle hooks in sandbox (#54)
internal/selfdev/      doctor, upgrade, rollback
internal/observability/ run metrics + loopback status HTTP
internal/store/        sqlite open/migrations (modernc.org/sqlite)
docs/                  this plan, research notes, deploy, ADRs
```

Key dependencies (all pure Go where possible): `modernc.org/sqlite`,
`robfig/cron/v3`, `filippo.io/age`, `github.com/zalando/go-keyring`,
Anthropic SDK, stdlib `net/http` for Telegram Bot API and OpenAI-compatible
providers, OTel SDK for tracing. **Not** used (deliberate cuts):
`charmbracelet/bubbletea`, `modelcontextprotocol/go-sdk`, `go-telegram/bot`,
`bwmarrin/discordgo` — see [Deviations](#deviations).

## Deviations

Deliberate departures from the original sketch (issue #39). These are not
incomplete work; they are choices to stay small enough to read (principle 2).

1. **Gemini provider** — no `internal/llm/gemini/`. Point the OpenAI-compatible
   provider at Gemini's compatible endpoint (`name = "openai"`, suitable
   `base_url` and model). One translator covers OpenRouter, Ollama, Gemini,
   and weave-router.
2. **bubbletea TUI** — cut. `waffle chat` is a line-oriented REPL (stdin/stdout
   with light ANSI), not a full-screen TUI. Keeps the terminal surface
   reviewable and dependency-free beyond `golang.org/x/term` for raw input
   where needed.
3. **MCP SDK** — hand-rolled stdio JSON-RPC in `internal/mcp` instead of
   `modelcontextprotocol/go-sdk`. **stdio-only; no HTTP/SSE transport.** The
   surface waffle needs (initialize, tools/list, tools/call) is small enough
   to own; an SDK would pull a large dependency graph for little gain.
4. **Channel deps** — Telegram is hand-rolled Bot API HTTP in
   `internal/channel/telegram` (no `go-telegram/bot`). **Discord is optional
   and not shipped** (`bwmarrin/discordgo` never added; a second channel
   remains an optional later addition under principle 2).

### Remaining functional gaps

Phases **0–4** of the original roadmap (skeleton → isolation & broker) are
fully delivered and in daily use. Phases **5–7** (workspaces, automation,
learning/self-dev) are also landed in substance; remaining gaps are the
optional/cut items above plus anything still open on the tracker:

| Gap | Status | Notes |
|---|---|---|
| Discord adapter | not shipped | deliberate; see deviation 4 |
| Native Gemini package | not shipped | deliberate; use OpenAI-compat |
| Full-screen TUI | not shipped | deliberate; line REPL |
| MCP over HTTP/SSE | not shipped | deliberate; stdio-only |
| In-process host hooks (Lua/JS) | deferred | extension-surface decision (#41) |
| Smart routing in-tree | out of scope | use weave-router as an endpoint |

Cross-check open GitHub issues for anything newer than this table; the
deviations above are closed by design, not backlog.

## Roadmap

Each phase ends with something you actually use daily; nothing depends on a
later phase to be useful. **Phases 0–4 are fully delivered; phases 5–7 are
delivered with the deliberate cuts in [Deviations](#deviations).** The status
line in [README.md](../README.md) tracks what's landed; the notes below are
the original plan, kept as the record of intent.

**Phase 0 — Skeleton (small).** Go module, `cmd/waffle`, config loading
(`~/.waffle/config.toml`), SQLite store + migrations, CI (build, test,
`golangci-lint`), OTel wiring; `internal/secret` Store interface with the
age+keyring backend and `waffle secret` CLI (needed before the first
provider key is configured).

**Phase 1 — The loop (the heart).** `internal/llm` canonical types +
Anthropic and openai-compatible providers; agent loop with streaming; host
tools (`bash`, file ops, `fetch`); `waffle chat` line REPL (not a
bubbletea TUI — see [Deviations](#deviations)). *Milestone: a useful
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
(provider proxy + `wk_` session tokens), secret redaction filter.
*Milestone: untrusted work (repo checkouts, scheduled jobs) runs in
containers, not on the host.*

**Phase 5 — Repo workspaces.** `internal/workspace` lifecycle
(open/idle/close), devcontainer image selection, broker-minted git
credentials + `waffle` as in-container credential helper, egress policy,
`waffle ws` CLI and `/repo` command. *Milestone: "work on repo X" from any
channel spins up a container and ends in a pushed branch.*

**Phase 6 — Automation.** Cron scheduler with channel delivery; subagents
(parallel sandboxed sessions reporting back to a parent); MCP client
(hand-rolled stdio JSON-RPC — see [Deviations](#deviations)). *Milestone:
unattended recurring jobs, including scheduled repo work.*

**Phase 7 — The learning loops.** Post-task skill distillation, in-use
skill refinement, memory-curation nudges; the self-development loop
(`waffle upgrade`, `waffle doctor`, `waffle rollback`, skill→Go-tool
promotion via self-PRs); optional weave-router deployment docs for smart
model routing; second channel (Discord) remains optional and **not
shipped** (see [Deviations](#deviations)).

**Phase 7 mine→propose→validate (#65).** Offline loop owned by `waffle learn`
(and `waffle skills audit`):

1. **Mine.** Sessions updated since the last `learn_runs` high-water mark are
   scanned for recurring tool-error fingerprints. Output is failure classes
   with counts and evidence session IDs (SQLite + fixture-tested).
2. **Attribute.** When `[provider] utility_model` is set, each class is labeled
   via that model; results land in `learn_attr_cache` keyed by content hash so
   a re-run on unchanged data makes **zero** provider calls.
3. **Propose.** Edits are constrained to enumerated surfaces: `skill`,
   `memory` (MEMORY.md), `config_stub`. Other surfaces are rejected.
4. **Validate / promote.** Conservative rule: held-in evidence must improve
   and held-out must not regress. Rejected proposals are stored with audit;
   accepted skill edits write **inactive** skills and attempt a git commit
   message linking the pattern (audit-only when no git repo).
5. **Activate.** `distill_skill` and learn writes are inactive until
   `waffle skills activate <name>`; the skills index lists only active skills
   and refuses to overwrite an active skill without validation.

Cron surface: use the single reserved prompt `/learn`, for example
`waffle cron add learn-daily 0 3 * * * /learn --deliver telegram:900`.
Serve runs the same mine→propose→validate pipeline and delivers its **digest**
(pattern counts, proposal statuses, provider call count). `/learn` is a closed
internal callback, not a general CLI-command dispatch surface; all other cron
prompts continue through the restricted cron agent.

**Action-level policy (#66).** Host config declares ordered rules:

```toml
[sandbox]
enforcer = "none" # or "feedback" to include rule guidance in deny messages

[[policy.rule]]
name = "no-rm"
tool = "bash"
match = "rm -rf"          # quote-aware token prefix
# regex = "^curl\\s+http:"  # optional raw-command regex
action = "deny"
guidance = "use safer cleanup"

[[policy.rule]]
name = "go-test-green"
tool = "bash"
match = "go test"
action = "allow"          # successful match records a session event

[[policy.rule]]
name = "tests-before-commit"
tool = "bash"
match = "git commit"
action = "require"
requires = "go-test-green"
guidance = "run `go test ./...` after your last edit and before committing"
```

Unknown keys are rejected at load. Actions are `allow` | `deny` | `require`.
A `require` rule blocks until its `requires` predicate event has occurred
in-session after the most recent matching write (`write_file` / `edit_file`);
successful allow-matched tools (and bash prefixes matching the requires key)
record that event. Rules integrate with `tool.Restrict` / `restricted.Run`
(after tool allow/deny and `deny_prefixes`); `ObserveSuccess` fires after a
successful run. Bash matching splits the command respecting quotes; **shell
indirection** (`eval`, variables, `$()`, aliases) is **not** expanded —
prefix/regex policy is not a substitute for sandbox isolation. Decisions
matching a rule are logged to `policy_audit` (session, rule, verdict).
Subagent/workspace child policies may only *narrow* the parent (a child
`allow` of a parent-denied match is rejected at construction). Layered
trust: agent-group tool lists → action rules → sandbox executor.

## Decisions to make now

- **Name the trust boundary in config, not code:** `agent groups` and
  named `agent profiles` (#71) carry `sandbox: host|docker` and tool
  allow/deny lists from day one, even while Docker support is unimplemented —
  retrofitting policy is much harder.
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
