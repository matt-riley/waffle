package chatui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// View returns a declarative Bubble Tea v2 alternate-screen view.
func (m *Model) View() tea.View {
	contentWidth := max(20, m.width-4)
	header := m.renderHeader(contentWidth)
	composer := m.renderComposer(contentWidth)
	footer := m.renderFooter(contentWidth)
	body := m.viewport.View()
	if m.overlay != overlayNone {
		body = overlayBody(body, m.renderOverlay(contentWidth), m.viewport.Height())
	}
	parts := []string{header, strings.Repeat("─", contentWidth), body, composer, footer}
	if m.paletteVisible {
		parts = append(parts, m.renderPalette(contentWidth))
	}
	content := trimRightLines(strings.Join(parts, "\n"))
	view := tea.NewView(content)
	view.AltScreen = true
	view.WindowTitle = "Waffle chat"
	view.MouseMode = tea.MouseModeCellMotion
	if !m.theme.noColor {
		view.BackgroundColor = m.theme.surface
	}
	return view
}

func (m *Model) renderHeader(width int) string {
	title := sanitizeLine(m.state.Title)
	if title == "" {
		title = "Focused Conversation"
	}
	session := sanitizeLine(m.state.SessionID)
	if len(session) > 8 {
		session = session[:8]
	}
	if session != "" {
		title += " · " + session
	}
	model := sanitizeLine(m.state.ModelAlias)
	if model == "" {
		model = "model pending"
	}
	profile := sanitizeLine(m.state.Profile)
	if profile == "" {
		profile = "main"
	}
	mode := "direct"
	if m.state.ConnectionMode == "unix" {
		mode = "local service"
	}
	connection := model + " · " + profile + " · " + mode
	if !m.connected {
		if m.awaitingAck || m.err != nil {
			connection += " · disconnected"
		} else {
			connection += " · connecting"
		}
	}
	if m.width < 72 {
		return " " + m.theme.brandText("Waffle") + "  " + ansi.Truncate(title, max(8, width-10), "…") + "\n " + m.theme.mutedText(ansi.Truncate(connection, width-1, "…"))
	}
	prefix := " " + m.theme.brandText("Waffle") + "  "
	maxTitleWidth := max(8, width-lipgloss.Width(prefix)-lipgloss.Width(connection)-1)
	left := prefix + ansi.Truncate(title, maxTitleWidth, "…")
	gap := max(1, width-lipgloss.Width(left)-lipgloss.Width(connection))
	return left + strings.Repeat(" ", gap) + m.theme.mutedText(connection)
}

func (m *Model) renderTranscript() string {
	width := max(12, m.viewport.Width()-2)
	var out []string
	toolIndex := 0
	for messageIndex, card := range m.messages {
		label := "Waffle"
		switch card.role {
		case roleUser:
			label = "You"
		case roleNotice:
			label = "Notice"
		case roleError:
			label = "Error"
		}
		styledLabel := m.theme.roleText(card.role, label)
		out = append(out, styledLabel, renderMarkdown(card.text, m.theme, width), "")
		if card.role == roleAssistant {
			for toolIndex < len(m.tools) && m.tools[toolIndex].messageIndex == messageIndex {
				out = append(out, "  "+m.renderTool(m.tools[toolIndex]))
				toolIndex++
			}
		}
	}
	for toolIndex < len(m.tools) {
		out = append(out, "  "+m.renderTool(m.tools[toolIndex]))
		toolIndex++
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n")
}

func (m *Model) renderTool(tool toolRow) string {
	status := "…"
	if tool.done {
		status = "✓"
	}
	if tool.failed {
		status = "✗"
	}
	row := status + " " + sanitizeLine(tool.name)
	if tool.byteCount > 0 {
		row += "   " + compactBytes(tool.byteCount)
	}
	if tool.failed {
		return m.theme.errorText(row)
	}
	if tool.done {
		return m.theme.successText(row)
	}
	return m.theme.mutedText(row)
}

func compactBytes(bytes int) string {
	if bytes < 1000 {
		return fmt.Sprintf("%d B", bytes)
	}
	return fmt.Sprintf("%.1f KB", float64(bytes)/1000)
}

func (m *Model) renderComposer(width int) string {
	inner := max(8, width-4)
	value := m.composer.View()
	lines := strings.Split(value, "\n")
	for i, line := range lines {
		lines[i] = "│ " + ansi.Truncate(line, inner, "…") + strings.Repeat(" ", max(0, inner-lipgloss.Width(ansi.Truncate(line, inner, "…")))) + " │"
	}
	top := m.theme.borderText("┌" + strings.Repeat("─", inner+2) + "┐")
	bottom := m.theme.borderText("└" + strings.Repeat("─", inner+2) + "┘")
	return top + "\n" + strings.Join(lines, "\n") + "\n" + bottom
}

func (m *Model) renderFooter(width int) string {
	left := "/help  /model  /sessions"
	if m.width < 72 {
		if m.turnActive || m.commandActive {
			return left + "  ·  working…"
		}
		return left
	}
	right := "Alt+↵ newline · ↵ send"
	if m.turnActive || m.commandActive {
		right = "Esc cancel · working…"
	} else if m.inputTokens > 0 || m.outputTokens > 0 {
		right = fmt.Sprintf("%d in · %d out · %s", m.inputTokens, m.outputTokens, right)
	}
	gap := max(1, width-lipgloss.Width(left)-lipgloss.Width(right))
	return m.theme.mutedText(left + strings.Repeat(" ", gap) + right)
}

func (m *Model) renderPalette(width int) string {
	var values []string
	for i, command := range m.palette {
		prefix := "  "
		if i == m.paletteIndex {
			prefix = "› "
		}
		values = append(values, prefix+sanitizeLine(command.Usage))
	}
	return m.theme.mutedText("Commands: " + ansi.Truncate(strings.Join(values, "  "), width-10, "…"))
}

func (m *Model) renderOverlay(width int) string {
	maxWidth := min(max(24, width-8), 72)
	title, body := m.overlayContent()
	maxBodyRows := min(12, max(4, m.viewport.Height()-4))
	body = clipLines(body, maxBodyRows)
	boxWidth := min(maxWidth, max(24, lipgloss.Width(title)+4))
	for _, line := range strings.Split(body, "\n") {
		boxWidth = min(maxWidth, max(boxWidth, lipgloss.Width(line)+4))
	}
	border := "┌" + strings.Repeat("─", boxWidth-2) + "┐\n"
	rows := []string{padOverlay(m.theme.brandText(title), boxWidth-4)}
	for _, line := range strings.Split(body, "\n") {
		rows = append(rows, padOverlay(ansi.Truncate(line, boxWidth-4, "…"), boxWidth-4))
	}
	for i, row := range rows {
		rows[i] = "│ " + row + " │"
	}
	box := border + strings.Join(rows, "\n") + "\n└" + strings.Repeat("─", boxWidth-2) + "┘"
	return lipgloss.PlaceHorizontal(width, lipgloss.Center, box)
}

func (m *Model) overlayContent() (string, string) {
	switch m.overlay {
	case overlayHelp:
		return "Help", m.overlayList.View()
	case overlayModels:
		return "Models", m.overlayList.View()
	case overlaySessions:
		return "Sessions", m.overlayList.View()
	case overlayPermissions:
		if p := m.overlayResult.Permissions; p != nil {
			return "Permissions", fmt.Sprintf("Sandbox: %s\nAllow: %s\nDeny: %s\nDeny prefixes: %s", sanitizeLine(p.SandboxMode), sanitizeLines(p.Allow), sanitizeLines(p.Deny), sanitizeLines(p.DenyPrefixes))
		}
		return "Permissions", "No permission details available."
	case overlayConfirm:
		text := m.overlayResult.Text
		if text == "" {
			text = "Confirm this action?"
		}
		return "Confirm", sanitizeMultiline(text) + "\n\nEnter confirm · Esc cancel"
	default:
		return "", ""
	}
}

func sanitizeLines(values []string) string {
	clean := make([]string, len(values))
	for i, value := range values {
		clean[i] = sanitizeLine(value)
	}
	return strings.Join(clean, ", ")
}

func padOverlay(value string, width int) string {
	return value + strings.Repeat(" ", max(0, width-lipgloss.Width(value)))
}

func clipLines(value string, height int) string {
	lines := strings.Split(value, "\n")
	if len(lines) <= height {
		return value
	}
	lines = lines[:height]
	lines[len(lines)-1] = "…"
	return strings.Join(lines, "\n")
}

func overlayBody(body, overlay string, height int) string {
	bodyLines := strings.Split(body, "\n")
	overlayLines := strings.Split(overlay, "\n")
	if len(overlayLines) > height {
		overlayLines = overlayLines[:height]
	}
	for len(bodyLines) < height {
		bodyLines = append(bodyLines, "")
	}
	if len(bodyLines) > height {
		bodyLines = bodyLines[:height]
	}
	start := max(0, (height-len(overlayLines))/2)
	lines := bodyLines
	copy(lines[start:start+len(overlayLines)], overlayLines)
	return strings.Join(lines, "\n")
}

func trimRightLines(value string) string {
	lines := strings.Split(value, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " ")
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n")
}
