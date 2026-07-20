package chatui

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/matt-riley/waffle/internal/chat"
	"github.com/matt-riley/waffle/internal/llm"
)

type fakeBackend struct {
	mu            sync.Mutex
	state         chat.State
	turns         []string
	commands      []chat.ParsedCommand
	turnEvents    []chat.Event
	commandResult chat.Result
	openErr       error
	turnErr       error
	commandErr    error
	cancelCalls   int
	closeCalls    int
}

func newFakeBackend(state chat.State) *fakeBackend { return &fakeBackend{state: state} }

func (b *fakeBackend) Open(context.Context, chat.OpenOptions) (chat.State, error) {
	return b.state, b.openErr
}

func (b *fakeBackend) Turn(_ context.Context, input string, emit func(chat.Event)) error {
	b.mu.Lock()
	b.turns = append(b.turns, input)
	events := append([]chat.Event(nil), b.turnEvents...)
	err := b.turnErr
	b.mu.Unlock()
	for _, event := range events {
		emit(event)
	}
	return err
}

func (b *fakeBackend) Command(_ context.Context, command chat.ParsedCommand, emit func(chat.Event)) (chat.Result, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.commands = append(b.commands, command)
	return b.commandResult, b.commandErr
}

func (b *fakeBackend) Cancel() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cancelCalls++
}
func (b *fakeBackend) Close(context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closeCalls++
	return nil
}

func updateForTest(t *testing.T, m *Model, msg tea.Msg) *Model {
	t.Helper()
	updated, _ := m.Update(msg)
	got, ok := updated.(*Model)
	if !ok {
		t.Fatalf("Update returned %T", updated)
	}
	return got
}

func key(code rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: code} }
func modified(code rune, mod tea.KeyMod) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: code, Mod: mod}
}

func TestModelHandlesTurnCancelAndExit(t *testing.T) {
	backend := newFakeBackend(chat.State{SessionID: "01TEST", ModelAlias: "gpt"})
	m := New(backend, chat.OpenOptions{}, Options{Width: 100, Height: 30})
	m.composer.SetValue("hello")
	m = updateForTest(t, m, key(tea.KeyEnter))
	if !m.turnActive {
		t.Fatal("enter did not start a turn")
	}
	m = updateForTest(t, m, key(tea.KeyEscape))
	if backend.cancelCalls != 1 {
		t.Fatalf("cancel calls = %d", backend.cancelCalls)
	}
	m.composer.SetValue("/exit")
	m = updateForTest(t, m, key(tea.KeyEnter))
	if !m.quitting {
		t.Fatal("/exit did not quit")
	}
}

func TestModelCommandPaletteAndModelOverlay(t *testing.T) {
	backend := newFakeBackend(chat.State{Models: []chat.Model{{Alias: "gpt"}, {Alias: "claude"}}})
	m := New(backend, chat.OpenOptions{}, Options{Width: 100, Height: 30})
	m.state = backend.state
	m.composer.SetValue("/mo")
	m = updateForTest(t, m, key(tea.KeyTab))
	if !m.paletteVisible || len(m.palette) != 2 {
		t.Fatalf("palette = %+v", m.palette)
	}
	m.composer.SetValue("/model")
	m = updateForTest(t, m, key(tea.KeyEnter))
	m = updateForTest(t, m, commandResultMsg{command: chat.ParsedCommand{Name: chat.CommandModel}, result: chat.Result{Models: backend.state.Models}})
	if m.overlay != overlayModels {
		t.Fatalf("overlay = %v", m.overlay)
	}
}

func TestModelSlashCompletionFiltersNavigatesAndCompletes(t *testing.T) {
	m := New(newFakeBackend(chat.State{}), chat.OpenOptions{}, Options{Width: 100, Height: 30})
	for _, r := range "/mo" {
		m = updateForTest(t, m, tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	if !m.paletteVisible || len(m.palette) != 2 || m.palette[0].Name != chat.CommandModel {
		t.Fatalf("typing palette = %+v visible=%v", m.palette, m.paletteVisible)
	}
	m = updateForTest(t, m, key(tea.KeyDown))
	if m.paletteIndex != 1 {
		t.Fatalf("palette index = %d", m.paletteIndex)
	}
	m = updateForTest(t, m, key(tea.KeyTab))
	if got := m.composer.Value(); got != "/models" {
		t.Fatalf("completed value = %q", got)
	}
}

func TestModelExitKeysAndMultiline(t *testing.T) {
	backend := newFakeBackend(chat.State{})
	m := New(backend, chat.OpenOptions{}, Options{Width: 100, Height: 30})
	m.composer.SetValue("work")
	m = updateForTest(t, m, key(tea.KeyEnter))
	m = updateForTest(t, m, modified('c', tea.ModCtrl))
	if backend.cancelCalls != 1 || m.quitting {
		t.Fatalf("active ctrl+c: cancels=%d quitting=%v", backend.cancelCalls, m.quitting)
	}
	m = updateForTest(t, m, turnDoneMsg{})
	m = updateForTest(t, m, modified('c', tea.ModCtrl))
	if !m.exitArmed || m.quitting {
		t.Fatalf("first idle ctrl+c: armed=%v quitting=%v", m.exitArmed, m.quitting)
	}
	m = updateForTest(t, m, modified('c', tea.ModCtrl))
	if !m.quitting {
		t.Fatal("second idle ctrl+c did not quit")
	}

	m = New(backend, chat.OpenOptions{}, Options{Width: 100, Height: 30})
	m.composer.SetValue("line one")
	m = updateForTest(t, m, modified(tea.KeyEnter, tea.ModAlt))
	if got := m.composer.Value(); got != "line one\n" {
		t.Fatalf("alt+enter value = %q", got)
	}
	m.composer.Reset()
	m = updateForTest(t, m, modified('d', tea.ModCtrl))
	if !m.quitting {
		t.Fatal("ctrl+d on empty composer did not quit")
	}
}

func TestModelDoesNotStartASecondTurnWhileBusy(t *testing.T) {
	backend := newFakeBackend(chat.State{})
	m := New(backend, chat.OpenOptions{}, Options{Width: 100, Height: 30})
	m.turnActive = true
	m.composer.SetValue("second turn")
	updated, cmd := m.Update(key(tea.KeyEnter))
	m = updated.(*Model)
	if cmd != nil {
		t.Fatal("busy submit returned a backend command")
	}
	if got := m.composer.Value(); got != "second turn" {
		t.Fatalf("busy composer = %q", got)
	}
	if len(backend.turns) != 0 {
		t.Fatalf("busy turns = %q", backend.turns)
	}
}

func TestModelResizeStreamsToolsAndDisconnect(t *testing.T) {
	backend := newFakeBackend(chat.State{})
	m := New(backend, chat.OpenOptions{}, Options{Width: 100, Height: 30})
	for range 40 {
		m.messages = append(m.messages, messageCard{role: roleNotice, text: "transcript row"})
	}
	m.syncViewport(true)
	m.viewport.SetYOffset(2)
	m = updateForTest(t, m, tea.WindowSizeMsg{Width: 58, Height: 24})
	if m.width != 58 || !m.composer.Focused() || m.viewport.YOffset() != 2 {
		t.Fatalf("resize lost state: width=%d focused=%v offset=%d", m.width, m.composer.Focused(), m.viewport.YOffset())
	}
	m = updateForTest(t, m, eventMsg{event: chat.Event{Kind: chat.EventTextDelta, Text: "hello "}})
	m = updateForTest(t, m, eventMsg{event: chat.Event{Kind: chat.EventTextDelta, Text: "world"}})
	m = updateForTest(t, m, eventMsg{event: chat.Event{Kind: chat.EventToolStarted, ToolName: "read logs"}})
	m = updateForTest(t, m, eventMsg{event: chat.Event{Kind: chat.EventToolFinished, ToolName: "read logs", ByteCount: 2100}})
	if got := m.messages[len(m.messages)-1].text; got != "hello world" {
		t.Fatalf("streamed text = %q", got)
	}
	if len(m.tools) != 1 || !m.tools[0].done {
		t.Fatalf("tools = %+v", m.tools)
	}
	m = updateForTest(t, m, turnDoneMsg{err: errors.New("socket lost")})
	if m.connected || !m.awaitingAck || m.err == nil {
		t.Fatalf("disconnect state: connected=%v ack=%v err=%v", m.connected, m.awaitingAck, m.err)
	}
	before := len(m.messages)
	m = updateForTest(t, m, key(tea.KeyEnter))
	if !m.quitting || len(m.messages) != before {
		t.Fatalf("ack did not exit cleanly")
	}
}

func TestModelAssociatesToolRowsWithCurrentAssistantCard(t *testing.T) {
	m := New(newFakeBackend(chat.State{}), chat.OpenOptions{}, Options{Width: 100, Height: 30})
	m.messages = []messageCard{{role: roleAssistant, text: "old answer"}, {role: roleUser, text: "new question"}}
	m.applyEvent(chat.Event{Kind: chat.EventTextDelta, Text: "new answer"})
	m.applyEvent(chat.Event{Kind: chat.EventToolStarted, ToolName: "inspect unit"})
	if got, want := m.tools[0].messageIndex, len(m.messages)-1; got != want {
		t.Fatalf("tool message index = %d, want %d", got, want)
	}
}

func TestModelHeaderConnectionAndFooterLifecycle(t *testing.T) {
	state := chat.State{SessionID: "01ABCDEFGHIJK", Title: "Deploy", ModelAlias: "gpt", Profile: "review", ConnectionMode: "unix"}
	m := New(newFakeBackend(state), chat.OpenOptions{}, Options{Width: 100, Height: 30, NoColor: true})
	m.state, m.connected = state, true
	header := m.renderHeader(96)
	for _, want := range []string{"Deploy · 01ABCDEF", "gpt · review · local service"} {
		if !strings.Contains(header, want) {
			t.Errorf("header missing %q in %q", want, header)
		}
	}
	m.turnActive = true
	if footer := m.renderFooter(96); !strings.Contains(footer, "Esc cancel · working…") {
		t.Fatalf("busy footer = %q", footer)
	}
	m.turnActive = false
	m.applyEvent(chat.Event{Kind: chat.EventTurnDone, Usage: llm.Usage{InputTokens: 12, OutputTokens: 7}})
	if footer := m.renderFooter(96); !strings.Contains(footer, "12 in · 7 out") {
		t.Fatalf("usage footer = %q", footer)
	}
	m.connected, m.awaitingAck = false, true
	if header := m.renderHeader(96); !strings.Contains(header, "disconnected") {
		t.Fatalf("disconnected header = %q", header)
	}
}

func TestModelInitialHeaderConnectsAndLongTitlesStayBounded(t *testing.T) {
	m := New(newFakeBackend(chat.State{}), chat.OpenOptions{}, Options{Width: 80, Height: 24, NoColor: true})
	if got := m.renderHeader(76); !strings.Contains(got, "connecting") {
		t.Fatalf("initial header = %q", got)
	}
	m.opened, m.connected = true, true
	m.state = chat.State{Title: strings.Repeat("long title ", 20), ModelAlias: "gpt", Profile: "main"}
	if got := m.renderHeader(76); lipgloss.Width(got) > 76 {
		t.Fatalf("header width = %d, want <= 76: %q", lipgloss.Width(got), got)
	}
}

func TestModelMonochromeListOverlayHasNoANSI(t *testing.T) {
	m := New(newFakeBackend(chat.State{}), chat.OpenOptions{}, Options{Width: 80, Height: 24, NoColor: true})
	m.applyResult(chat.ParsedCommand{Name: chat.CommandModels}, chat.Result{Models: []chat.Model{{Alias: "gpt", Provider: "local", Upstream: "test"}}})
	if got := m.renderOverlay(76); strings.Contains(got, "\x1b[") {
		t.Fatalf("monochrome overlay contains ANSI: %q", got)
	}
}

func TestModelCommandOverlaysResumeAndConfirmation(t *testing.T) {
	backend := newFakeBackend(chat.State{})
	tests := []struct {
		name   string
		cmd    chat.Name
		result chat.Result
		want   overlayKind
	}{
		{"help", chat.CommandHelp, chat.Result{Commands: chat.Commands()}, overlayHelp},
		{"permissions", chat.CommandPermissions, chat.Result{Permissions: &chat.PermissionView{SandboxMode: "docker"}}, overlayPermissions},
		{"sessions", chat.CommandSessions, chat.Result{Sessions: []chat.Session{{ID: "01A"}}}, overlaySessions},
		{"resume-no-id", chat.CommandResume, chat.Result{Sessions: []chat.Session{{ID: "01A"}}}, overlaySessions},
		{"confirm", chat.CommandNew, chat.Result{Text: "Start a new session?", Confirm: true}, overlayConfirm},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := New(backend, chat.OpenOptions{}, Options{Width: 100, Height: 30})
			command := chat.ParsedCommand{Name: tt.cmd}
			m = updateForTest(t, m, commandResultMsg{command: command, result: tt.result})
			if m.overlay != tt.want {
				t.Fatalf("overlay = %v, want %v", m.overlay, tt.want)
			}
		})
	}
}
