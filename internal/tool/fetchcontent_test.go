package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// fetchFixtureServer serves a checked-in testdata file with the given
// Content-Type, plus optional extra headers. All fetch tests run against
// loopback with allow_private so only content shaping (not SSRF policy) is
// under test; the security posture itself is covered by the existing
// TestFetch* tests, which pass unmodified.
func fetchFixtureServer(t *testing.T, file, contentType string, extraHeaders map[string]string) (string, error) {
	t.Helper()
	body, err := os.ReadFile(filepathJoinTestdata(file))
	if err != nil {
		t.Fatal(err)
	}
	return fetchRaw(t, contentType, body, extraHeaders, "")
}

func fetchRaw(t *testing.T, contentType string, body []byte, extraHeaders map[string]string, urlPath string) (string, error) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		for k, v := range extraHeaders {
			w.Header().Set(k, v)
		}
		if _, err := w.Write(body); err != nil {
			// The fetch client stops reading at the read cap and closes the
			// connection; a broken pipe here is expected, not a failure.
			return
		}
	}))
	defer srv.Close()
	target := srv.URL + urlPath
	return run(t, &Fetch{AllowPrivate: []string{"127.0.0.0/8"}}, fmt.Sprintf(`{"url":%q}`, target))
}

func filepathJoinTestdata(name string) string {
	return "testdata/" + name
}

// TestFetchHTMLArticleFixture pins the exact extracted output for a checked-in
// realistic article fixture, asserts none of the dropped junk survives, and
// asserts numerically that extraction is at least 80% smaller than the raw
// body (#248).
func TestFetchHTMLArticleFixture(t *testing.T) {
	raw, err := os.ReadFile(filepathJoinTestdata("article.html"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := fetchFixtureServer(t, "article.html", "text/html; charset=utf-8", nil)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	want := `Waffle: the terminal waffle iron simulator hits 1.0

# Waffle simulators reach 1.0

After three years of development, the waffle project has shipped its first stable release. Read the announcement (https://example.com/announcement) for details.

## What changed

- Grid rendering is now ` + "`" + `60 fps` + "`" + ` on a Raspberry Pi.
- Batter temperature is monitored (https://example.com/batter) every 100 ms.
- Pattern memory survives a power cycle.

Getting started with a 12 by 8 grid:

` + "```" + `
waffle run --grid 12x8 --batter 210c
waffle pour --pattern hearts
` + "```" + `

SVG diagrams and footer links were removed from the feed.`
	if out != want {
		t.Fatalf("extracted output mismatch\n--- got ---\n%q\n--- want ---\n%q", out, want)
	}

	// Dropped junk: script/style/nav/header/footer/svg/comments.
	for _, junk := range []string{
		"tracking", "track('event_", ".rule1 {", "Home", "Docs", "Pricing",
		"Waffle HQ banner", "Copyright footer text", "<svg", "<circle",
		"<!--", "script payload",
	} {
		if strings.Contains(out, junk) {
			t.Errorf("extracted output contains dropped junk %q", junk)
		}
	}

	// At least 80% smaller than the raw body, asserted numerically.
	if len(out) > len(raw)/5 {
		t.Errorf("extraction only %.1f%% smaller: raw %d bytes -> %d bytes", 100*(1-float64(len(out))/float64(len(raw))), len(raw), len(out))
	}
}

// TestFetchHTMLCharsetISO88591: a charset=iso-8859-1 fixture decodes to the
// correct runes and the result is valid UTF-8 (#248).
func TestFetchHTMLCharsetISO88591(t *testing.T) {
	out, err := fetchFixtureServer(t, "latin1.html", "text/html; charset=iso-8859-1", nil)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	for _, want := range []string{
		"Crème brûlée at Café déjà vu", // title
		"Café déjà vu, naïve señor: résumé über fünfzehn Straße, crème brûlée.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if !utf8.ValidString(out) {
		t.Error("extracted output is not valid UTF-8")
	}
}

// TestFetchHTMLMalformed: an unclosed-tag, stray-angle-bracket fixture
// extracts without error and without panic (#248).
func TestFetchHTMLMalformed(t *testing.T) {
	out, err := fetchFixtureServer(t, "malformed.html", "text/html", nil)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	for _, want := range []string{"Broken page", "First paragraph is fine", "Second block with bold never closed", "item one", "item two", "Done"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "footer text") {
		t.Error("malformed fixture: footer text survived extraction")
	}
}

// TestFetchHTMLUnderReturnCap: extraction happens before capHostReturn, so a
// 2 MiB HTML page (mostly junk) yields prose well under the 512 KiB budget
// plus an explicit read-cap marker (#248).
func TestFetchHTMLUnderReturnCap(t *testing.T) {
	var body bytes.Buffer
	body.WriteString(`<html><head><title>Big page</title></head><body><article>`)
	body.WriteString(`<p>`)
	body.WriteString(strings.Repeat("The quick brown fox jumps over the lazy dog. ", 200))
	body.WriteString(`</p>`)
	body.WriteString(`<style>`)
	body.WriteString(strings.Repeat(".x { margin: 0; padding: 0; color: #fff; }", 40000))
	body.WriteString(`</style>`)
	body.WriteString(`<script>`)
	body.WriteString(strings.Repeat("analytics.push({a:1,b:2,c:3});", 40000))
	body.WriteString(`</script>`)
	body.WriteString(`</article></body></html>`)
	if body.Len() <= 2*1024*1024 {
		t.Fatalf("fixture only %d bytes, want > 2 MiB", body.Len())
	}
	out, err := fetchRaw(t, "text/html", body.Bytes(), nil, "")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !strings.Contains(out, "The quick brown fox") {
		t.Error("prose missing from extracted output")
	}
	if strings.Contains(out, "analytics.push") || strings.Contains(out, ".x { margin") {
		t.Error("junk survived extraction")
	}
	if len(out) >= HostReturnCap/2 {
		t.Errorf("extracted output %d bytes; want well under %d-byte cap", len(out), HostReturnCap)
	}
	if !strings.Contains(out, "[fetch-truncated:") || !strings.Contains(out, "read cap") {
		t.Errorf("missing read-cap truncation marker:\n%s", out)
	}
}

// TestFetchJSONPrettyPrint: application/json is returned as readable JSON;
// a body that is not valid JSON passes through rather than erroring (#248).
func TestFetchJSONPrettyPrint(t *testing.T) {
	out, err := fetchRaw(t, "application/json", []byte(`{"a":1,"b":[1,2],"c":{"d":true}}`), nil, "")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	for _, want := range []string{`"a": 1`, "\n  \"b\": [", "\n    \"d\": true"} {
		if !strings.Contains(out, want) {
			t.Errorf("pretty JSON missing %q:\n%s", want, out)
		}
	}
	if out == `{"a":1,"b":[1,2],"c":{"d":true}}` {
		t.Error("JSON was not pretty-printed")
	}

	// Invalid JSON with a JSON content type passes through unchanged.
	raw := `{"broken": `
	invalid, err := fetchRaw(t, "application/json", []byte(raw), nil, "")
	if err != nil || invalid != raw {
		t.Fatalf("invalid JSON = %q, %v; want raw pass-through", invalid, err)
	}

	// JSON with a +json media type is pretty-printed too.
	ld, err := fetchRaw(t, "application/ld+json", []byte(`{"@context":"x"}`), nil, "")
	if err != nil {
		t.Fatalf("fetch ld+json: %v", err)
	}
	if !strings.Contains(ld, "\"@context\": \"x\"") {
		t.Errorf("ld+json not pretty-printed: %q", ld)
	}
}

// TestFetchTextPassThrough: text/plain, text/markdown and text/csv pass
// through byte-identical, proving the historical behaviour is untouched
// (#248).
func TestFetchTextPassThrough(t *testing.T) {
	for _, tc := range []struct {
		name        string
		contentType string
		body        string
	}{
		{"plain", "text/plain", "line one\nline two\n"},
		{"plain with charset", "text/plain; charset=utf-8", "héllo wörld\n"},
		{"markdown", "text/markdown", "# Title\n\n- a\n- b\n"},
		{"csv", "text/csv", "a,b,c\n1,2,3\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := fetchRaw(t, tc.contentType, []byte(tc.body), nil, "")
			if err != nil {
				t.Fatalf("fetch: %v", err)
			}
			if out != tc.body {
				t.Errorf("pass-through changed body: got %q want %q", out, tc.body)
			}
		})
	}
}

// TestFetchBinaryDescriptor: binary content types return a short typed
// descriptor (content-type, byte length, filename) with zero bytes of
// stringified payload and no replacement characters (#248).
func TestFetchBinaryDescriptor(t *testing.T) {
	pdf := []byte("%PDF-1.4 fake pdf payload \x00\x01\xff\xfe")
	png := []byte("\x89PNG\r\n\x1a\n fake png payload")
	for _, tc := range []struct {
		name        string
		contentType string
		body        []byte
		urlPath     string
		headers     map[string]string
		wantLen     int
		wantFile    string
	}{
		{"pdf", "application/pdf", pdf, "", map[string]string{"Content-Disposition": `attachment; filename="report.pdf"`}, len(pdf), "report.pdf"},
		{"png", "image/png", png, "/img/photo.png", nil, len(png), "photo.png"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := fetchRaw(t, tc.contentType, tc.body, tc.headers, tc.urlPath)
			if err != nil {
				t.Fatalf("fetch: %v", err)
			}
			if !strings.Contains(out, "[fetch: non-text content-type, body not shown]") {
				t.Errorf("missing descriptor intro: %q", out)
			}
			if !strings.Contains(out, "content-type: "+tc.contentType) {
				t.Errorf("missing content-type line: %q", out)
			}
			if !strings.Contains(out, fmt.Sprintf("bytes: %d", tc.wantLen)) {
				t.Errorf("missing byte length %d: %q", tc.wantLen, out)
			}
			if !strings.Contains(out, "filename: "+tc.wantFile) {
				t.Errorf("missing filename %q: %q", tc.wantFile, out)
			}
			if strings.Contains(out, "fake pdf payload") || strings.Contains(out, "fake png payload") {
				t.Errorf("descriptor leaked payload bytes: %q", out)
			}
			if strings.ContainsRune(out, '\uFFFD') {
				t.Errorf("descriptor contains replacement characters: %q", out)
			}
			if len(out) > 512 {
				t.Errorf("descriptor not short: %d bytes", len(out))
			}
		})
	}
}

// TestFetchNoContentTypeFallsBackToPassThrough: a response without a
// Content-Type header keeps the previous pass-through behaviour instead of
// erroring (#248).
func TestFetchNoContentTypeFallsBackToPassThrough(t *testing.T) {
	body := "no content type header body"
	out, err := fetchRaw(t, "", []byte(body), nil, "")
	if err != nil || out != body {
		t.Fatalf("fetch = %q, %v; want pass-through", out, err)
	}
}

// TestFetchTruncationMarkers: hitting the read cap or the return cap yields
// an explicit machine-readable marker naming what was dropped (#248).
func TestFetchTruncationMarkers(t *testing.T) {
	t.Run("read cap", func(t *testing.T) {
		body := bytes.Repeat([]byte("y"), fetchReadCap+1024)
		out, err := fetchRaw(t, "text/plain", body, nil, "")
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		if !strings.Contains(out, "[fetch-truncated:") || !strings.Contains(out, "read cap") {
			t.Errorf("missing read-cap marker: %.80q...", out)
		}
	})
	t.Run("return cap", func(t *testing.T) {
		body := bytes.Repeat([]byte("z"), 1024*1024)
		out, err := fetchRaw(t, "text/plain", body, nil, "")
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		if !strings.HasPrefix(out, "\n[fetch-truncated:") || !strings.Contains(out, "return cap") {
			t.Errorf("missing return-cap marker at head: %.120q...", out)
		}
		if len(out) != HostReturnCap {
			t.Errorf("capped output len = %d, want %d", len(out), HostReturnCap)
		}
	})
}

// TestFetchRefusedPrivateSkipsExtraction: a refused address errors before any
// body is read or extracted, so extraction can never be reached for an
// address the policy rejects (#248).
func TestFetchRefusedPrivateSkipsExtraction(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html><body><p>SECRET ARTICLE PROSE</p></body></html>")
	}))
	defer srv.Close()
	// No allow_private entry: loopback must be refused before the body matters.
	_, err := run(t, &Fetch{}, fmt.Sprintf(`{"url":%q}`, srv.URL))
	if err == nil || !strings.Contains(err.Error(), "private/link-local range") {
		t.Fatalf("refused fetch error = %v", err)
	}
	if strings.Contains(err.Error(), "SECRET ARTICLE PROSE") {
		t.Error("extraction output leaked into the refusal error")
	}
}

// TestFetchCancellation: a context cancelled mid-body-read returns promptly
// and the response body is closed by Run's deferred Close (#248).
func TestFetchCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		chunk := bytes.Repeat([]byte("z"), 64*1024)
		for {
			if _, err := w.Write(chunk); err != nil {
				return // client went away
			}
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(20 * time.Millisecond)
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	f := &Fetch{AllowPrivate: []string{"127.0.0.0/8"}}
	done := make(chan struct{})
	var runErr error
	go func() {
		_, runErr = f.Run(ctx, json.RawMessage(fmt.Sprintf(`{"url":%q}`, srv.URL)))
		close(done)
	}()
	time.Sleep(150 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return promptly after cancellation")
	}
	if !errors.Is(runErr, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", runErr)
	}
}

// TestFetchHTMLBlockFlowAnchorKeepsDestination: an anchor wrapping block
// content (a linked card or article block) must not silently lose its href
// when the block boundaries flush lines (#248 Greptile review).
func TestFetchHTMLBlockFlowAnchorKeepsDestination(t *testing.T) {
	html := `<html><body><article><a href="/guide"><div>Getting started</div><div>Read the guide here</div></a>` +
		`<p>Plain paragraph</p><a href="/single"><p>Single block card</p></a></article></body></html>`
	out, err := fetchRaw(t, "text/html", []byte(html), nil, "")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	for _, want := range []string{
		"Getting started",
		"Read the guide here (/guide)",
		"Plain paragraph",
		"Single block card (/single)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "guide)") && strings.Contains(out, "Read the guide here\n") {
		// The href must be on the same line as the card text, not lost.
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, "Read the guide here") && !strings.Contains(line, "/guide") {
				t.Errorf("card line lost its destination: %q", line)
			}
		}
	}
}

// TestFetchHTMLListItemWithBlockChildKeepsMarker: a list item whose first
// child is a block element must render as a bullet, not as an orphan dash
// followed by unindented content (#248 Greptile review).
func TestFetchHTMLListItemWithBlockChildKeepsMarker(t *testing.T) {
	html := `<html><body><ul><li><div>alpha</div></li><li>beta</li><li><div><p>gamma</p></div></li></ul></body></html>`
	out, err := fetchRaw(t, "text/html", []byte(html), nil, "")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var bullets int
	for _, line := range lines {
		if strings.HasPrefix(line, "- alpha") || strings.HasPrefix(line, "- beta") || strings.HasPrefix(line, "- gamma") {
			bullets++
		}
		if line == "-" {
			t.Errorf("orphan dash line rendered:\n%s", out)
		}
		if strings.HasPrefix(line, "alpha") || strings.HasPrefix(line, "gamma") {
			t.Errorf("block-child list item lost its bullet: %q", line)
		}
	}
	if bullets != 3 {
		t.Errorf("want 3 bullet lines, got %d:\n%s", bullets, out)
	}
}

// TestFetchReturnCapMarkerCountsExactDroppedBytes: the return-cap marker must
// report the exact number of content bytes omitted, excluding the marker's
// own length and any UTF-8 boundary adjustment (#248 Greptile review).
func TestFetchReturnCapMarkerCountsExactDroppedBytes(t *testing.T) {
	body := bytes.Repeat([]byte("z"), 1024*1024)
	out, err := fetchRaw(t, "text/plain", body, nil, "")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !strings.HasPrefix(out, "\n[fetch-truncated:") {
		t.Fatalf("missing return-cap marker: %.120q...", out)
	}
	markerEnd := strings.Index(out, "]\n") // end of the marker line
	if markerEnd < 0 {
		t.Fatalf("marker line not terminated: %.120q...", out)
	}
	markerLen := markerEnd + 2
	head := out[:markerEnd]
	if !strings.Contains(head, "bytes dropped") {
		t.Fatalf("marker text malformed: %q", head)
	}
	fields := strings.Fields(head)
	dropped, err := strconv.Atoi(fields[len(fields)-3])
	if err != nil {
		t.Fatalf("parse dropped count from %q: %v", head, err)
	}
	// The count must equal the content bytes actually omitted: the total
	// minus what survives after the marker's own length is carved out.
	retained := len(out) - markerLen
	if dropped != len(body)-retained {
		t.Errorf("dropped = %d, want %d (marker length %d must not count as payload)", dropped, len(body)-retained, markerLen)
	}
	if len(out) > HostReturnCap {
		t.Errorf("capped output len = %d exceeds %d", len(out), HostReturnCap)
	}
}
