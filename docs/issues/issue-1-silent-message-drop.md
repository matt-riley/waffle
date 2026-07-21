### Title
fix: chat composer silently drops messages sent while a turn is active

### Labels
bug, chatui, ux

### Body

**Problem**

In `internal/chatui/update.go`, `submit()` contains:

```go
if m.turnActive && !ok {
    return m, nil
}
```

If the user types a plain-text message (not a recognized `/command`) and presses Enter while an agent turn is already running, the input is discarded with no feedback: no error, no queued message, nothing appended to the transcript, no notice event emitted. The composer clears (or doesn't — needs verification, see AC1) and the user has no way to tell whether their message was received, ignored, or lost.

This is the single most likely cause of "some commands don't seem to work" — the failure mode looks identical to a hung UI.

**Context**

`docs/tui-ux-comparison.md` documents this against hermes-agent, which handles the same moment with a named `busy_input_mode` (`interrupt` / `queue` / `steer`), always with explicit on-screen acknowledgment, plus a one-time first-touch hint explaining the behavior.

**Proposed fix**

At minimum, stop the silent drop: emit a `chat.Event{Kind: chat.EventNotice}` telling the user their message wasn't sent because a turn is active, and do not clear the composer (preserve their draft so they don't have to retype it).

Preferred fix: implement a `queue` mode — hold the message and automatically submit it as the next turn once the active turn completes — since that requires no new user-facing concept and matches what most users already expect Enter to do.

**Acceptance criteria**

- AC1: Given a turn is active (`m.turnActive == true`) and the user submits a non-command message, the composer's draft text is preserved (not cleared) and a `chat.EventNotice` is emitted informing the user the message was not sent (or was queued, if the queue behavior is implemented).
- AC2: Given a turn is active and the user submits a non-command message, the message never silently disappears — it either appears in the transcript once queued/sent, or remains in the composer for the user to resend.
- AC3: A new unit test in `internal/chatui/update_test.go` (or existing test file) exercises: turn active → submit plain text → assert composer value is unchanged AND a notice event was recorded (or, if queue mode is implemented, assert the message is held and auto-submitted via `turnCmd` once `turnDoneMsg` fires for the prior turn).
- AC4: Recognized `/commands` (e.g. `/status`, `/help`) continue to work identically while a turn is active, per current behavior — this fix must not change command dispatch during an active turn.
- AC5: `docs/chat.md` is updated to document the new behavior under the "Keys" or "Commands" section.
- AC6: `gofmt` clean; existing `internal/chatui` test suite passes with `-count=1`.
