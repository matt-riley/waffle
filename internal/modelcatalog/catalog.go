// Package modelcatalog defines provider-neutral model catalogue data.
package modelcatalog

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/matt-riley/waffle/internal/config"
)

const (
	MaxModels     = 10_000
	MaxFieldBytes = 4 * 1024
)

type Model struct {
	ID            string   `json:"id"`
	DisplayName   string   `json:"display_name,omitempty"`
	Owner         string   `json:"owner,omitempty"`
	ContextWindow int64    `json:"context_window,omitempty"`
	Capabilities  []string `json:"capabilities,omitempty"`
}

type Source interface {
	ListModels(context.Context) ([]Model, error)
}

type Connection struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	BaseURL string `json:"base_url"`
	ScopeID string `json:"scope_id"`
}

func Normalize(models []Model) ([]Model, error) {
	if len(models) > MaxModels {
		return nil, fmt.Errorf("model catalogue has %d entries; maximum is %d", len(models), MaxModels)
	}
	for i, model := range models {
		if model.ID == "" {
			return nil, fmt.Errorf("model %d has an empty ID", i)
		}
		if model.ContextWindow < 0 {
			return nil, fmt.Errorf("model %q has a negative context window", model.ID)
		}
		fields := []struct {
			name  string
			value string
		}{
			{name: "ID", value: model.ID},
			{name: "display name", value: model.DisplayName},
			{name: "owner", value: model.Owner},
		}
		for _, field := range fields {
			if err := validateField(field.name, field.value); err != nil {
				return nil, fmt.Errorf("model %d: %w", i, err)
			}
		}
		for capabilityIndex, capability := range model.Capabilities {
			if err := validateField("capability", capability); err != nil {
				return nil, fmt.Errorf("model %d capability %d: %w", i, capabilityIndex, err)
			}
		}
	}

	normalized := make([]Model, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		if _, ok := seen[model.ID]; ok {
			continue
		}
		seen[model.ID] = struct{}{}
		model.Capabilities = append([]string(nil), model.Capabilities...)
		sort.Strings(model.Capabilities)
		normalized = append(normalized, model)
	}
	sort.Slice(normalized, func(i, j int) bool {
		return normalized[i].ID < normalized[j].ID
	})
	return normalized, nil
}

func Search(models []Model, query string) []Model {
	query = strings.ToLower(query)
	matches := make([]Model, 0)
	for _, model := range models {
		if strings.Contains(strings.ToLower(model.ID), query) ||
			strings.Contains(strings.ToLower(model.DisplayName), query) {
			matches = append(matches, model)
		}
	}
	return matches
}

func AliasFor(upstream string) (string, error) {
	var alias strings.Builder
	separator := false
	for _, r := range strings.ToLower(upstream) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			if separator && alias.Len() > 0 {
				alias.WriteByte('-')
			}
			alias.WriteRune(r)
			separator = false
			continue
		}
		separator = true
	}
	value := alias.String()
	if len(value) > config.ProviderConnectionNameMax {
		value = value[:config.ProviderConnectionNameMax]
		value = strings.TrimRight(value, "-")
	}
	if !config.ValidModelAlias(value) {
		return "", fmt.Errorf("cannot derive a valid model alias from %q", upstream)
	}
	return value, nil
}

func SafeText(value string) string {
	var safe strings.Builder
	for _, r := range value {
		if !unicode.IsControl(r) {
			safe.WriteRune(r)
			continue
		}
		switch r {
		case '\a':
			safe.WriteString(`\a`)
		case '\b':
			safe.WriteString(`\b`)
		case '\f':
			safe.WriteString(`\f`)
		case '\n':
			safe.WriteString(`\n`)
		case '\r':
			safe.WriteString(`\r`)
		case '\t':
			safe.WriteString(`\t`)
		case '\v':
			safe.WriteString(`\v`)
		default:
			_, _ = fmt.Fprintf(&safe, `\u%04x`, r)
		}
	}
	return safe.String()
}

func validateField(name, value string) error {
	if len(value) > MaxFieldBytes {
		return fmt.Errorf("%s is %d bytes; maximum is %d", name, len(value), MaxFieldBytes)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return fmt.Errorf("%s contains control character %U", name, r)
		}
	}
	return nil
}

// RedactText replaces every occurrence of the private values with
// "[REDACTED]". Empty private values are ignored.
func RedactText(value string, private ...string) string {
	for _, privateValue := range private {
		if privateValue != "" {
			value = strings.ReplaceAll(value, privateValue, "[REDACTED]")
		}
	}
	return value
}

// RedactModels returns a copy of models with the private values scrubbed from
// every text field. CLI and Desk used to each own a byte-identical copy of
// this walker; keep it here so both surfaces share one (#289).
func RedactModels(models []Model, private ...string) []Model {
	redacted := make([]Model, len(models))
	for i, model := range models {
		model.ID = RedactText(model.ID, private...)
		model.DisplayName = RedactText(model.DisplayName, private...)
		model.Owner = RedactText(model.Owner, private...)
		model.Capabilities = append([]string(nil), model.Capabilities...)
		for j := range model.Capabilities {
			model.Capabilities[j] = RedactText(model.Capabilities[j], private...)
		}
		redacted[i] = model
	}
	return redacted
}
