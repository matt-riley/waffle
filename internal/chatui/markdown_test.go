package chatui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestRenderMarkdownNestedStructures(t *testing.T) {
	palette := newTheme(true, true) // monochrome for stable substrings
	input := strings.Join([]string{
		"- item with **bold** text",
		"- see [docs](https://example.com/path) and `inline`",
		"```go",
		"func main() {",
		"\tfmt.Println(1)",
		"}",
		"```",
	}, "\n")

	got := renderMarkdown(input, palette, 80)
	stripped := ansi.Strip(got)

	// Nested bold in list: markers gone, content present.
	if strings.Contains(stripped, "**") {
		t.Errorf("raw strong markers remain: %q", stripped)
	}
	if !strings.Contains(stripped, "• item with bold text") {
		t.Errorf("bold-in-list missing: %q", stripped)
	}

	// Link + adjacent code on a list line.
	if strings.Contains(stripped, "[docs]") || strings.Contains(stripped, "](") {
		t.Errorf("raw link markup remains: %q", stripped)
	}
	if !strings.Contains(stripped, "docs (https://example.com/path)") {
		t.Errorf("link label/url form missing: %q", stripped)
	}
	if strings.Contains(stripped, "`inline`") {
		t.Errorf("raw code backticks remain: %q", stripped)
	}
	if !strings.Contains(stripped, "inline") {
		t.Errorf("inline code content missing: %q", stripped)
	}

	// Multi-line fenced code block.
	for _, want := range []string{"func main() {", "fmt.Println(1)", "}"} {
		if !strings.Contains(stripped, want) {
			t.Errorf("fenced code missing %q in %q", want, stripped)
		}
	}
	if !strings.Contains(stripped, "  code") {
		t.Errorf("code block label missing: %q", stripped)
	}
	if !strings.Contains(stripped, "  │ ") {
		t.Errorf("code block gutter missing: %q", stripped)
	}
}

func TestRenderMarkdownANSIWrapIgnoresEscapeWidth(t *testing.T) {
	palette := newTheme(true, false) // color on so output carries ANSI
	// Enough bold words that wrapping is required at width 32.
	// No space before closing ** (CommonMark delimiter rules).
	input := "Prefix **" + strings.TrimSpace(strings.Repeat("boldword ", 12)) + "** suffix and `code`."
	const width = 32
	got := renderMarkdown(input, palette, width)

	if !strings.Contains(got, "\x1b[") && !strings.Contains(got, "\x1b") {
		// lipgloss may emit OSC/CSI; accept either SGR form. If noColor somehow,
		// still validate wrap on plain text.
		t.Logf("note: no ANSI detected in colored theme output: %q", got)
	}

	stripped := ansi.Strip(got)
	if strings.Contains(stripped, "**") {
		t.Errorf("raw ** remain after styled render: %q", stripped)
	}
	if strings.Contains(stripped, "`code`") {
		t.Errorf("raw backticks remain: %q", stripped)
	}
	if !strings.Contains(stripped, "boldword") || !strings.Contains(stripped, "code") {
		t.Errorf("styled content missing: %q", stripped)
	}

	for i, line := range strings.Split(got, "\n") {
		w := ansi.StringWidth(line)
		if w > width {
			t.Errorf("line %d visible width %d > %d: raw=%q stripped=%q", i, w, width, line, ansi.Strip(line))
		}
	}
}

func TestRenderMarkdownNoColorPreservesStructure(t *testing.T) {
	got := renderMarkdown(
		"# Heading\n- *one* and **two**\n- [docs](https://example.com) with `code`\n```go\nfmt.Println(1)\n```",
		newTheme(false, true),
		60,
	)
	for _, want := range []string{"Heading", "• one and two", "docs (https://example.com) with code", "fmt.Println(1)"} {
		if !strings.Contains(got, want) {
			t.Errorf("markdown missing %q in %q", want, got)
		}
	}
}

func TestRenderMarkdownCodeInsideEmphasis(t *testing.T) {
	// Nested: emphasis wrapping code span should not leak raw markers.
	got := renderMarkdown("use *`os.Exit`* carefully and **`must` bold**", newTheme(true, true), 80)
	if strings.Contains(got, "*") || strings.Contains(got, "`") {
		// After AST render monochrome, markers should be gone (bare * or `).
		// Allow only if leftover from unrelated content — none expected here.
		if strings.Contains(got, "*`") || strings.Contains(got, "`*") || strings.Contains(got, "**") || strings.Contains(got, "`must`") {
			t.Errorf("raw nested markup remains: %q", got)
		}
	}
	if !strings.Contains(got, "os.Exit") || !strings.Contains(got, "must") {
		t.Errorf("nested code content missing: %q", got)
	}
}
