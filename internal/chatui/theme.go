package chatui

import (
	"image/color"

	"charm.land/lipgloss/v2"
)

type theme struct {
	noColor                                                                 bool
	dark                                                                    bool
	brand, muted, user, assistant, notice, danger, success, border, surface color.Color
}

func newTheme(dark, noColor bool) theme {
	t := theme{noColor: noColor, dark: dark}
	if noColor {
		return t
	}
	if dark {
		t.brand = lipgloss.Color("212")
		t.muted = lipgloss.Color("245")
		t.user = lipgloss.Color("117")
		t.assistant = lipgloss.Color("212")
		t.notice = lipgloss.Color("221")
		t.danger = lipgloss.Color("203")
		t.success = lipgloss.Color("114")
		t.border = lipgloss.Color("240")
		t.surface = lipgloss.Color("#101827")
	} else {
		t.brand = lipgloss.Color("127")
		t.muted = lipgloss.Color("243")
		t.user = lipgloss.Color("25")
		t.assistant = lipgloss.Color("127")
		t.notice = lipgloss.Color("136")
		t.danger = lipgloss.Color("160")
		t.success = lipgloss.Color("28")
		t.border = lipgloss.Color("248")
		t.surface = lipgloss.Color("#fffaf0")
	}
	return t
}

func (t theme) style(value string, color color.Color, bold bool) string {
	if t.noColor {
		return value
	}
	style := lipgloss.NewStyle().Foreground(color)
	if bold {
		style = style.Bold(true)
	}
	return style.Render(value)
}

func (t theme) brandText(value string) string   { return t.style(value, t.brand, true) }
func (t theme) mutedText(value string) string   { return t.style(value, t.muted, false) }
func (t theme) errorText(value string) string   { return t.style(value, t.danger, false) }
func (t theme) successText(value string) string { return t.style(value, t.success, false) }
func (t theme) roleText(role cardRole, value string) string {
	switch role {
	case roleUser:
		return t.style(value, t.user, true)
	case roleAssistant:
		return t.style(value, t.assistant, true)
	case roleNotice:
		return t.style(value, t.notice, true)
	case roleError:
		return t.style(value, t.danger, true)
	default:
		return value
	}
}
func (t theme) borderText(value string) string { return t.style(value, t.border, false) }
