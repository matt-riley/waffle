package chatui

import (
	"strings"
	"unicode"

	"github.com/charmbracelet/x/ansi"
)

func sanitizeMultiline(value string) string {
	value = ansi.Strip(strings.ReplaceAll(value, "\r\n", "\n"))
	return strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\t':
			return r
		case '\r':
			return '\n'
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, value)
}

func sanitizeLine(value string) string {
	return strings.Join(strings.Fields(sanitizeMultiline(value)), " ")
}
