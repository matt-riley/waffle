# Waffle Chat TUI and Local Socket Design

## Summary

`waffle chat` becomes a polished, keyboard-first Bubble Tea interface with a
useful slash-command surface. On a managed host, the normal-user CLI connects
to the already-running Waffle service over a permission-controlled Unix socket
instead of opening service-owned configuration, credentials, and SQLite state.
Standalone installations retain a direct local mode.

This is one cross-repository feature. The Waffle repository owns the chat
runtime, protocol, TUI, persistence, tests, and product documentation. The
Infra repository owns the systemd socket, managed command routing, rollout
contract, host tests, and operator documentation required to make the feature
true on the deployed host.

## Goals

- Make `waffle chat` run without `sudo` on the managed host.
- Keep provider credentials, the age identity, configuration, SQLite state,
  and the Waffle service identity unavailable to the interactive user.
- Replace the line-oriented interactive experience with the approved
  **Focused Conversation** Bubble Tea interface.
- Add model selection and a practical set of familiar chat commands.
- Preserve model selection with its session across `/resume` and
  `waffle chat --continue`.
- Preserve a deterministic plain-text mode for redirected input/output,
  automation, accessibility, and focused tests.
- Test and document the application and managed-host behavior end to end.

## Non-goals

- A network-accessible chat API or browser UI.
- Remote multi-user tenancy or per-user Waffle state.
- Giving an unprivileged account direct access to `/var/lib/waffle`.
- Running the chat client, its tools, or its shell as root or as the `waffle`
  service account.
- Reproducing every command from Codex, Claude Code, or GitHub Copilot.
- Changing provider enrollment, model-catalog discovery, or provider routing.
- A persistent sidebar, multi-pane dashboard, or IDE-style command center.

## Chosen Approach

### Managed host: local service boundary

The managed Waffle service continues to own all privileged application state.
It accepts chat clients on `/run/waffle/chat.sock`. The interactive
`waffle chat` process owns only terminal presentation and a local protocol
connection; agent construction, model calls, tool execution, session writes,
memory, skills, and workspace behavior remain in the service.

Infra provides the socket through a systemd socket unit with `Accept=no`, mode
`0660`, and group `sudo`. This does not authorize a new class of user: it gives
users already permitted to elevate on the Ubuntu managed host a narrower way
to use chat without elevation. systemd creates the socket and passes its file
descriptor to `waffle serve`; the service does not need directory ownership or
membership in the socket's client group. Unix peer credentials are captured
for security logging and audit context after the kernel has enforced the
filesystem permission.

The managed `/usr/local/bin/waffle` entry point recognizes `chat` before its
administrative effective-UID check and executes the released binary as the
calling user with the socket path. It does not source `/etc/waffle/identity.env`,
set `WAFFLE_HOME=/var/lib/waffle`, invoke `runuser`, or acquire the host-mutation
lock. Provider administration and lifecycle reconciliation retain their
current `sudo waffle ...` contract.

When the managed socket is explicitly selected, failure to connect is final
and actionable. The client never falls back to opening service-owned state.

### Standalone hosts: direct mode

With no `--socket` option or `WAFFLE_CHAT_SOCKET` value, `waffle chat` uses the
current local config/store/agent path. This keeps development and single-user
installations simple. `--plain` selects the line renderer explicitly. A
non-interactive stdin or stdout selects plain mode automatically, even when
the backend is a Unix socket.

Mode selection is deterministic. `--socket <absolute-path>` takes precedence,
then `WAFFLE_CHAT_SOCKET`; if neither is present the command uses direct mode.
An empty environment value does not select a socket. `--socket` rejects a
relative path and is incompatible with any future explicit direct-mode flag.
The managed wrapper always supplies `--socket /run/waffle/chat.sock`, so it
cannot consult or open the caller's own `~/.waffle` state by accident.

On the server, a valid systemd-inherited listener takes precedence. Without
one, an optional absolute `[chat] socket` path lets a standalone `waffle serve`
offer the same local protocol. With neither source, `serve` starts no chat
listener. A configured listener that cannot be established is a startup error,
not a warning.

### Rejected alternatives

- Relaxing state-directory or identity permissions would expose credentials
  and mutable state.
- A sudoers helper or setuid wrapper would hide the typed `sudo` while still
  executing chat and tools with a privileged identity.
- Separate user-owned chat state would split sessions, memory, models, and
  workspaces from the running Waffle instance.

## Component Boundaries

### Chat runtime

The existing orchestration in `cmd/waffle/chat_cmd.go` is separated from its
scanner-based rendering. A backend-neutral chat runtime owns one active
session, agent, transcript, selected model, profile, skills, workset, and repo
workspace wiring. It exposes typed operations for turns and commands and emits
presentation-neutral events.

The direct CLI creates this runtime in process. The managed server creates one
runtime per accepted client connection. Runtimes do not mutate or reuse the
gateway's shared agent; this prevents `/model` and cancellation in one client
from affecting gateway messages or another client.

### TUI

An `internal/chatui` package owns the Bubble Tea model and renderer. It depends
on a small client interface rather than config, store, secrets, agents, or
sandbox packages. The same interface is implemented by direct and Unix-socket
clients.

The UI uses Bubble Tea with Bubbles components for the viewport, multiline
composer, list overlays, and spinner, plus Lip Gloss for adaptive styling. It
does not encode command behavior in key handlers: both TUI and plain mode use
the same command registry and typed backend operations.

### Local wire protocol

An `internal/chatwire` package owns a versioned newline-delimited JSON protocol,
the Unix client, server connection loop, bounds, and redaction-safe error
envelopes. Each frame contains a protocol version, type, request ID where
applicable, and a typed payload. Newlines inside message content are JSON
escaped, so each physical line is one complete frame.

Client frames comprise:

- `open`: profile, continue/session selection, client capabilities;
- `turn`: user text for the current session;
- `command`: a command name plus parsed arguments;
- `cancel`: cancel the active turn only;
- `close`: summarize when appropriate and end the connection.

Server frames comprise:

- `ready`: session, transcript, model, provider label, profile, and capabilities;
- `state`: changes to session, model, connection state, or command availability;
- `text_delta`: streamed assistant text;
- `tool_started` and `tool_finished`: sanitized compact tool activity;
- `command_result`: typed data for help, models, sessions, status, usage,
  permissions, workset, skill, and repo views;
- `notice`: non-fatal user-facing information;
- `turn_done`: persisted turn completion and usage;
- `error`: a stable code plus redacted user-facing message;
- `goodbye`: graceful close acknowledgement.

Frames have a fixed maximum size, unknown frame types fail closed, malformed
commands do not reach the runtime, and only one turn may be active per
connection. The server keeps reading `cancel` and `close` frames while a turn
is active. Client disconnect cancels that connection's turn and releases its
agent, sandbox, MCP, broker, and workspace resources.

Protocol payloads never contain provider credentials, the age identity,
configuration bodies, database paths, raw environment values, or arbitrary
Go error chains. Existing agent redaction is applied before an error crosses
the protocol boundary.

### Session model selection

A new ordered SQLite migration adds a nullable/empty `model_alias` field to
sessions. Empty means resolve the profile/default model using existing config
semantics. Selecting `/model <alias>` first validates the alias, then updates
the runtime and session row atomically from the user's point of view. A failed
validation or persistence write leaves both unchanged.

Opening, resuming, or continuing a session uses its stored alias when present.
If the configured alias has since been removed, the session opens with an
actionable error and a model picker; it does not silently switch models.

## Focused Conversation Interface

Interactive terminals enter the alternate screen and render four regions:

1. A compact header with the Waffle mark, selected model, profile, shortened
   session ID, and `local service` or `direct` connection state.
2. A scrollable transcript with distinct user and assistant cards. Markdown is
   styled for terminal readability. Tool calls appear as compact activity rows
   that transition from running to success/error without dumping raw payloads.
3. A bordered multiline composer. Typing `/` opens filtered command
   completion; arrow keys navigate, Tab completes, Enter submits, and the
   documented multiline key inserts a newline.
4. A footer with context-sensitive keys, busy/cancel state, and concise usage
   information when available.

Model, session, help, permissions, and confirmation views open as overlays and
return focus to the composer when dismissed. Streaming updates the current
assistant card. `Ctrl+C` cancels an active turn; when idle, the first press
arms exit and the second press exits. `Ctrl+D` on an empty composer exits
gracefully. Terminal resize recomputes card and viewport widths without losing
the composer or scroll position.

The palette follows the selected mockup's dark navy surface, warm waffle-gold
focus/accent, cool cyan assistant accents, green success, and restrained muted
text. Styling adapts to light terminals, respects `NO_COLOR`, and remains
legible in monochrome. Narrow terminals remove decorative padding and metadata
before truncating content.

## Command Contract

Command names match on the complete first word. Near misses such as `/modelsx`
remain ordinary messages. Arguments are parsed once by the shared registry,
and errors always include exact usage.

| Command | Behavior |
|---|---|
| `/help` | Open the searchable command/key reference. |
| `/exit`, `/quit` | Finish/summarize the current session where configured and close chat. |
| `/model [alias]` | With no alias, open the model picker; with an alias, validate, persist, and switch the active session. |
| `/models` | Open the model picker with aliases, provider labels, and current selection; never show credentials. |
| `/new`, `/reset`, `/clear` | Preserve the old session, discard no stored history, and start a fresh session after confirmation when an active/unsent turn would be interrupted. |
| `/sessions` | List recent sessions with title, summary excerpt, model, and updated time. |
| `/resume <session>` | Finish the current session where appropriate, then load the exact selected session, repaired transcript, workset, and stored model. With no ID, open the session picker. |
| `/status` | Show session, model/provider label, profile, connection mode, sandbox mode, and active workspace. |
| `/usage` | Show current-session usage and persisted aggregate usage available from the existing usage store. |
| `/permissions` | Show the resolved sandbox and tool allow/deny policy without changing it. |
| `/skill <name> [args]` | Preserve the existing skill invocation behavior. |
| `/repo <owner/repo>` | Preserve the existing workspace behavior and surface progress/errors as UI events. |
| `/workset [list\|replace <id> <text>\|drop <id>\|clear]` | Preserve the existing working-set inspection and correction behavior. |

Command completion displays the command, argument hint, and one-line purpose.
Commands that return lists use overlays in the TUI and stable plain text in
plain mode. Commands do not get sent to the model when their local validation
fails.

## Error and Lifecycle Behavior

- An unavailable managed socket reports its exact path and recommends checking
  `waffle.service`/`waffle-chat.socket`; it never recommends `sudo waffle chat`.
- A protocol-version mismatch tells the operator the client and service
  versions and requests a coordinated deployment.
- An invalid or removed model leaves the active model unchanged and opens or
  recommends `/models`.
- Canceling a turn keeps already persisted history. Existing session repair
  closes any durable dangling tool-use sequence on resume.
- A service restart or socket loss freezes the transcript, marks the TUI
  disconnected, and exits cleanly after acknowledgement; it does not retry a
  possibly non-idempotent turn automatically.
- Terminal restoration runs on normal exit, signals, backend failure, and
  panic paths supported by Bubble Tea.
- Summarization failure is a warning and must not trap the user in the TUI.

## Managed Deployment

Infra adds `waffle-chat.socket` to the immutable deployment bundle and updates
`waffle.service` to consume the inherited descriptor. The lifecycle
reconciler enables/starts both service and socket only in Ready state and
disables/stops both in Installed state. Routine rollout and rollback treat the
binary, service unit, socket unit, and wrapper as one compatible release set.

The administrative wrapper's chat path is deliberately small and occurs before
identity-file checks, root checks, mutation locking, provider-argument parsing,
and lifecycle reconciliation. All non-chat management behavior remains
unchanged.

The systemd socket uses `/run`, is removed when stopped, is never exposed over
TCP/Tailscale/public interfaces, and carries no reusable bearer token. The
managed host remains single-operator: members of the existing `sudo` group may
chat without elevation, while other local users receive the kernel's
permission-denied error.

## Testing Strategy

### Waffle repository

- Command-registry table tests cover every command, alias, argument form,
  whole-word matching, completion metadata, and usage error.
- Runtime tests cover model validation, atomic selection, persistence,
  continue/resume, removed aliases, reset/session switching, cancellation,
  and cleanup.
- Bubble Tea update/view tests cover focused layout, overlays, streaming/tool
  transitions, narrow widths, resize, `NO_COLOR`, monochrome, confirmation,
  cancel, double-press exit, and terminal restoration behavior.
- Plain-mode tests preserve deterministic redirected I/O and exercise the same
  command registry.
- Unix integration tests use a temporary socket and fake provider to cover
  handshake, streaming, command results, concurrent clients, one-turn-per-
  connection enforcement, cancellation, disconnect cleanup, frame bounds,
  malformed/unknown frames, and version mismatch.
- Protocol security tests seed credentials, identity, environment, paths, and
  provider-error canaries and assert none crosses any server frame.
- A PTY smoke test launches the server and TUI, selects `/model`, sends a
  streamed turn, runs `/status`, and exits with `/exit`, then inspects stored
  session/model/turn state.
- A manual terminal pass compares the running TUI with the approved Focused
  Conversation mockup at wide and narrow sizes and in no-color mode.

Focused tests run throughout implementation. Final Waffle verification is:

```sh
mise run test
mise run fmt
mise run vet
mise run lint
mise run build
```

### Infra repository

- Bootstrap/rollout contract tests assert socket path, mode `0660`, group
  `sudo`, socket activation, Ready/Installed lifecycle, release-set rollback,
  and no public listener.
- Wrapper tests run `waffle chat` as a non-root test user and prove it bypasses
  identity loading, `runuser`, mutation locks, and lifecycle reconciliation
  while preserving root requirements for provider mutations.
- Runtime-oriented tests prove unauthorized local users cannot connect and
  operator-group users can connect without sudo.
- Shell syntax, workflow contract, and Terraform validation gates remain
  required where touched.

Final Infra verification includes the relevant focused Ruby tests followed by:

```sh
ruby -Itest -e 'Dir["tests/*waffle*test.rb"].sort.each { |f| require_relative f }'
bash -n scripts/waffle-bootstrap.sh scripts/waffle-rollout.sh scripts/waffle-admin.sh
actionlint
git diff --check
```

Terraform roots touched by the deployment change are initialized with
`-backend=false` and validated.

## Documentation

Waffle documentation changes include:

- a `docs/chat.md` guide with screenshots/terminal captures, commands, keys,
  model/session behavior, plain mode, connection modes, and troubleshooting;
- README quick-start examples that use `waffle chat` without sudo;
- `docs/deploy.md` managed socket security and lifecycle guidance;
- `config.example.toml` comments for the optional standalone `[chat] socket`;
- `docs/plan.md` updates replacing the former deliberate Bubble Tea cut with
  the delivered TUI and explaining why the dependency is now justified;
- CLI usage/help text for `--socket`, `--plain`, commands, and managed errors.

Infra updates its current operator and reference documentation to describe the
socket unit, access group, unprivileged chat path, rollout/rollback unit set,
and troubleshooting commands. Historical plans remain historical rather than
being treated as live runbooks.

## Rollout Order and Compatibility

1. Land and release Waffle with direct mode, TUI, chat protocol server/client,
   session migration, and socket-activation support.
2. Land Infra with the compatible Waffle artifact requirement, socket unit,
   service wiring, wrapper route, tests, and docs.
3. Deploy the release set and verify a Ready host with an operator-group SSH
   session can run `waffle chat`, change/resume a model, complete a turn, and
   exit without sudo.
4. Verify provider administration still rejects non-root use and that a
   non-operator local account cannot connect.

An Infra rollout must not install the unprivileged wrapper route with an older
binary that lacks the protocol client. Rollback restores all coupled units and
the wrapper with the previous binary.

## Acceptance Criteria

The work is complete only when current evidence proves all of the following:

1. On the managed host, an authorized operator runs `waffle chat` without
   `sudo`; the client has no direct access to service credentials/state and an
   unauthorized local account cannot connect.
2. Interactive chat uses the approved Focused Conversation Bubble Tea UI, has
   verified streaming/tool/overlay/responsive/no-color behavior, and retains a
   tested plain fallback.
3. Every command in the command contract is implemented, model selection is
   persisted per session, `/exit` closes cleanly, and focused plus end-to-end
   tests cover the behaviors.
4. Waffle and Infra documentation describe the actual shipped experience and
   security boundary.
5. All listed repository gates pass, the TUI receives a manual visual check,
   and the managed-host smoke test proves the deployed outcome rather than
   only the local implementation.
