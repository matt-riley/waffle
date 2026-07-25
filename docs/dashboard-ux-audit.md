# Waffle Desk UX audit

Audit of the Desk dashboard as shipped in 0.8.4, prompted by the report that
adding models, adding skills, and anything git/GitHub are impractical from the
dashboard.

Scope: every route, service, template, stylesheet, and client script under
`internal/dashboard/**`, plus the `cmd/waffle` wiring that supplies its
dependencies, compared against
[the Desk design spec](superpowers/specs/2026-07-23-waffle-desk-dashboard-design.md)
and [the Desk operator doc](waffle-desk.md). Tracking issue: #190.

## Summary

The server side is sound. Routes, sanitization, idempotency, restart deferral,
the loopback/CSRF boundary, and the `policy_audit` trail all hold up, and the
audit found no security defects.

Most problems are a presentation gap: **the API already returns the data the UI
needs, and the UI does not use it.**

| Endpoint returns | UI does |
| --- | --- |
| a provider's full model catalogue | renders read-only cards; the user retypes the model ID into another form |
| `chat.Result.Sessions`, `.Usage`, `.Permissions`, `.Workset` | ignores all four |
| `skillinstall.Manifest.Files` + `.Audit.Flags` | `JSON.stringify` into a `<pre>` |
| every provider, profile, and connection name | asks the user to type them |
| a restart outcome explaining why nothing is happening | writes it to the server log |

The exceptions are findings 15-17, which are genuinely absent capability rather
than unrendered data: Desk cannot bootstrap an installation, cannot show the
system prompt, and cannot author an agent profile.

## What works

Worth stating, because the fixes below should not disturb it:

- The mutation boundary — process token, `Idempotency-Key`, body limits,
  response-envelope replay, detached execution, `policy_audit` rows
  (`internal/dashboard/router.go`).
- Redaction discipline. Credentials never round-trip; catalogue text is
  redacted against known private values
  (`internal/dashboard/capabilities.go:204-226`); chat state and results are
  redacted structurally (`internal/dashboard/chat.go:709-729`).
- Workspace close: preview token, TTL, fresh re-inspection on confirm, refusal
  on dirty/unpushed, no force path (`internal/dashboard/workspaces.go:194-275`).
- Section independence in Tasks — one failed dependency yields a sanitized
  section error without discarding healthy cards
  (`internal/dashboard/tasks.go:107-190`).
- Responsive breakpoints and a skip link exist across all four stylesheets.

## Findings

Severity is relative to the reported complaint: **P1** blocks a task the user
came to do, **P2** makes it materially harder, **P3** is quality.

### 1. Adding a model or skill ends in an indefinite wait — P1 (#189)

Every Waffle-wide mutation commits with `CommitForRestart` and returns `202
restart_required`; the client then polls `/api/v1/desk/bootstrap` once a second,
**without a bound**, until the process generation changes
(`internal/dashboard/ui/assets/capabilities.js:305-324`).

Whether that ever happens depends on `INVOCATION_ID` being set, i.e. on Waffle
running under systemd (`cmd/waffle/dashboard_wiring.go:69-78`). Under the
`waffle serve` documented in `CLAUDE.md` and `docs/waffle-desk.md`, the
scheduler is `StandaloneRestartScheduler`, which always returns
`ErrManualRestartRequired` — nothing restarts, the generation never changes, the
poll never ends.

The explanation exists and is discarded: `deferRestart` builds a
`RestartScheduleOutcome{Code: "manual_restart_required"}`
(`internal/dashboard/capabilities.go:562-586`) which the observer writes to the
server log (`cmd/waffle/serve_cmd.go:366-372`). The browser is never told.

The model *was* added. The page just never says so.

### 2. Every failure reads the same — P1 (#179)

`writeCapabilityError` (`internal/dashboard/capabilities.go:597-618`) maps four
sentinels and sends everything else as `capability_failed` / "capability request
could not be completed". Collapsed into that one string: eleven `skillinstall`
sentinels (`ErrSourceNotAllowed`, `ErrGitHostNotAllowed`, `ErrCommitRequired`,
`ErrCommitMismatch`, `ErrUnsafeTree`, `ErrTreeTooLarge`, `ErrAuditFailed`,
`ErrSkillExists`, `ErrStageExpired`, `ErrStageChanged`, `ErrDigestMismatch`),
five `providerconfig` sentinels (`ErrLocked`, `ErrReferenced`,
`ErrDeferredRestartPending`, `ErrDeferredHealth`, `ErrDeferredIntegrity`), and
every provider probe or validation failure.

`writeWorkspaceServiceError` (`internal/dashboard/workspaces.go:495-512`) does
the same on the other side — a bad repo name, missing GitHub credentials, a
Docker failure, and a transient error are all `503 workspace_unavailable`.

This is what turns finding 3 from an inconvenience into a dead end.

### 3. The skill installer cannot succeed by default, and does not say so — P1 (#177)

`skill_import_roots` and `skill_git_hosts` are deny-by-default and ship
commented out (`config.example.toml:60-65`, wired at
`cmd/waffle/dashboard_wiring.go:94-99`). On a fresh install every stage attempt
fails — and by finding 2, fails as "capability request could not be completed".
The form (`internal/dashboard/ui/capabilities.templ:77-86`) shows three empty
boxes and no statement of what is allowed, that `commit` applies only to the Git
path, or that only public repos at exact commits are supported.

### 4. Nothing is selectable — P1 (#174, #173)

Every identifier is typed by hand into a bare `<input>`, including values
rendered on the same screen:

| Control | Source | Already known |
| --- | --- | --- |
| default / utility model alias | `internal/dashboard/ui/capabilities.templ:34-45` | the alias cards drawn directly above (`internal/dashboard/ui/assets/capabilities.js:133-155`) |
| catalogue connection | `internal/dashboard/ui/capabilities.templ:48-53` | `snapshot.providers.providers` |
| add-model connection | `internal/dashboard/ui/capabilities.templ:60-69` | same |
| memory attach session | `internal/dashboard/ui/memory.templ:31-32` | the `sessions` chat command |
| workspace profile | `internal/dashboard/ui/workspaces.templ:30-31` | `/api/v1/desk/connections` |

The catalogue is the sharpest case: `POST /models/catalogue/refresh` returns the
provider's real model list, `renderCatalogue()` draws it as read-only cards
(`internal/dashboard/ui/assets/capabilities.js:249-278`), and the user then
hand-copies the model ID into a different form. The spec lists "add a selected
catalogue model as an alias after the existing probe" as a Models capability; it
is not implemented.

### 5. Git and GitHub are absent — P1 (#181, #182)

A workspace card shows id, repository, session, status, profile, image, egress
(`internal/dashboard/workspaces.go:35-43`) — no branch, no dirty state, no
ahead/behind, no last commit. Git state exists but only through the close path:
`PreviewClose` returns dirty/unpushed evidence
(`internal/dashboard/workspaces.go:194-232`), so the only way to check for
uncommitted work is to open the close-confirmation dialog and cancel.

GitHub itself has no representation. `[github.app]` credentials
(`internal/config/config.go:637-647`) and `[[intake.github]]` watchers
(`:616-635`) exist and run, but `NewConnectionSource`
(`internal/dashboard/connections.go:49-114`) enumerates providers, Telegram,
MCP, and profiles only. There is no "is git auth working" signal anywhere in
Desk, which is why a misconfiguration surfaces as a generic workspace failure.

### 6. The skill review is a JSON dump — P2 (#176)

The security gate for installing third-party instructions renders as
`JSON.stringify(manifest, null, 2)`
(`internal/dashboard/ui/assets/capabilities.js:425`) under a heading promising
"Review manifest and diff". `Manifest` already carries a file table with
per-file digests and previews, and an `Audit{Passed, Flags}`
(`internal/skillinstall/manifest.go:48-69`) — the flags, the single most
important signal, are buried in the blob. The 10-minute stage lifetime is shown
as a raw timestamp, so expiry is discovered when Install fails.

### 7. Provider enrollment is unguided — P2 (#175)

Provider type is a free-text input with no list of accepted values; base URL is
conditionally required with no indication; only one model can be enrolled and it
is *always* made the Waffle-wide default
(`internal/dashboard/ui/assets/capabilities.js:348-355`), a side effect the user
did not ask for; `utility_model` and `max_tokens` are accepted by the endpoint
but unreachable from the UI. `Manager.Test`
(`internal/providerconfig/manager.go:404`) exists and is used by the CLI but is
not exposed, so a credential cannot be verified from Desk.

### 8. Today uses 2 of 14 chat commands — P2 (#183)

`/api/v1/desk/chat/command` accepts any `ParsedCommand` and returns a sanitized
`Result` carrying `Commands`, `Models`, `Sessions`, `Usage`, `Permissions`,
`Workset`, `State`. The client wires `model` and `skills`
(`internal/dashboard/ui/assets/today.js:588-591`, `622-625`). Unwired: `new` (so
**there is no way to start a new conversation**), `sessions`/`resume` (so there
is no session switcher — reaching a session means hand-editing the URL, which is
also why Memory asks for a raw session ID), `usage`, `permissions`, `status`,
`workset`, `help`. `chat.State.ModelError` and `.SandboxMode` are fetched and
never rendered.

### 9. Recoverable errors tear the desk down — P2 (#184)

`disconnect()` (`internal/dashboard/ui/assets/today.js:316-327`) closes the
stream, drops the turn, and disables the composer, and it is called on *any*
failure of model change (`:600`), skill toggle (`:634`), cancel (`:563`), turn
post (`:529`), or a single unparseable SSE frame (`:334`). The SSE stream has no
reconnect — the error listener closes it permanently (`:410-416`) — despite the
server supporting resumption via `Last-Event-ID`/`?after=`
(`internal/dashboard/api.go:118-146`). A laptop sleeping ends the session.

### 10. Operations are one-way — P2 (#178, #188)

No deactivate, uninstall, or update for skills
(`internal/dashboard/ui/assets/capabilities.js:157-187` renders Activate and
nothing else); no removal for model aliases or provider connections, though
`Manager.Remove` and `Manager.RemoveModel` exist and are used by the CLI.
Combined with forms that are easy to get wrong, Desk cannot undo its own
mistakes. Removal was a deliberate first-release cut in the spec; it now costs
more than it saves.

### 11. Conversation fidelity — P3 (#185)

Assistant text goes through `createTextNode`
(`internal/dashboard/ui/assets/today.js:121`, `139-141`), so code blocks, lists,
and headings render as one flat paragraph and code cannot be copied as a block —
the browser is a worse reader of Waffle's output than the TUI, which renders
markdown (`internal/chatui/markdown.go`). There is no send shortcut; the hint
says "Enter adds a new line" (`internal/dashboard/ui/today.templ:46`) and no key
handler exists. Tool activity prints `"<tool> started"` / `"<tool> finished"`
and discards the rest of the payload
(`internal/dashboard/ui/assets/today.js:144-155`) — no arguments, result,
duration, or failure state, under a heading that says "live evidence".

### 12. Forms share one status line — P3 (#180)

`setStatus()` writes every message from all six Capabilities forms into one
header paragraph (`internal/dashboard/ui/assets/capabilities.js:49-51`,
`internal/dashboard/ui/capabilities.templ:15`): errors appear away from the
field that caused them, with no `aria-invalid` or `aria-describedby` and no
focus move, and the next action from any other form erases them. No submit
handler disables its control while in flight
(`internal/dashboard/ui/assets/capabilities.js:342-447`), and each click mints a
fresh `Idempotency-Key` (`:122`), so a double-click runs two real mutations.

### 13. The left rail lies — P3 (#186)

`ShellHandler` hardcodes `Connection: "Connected"` and `ModelAlias: "default"`
(`internal/dashboard/shell.go:12-19`), rendered into a `rail-status` element
labelled "Connection and model" with a status dot
(`internal/dashboard/ui/navigation.templ:13-17`). No script updates it —
`internal/dashboard/ui/assets/today.js` writes `#desk-connection-text` only, and
nothing references `.rail-status`. So the rail reads "Connected · default" while
the provider is down, while the session is on another alias, and while the
disconnected banner is showing. On the four non-Today sections it is the only
connection indicator on screen.

### 14. Control baseline — P3 (#187)

`internal/dashboard/ui/assets/app.css` sets its typography and control baseline
for `a, button, select, textarea` (lines 32-36, 40-44, 69-73) and omits `input`
from all of it. There is no global `input` rule; four per-section rules each
repeat a partial subset and none sets `font: inherit`, so every text field
renders in the browser default font beside controls in Avenir Next — most
visible on Capabilities, which is almost entirely inputs. Disabled inputs also
miss the shared disabled treatment. Related: Capabilities and Memory link their
stylesheets from inside `<body>` (unstyled flash), and `aria-live` is applied to
whole list containers rather than status regions, so a re-render announces the
entire list.

### 15. Desk cannot bootstrap an installation — P1 (#192)

Desk presupposes a configured Waffle. Four things must already be true before it
is reachable: a secret identity exists, a provider connection is configured
(without one the chat runtime refuses outright — `"no provider configured; run
waffle setup to get started"`, `cmd/waffle/chat_cmd.go:171-175`), a model alias
resolves (`internal/config/config.go:1082-1100`), and `[dashboard] enabled =
true` has been hand-edited into `config.toml` with the service restarted.

`waffle setup` (`cmd/waffle/setup_cmd.go:23-102`) walks the first three
interactively — identity, guided provider add with credentials on stdin or a
0600 key file, a minimal `[agent.profile.main]` with a prompted system prompt,
then the active alias and next command. Desk mirrors none of it and has no
notion of "not set up yet": with no provider, Today just fails to open with a
generic error.

The fourth step cannot be a Desk action — a disabled dashboard cannot enable
itself — so that half belongs in `setup`, which should offer to enable Desk and
print the loopback URL.

### 16. The system prompt and profile posture are invisible — P1 (#193)

Desk never shows what the agent was told or what it is allowed to do. An
`AgentProfile` carries `System`, `Model`, `Sandbox`, `Tools{Allow, Deny,
DenyPrefixes, Guidance}`, `MaxTokens`, `MaxIterations`, and `AllowedChildren`
(`internal/config/config.go:258-279`). Desk shows the profile *name* in Today's
context list (`internal/dashboard/ui/today.templ:78-81`) and, in Tools &
connections, a sandbox label plus one of two canned strings — "Tool policy is
enforced." or "Runs in a sandbox." (`internal/dashboard/connections.go:94-107`).

The system prompt is exposed nowhere, so the single largest determinant of the
agent's behaviour is invisible on the surface built to observe it. The tool
policy is nearly as bad: `chat.PermissionView{SandboxMode, Allow, Deny,
DenyPrefixes}` (`internal/chat/types.go:51-56`) is already computed and
sanitized on the command endpoint and simply never requested (finding 8), so a
denied tool call cannot be traced to the rule that denied it, and `WAFFLE.md`
tightening is invisible.

### 17. Agent profiles cannot be authored — P2 (#194)

Profiles shape what Waffle is, and creating one means hand-editing `config.toml`
and restarting. Desk can select a profile for a workspace
(`internal/dashboard/ui/workspaces.templ:30-31`) and list profile names as
connections (`internal/dashboard/connections.go:72-107`), but cannot create,
edit, copy, or delete one. `waffle setup` writing `[agent.profile.main]` is the
only guided profile authoring in the product.

This is the most security-sensitive change the audit proposes and should not be
built without design signoff. Profiles and groups are trust boundaries —
"Profile selection must never widen a group's tool or sandbox policy; repo
policy (`WAFFLE.md`) can only tighten it further" (`CLAUDE.md`,
`internal/config/config.go:255-257`) — and the spec's non-goals rule out editing
raw TOML in the browser. A structured editor that can only narrow, validated by
the same code that enforces policy at runtime, is a different thing from
arbitrary config editing and should stay that way. Group editing should stay out
of Desk entirely: groups are the fixed point the narrowing check is measured
against.

## Implementation direction (#195)

Most findings above share one shape: the client re-implements what the server
already knows. The agreed direction is to stop doing that in the four
form-and-list sections — Capabilities, Tasks, Workspaces, Memory — by serving
templ fragments from the existing handlers via content negotiation
(`Accept: text/html` → fragment, else today's JSON) and swapping them with htmx.

That reshapes findings 2, 4, 6, 12, and 13 rather than fixing them one at a
time, and deletes the corresponding hand-written fetch/render JS. Finding 2 is
the prerequisite: server-rendered error partials are only worth having once the
server has a real error taxonomy to render.

Today stays on its bespoke client. Its streaming deltas, phase machine,
generation guards, and SSE resumption have no htmx equivalent, and porting them
would put finding 9's reconnect work on worse footing.

Three constraints hold regardless: the CSP does not change (no `hx-on:`, no
`js:` prefixes, no eval-based extensions — all require `unsafe-eval`), htmx is
vendored and embedded rather than fetched from a CDN, and the mutation boundary
keeps its token, per-request idempotency key, and audit row.

## Out of scope

Design-spec non-goals were respected and not filed: multi-user auth or remote
access, arbitrary TOML/secret editing in the browser, a file browser or
terminal, and public hosting. The loopback-only boundary, process token, and
idempotency model were reviewed and are sound.

Finding 17 sits closest to that line and is deliberately scoped to stay on the
right side of it: a structured, narrowing-only editor over a known schema, not a
config text box. If it cannot be built with server-side enforcement by the same
code that enforces policy at runtime, it should not be built.
