package chatui

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"charm.land/bubbles/v2/list"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"github.com/matt-riley/waffle/internal/chat"
)

// Update applies all component and backend state transitions.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case openMsg:
		if m.openResolved || m.awaitingAck || m.quitting {
			return m, nil
		}
		m.openResolved = true
		m.opened = msg.err == nil
		m.connected = msg.err == nil
		m.state = msg.state
		if msg.err != nil {
			m.disconnect(msg.err)
			return m, nil
		}
		m.messages = cardsFromHistory(msg.state.History)
		m.syncViewport(true)
		return m, nil
	case tea.BackgroundColorMsg:
		if !m.theme.noColor {
			dark := msg.IsDark()
			m.theme = newTheme(dark, false)
			m.composer.SetStyles(textarea.DefaultStyles(dark))
			items, selected := m.overlayList.Items(), m.overlayList.Index()
			m.overlayList = newOverlayList(items, m.overlayList.Width(), m.overlayList.Height(), dark, false)
			m.overlayList.Select(selected)
		}
		return m, nil
	case tea.WindowSizeMsg:
		m.resize(msg.Width, msg.Height)
		return m, nil
	case eventMsg:
		m.applyEvent(msg.operationID, msg.event)
		return m, m.waitEventCmd()
	case turnDoneMsg:
		matches := msg.operationID == 0 || msg.operationID == m.activeTurnID
		requestedCancel := matches && m.canceledTurnID != 0 && msg.operationID == m.canceledTurnID
		if matches {
			m.turnActive = false
			m.activeTurnID = 0
			m.canceledTurnID = 0
			m.turnTerminalSeen = false
		}
		if msg.err != nil && matches {
			switch {
			case m.state.ConnectionMode == "unix" && !connectionUsable(msg.err):
				m.disconnect(msg.err)
			case requestedCancel && isExpectedCancellation(msg.err):
				m.messages = append(m.messages, messageCard{role: roleNotice, text: "Turn cancelled."})
			default:
				m.messages = append(m.messages, messageCard{role: roleError, text: "Turn failed: " + msg.err.Error()})
			}
		}
		m.syncViewport(true)
		var next tea.Cmd
		if matches && m.deferredCommand != nil && !m.awaitingAck {
			command := *m.deferredCommand
			m.deferredCommand = nil
			next = m.startCommand(command, false)
		}
		return m, m.continuePump(next)
	case commandResultMsg:
		requestedCancel := m.commandCancelRequested
		m.commandActive = false
		m.commandCancelRequested = false
		m.stateChangeActive = false
		if msg.err != nil {
			m.pendingConfirm = chat.ParsedCommand{}
			if m.state.ConnectionMode == "unix" && !connectionUsable(msg.err) {
				m.disconnect(msg.err)
			} else if requestedCancel && isExpectedCommandCancellation(msg.err) {
				m.messages = append(m.messages, messageCard{role: roleNotice, text: "Command cancelled."})
			} else {
				m.messages = append(m.messages, messageCard{role: roleError, text: msg.err.Error()})
			}
			m.syncViewport(true)
			if m.quitting {
				return m, m.continuePump(m.closeCmd())
			}
			return m, m.continuePump(nil)
		}
		m.applyResult(msg.command, msg.result, msg.startedDuringTurn)
		if msg.result.ShouldClose || m.quitting {
			m.quitting = true
			return m, m.continuePump(m.closeCmd())
		}
		return m, m.continuePump(nil)
	case closeDoneMsg:
		if msg.err != nil && m.err == nil {
			m.err = msg.err
		}
		return m, tea.Quit
	case pumpStoppedMsg:
		return m, nil
	case tea.KeyPressMsg:
		return m.handleKey(msg)
	case tea.MouseMsg:
		updated, cmd := m.viewport.Update(msg)
		m.viewport = updated
		if m.overlay == overlayNone {
			_ = m.composer.Focus()
		}
		return m, cmd
	case tea.PasteMsg:
		if m.overlay != overlayNone {
			updated, cmd := m.overlayList.Update(tea.PasteMsg{Content: sanitizeMultiline(msg.Content)})
			m.overlayList = updated
			return m, cmd
		}
		updated, cmd := m.composer.Update(tea.PasteMsg{Content: sanitizeMultiline(msg.Content)})
		m.composer = updated
		m.refreshPalette()
		m.syncLayout()
		return m, cmd
	}

	if m.overlay == overlayNone {
		updated, cmd := m.composer.Update(msg)
		m.composer = updated
		return m, cmd
	}
	return m, nil
}

func (m *Model) continuePump(command tea.Cmd) tea.Cmd {
	if m.quitting {
		return command
	}
	return tea.Batch(command, m.waitEventCmd())
}

func (m *Model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.awaitingAck {
		switch msg.Key().Code {
		case tea.KeyEnter, tea.KeyEscape:
			m.quitting = true
			return m, m.closeCmd()
		}
		if msg.Key().Mod.Contains(tea.ModCtrl) && msg.Key().Code == 'c' {
			m.quitting = true
			return m, m.closeCmd()
		}
		return m, nil
	}

	key := msg.Key()
	if m.overlay == overlayNone && (key.Code == tea.KeyPgUp || key.Code == tea.KeyPgDown || key.Mod.Contains(tea.ModCtrl) && (key.Code == tea.KeyUp || key.Code == tea.KeyDown)) {
		updated, cmd := m.viewport.Update(msg)
		m.viewport = updated
		_ = m.composer.Focus()
		return m, cmd
	}
	if key.Mod.Contains(tea.ModCtrl) && key.Code == 'c' {
		if m.turnActive || m.commandActive {
			if m.turnActive {
				m.markTurnCancelRequested()
			}
			m.commandCancelRequested = m.commandActive
			m.backend.Cancel()
			return m, nil
		}
		if m.exitArmed {
			m.quitting = true
			return m, m.closeCmd()
		}
		m.exitArmed = true
		m.messages = append(m.messages, messageCard{role: roleNotice, text: "Press Ctrl+C again to exit."})
		m.syncViewport(true)
		return m, nil
	}
	if key.Mod.Contains(tea.ModCtrl) && key.Code == 'd' {
		if m.composer.Value() == "" && m.overlay == overlayNone {
			m.quitting = true
			return m, m.closeCmd()
		}
		return m, nil
	}
	if key.Code == tea.KeyEscape {
		if m.turnActive || m.commandActive {
			if m.turnActive {
				m.markTurnCancelRequested()
			}
			m.commandCancelRequested = m.commandActive
			m.backend.Cancel()
			return m, nil
		}
		if m.overlay != overlayNone || m.paletteVisible {
			m.overlay, m.paletteVisible = overlayNone, false
			_ = m.composer.Focus()
			m.syncLayout()
			return m, nil
		}
	}
	if m.paletteVisible {
		switch key.Code {
		case tea.KeyUp:
			m.paletteIndex = (m.paletteIndex - 1 + len(m.palette)) % len(m.palette)
			return m, nil
		case tea.KeyDown:
			m.paletteIndex = (m.paletteIndex + 1) % len(m.palette)
			return m, nil
		}
	}

	if m.overlay != overlayNone {
		return m.handleOverlayKey(msg)
	}

	if key.Code == tea.KeyTab {
		m.refreshPalette()
		if len(m.palette) > 0 {
			usage := m.palette[m.paletteIndex].Usage
			name, _, _ := strings.Cut(usage, " ")
			m.composer.SetValue(name)
			m.refreshPalette()
		}
		m.syncLayout()
		return m, nil
	}
	if key.Mod.Contains(tea.ModAlt) && key.Code == tea.KeyEnter {
		m.composer.InsertString("\n")
		m.syncLayout()
		return m, nil
	}
	if key.Code == tea.KeyEnter {
		return m.submit()
	}

	m.exitArmed = false
	updated, cmd := m.composer.Update(msg)
	m.composer = updated
	m.refreshPalette()
	m.syncLayout()
	return m, cmd
}

func (m *Model) refreshPalette() {
	value := strings.TrimSpace(m.composer.Value())
	if !strings.HasPrefix(value, "/") || strings.ContainsAny(value, " \t\n") {
		m.palette = nil
		m.paletteVisible = false
		m.paletteIndex = 0
		return
	}
	m.palette = chat.Complete(value)
	m.paletteVisible = len(m.palette) > 0
	if m.paletteIndex >= len(m.palette) {
		m.paletteIndex = 0
	}
}

func (m *Model) submit() (tea.Model, tea.Cmd) {
	input := strings.TrimSpace(m.composer.Value())
	if input == "" {
		return m, nil
	}
	if !m.openResolved || !m.opened || !m.connected {
		return m, nil
	}

	command, ok, err := chat.ParseInput(input)
	if err != nil {
		m.messages = append(m.messages, messageCard{role: roleError, text: err.Error()})
		m.syncViewport(true)
		return m, nil
	}
	if (m.deferredCommand != nil || m.stateChangeActive || m.commandActive) && (!ok || command.Name != chat.CommandExit) {
		return m, nil
	}
	if m.turnActive && !ok {
		return m, nil
	}
	m.composer.Reset()
	m.paletteVisible = false
	m.exitArmed = false
	if ok {
		if command.Name == chat.CommandExit && m.turnActive && !m.turnTerminalSeen {
			m.canceledTurnID = m.activeTurnID
		}
		return m, m.startCommand(command, m.turnActive)
	}

	m.messages = append(m.messages, messageCard{role: roleUser, text: input})
	m.turnActive = true
	operationID := m.nextOperation()
	m.activeTurnID = operationID
	m.turnTerminalSeen = false
	m.syncViewport(true)
	return m, m.turnCmd(operationID, input)
}

func (m *Model) startCommand(command chat.ParsedCommand, startedDuringTurn bool) tea.Cmd {
	if command.Name == chat.CommandSkill || command.Name == chat.CommandRepo || command.Name == chat.CommandResume && command.Args != "" || command.Name == chat.CommandNew && command.Args == "confirm" {
		m.commandActive = true
		m.commandCancelRequested = false
	}
	return m.commandCmd(m.nextOperation(), command, startedDuringTurn)
}

func (m *Model) handleOverlayKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.Key()
	if key.Code == tea.KeyEnter && m.overlayList.FilterState() != list.Filtering {
		if m.overlay == overlayConfirm && m.confirmNeedsTurnDrain {
			command := m.pendingConfirm
			if command.Name == chat.CommandNew && command.Args == "" {
				command.Args = "confirm"
				m.stateChangeActive = true
				m.confirmNeedsTurnDrain = false
				m.overlay = overlayNone
				_ = m.composer.Focus()
				return m, m.startCommand(command, true)
			}
			m.confirmNeedsTurnDrain = false
			m.overlay = overlayNone
			_ = m.composer.Focus()
			if m.turnActive {
				m.markTurnCancelRequested()
				m.backend.Cancel()
				m.deferredCommand = &command
				return m, nil
			}
			return m, m.startCommand(command, false)
		}
		command, ok := m.overlaySelection()
		if !ok {
			m.overlay = overlayNone
			_ = m.composer.Focus()
			return m, nil
		}
		m.overlay = overlayNone
		_ = m.composer.Focus()
		return m, m.startCommand(command, m.turnActive)
	}
	updated, cmd := m.overlayList.Update(msg)
	m.overlayList = updated
	return m, cmd
}

func connectionUsable(err error) bool {
	if err == nil {
		return true
	}
	if semantic, ok := err.(interface{ ConnectionUsable() bool }); ok {
		return semantic.ConnectionUsable()
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !connectionUsable(cause) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return connectionUsable(wrapped.Unwrap())
	}
	return false
}

func isExpectedCancellation(err error) bool {
	if errors.Is(err, context.Canceled) {
		return true
	}
	var coded interface{ ErrorCode() string }
	return errors.As(err, &coded) && coded.ErrorCode() == "turn_failed"
}

func isExpectedCommandCancellation(err error) bool {
	if errors.Is(err, context.Canceled) {
		return true
	}
	var coded interface{ ErrorCode() string }
	return errors.As(err, &coded) && coded.ErrorCode() == "command_failed"
}

func (m *Model) markTurnCancelRequested() {
	if !m.turnTerminalSeen {
		m.canceledTurnID = m.activeTurnID
	}
}

func (m *Model) overlaySelection() (chat.ParsedCommand, bool) {
	if m.overlay == overlayConfirm {
		command := m.pendingConfirm
		if command.Name == chat.CommandNew && command.Args == "" {
			command.Args = "confirm"
		}
		return command, command.Name != ""
	}
	item, ok := m.overlayList.SelectedItem().(overlayItem)
	if !ok {
		return chat.ParsedCommand{}, false
	}
	switch m.overlay {
	case overlayModels:
		return chat.ParsedCommand{Name: chat.CommandModel, Args: item.value}, true
	case overlaySessions:
		return chat.ParsedCommand{Name: chat.CommandResume, Args: item.value}, true
	default:
		return chat.ParsedCommand{}, false
	}
}

func (m *Model) applyEvent(operationID uint64, event chat.Event) {
	switch event.Kind {
	case chat.EventTextDelta:
		if len(m.messages) == 0 || m.messages[len(m.messages)-1].role != roleAssistant {
			m.messages = append(m.messages, messageCard{role: roleAssistant})
		}
		m.messages[len(m.messages)-1].text += event.Text
	case chat.EventToolStarted:
		m.tools = append(m.tools, toolRow{name: event.ToolName, messageIndex: m.currentAssistantIndex()})
	case chat.EventToolFinished:
		matched := false
		for i := len(m.tools) - 1; i >= 0; i-- {
			if m.tools[i].name == event.ToolName && !m.tools[i].done {
				m.tools[i].done, m.tools[i].failed, m.tools[i].byteCount = true, event.IsError, event.ByteCount
				matched = true
				break
			}
		}
		if !matched {
			m.tools = append(m.tools, toolRow{name: event.ToolName, messageIndex: m.currentAssistantIndex(), done: true, failed: event.IsError, byteCount: event.ByteCount})
		}
	case chat.EventNotice:
		role := roleNotice
		if event.IsError {
			role = roleError
		}
		m.messages = append(m.messages, messageCard{role: role, text: event.Text})
	case chat.EventState:
		if event.State != nil {
			m.state = *event.State
		}
	case chat.EventTurnDone:
		if operationID == 0 || operationID == m.activeTurnID {
			m.turnTerminalSeen = true
		}
		m.inputTokens += event.Usage.InputTokens
		m.outputTokens += event.Usage.OutputTokens
	}
	m.syncViewport(true)
}

func (m *Model) currentAssistantIndex() int {
	if len(m.messages) > 0 && m.messages[len(m.messages)-1].role == roleAssistant {
		return len(m.messages) - 1
	}
	return len(m.messages)
}

func (m *Model) applyResult(command chat.ParsedCommand, result chat.Result, startedDuringTurn ...bool) {
	wasDuringTurn := m.turnActive
	if len(startedDuringTurn) > 0 {
		wasDuringTurn = startedDuringTurn[0]
	}
	if result.State != nil {
		m.state = *result.State
		if command.Name == chat.CommandNew || command.Name == chat.CommandResume {
			m.messages = cardsFromHistory(result.State.History)
			m.tools = nil
			m.inputTokens, m.outputTokens = 0, 0
			m.turnActive, m.activeTurnID = false, 0
			m.turnTerminalSeen = false
			m.deferredCommand = nil
		}
	}
	if result.Text != "" {
		m.messages = append(m.messages, messageCard{role: roleNotice, text: result.Text})
	}
	m.overlayResult = result
	m.pendingConfirm = chat.ParsedCommand{}
	switch {
	case result.Confirm:
		m.pendingConfirm = command
		m.confirmNeedsTurnDrain = wasDuringTurn
		m.setOverlay(overlayConfirm, nil)
	case command.Name == chat.CommandHelp:
		commands := result.Commands
		if len(commands) == 0 {
			commands = chat.Commands()
		}
		m.setOverlay(overlayHelp, helpItems(commands))
	case command.Name == chat.CommandModels || command.Name == chat.CommandModel && command.Args == "":
		m.setOverlay(overlayModels, modelItems(result.Models))
	case command.Name == chat.CommandSessions || command.Name == chat.CommandResume && command.Args == "":
		m.setOverlay(overlaySessions, sessionItems(result.Sessions))
	case command.Name == chat.CommandPermissions:
		m.setOverlay(overlayPermissions, nil)
	default:
		m.overlay = overlayNone
		_ = m.composer.Focus()
	}
	m.syncViewport(true)
}

func (m *Model) setOverlay(kind overlayKind, items []list.Item) {
	m.overlay = kind
	m.overlayList.ResetFilter()
	m.overlayList.SetItems(items)
	m.overlayList.Select(0)
	m.composer.Blur()
}

func commandItems(commands []chat.Command) []list.Item {
	items := make([]list.Item, 0, len(commands))
	for _, command := range commands {
		items = append(items, overlayItem{title: sanitizeLine(command.Usage), detail: sanitizeLine(command.Description)})
	}
	return items
}

func helpItems(commands []chat.Command) []list.Item {
	items := commandItems(commands)
	keys := []overlayItem{
		{title: "Enter", detail: "send composer input or select an overlay item"},
		{title: "Ctrl+C", detail: "cancel active turn; press twice when idle to exit"},
		{title: "Ctrl+D", detail: "exit when the composer is empty"},
		{title: "Escape", detail: "cancel active turn or close the current overlay"},
		{title: "Alt+Enter", detail: "insert a composer newline"},
		{title: "Tab", detail: "complete the selected slash command"},
		{title: "Up/Down", detail: "navigate command choices and overlays"},
		{title: "PageUp/PageDown", detail: "scroll the conversation or overlay"},
	}
	for _, key := range keys {
		key.title = sanitizeLine(key.title)
		key.detail = sanitizeLine(key.detail)
		items = append(items, key)
	}
	return items
}
func modelItems(models []chat.Model) []list.Item {
	items := make([]list.Item, 0, len(models))
	for _, model := range models {
		title := sanitizeLine(model.Alias)
		if model.Current {
			title = "✓ " + title
		}
		items = append(items, overlayItem{title: title, detail: sanitizeLine(model.Provider) + " · " + sanitizeLine(model.Upstream), value: model.Alias})
	}
	return items
}
func sessionItems(sessions []chat.Session) []list.Item {
	items := make([]list.Item, 0, len(sessions))
	for _, session := range sessions {
		title := sanitizeLine(session.Title)
		if title == "" {
			title = sanitizeLine(session.ID)
		}
		detail := sanitizeLine(session.ID) + " · " + sanitizeLine(session.ModelAlias)
		if summary := sanitizeLine(session.Summary); summary != "" {
			detail += " · " + summary
		}
		if !session.UpdatedAt.IsZero() {
			detail += " · " + session.UpdatedAt.UTC().Format("2006-01-02 15:04Z")
		}
		items = append(items, overlayItem{title: title, detail: detail, value: session.ID})
	}
	return items
}

func (m *Model) disconnect(err error) {
	m.connected = false
	m.awaitingAck = true
	m.turnActive = false
	m.err = err
	m.messages = append(m.messages, messageCard{role: roleError, text: fmt.Sprintf("Connection lost: %v\nPress Enter to close.", err)})
	m.syncViewport(true)
}

func (m *Model) resize(width, height int) {
	if width > 0 {
		m.width = width
	}
	if height > 0 {
		m.height = height
	}
	wasBottom := m.viewport.AtBottom()
	offset := m.viewport.YOffset()
	contentWidth := max(20, m.width-4)
	m.viewport.SetWidth(contentWidth)
	m.composer.SetWidth(max(12, contentWidth-4))
	m.syncLayout()
	m.syncViewport(wasBottom)
	if !wasBottom {
		m.viewport.SetYOffset(offset)
	}
	if m.overlay == overlayNone {
		_ = m.composer.Focus()
	}
}

func (m *Model) syncLayout() {
	headerHeight := strings.Count(m.renderHeader(max(20, m.width-4)), "\n") + 1
	composerHeight := strings.Count(m.renderComposer(max(20, m.width-4)), "\n") + 1
	paletteHeight := 0
	if m.paletteVisible {
		paletteHeight = 1
	}
	fixed := headerHeight + 1 + composerHeight + 1 + paletteHeight
	m.viewport.SetHeight(max(1, m.height-fixed))
	m.viewport.YPosition = headerHeight + 1
	m.overlayList.SetSize(
		min(max(20, m.width-12), 76),
		min(12, max(4, m.viewport.Height()-4)),
	)
}

func (m *Model) syncViewport(follow bool) {
	m.syncLayout()
	m.viewport.SetContent(m.renderTranscript())
	if follow {
		m.viewport.GotoBottom()
	}
}
