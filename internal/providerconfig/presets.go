package providerconfig

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

const OpenRouterBaseURL = "https://openrouter.ai/api/v1"

// Preset is one credential-free provider choice shared by the CLI and Desk.
type Preset struct {
	Name            string `json:"name"`
	RuntimeType     string `json:"runtime_type"`
	BaseURL         string `json:"base_url,omitempty"`
	RequiresBaseURL bool   `json:"requires_base_url"`
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
