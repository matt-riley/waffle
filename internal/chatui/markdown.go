package chatui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

// renderMarkdown parses Markdown with goldmark (AST) and renders terminal text
// using palette styles. Word wrapping uses ansi-aware widths so style codes do
// not corrupt column calculations.
func renderMarkdown(input string, palette theme, width int) string {
	if width < 8 {
		width = 8
	}
	source := []byte(sanitizeMultiline(input))
	doc := goldmark.DefaultParser().Parse(text.NewReader(source))
	r := &mdRenderer{
		source:  source,
		palette: palette,
		width:   width,
	}
	r.renderChildren(doc)
	// Preserve trailing blank lines from the source. goldmark drops pure
	// trailing newlines, but the TUI uses them as viewport padding.
	for i := len(source) - 1; i >= 0 && source[i] == '\n'; i-- {
		r.out = append(r.out, "")
	}
	return strings.Join(r.out, "\n")
}

type mdRenderer struct {
	source  []byte
	palette theme
	width   int
	out     []string
}

func (r *mdRenderer) renderChildren(n ast.Node) {
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		r.renderBlock(c)
	}
}

func (r *mdRenderer) renderBlock(n ast.Node) {
	if n.HasBlankPreviousLines() && len(r.out) > 0 {
		r.out = append(r.out, "")
	}
	switch node := n.(type) {
	case *ast.Heading:
		line := r.palette.brandText(r.renderInlines(node))
		r.appendWrapped(line)
	case *ast.Paragraph:
		r.appendWrapped(r.renderInlines(node))
	case *ast.TextBlock:
		r.appendWrapped(r.renderInlines(node))
	case *ast.List:
		for item := node.FirstChild(); item != nil; item = item.NextSibling() {
			r.renderListItem(item)
		}
	case *ast.FencedCodeBlock:
		r.renderCodeBlock(node.Lines())
	case *ast.CodeBlock:
		r.renderCodeBlock(node.Lines())
	case *ast.Blockquote:
		r.renderChildren(node)
	case *ast.ThematicBreak:
		bar := strings.Repeat("─", min(r.width, 40))
		r.out = append(r.out, r.palette.mutedText(bar))
	case *ast.HTMLBlock:
		for _, line := range r.segmentLines(node.Lines()) {
			r.appendWrapped(line)
		}
	default:
		if n.HasChildren() {
			r.renderChildren(n)
		}
	}
}

func (r *mdRenderer) renderListItem(n ast.Node) {
	var inlineParts []string
	var deferred []ast.Node
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		switch c.(type) {
		case *ast.Paragraph, *ast.TextBlock:
			if s := r.renderInlines(c); s != "" {
				inlineParts = append(inlineParts, s)
			}
		default:
			deferred = append(deferred, c)
		}
	}
	line := "• " + strings.Join(inlineParts, " ")
	r.appendWrapped(line)
	for _, child := range deferred {
		r.renderBlock(child)
	}
}

func (r *mdRenderer) renderCodeBlock(lines *text.Segments) {
	r.out = append(r.out, r.palette.mutedText("  code"))
	for _, line := range r.segmentLines(lines) {
		styled := r.palette.mutedText("  │ " + line)
		r.out = append(r.out, ansi.Hardwrap(styled, r.width, true))
	}
}

func (r *mdRenderer) segmentLines(lines *text.Segments) []string {
	if lines == nil {
		return nil
	}
	out := make([]string, 0, lines.Len())
	for i := 0; i < lines.Len(); i++ {
		seg := lines.At(i)
		out = append(out, strings.TrimRight(string(seg.Value(r.source)), "\n\r"))
	}
	return out
}

func (r *mdRenderer) appendWrapped(line string) {
	// Hard line breaks in inlines become \n; wrap each resulting visual line.
	for _, part := range strings.Split(line, "\n") {
		r.out = append(r.out, ansi.Wordwrap(part, r.width, " "))
	}
}

func (r *mdRenderer) renderInlines(n ast.Node) string {
	var b strings.Builder
	r.writeInlines(&b, n)
	return b.String()
}

func (r *mdRenderer) writeInlines(b *strings.Builder, n ast.Node) {
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		r.writeInline(b, c)
	}
}

func (r *mdRenderer) writeInline(b *strings.Builder, n ast.Node) {
	switch node := n.(type) {
	case *ast.Text:
		b.Write(node.Segment.Value(r.source))
		switch {
		case node.HardLineBreak():
			b.WriteByte('\n')
		case node.SoftLineBreak():
			b.WriteByte(' ')
		}
	case *ast.String:
		b.Write(node.Value)
	case *ast.CodeSpan:
		b.WriteString(r.palette.mutedText(r.rawText(node)))
	case *ast.Emphasis:
		inner := r.renderInlines(node)
		if node.Level >= 2 {
			b.WriteString(r.palette.brandText(inner))
		} else {
			b.WriteString(r.palette.mutedText(inner))
		}
	case *ast.Link:
		label := r.renderInlines(node)
		dest := string(node.Destination)
		b.WriteString(label)
		b.WriteString(" (")
		b.WriteString(dest)
		b.WriteString(")")
	case *ast.AutoLink:
		b.WriteString(string(node.URL(r.source)))
	case *ast.Image:
		alt := r.renderInlines(node)
		if alt == "" {
			alt = string(node.Destination)
		}
		b.WriteString(alt)
	case *ast.RawHTML:
		for i := 0; i < node.Segments.Len(); i++ {
			seg := node.Segments.At(i)
			b.Write(seg.Value(r.source))
		}
	default:
		if n.HasChildren() {
			r.writeInlines(b, n)
		}
	}
}

func (r *mdRenderer) rawText(n ast.Node) string {
	var b strings.Builder
	_ = ast.Walk(n, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch t := node.(type) {
		case *ast.Text:
			b.Write(t.Segment.Value(r.source))
		case *ast.String:
			b.Write(t.Value)
		}
		return ast.WalkContinue, nil
	})
	return b.String()
}
