package chatui

import (
	"regexp"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

var (
	strongPattern   = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	emphasisPattern = regexp.MustCompile(`\*([^*]+)\*`)
	codePattern     = regexp.MustCompile("`([^`]+)`")
	linkPattern     = regexp.MustCompile(`\[([^]]+)\]\(([^)]+)\)`)
)

func renderMarkdown(input string, palette theme, width int) string {
	if width < 8 {
		width = 8
	}
	lines := strings.Split(sanitizeMultiline(input), "\n")
	inFence := false
	var out []string
	for _, raw := range lines {
		line := raw
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			if inFence {
				out = append(out, palette.mutedText("  code"))
			}
			continue
		}
		if inFence {
			line = "  │ " + line
			out = append(out, ansi.Hardwrap(palette.mutedText(line), width, true))
			continue
		}
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "### "):
			line = palette.brandText(strings.TrimPrefix(trimmed, "### "))
		case strings.HasPrefix(trimmed, "## "):
			line = palette.brandText(strings.TrimPrefix(trimmed, "## "))
		case strings.HasPrefix(trimmed, "# "):
			line = palette.brandText(strings.TrimPrefix(trimmed, "# "))
		case strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* "):
			line = "• " + strings.TrimSpace(trimmed[2:])
		}
		line = linkPattern.ReplaceAllString(line, "$1 ($2)")
		line = applyInlineStyle(line, strongPattern, palette, true)
		line = applyInlineStyle(line, emphasisPattern, palette, false)
		line = applyInlineStyle(line, codePattern, palette, false)
		out = append(out, ansi.Wordwrap(line, width, " "))
	}
	return strings.Join(out, "\n")
}

func applyInlineStyle(input string, pattern *regexp.Regexp, palette theme, bold bool) string {
	return pattern.ReplaceAllStringFunc(input, func(match string) string {
		parts := pattern.FindStringSubmatch(match)
		if len(parts) < 2 {
			return match
		}
		if bold {
			return palette.brandText(parts[1])
		}
		return palette.mutedText(parts[1])
	})
}
