package tool

// Content-type-aware shaping of fetch response bodies (#248).
//
// fetch previously stringified the raw response body, so an HTML page arrived
// as doctype, script blocks, CSS and nav chrome (costing ~4 characters per
// token), a PDF arrived as replacement-character soup, and no charset was
// respected. This file turns the body into model-facing text:
//
//   - text/html, application/xhtml+xml -> readable extraction via a
//     hand-rolled golang.org/x/net/html walk (title, headings, paragraphs,
//     list items, link text, code blocks; script/style/nav/header/footer/svg
//     and comments dropped; links kept as "text (url)").
//   - application/json and *+json -> pretty-printed (or passed through when
//     the body is not valid JSON).
//   - text/* -> passed through as before, decoded per the declared charset.
//   - anything else -> a short typed descriptor (content-type, byte length,
//     filename if any) with zero bytes of the payload, so binary content is
//     never stringified into the context.
//
// The declared charset is respected, defaulting to UTF-8 (previous
// behaviour). Truncation is stated explicitly: when the read cap or the
// HostReturnCap is hit, a machine-readable marker is prepended so the model
// knows it received a prefix. Extraction happens before the return cap, so
// the 512 KiB budget is spent on prose rather than markup.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime"
	"net/url"
	"path"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/text/encoding/htmlindex"
	"golang.org/x/text/transform"
)

// fetchReadCap bounds how many bytes of a response body fetch reads. The
// +1-byte read in Run distinguishes "body ends exactly at the cap" from
// "body was truncated by the cap". Matches the 2 MiB cap used elsewhere
// (fileContentMaxBytes) so a single fetch cannot buffer unbounded memory.
const fetchReadCap = 2 * 1024 * 1024

// formatFetchBody shapes a response body for the model. contentType is the
// raw Content-Type header (possibly empty), body is at most fetchReadCap
// bytes, readTruncated reports whether the body exceeded the read cap, and
// filename is the Content-Disposition or URL-derived name used by the
// non-text descriptor. The result is capped at HostReturnCap with an
// explicit marker when that cap is hit; missing/unparseable Content-Type
// falls back to the previous pass-through behaviour.
func formatFetchBody(contentType string, body []byte, readTruncated bool, filename string) string {
	mediatype, params, err := mime.ParseMediaType(contentType)
	if err != nil || mediatype == "" {
		// No usable Content-Type: keep the historical pass-through path.
		mediatype = "text/plain"
	}
	var content string
	switch {
	case mediatype == "text/html" || mediatype == "application/xhtml+xml":
		content = extractHTML(body, params["charset"])
	case mediatype == "application/json" || strings.HasSuffix(mediatype, "+json"):
		content = prettyJSON(decodeFetchBody(body, params["charset"]))
	case strings.HasPrefix(mediatype, "text/"):
		content = decodeFetchBody(body, params["charset"])
	default:
		content = describeFetchBody(mediatype, len(body), filename)
	}
	if readTruncated {
		content = fmt.Sprintf("\n[fetch-truncated: response body exceeds %d-byte read cap; remainder dropped]\n", fetchReadCap) + content
	}
	if len(content) > HostReturnCap {
		content = fmt.Sprintf("\n[fetch-truncated: output exceeds %d-byte return cap; %d bytes dropped]\n", HostReturnCap, len(content)-HostReturnCap) + content
		content = CapHostReturn(content)
	}
	return content
}

// fetchFilename picks a display name for a non-text body: the Content-
// Disposition filename when present, otherwise the URL path basename (which
// may be empty for bare origins).
func fetchFilename(contentDisposition string, u *url.URL) string {
	if contentDisposition != "" {
		if _, params, err := mime.ParseMediaType(contentDisposition); err == nil {
			if fn := params["filename"]; fn != "" {
				return fn
			}
		}
	}
	base := path.Base(u.Path)
	if base == "." || base == "/" || base == "" {
		return ""
	}
	return base
}

// decodeFetchBody decodes a text-ish body per the declared charset, defaulting
// to UTF-8 (raw bytes, as before) when none is declared. Unknown charset
// names fall back to raw bytes rather than erroring; x/text decoders never
// fail on arbitrary input, so the result is always valid UTF-8.
func decodeFetchBody(body []byte, charset string) string {
	if charset == "" {
		return string(body)
	}
	enc, err := htmlindex.Get(charset)
	if err != nil {
		return string(body)
	}
	out, _, err := transform.Bytes(enc.NewDecoder(), body)
	if err != nil {
		return string(body)
	}
	return string(out)
}

// prettyJSON indents a JSON body for readability. Bodies that are not valid
// JSON pass through unchanged rather than erroring the tool.
func prettyJSON(body string) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, []byte(body), "", "  "); err != nil {
		return body
	}
	return buf.String()
}

// describeFetchBody is the short typed descriptor returned for non-text
// content types. It carries no payload bytes at all, so binary content can
// never leak into the model context as replacement-character soup.
func describeFetchBody(mediatype string, bodyLen int, filename string) string {
	var b strings.Builder
	b.WriteString("[fetch: non-text content-type, body not shown]\n")
	fmt.Fprintf(&b, "content-type: %s\n", mediatype)
	fmt.Fprintf(&b, "bytes: %d\n", bodyLen)
	if filename != "" {
		fmt.Fprintf(&b, "filename: %s\n", filename)
	}
	return b.String()
}

// extractHTML walks an HTML document and returns a compact Markdown-ish
// rendering: the <title> on its own line, headings as #..######, list items
// as "- item", links as "text (url)", code blocks fenced in ```, and
// paragraphs as plain lines separated by blank lines. script, style, nav,
// header, footer, svg, comments and (except the title) head contents are
// dropped. Whitespace is collapsed within a line. The parser is lenient by
// design: malformed and unclosed-tag documents extract without error.
func extractHTML(body []byte, charset string) string {
	doc, err := html.Parse(strings.NewReader(decodeFetchBody(body, charset)))
	if err != nil {
		// html.Parse is lenient and effectively never fails on bytes; if it
		// ever does, surface that rather than returning raw markup.
		return fmt.Sprintf("(fetch: HTML extraction failed: %v)", err)
	}
	x := &htmlExtractor{}
	x.walk(doc)
	return strings.TrimSpace(x.out.String())
}

var htmlSkipTags = map[string]bool{
	"script": true,
	"style":  true,
	"nav":    true,
	"header": true,
	"footer": true,
	"svg":    true,
}

var htmlBlockTags = map[string]bool{
	"html": true, "body": true, "main": true, "section": true, "article": true,
	"p": true, "div": true, "blockquote": true, "address": true, "form": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"ul": true, "ol": true, "li": true, "dl": true, "dt": true, "dd": true,
	"figure": true, "figcaption": true, "hr": true,
	"table": true, "thead": true, "tbody": true, "tfoot": true, "tr": true, "td": true, "th": true,
	"pre": true,
}

type htmlFrame struct {
	lineLen int
	kind    string // "a" (append " (href)") or "code" (append "`")
	href    string
}

type htmlExtractor struct {
	out    bytes.Buffer // finished lines, each ending in "\n"
	line   bytes.Buffer // current line being accumulated
	inPre  bool
	inHead bool
	frames []htmlFrame
}

func (x *htmlExtractor) walk(n *html.Node) {
	switch n.Type {
	case html.TextNode:
		if !x.inHead {
			x.line.WriteString(n.Data)
		}
		return
	case html.CommentNode:
		return
	case html.DocumentNode:
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			x.walk(c)
		}
		return
	case html.ElementNode:
	default:
		return
	}
	tag := n.Data
	if tag == "head" {
		prev := x.inHead
		x.inHead = true
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			x.walk(c)
		}
		x.inHead = prev
		return
	}
	if x.inHead {
		if tag == "title" {
			// Collect the title text into the line, then emit it as its own
			// line; temporarily leave head mode so text nodes are kept.
			prev := x.inHead
			x.inHead = false
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				x.walk(c)
			}
			x.inHead = prev
			x.flushLine()
			x.gap()
		}
		return
	}
	if htmlSkipTags[tag] {
		return
	}
	if x.inPre {
		// Inside a code block only text matters; tags (e.g. nested <code>)
		// are walked through without shaping.
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			x.walk(c)
		}
		return
	}
	if htmlBlockTags[tag] {
		if tag == "li" {
			x.newline()
		} else {
			x.gap()
		}
		switch tag {
		case "h1", "h2", "h3", "h4", "h5", "h6":
			x.line.WriteString(strings.Repeat("#", int(tag[1]-'0')) + " ")
		case "li":
			x.line.WriteString("- ")
		case "pre":
			x.out.WriteString("```\n")
			x.line.Reset()
			x.inPre = true
		}
	} else {
		switch tag {
		case "a":
			if href := hrefAttr(n); href != "" {
				x.frames = append(x.frames, htmlFrame{lineLen: x.line.Len(), kind: "a", href: href})
			}
		case "code":
			x.frames = append(x.frames, htmlFrame{lineLen: x.line.Len(), kind: "code"})
			x.line.WriteByte('`')
		case "img":
			if alt := attr(n, "alt"); alt != "" {
				x.line.WriteString(alt)
				x.line.WriteByte(' ')
			}
		case "br":
			x.line.WriteByte(' ')
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		x.walk(c)
	}
	if x.inPre {
		// Closing </pre>: emit the accumulated raw text verbatim.
		x.out.WriteString(x.line.String())
		x.out.WriteString("\n```\n")
		x.line.Reset()
		x.inPre = false
		return
	}
	if htmlBlockTags[tag] {
		x.flushLine()
		return
	}
	switch tag {
	case "a":
		if len(x.frames) > 0 && x.frames[len(x.frames)-1].kind == "a" {
			f := x.frames[len(x.frames)-1]
			x.frames = x.frames[:len(x.frames)-1]
			if strings.TrimSpace(x.line.String()[f.lineLen:]) != "" {
				x.line.WriteString(" (" + f.href + ")")
			}
		}
	case "code":
		if len(x.frames) > 0 && x.frames[len(x.frames)-1].kind == "code" {
			f := x.frames[len(x.frames)-1]
			x.frames = x.frames[:len(x.frames)-1]
			if x.line.Len() > f.lineLen {
				x.line.WriteByte('`')
			} else {
				x.line.Truncate(f.lineLen)
			}
		}
	}
}

// flushLine writes the accumulated line (whitespace collapsed) to out and
// clears the line. Block boundaries end any open link/code frames: text that
// was flushed can no longer carry a " (url)" suffix.
func (x *htmlExtractor) flushLine() {
	if s := strings.Join(strings.Fields(x.line.String()), " "); s != "" {
		x.out.WriteString(s)
		x.out.WriteByte('\n')
	}
	x.line.Reset()
	x.frames = nil
}

// gap flushes the pending line and ensures a blank line separates blocks.
func (x *htmlExtractor) gap() {
	x.flushLine()
	if x.out.Len() > 0 && !bytes.HasSuffix(x.out.Bytes(), []byte("\n\n")) {
		x.out.WriteByte('\n')
	}
}

// newline flushes the pending line and ensures at least one line break, so
// list items run together instead of being separated by blank lines.
func (x *htmlExtractor) newline() {
	x.flushLine()
	if x.out.Len() > 0 && !bytes.HasSuffix(x.out.Bytes(), []byte("\n")) {
		x.out.WriteByte('\n')
	}
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// hrefAttr returns an <a> href that is worth showing: non-empty and not a
// javascript:/data: pseudo-URL. Relative and fragment-free links are kept.
func hrefAttr(n *html.Node) string {
	href := strings.TrimSpace(attr(n, "href"))
	if href == "" || href == "#" {
		return ""
	}
	for _, scheme := range []string{"javascript:", "data:"} {
		if strings.HasPrefix(strings.ToLower(href), scheme) {
			return ""
		}
	}
	return href
}
