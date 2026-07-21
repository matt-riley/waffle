### Title
feat: show live token count and elapsed time in chat footer during an active turn

### Labels
enhancement, chatui, ux

### Body

**Problem**

`internal/chatui/render.go`'s `renderFooter()` shows a static `/help  /model  /sessions` on the left at all times, and on the right either `Alt+↵ newline · ↵ send` (idle), `Esc cancel · working…` (busy — with no further detail), or token counts (`%d in · %d out`) but only when idle and only once `m.inputTokens`/`m.outputTokens` are already nonzero from a completed turn. While a turn is running there is no live indication of token usage, elapsed time, or progress — just a static "working…" that looks identical whether the agent has been running for two seconds or two minutes.

**Context**

`docs/tui-ux-comparison.md` compares this against hermes-agent's persistent status bar (model, token/context-window fraction with color-coded fill, cost estimate, elapsed session time), which stays visible during active turns, not just after.

**Scope for this issue** (kept minimal — no cost model, no context-window percentage, both flagged as separate future work):

- Show elapsed time for the in-progress turn, updating live (e.g. `working… 12s`).
- Show running input/output token counts as they update mid-turn, not just after `turnDoneMsg`.

**Acceptance criteria**

- AC1: While `m.turnActive == true`, `renderFooter()`'s right-hand side includes an elapsed-time readout for the current turn (format: `Ns` under 60s, `Mm Ns` at or above 60s) that increases across successive renders without a full turn completing.
- AC2: While `m.turnActive == true`, if `chat.Event` payloads carry incremental token counts (verify current `chat.Event`/`chat.EventTextDelta` payload in `internal/chat/types.go` — extend it if token deltas aren't already surfaced), the footer's token count updates at least once before the turn completes, not only after.
- AC3: The idle-state footer (post-turn token summary, command hints) is unchanged — this issue only adds detail to the busy state, it doesn't remove or alter the idle-state layout.
- AC4: Narrow-terminal behavior (`m.width < 72`) is preserved: the compact footer still fits and doesn't wrap or truncate the essential "working" indicator.
- AC5: A new or extended test in `internal/chatui/render_test.go` renders the footer mid-turn at two different elapsed times and asserts the rendered string differs (proving it's live, not static).
- AC6: `gofmt` clean; existing `internal/chatui` test suite passes with `-count=1`.
