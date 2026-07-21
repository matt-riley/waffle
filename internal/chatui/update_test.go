package chatui

import (
	"strings"
	"testing"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/matt-riley/waffle/internal/chat"
)

func TestPromptHistoryTraversalUpDown(t *testing.T) {
	backend := newFakeBackend(chat.State{})
	m := New(backend, chat.OpenOptions{}, Options{Width: 100, Height: 30})
	m.openResolved, m.opened, m.connected = true, true, true
	m.history = []string{"one", "two"}
	m.historyIdx = -1
	m.composer.SetValue("draft")

	// First Up saves draft and loads most recent history entry.
	m = updateForTest(t, m, key(tea.KeyUp))
	if got := m.composer.Value(); got != "two" {
		t.Fatalf("first Up composer = %q, want %q", got, "two")
	}
	if m.historyIdx != 1 {
		t.Fatalf("historyIdx after first Up = %d, want 1", m.historyIdx)
	}
	if m.historyDraft != "draft" {
		t.Fatalf("historyDraft = %q, want %q", m.historyDraft, "draft")
	}

	// Second Up loads older entry.
	m = updateForTest(t, m, key(tea.KeyUp))
	if got := m.composer.Value(); got != "one" {
		t.Fatalf("second Up composer = %q, want %q", got, "one")
	}
	if m.historyIdx != 0 {
		t.Fatalf("historyIdx after second Up = %d, want 0", m.historyIdx)
	}

	// Further Up stays at oldest.
	m = updateForTest(t, m, key(tea.KeyUp))
	if got := m.composer.Value(); got != "one" {
		t.Fatalf("third Up composer = %q, want %q", got, "one")
	}
	if m.historyIdx != 0 {
		t.Fatalf("historyIdx at oldest = %d, want 0", m.historyIdx)
	}

	// Down traverses forward toward the draft.
	m = updateForTest(t, m, key(tea.KeyDown))
	if got := m.composer.Value(); got != "two" {
		t.Fatalf("Down composer = %q, want %q", got, "two")
	}
	if m.historyIdx != 1 {
		t.Fatalf("historyIdx after Down = %d, want 1", m.historyIdx)
	}

	// Down past newest restores the in-progress draft.
	m = updateForTest(t, m, key(tea.KeyDown))
	if got := m.composer.Value(); got != "draft" {
		t.Fatalf("restore draft composer = %q, want %q", got, "draft")
	}
	if m.historyIdx != -1 {
		t.Fatalf("historyIdx after restore = %d, want -1", m.historyIdx)
	}
	if m.historyDraft != "" {
		t.Fatalf("historyDraft after restore = %q, want empty", m.historyDraft)
	}
}

func TestSubmitAppendsPromptHistory(t *testing.T) {
	backend := newFakeBackend(chat.State{})
	m := New(backend, chat.OpenOptions{}, Options{Width: 100, Height: 30})
	m.openResolved, m.opened, m.connected = true, true, true

	m.composer.SetValue("hello")
	m = updateForTest(t, m, key(tea.KeyEnter))
	if len(m.history) != 1 || m.history[0] != "hello" {
		t.Fatalf("history after submit = %v, want [hello]", m.history)
	}
	if m.historyIdx != -1 {
		t.Fatalf("historyIdx after submit = %d, want -1", m.historyIdx)
	}
	if m.historyDraft != "" {
		t.Fatalf("historyDraft after submit = %q, want empty", m.historyDraft)
	}
	if m.composer.Value() != "" {
		t.Fatalf("composer after submit = %q, want empty", m.composer.Value())
	}

	// Finish turn so a second plain-text submit is accepted (not queued).
	m = updateForTest(t, m, turnDoneMsg{operationID: m.activeTurnID})
	m.composer.SetValue("world")
	m = updateForTest(t, m, key(tea.KeyEnter))
	if len(m.history) != 2 || m.history[0] != "hello" || m.history[1] != "world" {
		t.Fatalf("history after second submit = %v, want [hello world]", m.history)
	}

	// Empty submit does not append.
	m = updateForTest(t, m, turnDoneMsg{operationID: m.activeTurnID})
	m.composer.SetValue("   ")
	m = updateForTest(t, m, key(tea.KeyEnter))
	if len(m.history) != 2 {
		t.Fatalf("empty submit changed history = %v", m.history)
	}

	// Consecutive duplicate is skipped but still resets navigation.
	m.historyIdx = 0
	m.historyDraft = "stale"
	m.composer.SetValue("world")
	m = updateForTest(t, m, key(tea.KeyEnter))
	if len(m.history) != 2 {
		t.Fatalf("duplicate submit changed history = %v", m.history)
	}
	if m.historyIdx != -1 || m.historyDraft != "" {
		t.Fatalf("duplicate submit did not reset nav: idx=%d draft=%q", m.historyIdx, m.historyDraft)
	}
}

func TestPromptHistoryEmptyDoesNothing(t *testing.T) {
	m := New(newFakeBackend(chat.State{}), chat.OpenOptions{}, Options{Width: 100, Height: 30})
	m.composer.SetValue("draft")
	m = updateForTest(t, m, key(tea.KeyUp))
	if got := m.composer.Value(); got != "draft" {
		t.Fatalf("Up with empty history composer = %q, want draft", got)
	}
	if m.historyIdx != -1 {
		t.Fatalf("historyIdx = %d, want -1", m.historyIdx)
	}
}

func TestPromptHistoryDoesNotStealPaletteKeys(t *testing.T) {
	m := New(newFakeBackend(chat.State{}), chat.OpenOptions{}, Options{Width: 100, Height: 30})
	m.history = []string{"prior"}
	m.historyIdx = -1
	for _, r := range "/mo" {
		m = updateForTest(t, m, tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	if !m.paletteVisible || len(m.palette) < 2 {
		t.Fatalf("palette visible=%v items=%d", m.paletteVisible, len(m.palette))
	}
	before := m.composer.Value()
	m = updateForTest(t, m, key(tea.KeyDown))
	if m.paletteIndex != 1 {
		t.Fatalf("palette index = %d, want 1 (history must not intercept)", m.paletteIndex)
	}
	if m.composer.Value() != before {
		t.Fatalf("composer changed under palette: %q -> %q", before, m.composer.Value())
	}
	if m.historyIdx != -1 {
		t.Fatalf("historyIdx = %d, want -1 while palette open", m.historyIdx)
	}
}

func TestPromptHistoryMultilineFirstLineOnly(t *testing.T) {
	m := New(newFakeBackend(chat.State{}), chat.OpenOptions{}, Options{Width: 100, Height: 30})
	m.history = []string{"older"}
	m.historyIdx = -1
	m.composer.SetValue("line one")
	m = updateForTest(t, m, modified(tea.KeyEnter, tea.ModAlt))
	m.composer.InsertString("line two")
	// Cursor is on the second line after Alt+Enter + insert.
	if m.composer.Line() == 0 {
		t.Fatal("expected cursor on second line for multi-line draft")
	}
	m = updateForTest(t, m, key(tea.KeyUp))
	// Up on a non-first line should move within the composer, not load history.
	if m.historyIdx != -1 {
		t.Fatalf("historyIdx = %d, want -1 (multi-line non-first-line Up)", m.historyIdx)
	}
	if got := m.composer.Value(); got == "older" {
		t.Fatal("Up on second line loaded history; want in-composer cursor move")
	}
	// After moving to the first line, Up should enter history.
	// Line() may already be 0 after the composer handled KeyUp; if not, Up once more.
	if m.composer.Line() != 0 {
		m = updateForTest(t, m, key(tea.KeyUp))
	}
	if m.composer.Line() != 0 {
		t.Fatalf("expected cursor on first line, got line %d", m.composer.Line())
	}
	m = updateForTest(t, m, key(tea.KeyUp))
	if got := m.composer.Value(); got != "older" {
		t.Fatalf("Up on first line composer = %q, want %q", got, "older")
	}
	if m.historyIdx != 0 {
		t.Fatalf("historyIdx = %d, want 0", m.historyIdx)
	}
}

func TestQueuedSubmitAppendsHistory(t *testing.T) {
	backend := newFakeBackend(chat.State{})
	m := New(backend, chat.OpenOptions{}, Options{Width: 100, Height: 30})
	m.openResolved, m.opened, m.connected = true, true, true
	m.turnActive = true
	m.activeTurnID = 1
	m.composer.SetValue("queued prompt")
	m = updateForTest(t, m, key(tea.KeyEnter))
	if len(m.history) != 1 || m.history[0] != "queued prompt" {
		t.Fatalf("queued submit history = %v, want [queued prompt]", m.history)
	}
	if m.historyIdx != -1 {
		t.Fatalf("historyIdx after queue = %d, want -1", m.historyIdx)
	}
}

func TestActivitySpinnerTicksWhenTurnActive(t *testing.T) {
	m := New(newFakeBackend(chat.State{}), chat.OpenOptions{}, Options{Width: 100, Height: 30, NoColor: true})
	m.openResolved, m.opened, m.connected = true, true, true
	m.turnActive = true

	before := m.spinner.View()
	// spinner.Tick produces a TickMsg belonging to this spinner instance.
	updated, cmd := m.Update(m.spinner.Tick())
	m = updated.(*Model)
	after := m.spinner.View()
	if after == before {
		t.Fatalf("spinner frame did not advance while turnActive: before=%q after=%q", before, after)
	}
	if cmd == nil {
		t.Fatal("expected re-tick command while turn is active")
	}
	// Busy footer should include the animated glyph.
	if footer := m.renderFooter(96); footer == "" || !containsSpinnerFrame(footer) {
		t.Fatalf("busy footer missing spinner glyph: %q", footer)
	}

	// When the turn ends, further ticks must not keep animating.
	m.turnActive = false
	idleBefore := m.spinner.View()
	updated, cmd = m.Update(m.spinner.Tick())
	m = updated.(*Model)
	if m.spinner.View() != idleBefore {
		t.Fatalf("spinner advanced while turn inactive: before=%q after=%q", idleBefore, m.spinner.View())
	}
	if cmd != nil {
		t.Fatal("inactive turn must not schedule another spinner tick")
	}

	// Command-only busy path also advances the spinner.
	m.commandActive = true
	cmdBefore := m.spinner.View()
	updated, cmd = m.Update(m.spinner.Tick())
	m = updated.(*Model)
	if m.spinner.View() == cmdBefore {
		t.Fatal("spinner did not advance while commandActive")
	}
	if cmd == nil {
		t.Fatal("expected re-tick command while command is active")
	}
}

func containsSpinnerFrame(s string) bool {
	for _, frame := range spinner.Dot.Frames {
		if glyph := strings.TrimSpace(frame); glyph != "" && strings.Contains(s, glyph) {
			return true
		}
	}
	return false
}
