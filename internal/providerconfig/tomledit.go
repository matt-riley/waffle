package providerconfig

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/matt-riley/waffle/internal/config"
)

func setConnection(doc *tomlDocument, name string, connection config.ProviderConnection) {
	table := "providers." + name
	doc.setValue(table, "type", strconv.Quote(connection.Type))
	doc.setOptional(table, "api_key", connection.APIKey)
	doc.setOptional(table, "base_url", connection.BaseURL)
	doc.setOptionalInt(table, "max_tokens", connection.MaxTokens)
}

func setModel(doc *tomlDocument, alias string, target config.ModelTarget) {
	table := "models." + alias
	doc.setValue(table, "provider", strconv.Quote(target.Provider))
	doc.setValue(table, "model", strconv.Quote(target.Model))
	doc.setOptionalInt(table, "max_tokens", target.MaxTokens)
}

var (
	tableHeaderRE    = regexp.MustCompile(`^\s*\[([^\[\]]+)\]\s*(?:#.*)?$`)
	anyTableHeaderRE = regexp.MustCompile(`^\s*\[\[?[^\]]+\]\]?\s*(?:#.*)?$`)
)

// tomlDocument is a narrow, syntax-aware editor for operator-owned TOML. It
// preserves all lines outside the exact managed keys and tables.
type tomlDocument struct{ lines []string }

func newTOMLDocument(raw []byte) *tomlDocument {
	text := string(raw)
	if text == "" {
		return &tomlDocument{}
	}
	return &tomlDocument{lines: strings.Split(strings.TrimSuffix(text, "\n"), "\n")}
}

func (d *tomlDocument) bytes() []byte {
	if len(d.lines) == 0 {
		return nil
	}
	return []byte(strings.Join(d.lines, "\n") + "\n")
}

func (d *tomlDocument) tableSpan(table string) (start, end int, ok bool) {
	for i, line := range d.lines {
		match := tableHeaderRE.FindStringSubmatch(line)
		if match == nil || strings.TrimSpace(match[1]) != table {
			continue
		}
		end = len(d.lines)
		for j := i + 1; j < len(d.lines); j++ {
			if anyTableHeaderRE.MatchString(d.lines[j]) {
				end = j
				break
			}
		}
		return i, end, true
	}
	return 0, 0, false
}

func (d *tomlDocument) ensureTable(table string) (start, end int) {
	if start, end, ok := d.tableSpan(table); ok {
		return start, end
	}
	if len(d.lines) > 0 && strings.TrimSpace(d.lines[len(d.lines)-1]) != "" {
		d.lines = append(d.lines, "")
	}
	d.lines = append(d.lines, "["+table+"]")
	return len(d.lines) - 1, len(d.lines)
}

func (d *tomlDocument) setValue(table, key, rendered string) {
	start, end := d.ensureTable(table)
	keyRE := regexp.MustCompile(`^\s*` + regexp.QuoteMeta(key) + `\s*=`)
	for i := start + 1; i < end; i++ {
		if !keyRE.MatchString(d.lines[i]) {
			continue
		}
		comment := inlineComment(d.lines[i])
		d.lines[i] = key + " = " + rendered + comment
		return
	}
	d.lines = append(d.lines[:end], append([]string{key + " = " + rendered}, d.lines[end:]...)...)
}

// SetTableBool returns raw with table.key set to value, creating the table or
// the key when either is absent and preserving every other line, comment, and
// inline comment exactly.
//
// It is the one syntax-aware config.toml editor in Waffle. `waffle setup` uses
// it to flip [dashboard] enabled (#192 AC3) rather than growing a second
// rewriter that would drift from this one. Callers own validation: stage the
// result, re-read it with config.Load, and only then replace config.toml.
func SetTableBool(raw []byte, table, key string, value bool) []byte {
	doc := newTOMLDocument(raw)
	doc.setValue(table, key, strconv.FormatBool(value))
	return doc.bytes()
}

func (d *tomlDocument) deleteValue(table, key string) {
	start, end, ok := d.tableSpan(table)
	if !ok {
		return
	}
	keyRE := regexp.MustCompile(`^\s*` + regexp.QuoteMeta(key) + `\s*=`)
	for i := start + 1; i < end; i++ {
		if keyRE.MatchString(d.lines[i]) {
			d.lines = append(d.lines[:i], d.lines[i+1:]...)
			return
		}
	}
}

func (d *tomlDocument) setOptional(table, key, value string) {
	if value == "" {
		d.deleteValue(table, key)
		return
	}
	d.setValue(table, key, strconv.Quote(value))
}

func (d *tomlDocument) setOptionalInt(table, key string, value int) {
	if value == 0 {
		d.deleteValue(table, key)
		return
	}
	d.setValue(table, key, strconv.Itoa(value))
}

// setOptionalStrings writes a TOML string array, deleting the key when the
// list is empty so a profile never keeps an empty policy list behind (#194).
func (d *tomlDocument) setOptionalStrings(table, key string, values []string) {
	if len(values) == 0 {
		d.deleteValue(table, key)
		return
	}
	d.setValue(table, key, renderStringArray(values))
}

func (d *tomlDocument) deleteTable(table string) {
	start, end, ok := d.tableSpan(table)
	if !ok {
		return
	}
	d.lines = append(d.lines[:start], d.lines[end:]...)
}

// deleteTableTree removes a table and every sub-table beneath it. Deleting
// only [agent.profile.x] would leave [agent.profile.x.tools] behind, and TOML
// resurrects the parent from the orphan on the next load (#194).
func (d *tomlDocument) deleteTableTree(table string) {
	d.deleteTable(table)
	prefix := table + "."
	for {
		name, ok := d.firstTableWithPrefix(prefix)
		if !ok {
			return
		}
		d.deleteTable(name)
	}
}

func (d *tomlDocument) firstTableWithPrefix(prefix string) (string, bool) {
	for _, line := range d.lines {
		match := tableHeaderRE.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		if name := strings.TrimSpace(match[1]); strings.HasPrefix(name, prefix) {
			return name, true
		}
	}
	return "", false
}

func inlineComment(line string) string {
	quoted := false
	escaped := false
	for i, r := range line {
		if escaped {
			escaped = false
			continue
		}
		if r == '\\' && quoted {
			escaped = true
			continue
		}
		if r == '"' {
			quoted = !quoted
			continue
		}
		if r == '#' && !quoted {
			return " " + strings.TrimSpace(line[i:])
		}
	}
	return ""
}
