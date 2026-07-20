package chatui

import (
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
		m.opened = msg.err == nil
		m.connected = msg.err == nil
		if msg.err != nil {
			m.disconnect(msg.err)
			return m, nil
		}
		m.state = msg.state
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
		m.applyEvent(msg.event)
		return m, m.waitEventCmd()
	case turnDoneMsg:
		m.turnActive = false
		if msg.err != nil {
			m.disconnect(msg.err)
		}
		m.syncViewport(true)
		return m, nil
	case commandResultMsg:
		if msg.err != nil {
			m.pendingConfirm = chat.ParsedCommand{}
			m.messages = append(m.messages, messageCard{role: roleError, text: msg.err.Error()})
			m.syncViewport(true)
			if m.quitting {
				return m, m.closeCmd()
			}
			return m, nil
		}
		m.applyResult(msg.command, msg.result)
		if msg.result.ShouldClose || m.quitting {
			m.quitting = true
			return m, m.closeCmd()
		}
		return m, nil
	case closeDoneMsg:
		if msg.err != nil && m.err == nil {
			m.err = msg.err
		}
		return m, tea.Quit
	case tea.KeyPressMsg:
		return m.handleKey(msg)
	}

	if m.overlay == overlayNone {
		updated, cmd := m.composer.Update(msg)
		m.composer = updated
		return m, cmd
	}
	return m, nil
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
	if key.Mod.Contains(tea.ModCtrl) && key.Code == 'c' {
		if m.turnActive {
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
		if m.turnActive {
			m.backend.Cancel()
			return m, nil
		}
		if m.overlay != overlayNone || m.paletteVisible {
			m.overlay, m.paletteVisible = overlayNone, false
			_ = m.composer.Focus()
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
		return m, nil
	}
	if key.Mod.Contains(tea.ModAlt) && key.Code == tea.KeyEnter {
		m.composer.InsertString("\n")
		return m, nil
	}
	if key.Code == tea.KeyEnter {
		return m.submit()
	}

	m.exitArmed = false
	updated, cmd := m.composer.Update(msg)
	m.composer = updated
	m.refreshPalette()
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

	command, ok, err := chat.ParseInput(input)
	if err != nil {
		m.messages = append(m.messages, messageCard{role: roleError, text: err.Error()})
		m.syncViewport(true)
		return m, nil
	}
	if m.turnActive && !ok {
		return m, nil
	}
	m.composer.Reset()
	m.paletteVisible = false
	m.exitArmed = false
	if ok {
		if command.Name == chat.CommandExit {
			m.quitting = true
		}
		return m, tea.Batch(m.commandCmd(command), m.waitEventCmd())
	}

	m.messages = append(m.messages, messageCard{role: roleUser, text: input})
	m.turnActive = true
	m.syncViewport(true)
	return m, tea.Batch(m.turnCmd(input), m.waitEventCmd())
}

func (m *Model) handleOverlayKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.Key()
	switch key.Code {
	case tea.KeyUp:
		m.overlayList.CursorUp()
		return m, nil
	case tea.KeyDown:
		m.overlayList.CursorDown()
		return m, nil
	case tea.KeyEnter:
		command, ok := m.overlaySelection()
		if !ok {
			m.overlay = overlayNone
			_ = m.composer.Focus()
			return m, nil
		}
		m.overlay = overlayNone
		_ = m.composer.Focus()
		return m, tea.Batch(m.commandCmd(command), m.waitEventCmd())
	}
	return m, nil
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

func (m *Model) applyEvent(event chat.Event) {
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
		m.turnActive = false
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

func (m *Model) applyResult(command chat.ParsedCommand, result chat.Result) {
	if result.State != nil {
		m.state = *result.State
	}
	if result.Text != "" {
		m.messages = append(m.messages, messageCard{role: roleNotice, text: result.Text})
	}
	m.overlayResult = result
	m.pendingConfirm = chat.ParsedCommand{}
	switch {
	case result.Confirm:
		m.pendingConfirm = command
		m.setOverlay(overlayConfirm, nil)
	case command.Name == chat.CommandHelp:
		m.setOverlay(overlayHelp, commandItems(result.Commands))
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
	m.overlayList.SetItems(items)
	m.overlayList.Select(0)
	m.composer.Blur()
}

func commandItems(commands []chat.Command) []list.Item {
	items := make([]list.Item, 0, len(commands))
	for _, command := range commands {
		items = append(items, overlayItem{title: command.Usage, detail: command.Description})
	}
	return items
}
func modelItems(models []chat.Model) []list.Item {
	items := make([]list.Item, 0, len(models))
	for _, model := range models {
		items = append(items, overlayItem{title: model.Alias, detail: model.Provider + " · " + model.Upstream, value: model.Alias})
	}
	return items
}
func sessionItems(sessions []chat.Session) []list.Item {
	items := make([]list.Item, 0, len(sessions))
	for _, session := range sessions {
		items = append(items, overlayItem{title: session.Title, detail: session.ID + " · " + session.ModelAlias, value: session.ID})
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
	m.viewport.SetHeight(max(3, m.height-7))
	m.composer.SetWidth(max(12, contentWidth-4))
	m.overlayList.SetSize(min(max(20, m.width-12), 76), min(max(5, m.height-10), 16))
	m.syncViewport(wasBottom)
	if !wasBottom {
		m.viewport.SetYOffset(offset)
	}
	if m.overlay == overlayNone {
		_ = m.composer.Focus()
	}
}

func (m *Model) syncViewport(follow bool) {
	m.viewport.SetContent(m.renderTranscript())
	if follow {
		m.viewport.GotoBottom()
	}
}
