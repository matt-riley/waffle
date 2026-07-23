# Waffle Desk Dashboard Design

**Status:** Approved visual direction; ready for implementation planning

## Summary

Waffle Desk is a single-user, browser-based personal cockpit for a running
Waffle agent. It brings conversation, active work, schedules, workspaces,
memory, models, skills, and connections into one calm local interface without
creating a second control plane.

The selected direction is the approved **Waffle Desk** prototype: a warm,
conversation-first desktop with a compact left rail, cream paper-like surfaces,
dark ink text, restrained ginger accents, small operational labels, and dense
detail only where it supports a decision. It is intentionally different from
the public Astro site in `website/`: the public site explains Waffle, while
Waffle Desk operates the private local agent.

Waffle Desk is served by `waffle serve` on the existing loopback-only admin
listener. The UI and its JSON/event endpoints are embedded in the Go binary.
Existing domain operations remain authoritative and are shared with the CLI and
chat socket rather than reimplemented in HTTP handlers.

## Goals

- Give one person a useful daily home for talking to and operating Waffle.
- Make active work, scheduled work, evidence, and failures easy to understand.
- Expose workspace lifecycle and security posture without requiring CLI recall.
- Make memory searchable, attributable, attachable, and safely forgettable.
- Support both session-scoped model/skill choices and Waffle-wide capability
  management.
- Add a reviewed skill-install path, because Waffle currently lists, audits,
  and activates skills but does not install a new external skill.
- Preserve the existing local-only security boundary and managed-host ownership
  of configuration, credentials, SQLite state, and workspaces.
- Work well from 320-pixel mobile width through a wide desktop viewport and be
  fully operable by keyboard.

## Non-goals

- Public hosting or deployment through the marketing site.
- Multi-user authentication, remote access, fleet management, or tenancy.
- A replacement for the CLI, chat TUI, Telegram adapter, or other channels.
- A general file browser, terminal, IDE, or arbitrary process launcher.
- Editing raw TOML, environment variables, encrypted secret files, or database
  rows in the browser.
- Installing executable plugins or arbitrary host programs.
- Silently changing a session when a Waffle-wide default changes.
- Treating cached browser state as authoritative over Waffle's services.

## Product Model

### Primary user

The first release is a personal cockpit for the person who owns one Waffle
installation. A managed installation still executes all privileged operations
inside the already-running Waffle service; the browser never reads service
files or credentials directly.

### Navigation

The application has five persistent sections:

1. **Today** — conversation, quick actions, current session context, task model,
   and attached skills.
2. **Tasks** — active and recent runs, schedules, attention items, outcomes,
   usage, and evidence.
3. **Workspaces** — repository workspaces, profile and egress posture,
   lifecycle controls, and handoff to Today.
4. **Memory** — attributed search results, attach-to-session, add-through-chat,
   and a confirmation-gated forget flow.
5. **Capabilities** — model aliases and roles, the skill library and installer,
   and provider/tool connections.

The left rail shows the Waffle mark, these five destinations, connection health,
and the active model. On narrow screens it becomes a bottom destination bar;
section labels stay available to assistive technology.

### Scope rules

The interface always labels the scope of a capability mutation:

- A **session model** uses the session's existing persisted `model_alias`. It
  affects only that conversation and survives resume.
- The **default model** affects new sessions and Waffle operations that already
  resolve the default model role. It does not rewrite existing session choices.
- A **session skill** is attached to one session through a new persisted
  `session_skills` association. Attachment forces that active skill into the
  session's context; other active library skills remain available through
  Waffle's normal implicit invocation.
- A skill's **library status** is Waffle-wide. New installs enter the library as
  inactive; activation is a separate explicit action.
- Attaching a skill does not activate an inactive skill. The UI explains the
  required library action and leaves the session unchanged.
- Detaching a skill stops its forced session inclusion but does not deactivate
  or uninstall it.
- Removing or deactivating a model or skill that is referenced by a session
  does not silently substitute another capability. Resume shows the existing
  actionable missing-capability state.

In the UI, the session-scoped controls live beside the composer. Waffle-wide
defaults and library management live only in Capabilities.

## Visual and Interaction Direction

### Visual system

- Warm cream is the primary canvas; slightly deeper cream separates cards.
- Near-black ink carries primary text and dark navigation surfaces.
- Ginger orange is the only general accent and marks focus, selection, Waffle
  identity, and primary actions.
- Green is reserved for verified healthy or completed states; red is reserved
  for failures and destructive confirmation.
- Display copy uses a friendly rounded sans-serif where available. Operational
  labels, timestamps, IDs, and token counts use a compact monospace face.
- Borders and small offset shadows create a tactile desk-object feel. There are
  no gradients, glass effects, generic bento grids, or decorative data charts.
- Waffle character art is optional and restrained; status must never depend on
  an illustration.

### Motion

Motion is functional and short: rail selection, card expansion, sent-message
entry, toast entry, and state-change confirmation use 120–200 ms transitions.
Streaming text does not animate individual characters. With
`prefers-reduced-motion`, transitions become effectively instant and no content
movement is required to understand a state change.

### Shared behaviors

- Every mutation has a visible pending state and disables only the affected
  control.
- The server response is the canonical state after a mutation.
- Filters, search text, and expanded cards may remain in URL or browser state;
  operational data does not.
- A command palette provides destination and non-destructive quick-action
  access. It does not bypass confirmations.
- Empty states explain the next useful action instead of showing blank panels.
- Times are rendered in the browser's locale with an accessible absolute time.

## Section Details

### Today

Today opens the most recently active session or a fresh session when none
exists. Its center is the transcript and composer. The header shows session
title, connection state, profile, active model, attached skills, workspace, and
busy state.

The composer supports multiline text, send, cancel, model selection, and skill
attachment. Streaming assistant text and sanitized tool activity arrive as live
events. Browser reload resumes the selected persisted session but never retries
an incomplete or failed turn.

Quick actions link to the scoped operation rather than duplicating it:

- inspect the current task in Tasks;
- open or select a repository in Workspaces;
- search memory with the current session as context;
- manage the selected model or skill in Capabilities.

Only one turn may be active for a session across the chat socket and dashboard.
The existing session-ownership guard becomes shared service wiring so the second
client receives an actionable busy response.

### Tasks

Tasks combines three related views without introducing a new generic task
database:

- active and recent agent runs from observability;
- scheduled jobs and retry state from the schedule store;
- attention items derived deterministically from failures, stalled runs,
  disabled jobs, and workspaces needing action.

Filters cover active, scheduled, completed, and attention-required. Each row
shows source, profile, session, elapsed/runtime, token usage, outcome, and the
most useful evidence link available. **Open at desk** selects that persisted
session in Today. Schedule editing uses the schedule service's typed fields and
validation; raw cron records are never edited directly.

The page does not imply that every run is resumable. Controls appear only when
the underlying service supports the operation.

### Workspaces

Workspaces lists the authoritative workspace manager state: repository,
workspace ID, session, lifecycle status, profile, sandbox image, and resolved
egress posture.

Available controls are derived from current state:

- open a host-approved `owner/repo` with a named profile;
- select a workspace for the current Today session;
- idle or resume an eligible workspace;
- close an eligible workspace through the existing clean/dirty/unpushed guard.

Close first returns a preview containing dirty and unpushed evidence. A normal
close is allowed only when the current manager permits it. Force-close is not
offered in the first dashboard release. Repository entry accepts only the same
validated `owner/repo` form as the CLI; it is not a host path picker.

### Memory

Memory searches session summaries, turns, durable notes, and other existing
searchable sources through a single service response. Every result includes its
source type, source ID, relevant timestamp, excerpt, and freshness or provenance
available from that source.

**Attach to session** adds a bounded reference through the existing working-set
context mechanism; it does not duplicate or rewrite the source. **Add via
conversation** moves to Today with a prepared request so normal Waffle memory
behavior remains the write path.

Forget is deliberately staged:

1. the user requests a preview;
2. the service returns the exact Waffle-owned item and documented exclusions;
3. the user confirms using a short-lived operation token;
4. the service performs the existing forget operation and returns canonical
   state.

The interface states that forgetting Waffle-owned memory does not erase provider
logs, delivered messages, or backups. Search results disappear only after the
server confirms deletion. An **Undo** label is never shown unless the underlying
operation is genuinely reversible; otherwise the prototype's reversible
wording becomes **Cancel** before confirmation.

### Capabilities

#### Models

Models lists provider connection labels, model aliases, upstream IDs, status,
and default/utility roles without returning credentials. A user can:

- select a model for the current Today session;
- set the Waffle-wide default or utility role;
- browse or refresh the authenticated model catalogue;
- add a selected catalogue model as an alias after the existing probe;
- enroll a supported provider connection through the existing transactional
  provider workflow.

Provider secrets travel only in a mutation request, are passed directly to the
existing secret-store transaction, are never echoed, and are cleared from the
form after success or failure. The page sets `autocomplete="off"` and does not
persist draft credentials in browser storage. Provider/model removal is not
part of the first dashboard release.

#### Skills

Skills lists discovered skills, descriptions, source, audit result, active
status, and whether each skill is attached to the current session. Activation
continues to use the existing audit and activation path.

A new installer supports two source classes:

- a local directory below a host-configured import root; or
- an HTTPS Git source from a host-configured allowlist, pinned to a complete
  commit hash.

The installer copies or fetches into a private staging directory, rejects
symlinks and special files, enforces file-count and byte limits, requires one
valid `SKILL.md`, and refuses path traversal or writes outside the resolved
skills root. It audits the staged skill and returns a manifest plus readable
diff for review. Confirmation atomically installs it as inactive. Activation is
a second explicit action. Failed review or installation removes the private
staging area and leaves the current library untouched.

Name collisions never overwrite an installed skill. Updating an existing skill
is a separate future operation.

#### Tools and connections

This view reports configured adapters, MCP/tool connections, sandbox profile,
and connection health using sanitized labels and resolved policy summaries. It
offers links to relevant setup or provider flows but does not expose arbitrary
MCP configuration editing, environment values, tokens, or secret material.

## System Architecture

### Process boundary

Waffle Desk is part of `waffle serve` and shares the existing configured
loopback listener. The listener continues to reject non-loopback configuration.
The stable `/healthz` and `/status` responses remain backward compatible.

The new package boundary is:

```text
cmd/waffle
  constructs dependencies and one admin HTTP mux
internal/dashboard
  handler, application service, mutation validation, event hub
internal/dashboard/ui
  embedded HTML, CSS, ES modules, icons
existing internal packages
  sessions, chat backend, observability, schedule, workspace,
  memory, skills, providers, usage, configuration, secrets
```

The dashboard frontend uses semantic HTML, CSS, and small ES modules with no
runtime framework or separate Node service. `go:embed` makes assets part of the
single Waffle binary. Asset hashes provide immutable caching; the HTML shell and
all operational JSON responses use `Cache-Control: no-store`.

The existing public `website/` remains independent and is neither built into
nor served by Waffle Desk.

### Application service

`internal/dashboard.Service` depends on narrow interfaces for each domain. It
may aggregate data but does not issue SQL or edit configuration files directly.
Provider and chat construction remain in `cmd/waffle`, where current secret and
configuration wiring already lives, and are injected as typed functions.

The chat socket and dashboard share:

- the backend-neutral chat interface;
- the session-ownership registry;
- command and model validation;
- agent/runtime construction;
- session persistence and cancellation behavior;
- sanitized event and error envelopes.

CLI commands, dashboard handlers, and chat commands call the same extracted
domain operations for workspace lifecycle, schedules, model roles, skill
activation, and provider enrollment.

### Persistence changes

One ordered SQLite migration adds `session_skills` with:

- `session_id` referencing `sessions`;
- canonical `skill_name`;
- attachment timestamp;
- a unique key on `(session_id, skill_name)`.

Session deletion cascades its attachments. Skill files and Waffle-wide skill
status remain authoritative in the existing skill system. No dashboard-only
copy of model, workspace, run, memory, or schedule state is added.

The same migration adds `source_ref` and `content_digest` fields to
`skill_status`. `source_ref` is a sanitized local-import label or pinned HTTPS
Git URL without credentials; `content_digest` is the digest of the reviewed
manifest. This lets the library explain an installed skill's origin without
persisting staged file contents, local import paths, or credentials. None of
those values are written to browser storage.

## HTTP and Event Contract

All new endpoints are versioned below `/api/v1/desk`. JSON uses stable string
error codes and sanitized user-facing messages.

| Area | Read operations | Mutations |
| --- | --- | --- |
| Bootstrap | `GET /bootstrap`, `GET /events` | none |
| Today | sessions, transcript, current context | open/resume, turn, cancel, model, skills, workspace |
| Tasks | runs, schedules, attention | add/edit/enable schedule |
| Workspaces | list and close preview | open, select, idle, resume, close-confirm |
| Memory | search and forget preview | attach, forget-confirm |
| Models | aliases, roles, catalogues, connections | set role, add alias, refresh, enroll |
| Skills | library and install preview | attach, detach, stage, install, activate |
| Connections | health and policy summaries | supported guided setup only |

`GET /bootstrap` returns the minimum data needed for the shell and selected
section, plus version, server time, connection health, a per-boot request token,
and an event cursor. Section reads are independently refreshable so a large
memory result or model catalogue does not block Today.

`GET /events` is a server-sent event stream. Events carry a monotonically
increasing in-process cursor, type, affected resource IDs, and canonical
sanitized payload. It streams chat output, run state, health, schedule changes,
workspace lifecycle, and capability updates. Reconnect uses `Last-Event-ID`;
when the cursor is no longer available, the server sends `resync_required` and
the client refreshes affected reads.

Mutations use JSON `POST` requests with the request token and an idempotency key.
Two-step destructive operations also require the short-lived, single-use token
returned by their preview. The server never automatically retries a chat turn,
provider enrollment, workspace close, or forget confirmation.

## Security

- The listener remains loopback-only by configuration validation and tests.
- The handler rejects unexpected `Host`, `Origin`, and `Sec-Fetch-Site` values
  to defend against DNS rebinding and cross-site requests.
- State-changing requests require a high-entropy per-process token supplied in
  the HTML bootstrap response and a custom request header.
- No permissive CORS headers are emitted.
- Security headers include a restrictive Content Security Policy, `nosniff`,
  `Referrer-Policy: no-referrer`, and frame denial.
- The UI has no third-party scripts, fonts, analytics, images, or network
  dependencies.
- Request and audit logs record operation type and stable resource IDs, never
  prompts, transcript bodies, memory excerpts, provider keys, raw config,
  environment values, staged skill content, or error chains.
- JSON responses expose resolved labels and policy summaries rather than secret
  references or filesystem locations.
- Existing provider probes, workspace guards, sandbox policy, egress policy,
  secret-store transactions, skill audit, and memory exclusions remain in
  force.
- Managed-host browser access does not grant direct filesystem access to the
  invoking desktop user; all operations execute through the service boundary.

Loopback access is the first-release trust model. Remote bind flags, reverse
proxy support, and bearer-token authentication are intentionally absent.

## Loading, Failure, and Recovery

- Initial loading uses stable skeleton regions with text alternatives and no
  fake operational values.
- If the event stream disconnects, current data remains visible but is marked
  stale. Mutations are disabled until a bootstrap refresh succeeds.
- A failed mutation keeps the prior canonical state and shows an inline,
  sanitized, actionable error beside the affected control.
- Validation errors identify the exact field without exposing raw backend
  errors.
- A Waffle service restart invalidates request, preview, client, and event
  tokens. The page refreshes and never replays an in-flight mutation.
- Provider catalogue, tool connection, or adapter failures degrade only their
  own cards; Today remains usable when its required chat backend is healthy.
- Browser back/forward navigation changes sections and filters without
  duplicating operations.

## Accessibility and Responsive Behavior

- All controls are native buttons, links, inputs, selects, or dialogs with
  visible labels and focus.
- The five-section navigation, command palette, composer, overlays, tables, and
  confirmation flows have complete keyboard paths.
- Dialog focus is trapped while open and returns to the invoking control.
- Streaming and operational updates use separate, rate-limited live regions so
  assistive technology is not flooded.
- Color is never the only status signal; iconography and text accompany it.
- Text meets WCAG AA contrast at normal and large sizes.
- At 768 pixels and below, multi-column content becomes one ordered column.
  Tables become labelled cards rather than horizontally scrolling data dumps.
- At 375 and 320 pixels, the composer, confirmation actions, and destination
  navigation remain fully visible with no page-level horizontal overflow.
- Zoom to 200 percent preserves access to every action.

## Testing and Verification

### Go tests

- Handler tests cover methods, content types, schema, limits, cancellation,
  no-store behavior, security headers, Host/Origin/fetch-metadata rejection,
  request-token validation, idempotency, and preview-token expiry/replay.
- Service tests cover aggregation, partial failures, scope labeling, model-role
  changes, session model persistence, session skill attachment, schedule
  validation, workspace guards, memory provenance, and canonical refresh.
- Shared chat tests cover dashboard/socket session ownership, streaming,
  disconnect cancellation, service restart, and no automatic turn retry.
- Skill installer tests cover traversal, absolute paths, symlinks, special
  files, nested repositories, name collisions, size/count bounds, invalid
  `SKILL.md`, source allowlists, commit pinning, audit failure, cleanup, atomic
  install, and inactive-by-default status.
- Provider tests prove secrets never enter responses or logs and that failed
  enrollment leaves configuration, roles, secrets, and service state unchanged.
- Race tests cover event subscribers, concurrent mutations, and shutdown.

### Frontend and browser tests

- Module tests cover routing, canonical reconciliation, event cursor handling,
  stale/disconnected behavior, error rendering, and reduced motion.
- Browser flows cover:
  1. send and cancel a Today turn;
  2. set a session model without changing the global default;
  3. attach an active skill, then stage/review/install/activate a new skill;
  4. filter Tasks and open a run's session at Today;
  5. open, idle, resume, select, and safely close a workspace;
  6. search Memory, attach a result, cancel a forget preview, and confirm one;
  7. change a global model role and enroll a provider without secret leakage.
- Visual and interaction checks run at 1470-pixel desktop, 768-pixel tablet,
  375-pixel mobile, and 320-pixel narrow-mobile widths, plus 200-percent zoom.
- Automated accessibility checks are supplemented by keyboard-only and
  screen-reader live-region checks.

### Repository gates

Implementation is not complete until these pass:

```text
mise run fmt
mise run vet
mise run lint
mise run test
mise run build
```

A deterministic dashboard asset/browser test task is added to `mise.toml` and
included by `mise run test`. Tests use fixtures and local fakes; they do not
require provider, Git, or other network access.

## Rollout and Compatibility

The dashboard lands disabled until its complete read-only shell, security
boundary, and Today flow are present. A `[dashboard] enabled` setting then
defaults to `false` for existing installations and is enabled explicitly during
the first rollout. After one compatibility release and documented operator
notice, new personal installations may default it to enabled while preserving
an explicit off switch.

The database migration is additive. Existing sessions have no attached skills
and keep their current model alias behavior. Existing `/healthz`, `/status`, CLI,
chat socket, gateway adapters, and public website behavior remain unchanged.

The first implementation should land in vertical slices:

1. secure shell, bootstrap, event hub, and read-only Today/Tasks;
2. shared chat runtime and Today mutations;
3. Workspaces and Memory with preview/confirm safety;
4. model roles, session skills, and capability reads;
5. reviewed skill installation and provider enrollment;
6. responsive, accessibility, and complete browser verification.

Each slice is usable and testable, but the dashboard setting remains off by
default until all five sections and their safety gates are complete.
