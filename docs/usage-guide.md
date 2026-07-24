# Waffle usage guide: skills, profiles, cron, self-learning

This fills the gap between `docs/plan.md` (architecture) and `docs/chat.md`
(the chat client): how to actually *use* the four features that don't have a
walkthrough yet. Written from the current source, not the design doc — if
behavior drifts, the code cited here is the source of truth.

## Skills

A skill is a directory with a `SKILL.md` inside it (agentskills.io-compatible,
so anything written for hermes-agent or openclaw ports over unchanged).

**Location:** `$WAFFLE_HOME/workspace/main/skills/<skill-name>/SKILL.md`
(`main` is the default agent; other agent workspaces follow the same pattern
under their own name).

**Format:** YAML frontmatter with `name:` and `description:`, then the body —
the instructions the model reads.

```markdown
---
name: standup
description: Summarize yesterday's commits and open PRs into a standup update.
---

Pull the last 24h of commits and open PRs. Group into Yesterday / Today /
Blockers. Keep it to five lines.
```

**Using one:**
- In chat: `/skill standup` — this loads the SKILL.md body and turns it into an
  instruction for the current turn (`internal/skill/skill.go`).
- The model can also read a skill on its own initiative when a task matches
  its description — every discovered skill is listed in the system prompt
  with its path, so you don't have to invoke it by hand every time.

**Inspecting what's there:**
```sh
waffle skills ls          # lists all discovered skills, active/inactive
```

Skills discovered on disk are visible immediately; "active" vs "inactive" only
matters for skills produced by self-learning (below) — hand-written skills you
drop into the directory are active by default.

## Agent profiles

Profiles (`[agent.profile.*]` in `config.toml`) are a trust boundary, not a
personality preset — they fix the system prompt, model, sandbox mode, and
allowed tools together. `config.example.toml` ships three: `main` (host
sandbox, full tools, owner-interactive), `researcher` (docker sandbox,
read/fetch only), `reviewer` (docker sandbox, read-only critique).

A profile can only ever be *tightened* relative to its parent — a child
profile can't widen tool or sandbox access, even if you ask for it.

**Where you select one:**
```sh
waffle chat --profile reviewer
waffle cron add ... --profile researcher
waffle ws open owner/repo --profile reviewer
waffle session profile telegram:42 reviewer
```

Add your own by copying one of the example blocks in `config.toml` and giving
it a new `[agent.profile.<name>]` name, an `allow`/`deny` tool list, and a
`sandbox` (`host` or `docker`).

## Subagents

Subagents aren't something you invoke from the CLI or `/` commands — they're
a tool the *model* calls on itself (`spawn_subagent`, defined in
`internal/agent/subagent.go`) when it decides a subtask deserves its own
fresh context. You'll see it appear as a tool call in the transcript, e.g.
when the main agent delegates "research X and report back" to a
`researcher`-profiled child. A spawned child can be pointed at a specific
profile, but it always inherits your tightening rules — it can never get
`workspace_update` or `spawn_subagent` itself, and a read-only parent's child
stays read-only.

There's nothing to configure to turn this on; it's available wherever
`spawn_subagent` isn't explicitly denied in the active profile's tool policy.

## Long-running / scheduled tasks (cron)

Cron jobs are a cron expression, a prompt, and an optional delivery target.
They only fire while `waffle serve` is running.

```sh
waffle cron add standup 0 9 * * 1-5 "Summarize my starred repos" \
  --deliver telegram:900 --profile researcher

waffle cron ls              # list jobs, next run, last status
waffle cron run <id>        # run one now, synchronously
waffle cron rm <id>         # remove
```

`--deliver channel:chat_id` sends the reply somewhere (e.g. `telegram:900`);
omit it and the reply is only logged. `--profile` scopes the job to one of
your agent profiles the same way `waffle chat --profile` does.

## Waffle Desk operations

Waffle Desk provides local browser views of the same runs, schedules,
workspaces, sessions, and memory used by the CLI. It is not a second task
database. Open the **Tasks**, **Workspaces**, or **Memory** destination from
the Desk navigation.

See the [Waffle Desk rollout guide](waffle-desk.md) for enablement, access,
security, release checks, and rollback.

### Tasks and schedules

Tasks combines scheduled jobs with active and recent cron runs. The available
filters are **All**, **Active**, **Scheduled**, **Completed**, and
**Attention**. Attention means a schedule or run has the canonical
`failed`, `error`, or `stalled` state; arbitrary text containing those words
does not qualify. **Open at desk** is shown only when the run names a session
that is still persisted. Following it selects that exact session in Today.

Schedule changes are validated before the stored job changes. Names and
prompts must be non-empty, cron uses the standard five-field format, profiles
must use the valid profile-name format, and an optional delivery target must
use `channel:chat_id` syntax. An invalid create or update leaves the previous
stored definition unchanged.

### Workspaces

Open accepts a repository in `owner/repo` form and an optional valid profile.
A new workspace creates its own persisted session; it does not inherit the
session of a failed run from Attention. The workspace keeps its session through
select, idle, and resume, while the UI shows its configured profile and
network-egress policy. Selecting a workspace opens that exact session in Today.

Close is deliberately guarded. Desk reinspects the workspace, displays dirty
and unpushed evidence, and issues a single-use confirmation that expires after
60 seconds. Confirmation closes only a workspace that is still eligible;
there is no force-close control in Desk.

### Memory

Memory search combines persisted turn excerpts, session summaries, and
Waffle-owned notes. Results retain their source kind, source ID, timestamp,
excerpt, archived state, and provenance. Attaching a result resolves it again
server-side and adds one pinned, user-sourced fact to the explicitly selected
session.

The session working set holds at most 32 entries and 8 KiB in total, and each
entry is at most 1 KiB. Reaching a count or byte limit rejects the new
attachment without evicting or partially writing another entry.

Forgetting a note is a preview followed by a single-use confirmation that
expires after 60 seconds. Cancel sends no confirmation request and changes
nothing. Confirm archives only the selected Waffle-owned note; it cannot erase
provider logs, delivered messages, or previous backups, and it does not offer
a provider-side delete or Undo action.

## Self-learning

This is the "waffle learns from its own failures" loop
(`internal/skill/learn.go`), and it's a mine → propose → validate pipeline,
not something that happens silently in the background.

```sh
waffle learn                 # or: waffle skills audit (identical, different name)
```

This mines recent session history for recurring failure/friction patterns,
proposes skill candidates with attribution and evidence (which sessions
triggered it), and writes accepted proposals as **inactive** skills — it
never auto-activates anything. It's meant to run on a schedule:

```sh
waffle cron add learn-daily 0 3 * * * /learn --deliver telegram:900
```

(`/learn` is a reserved cron action, not an arbitrary shell command — cron
jobs never dispatch arbitrary CLI commands from the prompt field.)

Review what it proposed, then promote what's worth keeping:

```sh
waffle skills ls              # see active/inactive skills and proposals
waffle skills activate <name> # promote a proposal into the live skills index
```

There's a second, slower rung above this: a skill that keeps getting
exercised by shelling out for the same thing can eventually get hardened into
a native Go tool via a self-PR — that path is described in `docs/plan.md`
under "Skills & memory" but isn't something you trigger directly; it's a
consequence of the reflection loop noticing repetition, not a command.

## Model aliases (the other gap)

Not one of the four above, but worth folding in since it trips people up: you
can't type an arbitrary OpenRouter model name into `/model`. You register
favorite aliases first, and only those show up:

```sh
sudo waffle provider add                                   # guided connection setup
sudo waffle provider models openrouter --search claude      # browse the catalog
sudo waffle provider model add openrouter anthropic/claude-sonnet-4.6 --default
```

Then in chat, `/models` lists what you've registered and `/model <alias>`
switches to one of them.
