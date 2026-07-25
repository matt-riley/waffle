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
	if !validPresetHost(u.Hostname()) {
		return "", errors.New("must include a valid host")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u.String(), nil
}

// validPresetHost accepts a literal IP or a hostname whose labels are all
// non-empty and made of letters, digits, and interior hyphens. url.Parse
// already rejects the grossly malformed cases; this rejects the shapes it
// tolerates (empty labels, stray dots, hyphen-edged labels) at enrolment
// instead of leaving them to fail later at connection time. Underscores stay
// allowed because container and service hostnames commonly use them.
func validPresetHost(host string) bool {
	if host == "" {
		return false
	}
	if net.ParseIP(host) != nil {
		return true
	}
	if len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(strings.TrimSuffix(host, "."), ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			isLetter := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
			if !isLetter && (r < '0' || r > '9') && r != '-' && r != '_' {
				return false
			}
		}
	}
	return true
}

// ProbeOutcome is the safe browser-facing classification of a provider probe.
type ProbeOutcome string

const (
	ProbeOutcomeSuccess        ProbeOutcome = "success"
	ProbeOutcomeAuthentication ProbeOutcome = "authentication_failed"
	ProbeOutcomeRequestFailed  ProbeOutcome = "request_failed"
	ProbeOutcomeUnreachable    ProbeOutcome = "unreachable"
)

var (
	probeAuthenticationMarkers = []string{"unauthorized", "forbidden", "authentication", " 401", " 403"}
	// Markers that only appear once the endpoint answered, so the operator is
	// told the request was rejected rather than sent to debug connectivity.
	probeRequestFailedMarkers = []string{
		" 400", " 404", " 409", " 413", " 422", " 429",
		" 500", " 502", " 503", " 504",
		"bad request", "not found", "rate limit", "too many requests",
		"unprocessable", "invalid request", "unsupported",
	}
)

// ClassifyProbeError exposes no upstream diagnostics. Runtime providers return
// library-specific errors, so recognized auth responses and recognized
// endpoint rejections are separated, and every remaining failure safely
// directs the operator to check reachability.
func ClassifyProbeError(err error) ProbeOutcome {
	if err == nil {
		return ProbeOutcomeSuccess
	}
	message := strings.ToLower(err.Error())
	if containsAnyMarker(message, probeAuthenticationMarkers) {
		return ProbeOutcomeAuthentication
	}
	if containsAnyMarker(message, probeRequestFailedMarkers) {
		return ProbeOutcomeRequestFailed
	}
	return ProbeOutcomeUnreachable
}

func containsAnyMarker(message string, markers []string) bool {
	for _, marker := range markers {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}
