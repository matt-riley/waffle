package providerconfig

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/matt-riley/waffle/internal/config"
)

const OpenRouterBaseURL = "https://openrouter.ai/api/v1"

// Preset is one credential-free provider choice shared by the CLI and Desk.
type Preset struct {
	Name            string `json:"name"`
	RuntimeType     string `json:"runtime_type"`
	BaseURL         string `json:"base_url,omitempty"`
	RequiresBaseURL bool   `json:"requires_base_url"`
}

// ProspectiveProbeRequest contains the unsaved provider inputs for a protected
// connection test. APIKey is intentionally never part of durable config.
type ProspectiveProbeRequest struct {
	ConnectionName string
	Connection     config.ProviderConnection
	Model          string
	APIKey         string
}

// ValidateProspectiveProbe checks the credential-free shape that the manager
// can probe without reading or changing config.toml or the secret store.
func ValidateProspectiveProbe(req ProspectiveProbeRequest) error {
	if !config.ValidProviderConnectionName(strings.TrimSpace(req.ConnectionName)) {
		return fmt.Errorf("invalid connection name %q", strings.TrimSpace(req.ConnectionName))
	}
	if strings.TrimSpace(req.Model) == "" {
		return errors.New("provider model is required")
	}
	switch req.Connection.Type {
	case "anthropic", "openai":
	default:
		return fmt.Errorf("unsupported provider type %q", req.Connection.Type)
	}
	if req.Connection.MaxTokens < 0 {
		return errors.New("provider max_tokens must be >= 0")
	}
	if req.Connection.APIKey != "" {
		return errors.New("prospective provider connection must not contain a credential reference")
	}
	if req.Connection.BaseURL != "" {
		if _, err := normalizePresetBaseURL(req.Connection.BaseURL); err != nil {
			return fmt.Errorf("provider base URL: %w", err)
		}
	}
	return nil
}

// Presets returns a stable copy of the supported enrollment choices.
func Presets() []Preset {
	return []Preset{
		{Name: "openai", RuntimeType: "openai"},
		{Name: "anthropic", RuntimeType: "anthropic"},
		{Name: "openrouter", RuntimeType: "openai", BaseURL: OpenRouterBaseURL},
		{Name: "openai-compatible", RuntimeType: "openai", RequiresBaseURL: true},
	}
}

// ResolvePreset validates a UI or CLI choice and returns the stored connection
// values. Transaction and full config validation remain manager-owned.
func ResolvePreset(name, baseURL string) (Preset, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	var preset Preset
	for _, candidate := range Presets() {
		if candidate.Name == name {
			preset = candidate
			break
		}
	}
	if preset.Name == "" {
		return Preset{}, fmt.Errorf("unsupported provider preset %q", name)
	}
	if strings.TrimSpace(baseURL) == "" {
		if preset.RequiresBaseURL {
			return Preset{}, errors.New("provider preset openai-compatible requires a base URL")
		}
		return preset, nil
	}
	normalized, err := normalizePresetBaseURL(baseURL)
	if err != nil {
		return Preset{}, fmt.Errorf("provider preset %q base URL: %w", name, err)
	}
	preset.BaseURL = normalized
	return preset, nil
}

func normalizePresetBaseURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", errors.New("must be an absolute http or https URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("must use http or https")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("must not include credentials, a query, or a fragment")
	}
	host := u.Hostname()
	if host == "" || (net.ParseIP(host) == nil && strings.Contains(host, " ")) {
		return "", errors.New("must include a valid host")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u.String(), nil
}

// ProbeOutcome is the safe browser-facing classification of a provider probe.
type ProbeOutcome string

const (
	ProbeOutcomeSuccess        ProbeOutcome = "success"
	ProbeOutcomeAuthentication ProbeOutcome = "authentication_failed"
	ProbeOutcomeUnreachable    ProbeOutcome = "unreachable"
)

// ClassifyProbeError exposes no upstream diagnostics. Runtime providers return
// library-specific errors, so recognized auth responses are separated and all
// remaining failures safely direct the operator to check reachability.
func ClassifyProbeError(err error) ProbeOutcome {
	if err == nil {
		return ProbeOutcomeSuccess
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "unauthorized") || strings.Contains(message, "forbidden") ||
		strings.Contains(message, "authentication") || strings.Contains(message, " 401") || strings.Contains(message, " 403") {
		return ProbeOutcomeAuthentication
	}
	return ProbeOutcomeUnreachable
}
