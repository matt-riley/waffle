### Title
feat: show command descriptions inline in the `/` palette and consider auto-registering skills as slash commands

### Labels
enhancement, chatui, ux

### Body

**Problem**

`internal/chatui/render.go`'s `renderPalette()` shows only each command's raw `Usage` string (e.g. `/model [alias]`) when the user types `/`. To learn what a command actually does, the user has to separately run `/help`, which opens a full overlay. Discovery of the twelve commands in `internal/chat/commands.go` is two steps deeper than it needs to be.

Separately, skills (`internal/skill/skill.go`) currently require the `/skill <name> [args]` wrapper — a skill named `standup` is invoked as `/skill standup`, not `/standup`. `Skill.Description` already exists on the type; nothing structural blocks giving each discovered skill its own top-level palette entry.

**Context**

`docs/tui-ux-comparison.md` compares this to hermes-agent's `/` autocomplete dropdown (which shows descriptions) and its auto-registration of every installed skill as its own slash command.

**Scope for this issue**

Part A (required): inline descriptions in the palette.
Part B (optional/separate PR if A is deemed sufficient on its own): auto-register skills as top-level commands.

**Acceptance criteria**

- AC1: `renderPalette()` in `internal/chatui/render.go` renders each filtered command as `Usage — Description` (or a layout achieving the same information density), truncating gracefully at narrow widths the same way `renderFooter` already does via `ansi.Truncate`.
- AC2: The palette remains legible with all twelve current commands visible/scrollable when the user has typed only `/` (no filter) — verify against the existing golden fixtures in `internal/chatui/testdata/` and update them if the layout changes.
- AC3: `internal/chatui/render_test.go` gains or updates a test asserting the rendered palette string contains at least one command's `Description` text, not just its `Usage`.
- AC4 (Part B, if implemented): typing `/<skill-name>` where `<skill-name>` matches a discovered skill in `r.skills` behaves identically to `/skill <skill-name>`, and both forms continue to work (no breaking change to `/skill`).
- AC5 (Part B, if implemented): `chat.Complete(prefix)` in `internal/chat/commands.go` is extended (or a parallel skill-aware completion path is added) so typing `/` includes matching skill names in the palette alongside the fixed command registry, without mutating the existing `commandRegistry` array (skills are dynamic per-install; commands are not).
- AC6 (Part B, if implemented): a skill name that collides with an existing reserved command name (e.g. a skill literally named `help`) does not shadow the built-in command — built-ins win, and `waffle skills ls` or skill discovery surfaces a warning for the collision.
- AC7: `docs/chat.md`'s command table is updated if Part B ships, noting skills are also directly invocable.
- AC8: `gofmt` clean; existing `internal/chatui` and `internal/chat` test suites pass with `-count=1`.
