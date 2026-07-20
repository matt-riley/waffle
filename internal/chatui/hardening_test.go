package chatui

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/matt-riley/waffle/internal/chat"
	"github.com/matt-riley/waffle/internal/llm"
)

type asyncBackend struct {
	openState   chat.State
	turn        func(string, func(chat.Event)) error
	command     func(chat.ParsedCommand, func(chat.Event)) (chat.Result, error)
	cancel      func()
	mu          sync.Mutex
	cancelCalls int
	closeCalls  int
}

func (b *asyncBackend) Open(context.Context, chat.OpenOptions) (chat.State, error) {
	return b.openState, nil
}
func (b *asyncBackend) Turn(_ context.Context, input string, emit func(chat.Event)) error {
	return b.turn(input, emit)
}
func (b *asyncBackend) Command(_ context.Context, command chat.ParsedCommand, emit func(chat.Event)) (chat.Result, error) {
	return b.command(command, emit)
}
func (b *asyncBackend) Cancel() {
	b.mu.Lock()
	b.cancelCalls++
	cancel := b.cancel
	b.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}
func (b *asyncBackend) Close(context.Context) error {
	b.mu.Lock()
	b.closeCalls++
	b.mu.Unlock()
	return nil
}

func runCommandAsync(cmd tea.Cmd) <-chan tea.Msg {
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	return done
}

func consumePump(t *testing.T, m *Model, pump tea.Cmd) (*Model, tea.Cmd, tea.Msg) {
	t.Helper()
	result := make(chan tea.Msg, 1)
	go func() { result <- pump() }()
	select {
	case msg := <-result:
		updated, next := m.Update(msg)
		return updated.(*Model), next, msg
	case <-time.After(2 * time.Second):
		t.Fatal("event pump timed out")
		return nil, nil, nil
	}
}

func updateAndRunComponentCmd(t *testing.T, m *Model, msg tea.Msg) *Model {
	t.Helper()
	updated, cmd := m.Update(msg)
	m = updated.(*Model)
	if cmd == nil {
		return m
	}
	return runComponentCmd(t, m, cmd)
}

func runComponentCmd(t *testing.T, m *Model, cmd tea.Cmd) *Model {
	t.Helper()
	result := cmd()
	if result == nil {
		return m
	}
	batch, ok := result.(tea.BatchMsg)
	if !ok {
		updated, _ := m.Update(result)
		return updated.(*Model)
	}
	results := make(chan tea.Msg, len(batch))
	for _, child := range batch {
		go func(command tea.Cmd) { results <- command() }(child)
	}
	deadline := time.After(250 * time.Millisecond)
	for range len(batch) {
		select {
		case childResult := <-results:
			if childResult == nil {
				continue
			}
			if _, relevant := childResult.(list.FilterMatchesMsg); !relevant {
				continue
			}
			updated, _ := m.Update(childResult)
			m = updated.(*Model)
		case <-deadline:
			return m
		}
	}
	return m
}

func expandBatch(t *testing.T, cmd tea.Cmd) []tea.Cmd {
	t.Helper()
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		return []tea.Cmd(batch)
	}
	t.Fatalf("command returned %T, want tea.BatchMsg", msg)
	return nil
}

func TestModelSinglePumpPreservesConcurrentOperationOrderAndRejectsStaleDone(t *testing.T) {
	aSent, releaseTurn := make(chan struct{}), make(chan struct{})
	backend := &asyncBackend{}
	backend.turn = func(_ string, emit func(chat.Event)) error {
		emit(chat.Event{Kind: chat.EventTextDelta, Text: "A"})
		close(aSent)
		<-releaseTurn
		emit(chat.Event{Kind: chat.EventTextDelta, Text: "C"})
		return nil
	}
	backend.command = func(_ chat.ParsedCommand, emit func(chat.Event)) (chat.Result, error) {
		emit(chat.Event{Kind: chat.EventNotice, Text: "B"})
		return chat.Result{Commands: chat.Commands()}, nil
	}
	m := New(backend, chat.OpenOptions{}, Options{Width: 80, Height: 24})
	m.openResolved, m.opened, m.connected = true, true, true
	m.composer.SetValue("turn")
	updated, turnCmd := m.Update(key(tea.KeyEnter))
	m = updated.(*Model)
	turnID := m.activeTurnID
	turnDone := runCommandAsync(turnCmd)
	<-aSent
	pump := m.waitEventCmd()
	m, pump, _ = consumePump(t, m, pump)
	m.composer.SetValue("/help")
	updated, commandCmd := m.Update(key(tea.KeyEnter))
	m = updated.(*Model)
	commandDone := runCommandAsync(commandCmd)
	m, pump, _ = consumePump(t, m, pump)
	m, pump, _ = consumePump(t, m, pump)
	close(releaseTurn)
	m, pump, _ = consumePump(t, m, pump)
	m, pump, _ = consumePump(t, m, pump)
	if got := []string{m.messages[1].text, m.messages[2].text, m.messages[3].text}; strings.Join(got, "") != "ABC" {
		t.Fatalf("operation order = %#v", got)
	}
	select {
	case <-turnDone:
	case <-time.After(time.Second):
		t.Fatal("turn command leaked")
	}
	select {
	case <-commandDone:
	case <-time.After(time.Second):
		t.Fatal("command leaked")
	}
	m.turnActive, m.activeTurnID = true, turnID+100
	m = updateForTest(t, m, eventMsg{operationID: turnID, event: chat.Event{Kind: chat.EventTurnDone}})
	if !m.turnActive {
		t.Fatal("stale turn_done cleared a newer turn")
	}
	m.pumpStopOnce.Do(func() { close(m.pumpStop) })
	_, _, stopped := consumePump(t, m, pump)
	if _, ok := stopped.(pumpStoppedMsg); !ok {
		t.Fatalf("pump stopped with %T", stopped)
	}
}

func TestModelExecutedActiveNewAndResumeConfirmationsDrainTurn(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		wantCalls    int
		secondResult func(chat.ParsedCommand) chat.Result
	}{
		{name: "new", input: "/new", wantCalls: 3, secondResult: func(command chat.ParsedCommand) chat.Result {
			if command.Args == "confirm" {
				state := chat.State{SessionID: "new"}
				return chat.Result{State: &state}
			}
			return chat.Result{Text: "Start a new session?", Confirm: true}
		}},
		{name: "resume", input: "/resume target", wantCalls: 2, secondResult: func(chat.ParsedCommand) chat.Result {
			state := chat.State{SessionID: "target", History: []llm.Message{llm.UserText("resumed")}}
			return chat.Result{State: &state}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			turnStarted, turnRelease := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			calls := 0
			backend := &asyncBackend{}
			backend.cancel = func() { releaseOnce.Do(func() { close(turnRelease) }) }
			backend.turn = func(_ string, _ func(chat.Event)) error { close(turnStarted); <-turnRelease; return nil }
			backend.command = func(command chat.ParsedCommand, _ func(chat.Event)) (chat.Result, error) {
				calls++
				if calls == 1 {
					return chat.Result{Text: "active confirmation", Confirm: true}, nil
				}
				return tt.secondResult(command), nil
			}
			m := New(backend, chat.OpenOptions{}, Options{Width: 80, Height: 24})
			m.openResolved, m.opened, m.connected = true, true, true
			m.composer.SetValue("active turn")
			updated, turnCmd := m.Update(key(tea.KeyEnter))
			m = updated.(*Model)
			turnDone := runCommandAsync(turnCmd)
			<-turnStarted
			m.composer.SetValue(tt.input)
			updated, firstCmd := m.Update(key(tea.KeyEnter))
			m = updated.(*Model)
			firstDone := runCommandAsync(firstCmd)
			pump := m.waitEventCmd()
			m, pump, _ = consumePump(t, m, pump)
			<-firstDone
			updated, ackCmd := m.Update(key(tea.KeyEnter))
			m = updated.(*Model)
			if ackCmd != nil || m.deferredCommand == nil {
				t.Fatalf("ack cmd=%v deferred=%+v", ackCmd != nil, m.deferredCommand)
			}
			m.composer.SetValue("must remain blocked")
			updated, blocked := m.Update(key(tea.KeyEnter))
			m = updated.(*Model)
			if blocked != nil {
				t.Fatal("submission was not blocked while deferred command waited")
			}
			m.composer.SetValue("/help")
			updated, blocked = m.Update(key(tea.KeyEnter))
			m = updated.(*Model)
			if blocked != nil {
				t.Fatal("command was not blocked while deferred command waited")
			}
			m, combined, _ := consumePump(t, m, pump)
			parts := expandBatch(t, combined)
			if len(parts) != 2 {
				t.Fatalf("deferred batch has %d commands", len(parts))
			}
			reissueDone := runCommandAsync(parts[0])
			pump = parts[1]
			m, pump, _ = consumePump(t, m, pump)
			<-reissueDone
			if tt.name == "new" {
				updated, confirmCmd := m.Update(key(tea.KeyEnter))
				m = updated.(*Model)
				confirmDone := runCommandAsync(confirmCmd)
				m, _, _ = consumePump(t, m, pump)
				<-confirmDone
			}
			if m.state.SessionID == "" {
				t.Fatal("session change did not complete")
			}
			if calls != tt.wantCalls {
				t.Fatalf("command calls=%d want=%d", calls, tt.wantCalls)
			}
			select {
			case <-turnDone:
			case <-time.After(time.Second):
				t.Fatal("turn did not drain")
			}
			m.pumpStopOnce.Do(func() { close(m.pumpStop) })
		})
	}
}

func TestModelExecutedCancellationStaysConnectedAndTransportLossDisconnects(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{"direct-context-canceled", context.Canceled},
		{"wire-turn-failed", errors.New("chat service turn_failed: chat turn failed")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			started, released := make(chan struct{}), make(chan struct{})
			var once sync.Once
			backend := &asyncBackend{}
			backend.cancel = func() { once.Do(func() { close(released) }) }
			backend.turn = func(_ string, _ func(chat.Event)) error { close(started); <-released; return tt.err }
			backend.command = func(chat.ParsedCommand, func(chat.Event)) (chat.Result, error) { return chat.Result{}, nil }
			m := New(backend, chat.OpenOptions{}, Options{})
			m.openResolved, m.opened, m.connected = true, true, true
			m.composer.SetValue("cancel me")
			updated, command := m.Update(key(tea.KeyEnter))
			m = updated.(*Model)
			done := runCommandAsync(command)
			<-started
			m = updateForTest(t, m, key(tea.KeyEscape))
			m, _, _ = consumePump(t, m, m.waitEventCmd())
			<-done
			if !m.connected || m.awaitingAck || m.turnActive {
				t.Fatalf("cancel disconnected: connected=%v ack=%v active=%v err=%v", m.connected, m.awaitingAck, m.turnActive, m.err)
			}
			m.pumpStopOnce.Do(func() { close(m.pumpStop) })
		})
	}

	m := New(newFakeBackend(chat.State{}), chat.OpenOptions{}, Options{})
	m.openResolved, m.opened, m.connected, m.turnActive, m.activeTurnID = true, true, true, true, 9
	m.state.ConnectionMode = "unix"
	m = updateForTest(t, m, turnDoneMsg{operationID: 9, err: errors.New("chat service disconnected")})
	if m.connected || !m.awaitingAck {
		t.Fatalf("transport loss did not disconnect: connected=%v ack=%v", m.connected, m.awaitingAck)
	}
	m = New(newFakeBackend(chat.State{}), chat.OpenOptions{}, Options{})
	m.openResolved, m.opened, m.connected, m.turnActive, m.activeTurnID = true, true, true, true, 10
	m = updateForTest(t, m, turnDoneMsg{operationID: 10, err: errors.New("provider failed")})
	if !m.connected || m.awaitingAck {
		t.Fatalf("ordinary turn failure disconnected: connected=%v ack=%v", m.connected, m.awaitingAck)
	}
	m = New(newFakeBackend(chat.State{}), chat.OpenOptions{}, Options{})
	m.openResolved, m.opened, m.connected, m.turnActive, m.activeTurnID = true, true, true, true, 11
	m = updateForTest(t, m, turnDoneMsg{operationID: 11, err: errors.New("provider connection reset by peer")})
	if !m.connected || m.awaitingAck {
		t.Fatal("direct provider error masqueraded as backend transport loss")
	}
}

func TestModelCommandTransportLossUsesConnectionMode(t *testing.T) {
	m := New(newFakeBackend(chat.State{}), chat.OpenOptions{}, Options{})
	m.openResolved, m.opened, m.connected = true, true, true
	m.state.ConnectionMode = "unix"
	m = updateForTest(t, m, commandResultMsg{err: errors.New("chat service disconnected")})
	if m.connected || !m.awaitingAck {
		t.Fatal("Unix command transport loss did not disconnect")
	}
	m = New(newFakeBackend(chat.State{}), chat.OpenOptions{}, Options{})
	m.openResolved, m.opened, m.connected = true, true, true
	m = updateForTest(t, m, commandResultMsg{err: errors.New("provider connection reset by peer")})
	if !m.connected || m.awaitingAck {
		t.Fatal("direct command provider error disconnected backend")
	}
}

func TestModelGatesSubmissionUntilOpenAndIgnoresLateOpen(t *testing.T) {
	backend := newFakeBackend(chat.State{SessionID: "late"})
	m := New(backend, chat.OpenOptions{}, Options{Width: 80, Height: 24})
	m.composer.SetValue("must not send")
	updated, cmd := m.Update(key(tea.KeyEnter))
	m = updated.(*Model)
	if cmd != nil || m.composer.Value() != "must not send" {
		t.Fatalf("opening submit cmd=%v composer=%q", cmd != nil, m.composer.Value())
	}
	m.disconnect(assertionError("socket failed"))
	m = updateForTest(t, m, openMsg{state: chat.State{SessionID: "late"}})
	if m.connected || !m.awaitingAck || m.state.SessionID == "late" {
		t.Fatalf("late Open overwrote failure: connected=%v ack=%v state=%+v", m.connected, m.awaitingAck, m.state)
	}
}

func TestModelOpenFailureRetainsBackendConnectionMode(t *testing.T) {
	m := New(newFakeBackend(chat.State{}), chat.OpenOptions{}, Options{Width: 80, Height: 24})
	m = updateForTest(t, m, openMsg{
		state: chat.State{ConnectionMode: "unix"},
		err:   assertionError("socket unavailable"),
	})
	if m.state.ConnectionMode != "unix" {
		t.Fatalf("connection mode=%q want unix", m.state.ConnectionMode)
	}
	if !m.awaitingAck || m.connected {
		t.Fatalf("failure state connected=%v awaitingAck=%v", m.connected, m.awaitingAck)
	}
}

func TestModelActiveConfirmationCancelsWaitsAndReissues(t *testing.T) {
	backend := newFakeBackend(chat.State{})
	m := New(backend, chat.OpenOptions{}, Options{Width: 80, Height: 24})
	m.opened, m.connected, m.turnActive = true, true, true
	command := chat.ParsedCommand{Name: chat.CommandNew}
	m.applyResult(command, chat.Result{Text: "wait for active turn", Confirm: true})
	updated, cmd := m.Update(key(tea.KeyEnter))
	m = updated.(*Model)
	if cmd != nil || backend.cancelCalls != 1 {
		t.Fatalf("ack cmd=%v cancels=%d", cmd != nil, backend.cancelCalls)
	}
	_, cmd = m.Update(turnDoneMsg{})
	if cmd == nil {
		t.Fatal("turn completion did not reissue /new")
	}
}

func TestModelSessionChangingStateRebuildsSessionScopedUI(t *testing.T) {
	m := New(newFakeBackend(chat.State{}), chat.OpenOptions{}, Options{})
	m.state = chat.State{SessionID: "old"}
	m.messages = []messageCard{{role: roleUser, text: "old"}}
	m.tools = []toolRow{{name: "old tool"}}
	m.inputTokens, m.outputTokens = 12, 7
	state := chat.State{SessionID: "new", History: []llm.Message{llm.UserText("new history")}}
	m.applyResult(chat.ParsedCommand{Name: chat.CommandResume, Args: "new"}, chat.Result{State: &state})
	if len(m.messages) != 1 || m.messages[0].text != "new history" || len(m.tools) != 0 || m.inputTokens != 0 || m.outputTokens != 0 {
		t.Fatalf("session UI not rebuilt: messages=%+v tools=%+v usage=%d/%d", m.messages, m.tools, m.inputTokens, m.outputTokens)
	}
}

func TestModelViewNeverExceedsTerminalAndViewportStillNavigates(t *testing.T) {
	m := New(newFakeBackend(chat.State{}), chat.OpenOptions{}, Options{Width: 58, Height: 24, NoColor: true})
	m.opened, m.connected = true, true
	for range 60 {
		m.messages = append(m.messages, messageCard{role: roleNotice, text: "a transcript row"})
	}
	m.composer.SetValue("one\ntwo\nthree\nfour\nfive")
	m.palette, m.paletteVisible = chat.Commands(), true
	m.setOverlay(overlayHelp, commandItems(chat.Commands()))
	m.syncViewport(true)
	view := m.View().Content
	if got := strings.Count(view, "\n") + 1; got > 24 {
		t.Fatalf("view height=%d want <=24\n%s", got, view)
	}
	m.overlay, m.paletteVisible = overlayNone, false
	_ = m.composer.Focus()
	m.syncLayout()
	m.viewport.GotoTop()
	m = updateForTest(t, m, key(tea.KeyPgDown))
	if m.viewport.YOffset() == 0 || !m.composer.Focused() {
		t.Fatalf("viewport navigation offset=%d focus=%v", m.viewport.YOffset(), m.composer.Focused())
	}
	beforeHeight, beforeOffset := m.viewport.Height(), m.viewport.YOffset()
	_ = m.View()
	_ = m.View()
	if m.viewport.Height() != beforeHeight || m.viewport.YOffset() != beforeOffset {
		t.Fatal("View mutated viewport layout state")
	}
}

func TestModelSanitizesDynamicTerminalContent(t *testing.T) {
	canary := "safe\x1b]52;c;SECRET-CANARY\x07\x1b[31mRED\x1b[0m\x00"
	state := chat.State{Title: canary, SessionID: canary, ModelAlias: canary, Profile: canary}
	m := New(newFakeBackend(state), chat.OpenOptions{}, Options{Width: 100, Height: 30, NoColor: true})
	m.state, m.opened, m.connected = state, true, true
	m.overlay = overlayNone
	m = updateAndRunComponentCmd(t, m, tea.PasteMsg{Content: canary})
	if strings.Contains(m.composer.Value(), "SECRET-CANARY") || strings.Contains(m.composer.Value(), "\x1b") {
		t.Fatalf("composer paste was not sanitized: %q", m.composer.Value())
	}
	m.messages = []messageCard{{role: roleError, text: canary + "\nsecond"}}
	m.tools = []toolRow{{name: canary, messageIndex: 0, done: true}}
	m.overlay = overlaySessions
	m.overlayResult = chat.Result{Sessions: []chat.Session{{ID: canary, Title: canary, Summary: canary, ModelAlias: canary, UpdatedAt: time.Unix(0, 0).UTC()}}}
	m.setOverlay(overlaySessions, sessionItems(m.overlayResult.Sessions))
	m.syncViewport(true)
	got := m.View().Content
	for _, forbidden := range []string{"\x1b", "SECRET-CANARY", "]52", "\x00"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("render contains %q: %q", forbidden, got)
		}
	}
}

func TestOverlayMetadataFilteringAndThemeRoleDistinction(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	updated := time.Date(2026, time.July, 20, 12, 0, 0, 0, time.UTC)
	items := sessionItems([]chat.Session{{ID: "01A", Title: "Deploy", Summary: "failed rollout", ModelAlias: "gpt", UpdatedAt: updated}})
	detail := items[0].(overlayItem).detail
	for _, want := range []string{"failed rollout", "2026-07-20", "gpt"} {
		if !strings.Contains(detail, want) {
			t.Errorf("session detail missing %q: %q", want, detail)
		}
	}
	models := modelItems([]chat.Model{{Alias: "gpt", Current: true}})
	if !strings.Contains(models[0].(overlayItem).title, "✓") {
		t.Fatalf("current model not marked: %+v", models[0])
	}

	m := New(newFakeBackend(chat.State{}), chat.OpenOptions{}, Options{Width: 80, Height: 24})
	m.opened, m.connected = true, true
	m.messages = []messageCard{{role: roleUser, text: "same"}, {role: roleAssistant, text: "same"}}
	colored := m.renderTranscript()
	if !strings.Contains(colored, "\x1b[") || ansi.Strip(colored) == colored {
		t.Fatalf("roles are not meaningfully colored: %q", colored)
	}
	if m.theme.roleText(roleUser, "role") == m.theme.roleText(roleAssistant, "role") {
		t.Fatal("user and assistant role styles are identical")
	}
	view := m.View()
	if view.BackgroundColor == nil {
		t.Fatal("colored conversation has no surface background")
	}
}

func TestOverlaySupportsSearchNavigationAndVerticalCentering(t *testing.T) {
	m := New(newFakeBackend(chat.State{}), chat.OpenOptions{}, Options{Width: 58, Height: 24, NoColor: true})
	m.openResolved, m.opened, m.connected = true, true, true
	m.applyResult(chat.ParsedCommand{Name: chat.CommandHelp}, chat.Result{Commands: chat.Commands()})
	if !m.overlayList.FilteringEnabled() {
		t.Fatal("help overlay filtering is disabled")
	}
	start := m.overlayList.Index()
	m = updateForTest(t, m, key(tea.KeyDown))
	if m.overlayList.Index() == start {
		t.Fatal("overlay down key did not navigate")
	}
	m = updateForTest(t, m, tea.KeyPressMsg{Code: '/', Text: "/"})
	if m.overlayList.FilterState() != list.Filtering {
		t.Fatalf("filter state=%v", m.overlayList.FilterState())
	}
	for _, r := range "permissions" {
		m = updateForTest(t, m, tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	if got := m.overlayList.FilterInput.Value(); got != "permissions" {
		t.Fatalf("filter input=%q", got)
	}
	m.overlayList.SetFilterText(m.overlayList.FilterInput.Value())
	if len(m.overlayList.VisibleItems()) != 1 {
		t.Fatalf("filtered items=%d", len(m.overlayList.VisibleItems()))
	}
	if rendered := m.renderOverlay(54); !strings.Contains(rendered, "/permissions") {
		t.Fatalf("late filtered help item is not visible:\n%s", rendered)
	}
	m.applyResult(chat.ParsedCommand{Name: chat.CommandHelp}, chat.Result{Commands: chat.Commands()})
	m = updateForTest(t, m, key(tea.KeyEnd))
	if rendered := m.renderOverlay(54); !strings.Contains(rendered, "/workset") {
		t.Fatalf("last help command is unreachable:\n%s", rendered)
	}
	body := overlayBody(strings.Repeat("transcript\n", 20), m.renderOverlay(76), 18)
	lines := strings.Split(body, "\n")
	firstBox := -1
	for i, line := range lines {
		if strings.Contains(line, "┌") {
			firstBox = i
			break
		}
	}
	boxHeight := 0
	for _, line := range lines {
		if strings.Contains(line, "│") || strings.Contains(line, "┌") || strings.Contains(line, "└") {
			boxHeight++
		}
	}
	wantStart := (18 - boxHeight) / 2
	if firstBox != wantStart {
		t.Fatalf("overlay is not vertically centered: first box row=%d\n%s", firstBox, body)
	}
}

type assertionError string

func (e assertionError) Error() string { return string(e) }
