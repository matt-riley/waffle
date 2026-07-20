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
		t.brand = lipgloss.Color("#F2B84B")
		t.muted = lipgloss.Color("#8A94A6")
		t.user = lipgloss.Color("#F5D08A")
		t.assistant = lipgloss.Color("#5ED7E8")
		t.notice = lipgloss.Color("#E9B949")
		t.danger = lipgloss.Color("#FF6B6B")
		t.success = lipgloss.Color("#6FCF97")
		t.border = t.brand
		t.surface = lipgloss.Color("#101827")
	} else {
		t.brand = lipgloss.Color("#9A5A00")
		t.muted = lipgloss.Color("#6B7280")
		t.user = lipgloss.Color("#7A4A00")
		t.assistant = lipgloss.Color("#007C91")
		t.notice = lipgloss.Color("#8A5A00")
		t.danger = lipgloss.Color("#B42318")
		t.success = lipgloss.Color("#237A3B")
		t.border = t.brand
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
