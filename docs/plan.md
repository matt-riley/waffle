# waffle — build plan

A personal AI agent, written in Go, that you run on your own hardware. It
combines hermes-agent's learning loop and memory, nanoclaw's minimal
single-writer architecture and isolation model, openclaw's gateway/trust
design, and a direct multi-provider layer. Background and rationale for each
borrowed idea are in [research.md](./research.md).

## Design principles

1. **One binary.** `waffle` compiles to a single static binary containing the
   gateway, the agent runtime, the terminal chat TUI, and all channel
   adapters. No Node, no Python, no service mesh. Subcommands select the role.
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
             │                     │                      (stdio + HTTP)  │
             │                     │  provider proxy ──► Anthropic /      │
             │                     │  (only key holder)   OpenAI-compat   │
	             │                     │                   (Ollama / Gemini / │
	             │                     │                    custom endpoint)  │
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

Messages may carry media attachments (see "Channel messages &
attachments" below): decoded metadata plus a fetch handle, resolved to
bytes only after the sender has been admitted, size-capped before any
fetch, and labelled untrusted before the content reaches the model.

### Channel messages & attachments

The adapter contract is text-first in both directions (`internal/channel`):
`Message` carries the conversation scope, sender, text (the caption for
media messages), and — since #251 — an `Attachments` slice. An attachment
is decoded metadata (media type, size, filename, MIME) plus either bytes or
an opaque fetch handle; adapters decode metadata without fetching. The
gateway resolves handles through the adapter's optional `AttachmentFetcher`
only after the sender has been admitted, inside the conversation lock, so
strangers' attachments are never downloaded and downloads serialize with
handling per conversation. Inbound attachment bytes are capped by config
(`[channel.telegram] max_attachment_bytes`, deny-by-default with a
conservative example) *before* any fetch — an oversized attachment is
refused without getFile or a download — and held in memory; no temp file is
written, so there is no world-readable path and nothing to clean up on
error or cancellation. `edited_message` updates are deliberately ignored
with a logged reason, and no update kind is dropped without a log line.

Outbound, `Send` stays text-only (splitting unchanged); adapters that can
carry bytes implement the optional `AttachmentSender` (Telegram:
`sendPhoto` / `sendDocument`). Callers use
`channel.SendAttachmentOrExplain`, which degrades to a short text
explanation on channels without attachment support instead of erroring the
run. Inbound attachment content is labelled untrusted before it reaches
the model, consistent with tool-output framing; until `llm.Block` can
carry media (#250), the model sees the labelled metadata and caption, and
every degraded path produces a user-visible reply — silence is the bug
this design exists to prevent.

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

### Direct provider layer

`internal/llm` defines the canonical types (`Message`, `ToolCall`,
`ContentBlock`, streaming events) and a `Provider` interface. Translators
implement it for Anthropic Messages and OpenAI Chat Completions
(covers OpenRouter, Ollama, Gemini's OpenAI-compatible endpoint, and most
local servers). There is **no** first-class `gemini/` package — use
`name = "openai"` with Gemini's compatible `base_url` (see
[Deviations](#deviations)). Named connections and model aliases select these
providers directly; a generic OpenAI-compatible endpoint remains supported
without adding an intermediary runtime service.

The *provider proxy* is a thin HTTP listener inside the gateway that
sandboxed sessions call with scoped `wk_...` tokens; it injects the real key,
enforces per-session policy/limits, and forwards upstream (following
nanoclaw's Agent Vault pattern). Provider dispatch atomically reserves a
declared output maximum plus a text-prompt byte upper bound. Missing/invalid
maxima, external image/file inputs, provider-side context handles, and unknown
request extensions reserve the remaining allowance because their token cost
cannot be bounded locally. Only explicitly completed streams reconcile
trustworthy final usage; aborted or partial streams retain their reservation.
Reconciliation carries base, cache-creation, and cache-read input token fields
separately, and OpenAI-compatible `prompt_tokens_details.cached_tokens` is split
out the same way. Usage rows are keyed per provider type and record the provider
that produced them (legacy rows default to Anthropic), so budget binding prices
each provider's cache counters with that provider's multipliers — Anthropic 1.25x writes / 0.1x reads,
OpenAI-compatible 1.0x writes / 0.5x reads — instead of billing every input
token at the full rate. SSE usage is observed incrementally without retaining
the bounded JSON response prefix or tail.


### Media content (images and documents)

`llm.BlockImage` and `llm.BlockDocument` carry media with a source struct
(`base64` inline data or `url`), mirroring the Anthropic Messages API. The
canonical layer in `internal/llm` owns the shape and the limits — no
translator re-decides them:

- **Size.** One media block caps at 5 MiB decoded (`llm.MaxMediaBytes`);
  URL references cap at 8 KiB (`llm.MaxMediaURLLen`). Over-limit content is
  rejected with an error naming the limit; it is never truncated into
  invalid base64 and never silently dropped.
- **Types.** Images: PNG/JPEG/GIF/WebP. Documents: PDF, text formats, and
  the common office formats. Anything else fails with
  `llm.ErrUnsupportedMediaType`.
- **JSON is the storage format.** Persisted turns stay additive and
  `omitempty`; turns written before media existed unmarshal and round-trip
  byte-identically (checked-in fixtures in `internal/llm/testdata`).

**Storage decision: inline base64 in the turns table.** Payloads ride inside
`source.data` in the persisted turn JSON. This keeps a session's transcript
in one SQLite row set — portable, backed up, and restorable with the rest of
waffle ("SQLite for everything") — at the cost of re-reading media bytes
whenever a transcript is loaded. That cost is bounded by the canonical 5 MiB
per-block cap and the store's streaming row scan; the single-connection
read path is covered by a regression test with images at the limit. If real
usage makes transcript reads heavy, a content-addressed blob store behind
the same `source` struct (an additive `blob` source type) is the escape
hatch — the JSON shape does not change.

**Provider translation.** Anthropic emits the SDK's image/document block
params (including inside tool results). OpenAI-compatible endpoints map
images to `image_url` content parts (data URI for inline base64); documents
have no universal equivalent in that dialect, so a document block fails
with an explicit error naming the provider and block type — convert the
document to text first, or use a document-capable provider. Images inside
tool results fail the same way on OpenAI-compatible endpoints (role `tool`
content is a plain string there). Silent loss is never acceptable: a user
who sends a photo must not get an answer computed without it.

**Untrusted posture.** Media content is untrusted input that no text filter
inspects. The agent labels every media-bearing user message with the
untrusted framing before it reaches a model (data, never instructions), on
the same path every tier shares — main, cron, issue, and group all route
through `agent.prepareContext`, so no tier inherits unlabelled media by
accident. The broker's content-part inspection treats image traffic as an
external token source (reservation of the remaining allowance), and
sandboxed requests carrying images are covered by tests. Retention: media
is persisted like any turn; a lifecycle policy (e.g. deleting media beyond
session retention) is future work, tracked with channel attachments.

### Tools

Native Go tools first: `bash` (policy-gated), `read`/`write`/`edit`, `fetch`,
`search`. Everything else arrives via MCP — waffle ships a **hand-rolled
JSON-RPC client** in `internal/mcp` behind one transport interface: **stdio**
(the default, for local commands) and **streamable HTTP** (POST + SSE +
session-id, for the remote connector ecosystem; see [Deviations](#deviations),
#249). The SDK is still deliberately not used: the surface (initialize,
tools/list, tools/call) is small enough to own, and hand-rolling keeps the
OAuth and egress posture under our control. Tool availability is decided by
the session's policy (openclaw-style allow/deny), evaluated in the gateway,
not trusted to the sandbox.

**Portable plugin MCP (#391).** A plugin's `mcp.json` is parsed and validated
in `internal/plugin` (`LoadMCP`) against the closed Agent Plugins §7.2.1
schemas: `$schema` must match the plugin's declared version (a mismatch
**disables MCP for that plugin only**, §7.2.2), a missing `mcp.json` is not an
error, an invalid server entry is skipped with a report while valid entries
load, and the deprecated `sse` transport is deliberately unsupported and
skipped. `internal/pluginmcp` maps entries onto the runtime
(`mcp.Server`/`HTTPOpts`): bare commands stay bare, `./`-relative commands
and cwd resolve within the plugin root, cwd defaults to the resolved root,
`env` objects overlay the `BuildProcessEnv` base as explicit name→value pairs
(never ambient secrets), and fixed `headers` are applied with
client-generated session/auth headers always winning. The portable surface
carries **no waffle policy** — no `execution`/`egress`/`groups`/`token`
fields exist in `mcp.json`, so a plugin cannot relax the #77/#79/#249
posture; policy lands via the waffle extension namespace (#394) and
operator config. `${PLUGIN_ROOT}`/`${PLUGIN_DATA}` expansion is the next
issue (#392).

**PLUGIN_ROOT/PLUGIN_DATA (#392).** Plugin stdio servers are launched with
the §9 runtime contract: `PLUGIN_ROOT` (the resolved plugin root) and
`PLUGIN_DATA` (a client-managed writable directory at
`<waffle home>/plugins-data/<plugin>/`, created `0700` before launch,
preserved across updates) are added to the child environment **after** the
configured env overlay, so the client's values always win. `${PLUGIN_ROOT}`
and `${PLUGIN_DATA}` in `args`, `env` values, and `cwd` are expanded exactly
once, non-recursively (`mcp.ExpandPlaceholders`); `command` and `env` keys
are never expanded, unrecognized placeholder-like text stays literal, and
an `env` entry named `PLUGIN_ROOT`/`PLUGIN_DATA` invalidates the server
entry at validation time. The `BuildProcessEnv` allowlist discipline is
unchanged — plugins never see ambient secrets — and native `[[mcp]]`
servers are never expanded.

**Component failure isolation (#393).** Plugins are loaded with spec §11.3
boundaries: `plugin.LoadComponents` aggregates manifest + skills + mcp.json
so only a broken manifest rejects the whole plugin; a disabled MCP component
type, a skipped skill, or an invalid server entry is reported, never fatal.
The agent build wires every installed plugin
(`<waffle home>/plugins/`, `plugin.Installed`): plugin skills join the
system-prompt index, and plugin MCP servers connect with the most
restrictive default posture — remote servers are refused (the portable
surface carries no credentials/egress; that is the waffle extension
namespace, #394), docker-mode groups refuse host-executed plugin stdio
servers, and every connect/toolbox failure is a `slog.Warn` naming plugin,
server, and reason. Native `[[mcp]]` servers keep fail-fast; the codeintel
optional-degrade becomes a per-server `mcp.Server.Optional` flag. `waffle
doctor` reports rejected and partially-loaded plugins.

**Waffle client-extension namespace (#394).** waffle reserves the stable
reverse-domain namespace `dev.mattriley.waffle` (`plugin.Namespace`) for all
waffle-specific plugin data, per spec §8 — the only channel; no new
`plugin.json` top-level fields are ever introduced. Foreign namespaces are
ignored without validating their values; malformed waffle-namespace data is
reported and ignored, never fatal. The namespace carries per-skill
activation overrides (`extensions.dev.mattriley.waffle.skills.<name>.status`,
applied between frontmatter and the `skill_status` table, which wins) and
per-server MCP policy (`mcp.<name>.<execution|egress|groups|token>`) that
may grant more than the portable default but can never bypass the
#77/#79/#249 posture — docker-mode groups still refuse `egress=direct` and
host-executed plugin stdio binaries, unattended tiers stay deny-by-default
unless the extension names the group, and credentials are secret-store
references only. The `dev.mattriley.waffle/` top-level directory is reserved
for future per-plugin files. SKILL.md `metadata` key prefixes for
waffle-written skills align with this identifier (#396): activation state
is recorded as `metadata.waffle/status` (`spec.WaffleStatusKey`) — the
legacy top-level `status` field is still read for existing on-disk skills
and migrated away on write.

**Write-path conformance (#396).** Every `SKILL.md` waffle writes is
routed through the shared validator and YAML-safe writer and must validate:
`distill_skill` and `waffle learn` refuse (with a clear tool error, no file
created) a name failing the Agent Skills constraints or an
empty/over-1024-char description; the skill installer's `status: inactive`
rewrite and activate/deactivate emit `metadata.waffle/status` and never
produce a status-only frontmatter block — frontmatter-less and
non-conforming legacy files are left untouched (activation state is
authoritative in the `skill_status` table). Waffle-specific frontmatter no
longer appears as non-standard top-level keys: `status` lives under
`metadata.waffle/status`, and the write-only provenance markers
(`provenance`/`source_id`/`trust_class`/`session_id`/`channel`/
`untrusted_context`) are dropped — authoritative provenance is re-derived
from context and the install journal. The #65 injection gate stays a
separate waffle policy layer applied before the spec validator.

**Mid-run owner messaging (#253).** The gateway attaches one session-scoped
outbound sender per run (`internal/notify`), bound to the conversation's
channel and chat id — the same adapter resolution the memory-change notifier
and usage alerts already used. The cron runner binds it to the job's delivery
target and the issue intake dispatcher to the watcher's target, so the
unattended tiers can reach the owner too. The `notify` tool sends a short,
length-capped message through it; destination comes from session origin only
(no destination field exists in the tool input). It is fire-and-forget: a
failed send surfaces as a tool error to the model but never fails the run,
and a per-run budget (5 messages) stops a looping agent from flooding the
owner. Tier availability is deliberate: `cron` and `issue` keep `notify` (the
unattended tiers that most need to reach the owner), the multi-party `group`
tier denies it by default (a group chat must not be able to make waffle send
the owner arbitrary text), and `spawn_subagent` children never inherit it.
Sessions with no owner channel (terminal chat, eval, log-only cron/issue
targets) no-op it with a clear log line instead of an error.

### Sandboxing & IPC

The agent loop always runs on the host — that keeps one loop implementation
and keeps memory, skills, and session history host-side. What varies per
session is the **executor**: where its `bash`/`read`/`write`/`edit` tool
calls actually run.

- `host` executor: in-process, owner-primary sessions only. Host Bash uses
  the configured `[sandbox] pids` budget. On Linux it creates a child cgroup
  with `pids.max` when the service cgroup is delegated, so the limit covers
  the shell and every descendant. If delegation is unavailable, a child-only
  `RLIMIT_NPROC` fallback is used; that fallback is per real UID rather than a
  true process-tree boundary and may be ignored for root. Other host platforms
  do not claim a hard process-tree limit; use Docker when that guarantee is
  required.
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

**Host Bash availability note (#275).** The host PID budget is availability
hardening, not an untrusted-code containment boundary. The owner tier can
already perform broad host operations, and prompt-injected or fetched content
can still request those operations if policy allows it. Keep the restricted
tiers denied host Bash and prefer Docker for untrusted work; the host file
roots and policy tightening in [#269](https://github.com/matt-riley/waffle/issues/269)
are defense in depth, not a substitute for that boundary. On Linux managed
systemd services need `Delegate=yes` for the per-command cgroup; see the
deployment example.

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
   clones the repo using a broker-minted `wk_` session token with a 24h TTL
   (see secret management). The token is used once by the runner and never stored.
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
store; everything else gets scoped, time-bounded derivatives.**

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
  of sessions. It authenticates callers with per-session `wk_` tokens
  (**24h TTL**, `broker.DefaultTokenTTL`; expired tokens are rejected and
  swept; resume/re-mint issues a fresh token) and applies the session's
  policy. It has four faces:
  - *LLM:* the provider proxy — injects the real API key upstream
    (unchanged from the base plan).
  - *Git:* mints least-privilege repo credentials — a GitHub
    App installation token or fine-grained PAT scoped to the single repo,
    ~1 h TTL for installation tokens — for workspace clone/push.
  - *HTTP:* an egress proxy that injects `Authorization` headers for
    allowlisted hosts, so a sandboxed tool can call an API it is entitled
    to without ever seeing the key (nanoclaw's Agent Vault pattern).
  - *API faces (#254):* named credentialed faces served at
    `/api/<name>/<path>` for third-party APIs — a generalisation of the
    LLM upstream with an explicit method and path-prefix allowlist per
    face. Deny-by-default for every tier: a session token only reaches a
    face the tier explicitly granted (a literal `api_<name>` entry in its
    tool allow list; `*` grants nothing), and the broker re-checks the
    grant, the method allowlist, the path allowlist (traversal and encoded
    separators refused), and the usage/pause gates on every request.
    Agents reach a face through a narrow generated `api_<name>` tool rather
    than a generic `api_call`: one tool per face keeps the schema and
    description precise about what the face permits, makes tier grants
    first-class tool policy, and shrinks the blast radius of a prompt
    injection to the single face the model was given. Credential
    containment is unchanged: the real value lives in the store and the
    broker process only; response bodies, audit rows, and errors are
    redacted before they leave the broker, and redirects are never
    followed (a face cannot reach a host outside its `base_url`).
- **Redaction.** The gateway keeps a digest set of all stored secret values
  and scrubs matches from tool output, model context, logs, and traces
  (`[redacted:github/pat]`) — protects against the "cat ~/.netrc into the
  transcript" class of leak even on the host executor.
- **Audit.** Every broker grant is a SQLite row: session, secret name,
  scope, TTL, timestamp. Expired `wk_` presentations are audited as
  `action=expired`, distinct from unknown/missing tokens (`action=denied`).

Threat model in one line: a fully compromised session (prompt injection,
malicious repo code) can spend its own scoped tokens until they expire, but
cannot read another repo, another session's secrets, or any raw key.

### Skills & memory (from hermes-agent)

- Workspace layout: `~/.waffle/workspace/<agent>/` with `AGENT.md` (persona +
  standing instructions), `MEMORY.md`, `USER.md`, and `skills/<name>/SKILL.md`
  (agentskills.io-compatible so hermes/openclaw skills port over).
- Skill format: [Agent Skills](https://agentskills.io/specification) is the
  single source of truth for `SKILL.md` frontmatter and naming rules.
  `internal/skill/spec` is the one shared validator/parser/serializer
  (`spec.ValidName`, `spec.Validate`, `spec.ParseFrontmatter`,
  `spec.MarshalSKILL`) used by discovery, distill_skill, the learn loop, the
  skill installer, and activate/deactivate (#395) so the rules cannot drift.
  Hand-rolled rather than the `skills-ref` CLI: waffle validates in-process
  with zero dependencies and no subprocess boundary.
- Plugin skills: `internal/plugin.DiscoverSkills` reads `<plugin root>/skills`
  (Agent Plugins §6.1/§7.1): one skill per immediate child directory with a
  regular-file `SKILL.md`, name must match the directory, and each `SKILL.md`
  must resolve within the plugin root (§4.1). A non-conforming skill, a
  `SKILL.md` that is not a regular file, or one resolving outside the root
  is **skipped and reported** — it never aborts the walk and never affects
  other skills or component types (#390). The workspace-global
  `skill.Discover` path is unchanged and stays lenient for legacy files;
  plugin-supplied skills are held to the spec strictly.
- Memory recall: every turn is indexed in SQLite FTS5; a `remember` tool lets
  the agent curate `MEMORY.md` (stable note IDs, exact-body dedupe); a
  `memory_update` tool supersedes or forgets by ID, archiving old lines to
  `MEMORY.archive.md` via localized line edits (never whole-file rewrites
  through the model). A shared reflection prompt (`session.Reflect`) writes
  session summaries for cross-session recall: chat finish, gateway
  `reflect_every_turns`, and idle reflection under `serve` when
  `[memory] reflect_after` is set (use `"0"` to disable idle). Idle
  reflection serializes on the same per-conversation group lock as message
  handling, and a summary watermark records the highest turn sequence a
  summary covers: an unchanged session is never reflected twice, while a
  resumed session with new turns is re-reflected after the next quiet period
  (once per quiet period, incrementally — the prior summary plus only the
  uncovered turns, never repeated full-history cost). Reflection metadata
  never bumps the session's conversation-activity timestamp, so idle timing
  stays based on user/assistant activity (#411).
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
Every memory mutation crosses the same gate (#44): `remember` appends, and
`memory_update` supersedes/forgets existing notes by ID with honest
model-derived provenance and a compare-and-swap digest on approval (#417) —
no mutation bypasses review mode. Rendered `MEMORY.md` is explicitly
observational data, not instructions.

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

   `approval = "ci"` adds a hard, commit-bound gate: the upgrade queries the
   repository's check runs for the exact immutable candidate SHA through the
   scoped GitHub App (a `checks:read` installation token, never ambient
   credentials) and requires every name in `selfdev.required_checks` to be
   completed with conclusion `success` on that SHA. Missing, failed,
   cancelled, timed-out, action-required, stale (another SHA), pending,
   skipped, or neutral required checks fail closed with the check name and
   URL; network/API errors are a distinct closed failure. The chosen SHA
   semantics are exact-commit: a force-push after verification cannot change
   what is built, because the build remains bound to the already-resolved
   SHA. CI evidence is persisted with the review/artifact record in
   `$WAFFLE_HOME/selfdev-upgrades.jsonl`. CI approval is additional evidence
   — it never skips the local verify/doctor gates.
4. **Deploy.** `waffle upgrade` resolves one immutable commit SHA (no-ref
   resolves HEAD), reviews it, verifies it, and builds it in an isolated
   detached worktree created from that SHA — the configured checkout is never
   modified and unreviewed local edits can never enter the binary. It runs
   `waffle doctor` against the new binary (self-check: config parses, DB
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
| Chat TUI/plain mode | `waffle chat --profile name` |
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

**Package format (issue #389):** waffle adopts the vendor-neutral [Agent Plugins](https://github.com/agentplugins/agent-plugins-spec) 1.0.0 *package format* for the two tiers it already ships — Skills and MCP servers — while still running **no plugin marketplace**. `internal/plugin` loads a plugin directory: closed `plugin.json` schema validation, `$schema` version selection from a fixed local compatibility map (schemas are pinned and never fetched over the network), and plugin-root path containment. Installed plugins live at `<waffle home>/plugins/<name>/`, which is canonical for plugin-supplied skills; the agent-curated `<workspace>/skills` tier stays separate. This is packaging only — not a fourth in-process tool API. Component discovery (`skills/`, `mcp.json`), `PLUGIN_ROOT`/`PLUGIN_DATA`, and the waffle extension namespace are follow-up issues; install/download/registry mechanics stay out of scope.

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
internal/channel/      Adapter interface; attachments (fetch/send capability
                       interfaces); telegram/ (hand-rolled Bot API HTTP,
                       media decode + sendPhoto/sendDocument)
internal/agent/        the loop: context assembly, streaming, tool dispatch,
                       subagents
internal/llm/          canonical types; anthropicp/, openaip/
                       (no gemini/ — OpenAI-compatible endpoint instead)
internal/tool/         Tool interface, builtins, policy
internal/mcp/          hand-rolled MCP client: stdio (default) + streamable
                       HTTP (#249), OAuth/PKCE token lifecycle
internal/codeintel/    structural code tools (#79) + go/parser fallback
internal/sandbox/      executors: host, docker; runner; sqlite queue IPC
internal/workspace/    repo workspaces: lifecycle, devcontainer, git helper
internal/broker/       credential broker (provider proxy, git, egress)
internal/secret/       Store iface; age+keyring backend; redaction; audit
internal/skill/        SKILL.md discovery, indexing, learning loop
internal/plugin/       Agent Plugins package format (#389): plugin.json,
                       schema selection, plugin-root containment
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
Anthropic SDK, stdlib `net/http` for Telegram Bot API, OpenAI-compatible
providers, streamable-HTTP MCP (#249), and OTel SDK for tracing. **Not**
used (deliberate cuts): `charmbracelet/bubbletea`,
`modelcontextprotocol/go-sdk`, `go-telegram/bot`, `bwmarrin/discordgo` —
see [Deviations](#deviations).

## Deviations

Deliberate departures from the original sketch (issue #39). These are not
incomplete work; they are choices to stay small enough to read (principle 2).

1. **Gemini provider** — no `internal/llm/gemini/`. Point the OpenAI-compatible
   provider at Gemini's compatible endpoint (`name = "openai"`, suitable
   `base_url` and model). One translator covers OpenRouter, Ollama, Gemini,
   and other OpenAI-compatible endpoints.
2. **MCP SDK** — hand-rolled JSON-RPC in `internal/mcp` instead of
   `modelcontextprotocol/go-sdk`. **Reopened for the remote connector
   ecosystem (#249).** The original stdio-only call was right for local
   commands, and still is; what changed is where servers live — Gmail,
   Notion, Linear, Slack, GitHub's own server are remote HTTP servers
   authenticated with OAuth, not local commands. The transport layer is now
   factored behind one interface: stdio stays the default implementation,
   and a streamable-HTTP transport (POST + SSE + `Mcp-Session-Id`
   resumability, per the MCP spec) reaches remote servers. OAuth is
   authorization-code + PKCE with dynamic client registration where the
   server offers it; tokens live in `internal/secret` (age-encrypted), never
   `config.toml`, refreshed ahead of expiry and fail-closed on rejection.
   Egress from docker-mode groups routes through the broker proxy
   (allowlist + audit rows) or is refused; the unattended tiers
   (cron/issue/group) are deny-by-default for remote servers. The SDK is
   still not pulled: the protocol surface is small enough to own, and
   hand-rolling keeps the CGO-free static-linux posture (no CGO anywhere in
   the module today) and the security boundary (token handling, egress)
   under our control rather than an SDK's default behavior.
3. **Channel deps** — Telegram is hand-rolled Bot API HTTP in
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
| Discord adapter | not shipped | deliberate; see deviation 3 |
| Native Gemini package | not shipped | deliberate; use OpenAI-compat |
| Remote MCP over streamable HTTP | shipped | #249; stdio remains the default; OAuth + brokered egress required for remote servers |
| In-process host hooks (Lua/JS) | deferred | extension-surface decision (#41) |
| Agent Plugins client conformance | partial | #389 adopts the package format (`plugin.json`, schema selection, containment); component loading, plugin data, and the waffle extension namespace are separate issues; no marketplace |
| Smart routing in-tree | out of scope | select explicit model aliases or use provider-hosted routing |

Cross-check open GitHub issues for anything newer than this table; the
deviations above are closed by design, not backlog.

## Roadmap

Each phase ends with something you actually use daily; nothing depends on a
later phase to be useful. **Phases 0–4 are fully delivered; phases 5–7 are
delivered with the deliberate cuts in [Deviations](#deviations).** The status
line in [README.md](../README.md) tracks what's landed; the notes below are
the original plan, kept as the record of intent.

### What v1 means

Feature phases are done. v1 is a compatibility promise: the owner loops
documented from [README.md](../README.md) (`setup` → `chat`, or `serve` +
pairing) stay accurate, `waffle upgrade` / `waffle rollback` stays the
install contract, breaking config or schema changes are called out in the
changelog, and deny-by-default sandbox, network, and secret posture does
not loosen. Until that promise is made, tagged releases are preview.

Discord, a native Gemini package, in-process host hooks, a plugin
marketplace, and in-tree smart routing are not v1 work. Those stay in
[Deviations](#deviations) and [Explicitly out of scope](#explicitly-out-of-scope-for-now).

**Phase 0 — Skeleton (small).** Go module, `cmd/waffle`, config loading
(`~/.waffle/config.toml`), SQLite store + migrations, CI (build, test,
`golangci-lint`), OTel wiring; `internal/secret` Store interface with the
age+keyring backend and `waffle secret` CLI (needed before the first
provider key is configured).

**Phase 1 — The loop (the heart).** `internal/llm` canonical types +
Anthropic and openai-compatible providers; agent loop with streaming; host
tools (`bash`, file ops, `fetch`); `waffle chat` Focused Conversation TUI with
deterministic plain fallback and a local Unix-socket backend. *Milestone: a
useful terminal agent.*

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
(hand-rolled JSON-RPC; stdio default + streamable HTTP, see
[Deviations](#deviations)). *Milestone:
unattended recurring jobs, including scheduled repo work.*

**Phase 7 — The learning loops.** Post-task skill distillation, in-use
skill refinement, memory-curation nudges; the self-development loop
(`waffle upgrade`, `waffle doctor`, `waffle rollback`, skill→Go-tool
promotion via self-PRs); second channel (Discord) remains optional and **not
shipped** (see [Deviations](#deviations)).

**Phase 7 mine→propose→validate (#65).** Offline loop owned by `waffle learn`
(and `waffle skills audit`):

1. **Mine.** Sessions updated since the last committed learn cursor are
   paged through in (updated_at, id) keyset order; only a fully successful
   run advances the cursor, and a failed/interrupted run is retried from its
   last committed position (never lossy under load). Output is failure
   classes with counts and evidence session IDs (SQLite + fixture-tested).
2. **Attribute.** When `[provider] utility_model` is set, each class is labeled
   via that model; results land in `learn_attr_cache` keyed by content hash so
   a re-run on unchanged data makes **zero** provider calls.
3. **Propose.** Edits are constrained to enumerated surfaces: `skill`,
   `memory` (MEMORY.md), `config_stub`. Proposal generation is
   mechanism-specific: the utility model (when configured) returns a strictly
   decoded, structured candidate set — or a deterministic rule table does —
   each with a concrete mechanism rationale, exact commands, and evidence;
   generic "reproduce, fix root cause, re-run" restatements are never
   generated or auto-promoted. Existing matching inactive skills are updated
   in place rather than minting redundant `recover-*` skills, active skills
   are never overwritten, prior rejected attempts are fed back so the same
   content hash is not re-proposed, and results are cached by evidence hash,
   model, prompt/schema version, existing-surface digest, and prior-attempt
   digest.
4. **Validate / promote.** Promotion is fail-closed: a real before/after
   measurement (baseline → score) must show held-in strictly improving and
   held-out not regressing, with at least one independent held-out case, or
   the proposal stays **pending for owner review** — a nil baseline can never
   produce `accepted` automatically. Scorer/evaluator errors reject the
   proposal and are persisted, never treated as zero. Exact held-in/held-out
   counts are persisted in the proposal audit. Accepted skill edits write
   **inactive** skills and attempt a git commit message linking the pattern
   (audit-only when no git repo).
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
- **Keep routing explicit, build the loop:** named direct-provider connections
  and model aliases make selection auditable; provider-hosted routing remains
  available through generic compatible endpoints. The agent loop, memory, and
  skills are the parts worth owning.
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
