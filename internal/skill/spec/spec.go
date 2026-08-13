// Package spec implements the Agent Skills specification
// (https://agentskills.io/specification): the single source of truth for
// SKILL.md frontmatter parsing, validation, and serialization. Every read
// and write path — discovery (#390), distill_skill and the learn loop
// (#396), the skill installer, activate/deactivate — uses these rules so
// they cannot drift (docs/plan.md, "Skills & memory").
//
// The package is deliberately dependency-free so internal/skill,
// internal/memory, internal/skillinstall, and the plugin loader can all
// import it without cycles.
package spec

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Field length limits from the specification's frontmatter table.
const (
	MaxNameLength          = 64
	MaxDescriptionLength   = 1024
	MaxCompatibilityLength = 500
)

// ErrInvalid rejects a skill that does not conform to the Agent Skills
// specification. Callers distinguish read-side (skip) from write-side
// (refuse) enforcement; the error itself is shared.
var ErrInvalid = errors.New("skill does not conform to the Agent Skills spec")

// WaffleStatusKey is the frontmatter key under which waffle records skill
// activation state (metadata.waffle/status), aligned with the
// dev.mattriley.waffle extension namespace (#394, #396). The legacy
// top-level status field is still read for existing on-disk skills.
const WaffleStatusKey = "metadata.waffle/status"

// StatusField returns the activation status from parsed frontmatter fields:
// the waffle metadata key first, then the legacy top-level status.
func StatusField(fields map[string]string) string {
	if v := fields[WaffleStatusKey]; v != "" {
		return v
	}
	return fields["status"]
}

// ValidName reports whether name satisfies the specification's name
// constraints: 1–64 characters of lowercase alphanumerics and hyphens,
// no leading or trailing hyphen, no consecutive hyphens.
func ValidName(name string) bool {
	if len(name) == 0 || len(name) > MaxNameLength {
		return false
	}
	for i := 0; i < len(name); i++ {
		if !isNameChar(name[i]) {
			return false
		}
	}
	if name[0] == '-' || name[len(name)-1] == '-' {
		return false
	}
	return !strings.Contains(name, "--")
}

func isNameChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-'
}

// Validate checks parsed frontmatter fields against the specification.
// dirName is the skill's parent directory name; when non-empty the name
// must match it. body is the Markdown content after the frontmatter (the
// spec places no format restrictions on it). The error names the offending
// field.
func Validate(name, description string, fields map[string]string, body, dirName string) error {
	// Nil maps are treated as empty so callers may pass nil; Go map reads
	// are already nil-safe, but making the contract explicit avoids relying
	// on that for a validation entry point.
	if fields == nil {
		fields = map[string]string{}
	}
	if !ValidName(name) {
		return fmt.Errorf("%w: name %q must be 1-%d chars of [a-z0-9-], not start/end with a hyphen, and contain no consecutive hyphens",
			ErrInvalid, name, MaxNameLength)
	}
	if dirName != "" && name != dirName {
		return fmt.Errorf("%w: name %q must match the parent directory name %q", ErrInvalid, name, dirName)
	}
	// The spec's limits are in characters, not bytes, and a whitespace-only
	// description is empty: trim first, then count runes.
	description = strings.TrimSpace(description)
	if description == "" {
		return fmt.Errorf("%w: description is required and must not be empty", ErrInvalid)
	}
	if runes := utf8.RuneCountInString(description); runes > MaxDescriptionLength {
		return fmt.Errorf("%w: description must be at most %d characters, got %d", ErrInvalid, MaxDescriptionLength, runes)
	}
	if compatibility := strings.TrimSpace(fields["compatibility"]); compatibility != "" {
		if runes := utf8.RuneCountInString(compatibility); runes > MaxCompatibilityLength {
			return fmt.Errorf("%w: compatibility must be at most %d characters, got %d",
				ErrInvalid, MaxCompatibilityLength, runes)
		}
	}
	for key := range fields {
		if strings.HasPrefix(key, "metadata.") {
			subkey := strings.TrimPrefix(key, "metadata.")
			if subkey == "" {
				return fmt.Errorf("%w: metadata key must not be empty", ErrInvalid)
			}
		}
	}
	return nil
}

// ParseFrontmatter splits raw SKILL.md into its frontmatter fields and
// Markdown body. The frontmatter block is required ("---" delimited) and
// line-oriented: "key: value" scalars plus a single-level "metadata:" block
// whose entries flatten to "metadata.<subkey>" keys. Unknown top-level
// fields are preserved as-is so waffle's legacy fields (status, provenance,
// …) keep round-tripping until #396 relocates them. Any malformed line
// rejects the file with a field-named error.
func ParseFrontmatter(raw string) (fields map[string]string, body string, err error) {
	fields = map[string]string{}
	if !strings.HasPrefix(raw, "---\n") {
		return nil, "", fmt.Errorf("%w: SKILL.md requires leading frontmatter", ErrInvalid)
	}
	rest := strings.TrimPrefix(raw, "---\n")
	end := strings.Index(rest, "\n---")
	switch {
	case end >= 0:
	case strings.HasPrefix(rest, "---\n"):
		// Empty frontmatter block: the closing "---" is the first line, so
		// the \n--- delimiter does not occur inside rest. Detect it so the
		// caller sees "missing required fields" rather than "not closed".
		end = 0
	default:
		return nil, "", fmt.Errorf("%w: SKILL.md frontmatter is not closed", ErrInvalid)
	}
	after := rest[end+4:]
	if after != "" && !strings.HasPrefix(after, "\n") {
		return nil, "", fmt.Errorf("%w: invalid SKILL.md frontmatter delimiter", ErrInvalid)
	}
	body = strings.TrimPrefix(after, "\n")
	body = strings.TrimPrefix(body, "\n")

	inMetadata := false
	for lineIndex, line := range strings.Split(rest[:end], "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(line, " ") {
			if !inMetadata {
				return nil, "", fmt.Errorf("%w: indented frontmatter line is only allowed inside a metadata block", ErrInvalid)
			}
			if len(line) < 2 || line[:2] != "  " || strings.HasPrefix(line, "    ") {
				return nil, "", fmt.Errorf("%w: metadata supports exactly one nesting level", ErrInvalid)
			}
			child := strings.TrimSpace(line)
			subkey, value, found := strings.Cut(child, ":")
			subkey = strings.TrimSpace(subkey)
			value = strings.TrimSpace(value)
			if !found || !metadataSubkeyPattern(subkey) {
				return nil, "", fmt.Errorf("%w: invalid metadata entry %q on line %d", ErrInvalid, child, lineIndex+1)
			}
			parsed, err := unquote(value)
			if err != nil {
				return nil, "", fmt.Errorf("%w: metadata entry %q: %v", ErrInvalid, child, err)
			}
			if _, duplicate := fields["metadata."+subkey]; duplicate {
				return nil, "", fmt.Errorf("%w: duplicate frontmatter key %q", ErrInvalid, "metadata."+subkey)
			}
			fields["metadata."+subkey] = parsed
			continue
		}
		inMetadata = false
		key, value, found := strings.Cut(line, ":")
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if !found || !frontmatterKeyPattern(key) {
			return nil, "", fmt.Errorf("%w: invalid frontmatter line %q on line %d", ErrInvalid, line, lineIndex+1)
		}
		if _, duplicate := fields[key]; duplicate {
			return nil, "", fmt.Errorf("%w: duplicate frontmatter key %q", ErrInvalid, key)
		}
		if key == "metadata" && value == "" {
			inMetadata = true
			continue
		}
		if key == "metadata" && value != "" {
			return nil, "", fmt.Errorf("%w: metadata must be a block of key: value entries", ErrInvalid)
		}
		parsed, err := unquote(value)
		if err != nil {
			return nil, "", fmt.Errorf("%w: frontmatter field %q: %v", ErrInvalid, key, err)
		}
		fields[key] = parsed
	}
	return fields, body, nil
}

func frontmatterKeyPattern(key string) bool {
	if key == "" {
		return false
	}
	if !isLetter(key[0]) {
		return false
	}
	for i := 0; i < len(key); i++ {
		if !isKeyChar(key[i]) {
			return false
		}
	}
	return true
}

func isLetter(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z'
}

func isKeyChar(c byte) bool {
	return isLetter(c) || c >= '0' && c <= '9' || c == '_' || c == '-'
}

func metadataSubkeyPattern(key string) bool {
	if key == "" {
		return false
	}
	for i := 0; i < len(key); i++ {
		if !isMetadataChar(key[i]) {
			return false
		}
	}
	return true
}

func isMetadataChar(c byte) bool {
	return isKeyChar(c) || c == '.' || c == '/'
}

// MarshalSKILL renders frontmatter fields plus body as a SKILL.md file whose
// frontmatter re-parses to the same fields. Keys prefixed "metadata." are
// emitted as a single-level metadata block. Values are YAML-escaped — never
// Go %q/strconv.Quote escapes, whose octal forms are not YAML.
func MarshalSKILL(fields map[string]string, body string) []byte {
	var buf bytes.Buffer
	_ = WriteFrontmatter(&buf, fields, body)
	return buf.Bytes()
}

// WriteFrontmatter writes the marshaled SKILL.md to w. See MarshalSKILL.
func WriteFrontmatter(w io.Writer, fields map[string]string, body string) error {
	if _, err := io.WriteString(w, "---\n"); err != nil {
		return err
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var metadataKeys []string
	for _, key := range keys {
		if strings.HasPrefix(key, "metadata.") {
			metadataKeys = append(metadataKeys, key)
			continue
		}
		if _, err := fmt.Fprintf(w, "%s: %s\n", key, yamlScalar(fields[key])); err != nil {
			return err
		}
	}
	if len(metadataKeys) > 0 {
		if _, err := io.WriteString(w, "metadata:\n"); err != nil {
			return err
		}
		for _, key := range metadataKeys {
			subkey := strings.TrimPrefix(key, "metadata.")
			if _, err := fmt.Fprintf(w, "  %s: %s\n", subkey, yamlScalar(fields[key])); err != nil {
				return err
			}
		}
	}
	if _, err := io.WriteString(w, "---\n"); err != nil {
		return err
	}
	body = strings.TrimRight(body, "\n")
	if body != "" {
		if _, err := io.WriteString(w, "\n"+body+"\n"); err != nil {
			return err
		}
	}
	return nil
}

// yamlScalar returns value as a YAML scalar: plain when it is unambiguously
// plain-safe, otherwise double-quoted with YAML escapes.
func yamlScalar(value string) string {
	if value == "" {
		return `""`
	}
	if plainSafe(value) {
		return value
	}
	return `"` + yamlEscape(value) + `"`
}

func plainSafe(value string) bool {
	if strings.HasPrefix(value, " ") || strings.HasSuffix(value, " ") {
		return false
	}
	if strings.ContainsAny(value[:1], "-?:,[]{}#&*!|>'\"%@`") {
		return false
	}
	if strings.Contains(value, ": ") || strings.Contains(value, " #") {
		return false
	}
	for _, r := range value {
		if r == '\n' || r == '\r' || r == '\t' || r < 0x20 || r == 0x7f {
			return false
		}
	}
	// YAML 1.1 reserves these bare words as booleans and null (and ~ is
	// null); unquoted, another client would parse a string value like
	// "true" as a boolean rather than text. Numeric-looking strings get
	// the same treatment so "123" round-trips as a string everywhere.
	switch strings.ToLower(value) {
	case "true", "false", "yes", "no", "on", "off", "y", "n", "null", "~":
		return false
	}
	return !looksNumeric(value)
}

// looksNumeric reports whether value parses as a YAML 1.1 int or float
// (decimal with optional sign, fraction, exponent, and digit underscores,
// plus the 0x/0o/0b prefixed integer forms). Such strings are quoted by
// yamlScalar so they stay strings when read by other clients.
func looksNumeric(value string) bool {
	s := value
	i := 0
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}
	digits := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '_') {
		if s[i] >= '0' && s[i] <= '9' {
			digits++
		}
		i++
	}
	if digits == 0 {
		return false
	}
	if i < len(s) && s[i] == '.' {
		i++
		for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '_') {
			i++
		}
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		exponent := 0
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			exponent++
			i++
		}
		if exponent == 0 {
			return false
		}
	}
	if i == len(s) {
		return true
	}
	// Prefixed integer forms: 0x1F, 0o17, 0b101.
	return s[0] == '0' && (s[1] == 'x' || s[1] == 'X' || s[1] == 'o' || s[1] == 'O' ||
		s[1] == 'b' || s[1] == 'B')
}

// yamlEscape escapes value for a YAML double-quoted scalar. Only the escapes
// YAML defines are emitted (no Go octal escapes).
func yamlEscape(value string) string {
	var buf strings.Builder
	for _, r := range value {
		switch r {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '\n':
			buf.WriteString(`\n`)
		case '\t':
			buf.WriteString(`\t`)
		case '\r':
			buf.WriteString(`\r`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&buf, `\u%04X`, r)
			} else {
				buf.WriteRune(r)
			}
		}
	}
	return buf.String()
}

// unquote parses a YAML scalar value: plain, double-quoted, or
// single-quoted. Double-quoted strings accept the YAML escapes the writer
// emits; unknown escapes are rejected rather than guessed at.
func unquote(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	switch value[0] {
	case '"':
		if len(value) < 2 || value[len(value)-1] != '"' {
			return "", errors.New("unmatched double quote")
		}
		return unescapeDouble(value[1 : len(value)-1])
	case '\'':
		if len(value) < 2 || value[len(value)-1] != '\'' {
			return "", errors.New("unmatched single quote")
		}
		return strings.ReplaceAll(value[1:len(value)-1], "''", "'"), nil
	default:
		if !plainSafe(value) {
			return "", errors.New("invalid plain frontmatter scalar")
		}
		return value, nil
	}
}

func unescapeDouble(s string) (string, error) {
	var buf strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			if s[i] == '"' {
				return "", errors.New("unescaped double quote in double-quoted scalar")
			}
			buf.WriteByte(s[i])
			continue
		}
		i++
		if i >= len(s) {
			return "", errors.New("dangling escape in double-quoted scalar")
		}
		switch s[i] {
		case '"':
			buf.WriteByte('"')
		case '\\':
			buf.WriteByte('\\')
		case 'n':
			buf.WriteByte('\n')
		case 't':
			buf.WriteByte('\t')
		case 'r':
			buf.WriteByte('\r')
		case 'u':
			if i+4 >= len(s) {
				return "", errors.New("truncated \\u escape")
			}
			hex := s[i+1 : i+5]
			code, err := strconv.ParseUint(hex, 16, 32)
			if err != nil {
				return "", fmt.Errorf("invalid \\u escape %q", hex)
			}
			buf.WriteRune(rune(code))
			i += 4
		case 'U':
			if i+8 >= len(s) {
				return "", errors.New("truncated \\U escape")
			}
			hex := s[i+1 : i+9]
			code, err := strconv.ParseUint(hex, 16, 32)
			if err != nil {
				return "", fmt.Errorf("invalid \\U escape %q", hex)
			}
			if code > 0x10FFFF {
				return "", fmt.Errorf("invalid \\U escape %q", hex)
			}
			buf.WriteRune(rune(code))
			i += 8
		default:
			return "", fmt.Errorf("unknown escape \\%c in double-quoted scalar", s[i])
		}
	}
	return buf.String(), nil
}
