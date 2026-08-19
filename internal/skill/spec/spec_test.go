package spec

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestValidName(t *testing.T) {
	// Valid names from the specification.
	for _, name := range []string{"pdf-processing", "data-analysis", "code-review", "a", "lint3r", "x-9"} {
		if !ValidName(name) {
			t.Errorf("ValidName(%q) = false, want true", name)
		}
	}
	// Invalid names from the specification plus boundary cases.
	for _, name := range []string{
		"", "PDF-Processing", "-pdf", "pdf--processing", "pdf-", "a--b", "under_score",
		"spa ce", strings.Repeat("a", 65),
	} {
		if ValidName(name) {
			t.Errorf("ValidName(%q) = true, want false", name)
		}
	}
	if !ValidName(strings.Repeat("a", 64)) {
		t.Error("64-char name rejected")
	}
}

func TestValidate(t *testing.T) {
	valid := map[string]string{}
	if err := Validate("pdf-processing", "Extracts text from PDF files.", valid, "# Body", "pdf-processing"); err != nil {
		t.Errorf("valid skill rejected: %v", err)
	}

	cases := []struct {
		name        string
		description string
		fields      map[string]string
		dirName     string
		wantSub     string
	}{
		{name: "PDF-Processing", description: "ok", wantSub: "name"},
		{name: "-pdf", description: "ok", wantSub: "name"},
		{name: "pdf--processing", description: "ok", wantSub: "name"},
		{name: "pdf-", description: "ok", wantSub: "name"},
		{name: strings.Repeat("a", 65), description: "ok", wantSub: "name"},
		{name: "", description: "ok", wantSub: "name"},
		{name: "ok", description: "", wantSub: "description"},
		{name: "ok", description: strings.Repeat("d", 1025), wantSub: "description"},
		{name: "ok", description: "fine", fields: map[string]string{"compatibility": strings.Repeat("c", 501)}, wantSub: "compatibility"},
		{name: "renamed", description: "ok", dirName: "directory-name", wantSub: "directory"},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%q", tc.name), func(t *testing.T) {
			err := Validate(tc.name, tc.description, tc.fields, "body", tc.dirName)
			if err == nil {
				t.Fatalf("Validate accepted, want error naming %q", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not name %q", err, tc.wantSub)
			}
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("errors.Is(err, ErrInvalid) = false")
			}
		})
	}

	// Boundary: 1024-char description and 500-char compatibility are valid.
	long := strings.Repeat("d", 1024)
	if err := Validate("ok", long, map[string]string{"compatibility": strings.Repeat("c", 500)}, "b", ""); err != nil {
		t.Errorf("boundary skill rejected: %v", err)
	}
	// Empty dirName skips the directory-match rule.
	if err := Validate("any-name", "ok", nil, "b", ""); err != nil {
		t.Errorf("dir-less validation rejected: %v", err)
	}
	// Whitespace-only description is empty (review fix).
	if err := Validate("ok", "   \t  ", nil, "b", ""); err == nil {
		t.Error("whitespace-only description accepted")
	}
	// Limits are characters, not bytes: a 1024-rune multi-byte description
	// is valid; 1025 runes are not (review fix).
	multiByte := strings.Repeat("é", 1024)
	if err := Validate("ok", multiByte, nil, "b", ""); err != nil {
		t.Errorf("1024-rune multi-byte description rejected: %v", err)
	}
	if err := Validate("ok", strings.Repeat("é", 1025), nil, "b", ""); err == nil {
		t.Error("1025-rune description accepted")
	}
}

func TestParseFrontmatter(t *testing.T) {
	fields, body, err := ParseFrontmatter("---\nname: pdf-processing\ndescription: Extracts text.\n---\n\n# Body\n")
	if err != nil {
		t.Fatalf("ParseFrontmatter: %v", err)
	}
	if fields["name"] != "pdf-processing" || fields["description"] != "Extracts text." {
		t.Errorf("fields = %v", fields)
	}
	if body != "# Body\n" {
		t.Errorf("body = %q", body)
	}

	// Quoted values, single and double.
	fields, _, err = ParseFrontmatter("---\nname: x\ndescription: 'say \"hi\"'\nstatus: \"inactive\"\n---\n")
	if err != nil {
		t.Fatalf("quoted: %v", err)
	}
	if fields["description"] != `say "hi"` || fields["status"] != "inactive" {
		t.Errorf("quoted fields = %v", fields)
	}

	// metadata block flattens to metadata.<subkey>.
	fields, _, err = ParseFrontmatter("---\nname: x\ndescription: d\nmetadata:\n  x-waffle/status: inactive\n  author: example-org\n---\n")
	if err != nil {
		t.Fatalf("metadata: %v", err)
	}
	if fields["metadata.x-waffle/status"] != "inactive" || fields["metadata.author"] != "example-org" {
		t.Errorf("metadata fields = %v", fields)
	}

	// Empty frontmatter block closes cleanly: the error is about missing
	// fields, not an unclosed block (review fix).
	if _, _, err := ParseFrontmatter("---\n---\n\nBody\n"); err != nil {
		t.Errorf("empty frontmatter = %v, want parseable", err)
	}

	cases := []struct {
		name    string
		raw     string
		wantSub string
	}{
		{name: "missing frontmatter", raw: "# no frontmatter\n", wantSub: "frontmatter"},
		{name: "unclosed frontmatter", raw: "---\nname: x\n", wantSub: "not closed"},
		{name: "invalid key", raw: "---\nname: x\n1bad: y\n---\n", wantSub: "invalid frontmatter line"},
		{name: "duplicate key", raw: "---\nname: a\nname: b\n---\n", wantSub: "duplicate"},
		{name: "metadata not a block", raw: "---\nname: x\nmetadata: flat\n---\n", wantSub: "metadata"},
		{name: "nested metadata", raw: "---\nname: x\nmetadata:\n  a:\n    b: c\n---\n", wantSub: "nesting"},
		{name: "indented outside metadata", raw: "---\nname: x\n  description: d\n---\n", wantSub: "metadata"},
		{name: "unknown escape", raw: "---\nname: x\ndescription: \"a\\q\"\n---\n", wantSub: "unknown escape"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := ParseFrontmatter(tc.raw); err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error = %v, want naming %q", err, tc.wantSub)
			}
		})
	}
}

func TestMarshalSKILLRoundTrip(t *testing.T) {
	// Descriptions exercising every YAML trap the old %q writer got wrong:
	// quotes, colon+space, hashes, non-ASCII, control characters.
	descriptions := []string{
		"plain description",
		`has "quotes" and 'single'`,
		"colon: space and #hash",
		"leading # hash",
		"café ☕ unicode",
		"tab\tand newline\ninside",
		`back\slash and "dquote"`,
		"trailing space ",
	}
	for _, description := range descriptions {
		fields := map[string]string{
			"name":                     "round-trip",
			"description":              description,
			"metadata.x-waffle/status": "inactive",
			"status":                   "inactive",
		}
		raw := string(MarshalSKILL(fields, "# Body with 'quotes'\nand : colon\n"))
		if strings.Contains(raw, `\x`) || strings.Contains(raw, `\1`) || strings.Contains(raw, `\0`) {
			t.Errorf("Go-style octal escape leaked for %q: %q", description, raw)
		}
		got, body, err := ParseFrontmatter(raw)
		if err != nil {
			t.Fatalf("round-trip parse for %q: %v\n%s", description, err, raw)
		}
		if got["description"] != description {
			t.Errorf("description round-trip = %q, want %q\n%s", got["description"], description, raw)
		}
		if got["name"] != "round-trip" {
			t.Errorf("name round-trip = %q", got["name"])
		}
		if got["metadata.x-waffle/status"] != "inactive" {
			t.Errorf("metadata round-trip = %q", got["metadata.x-waffle/status"])
		}
		if body != "# Body with 'quotes'\nand : colon\n" {
			t.Errorf("body round-trip = %q", body)
		}
	}

	// YAML 1.1 keywords and numeric-looking strings are double-quoted so
	// other clients never parse them as bools/null/numbers (review fix).
	for _, keyword := range []string{"true", "false", "null", "~", "yes", "on", "123", "1.5", "-3e2", "0x1F"} {
		raw := string(MarshalSKILL(map[string]string{"name": "n", "description": keyword}, ""))
		if !strings.Contains(raw, `description: "`+keyword+`"`) {
			t.Errorf("keyword %q not quoted: %s", keyword, raw)
		}
		got, _, err := ParseFrontmatter(raw)
		if err != nil || got["description"] != keyword {
			t.Errorf("keyword %q round-trip = %q, %v", keyword, got["description"], err)
		}
	}

	// Empty body.
	raw := string(MarshalSKILL(map[string]string{"name": "n", "description": "d"}, ""))
	if strings.HasSuffix(raw, "---\n\n") {
		t.Errorf("empty body emitted trailing blank: %q", raw)
	}
	if got, _, err := ParseFrontmatter(raw); err != nil || got["name"] != "n" {
		t.Errorf("empty-body round-trip = %v, %v", got, err)
	}
}

// TestParseFrontmatterUnicodeEscapeBounds pins the boundary values of both
// unicode escapes: \u tops out at four hex digits and \U must refuse anything
// above the Unicode maximum rather than truncate into a rune (CodeQL
// go/incorrect-integer-conversion).
func TestParseFrontmatterUnicodeEscapeBounds(t *testing.T) {
	cases := []struct {
		name    string
		escape  string
		want    string
		wantSub string
	}{
		{name: "u minimum", escape: "\\u0041", want: "A"},
		{name: "u maximum", escape: "\\uFFFF", want: "\uffff"},
		{name: "U basic", escape: `\U0001F600`, want: "\U0001f600"},
		{name: "U maximum", escape: `\U0010FFFF`, want: "\U0010ffff"},
		{name: "U above maximum", escape: `\U00110000`, wantSub: `invalid \U escape`},
		{name: "U far above maximum", escape: `\UFFFFFFFF`, wantSub: `invalid \U escape`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fields, _, err := ParseFrontmatter("---\nname: x\ndescription: \"" + tc.escape + "\"\n---\n")
			if tc.wantSub != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
					t.Fatalf("error = %v, want naming %q", err, tc.wantSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseFrontmatter: %v", err)
			}
			if fields["description"] != tc.want {
				t.Errorf("description = %q, want %q", fields["description"], tc.want)
			}
		})
	}
}
