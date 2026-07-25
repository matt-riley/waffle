package providerconfig

import (
	"errors"
	"testing"
)

func TestResolvePresetRejectsMalformedHosts(t *testing.T) {
	for _, baseURL := range []string{
		"https://foo..bar/v1",
		"https://.../v1",
		"https://-gateway.example/v1",
		"https://gateway-.example/v1",
		"https://gateway.example!/v1",
		"https:///v1",
	} {
		if _, err := ResolvePreset("openai-compatible", baseURL); err == nil {
			t.Errorf("base URL %q was accepted", baseURL)
		}
	}
	for _, baseURL := range []string{
		"https://gateway.example/v1",
		"http://127.0.0.1:11434/v1",
		"http://[::1]:11434/v1",
		"http://ollama_host:11434/v1",
		"https://gateway.example./v1",
	} {
		if _, err := ResolvePreset("openai-compatible", baseURL); err != nil {
			t.Errorf("base URL %q was rejected: %v", baseURL, err)
		}
	}
}

func TestClassifyProbeErrorSeparatesRejectionsFromUnreachableEndpoints(t *testing.T) {
	for _, testCase := range []struct {
		name string
		err  error
		want ProbeOutcome
	}{
		{name: "success", err: nil, want: ProbeOutcomeSuccess},
		{name: "unauthorized", err: errors.New("provider returned 401 unauthorized"), want: ProbeOutcomeAuthentication},
		{name: "forbidden", err: errors.New("HTTP 403 forbidden"), want: ProbeOutcomeAuthentication},
		{name: "unknown model", err: errors.New("provider returned 404 not found"), want: ProbeOutcomeRequestFailed},
		{name: "rate limited", err: errors.New("provider returned 429 too many requests"), want: ProbeOutcomeRequestFailed},
		{name: "upstream error", err: errors.New("provider returned 502 bad gateway"), want: ProbeOutcomeRequestFailed},
		{name: "dial failure", err: errors.New("dial tcp 10.0.0.1:443: connect: connection refused"), want: ProbeOutcomeUnreachable},
		{name: "dns failure", err: errors.New("lookup gateway.example: no such host"), want: ProbeOutcomeUnreachable},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := ClassifyProbeError(testCase.err); got != testCase.want {
				t.Fatalf("ClassifyProbeError(%v) = %q, want %q", testCase.err, got, testCase.want)
			}
		})
	}
}

func TestPresetListValidatesSupportedTypesAndBaseURLRequirements(t *testing.T) {
	presets := Presets()
	if len(presets) != 4 {
		t.Fatalf("presets = %#v, want four supported choices", presets)
	}

	compatible, err := ResolvePreset("openai-compatible", "https://gateway.example/v1/")
	if err != nil {
		t.Fatal(err)
	}
	if compatible.RuntimeType != "openai" || compatible.BaseURL != "https://gateway.example/v1" || !compatible.RequiresBaseURL {
		t.Fatalf("compatible preset = %#v", compatible)
	}
	if _, err := ResolvePreset("openai-compatible", ""); err == nil {
		t.Fatal("openai-compatible accepted an absent base URL")
	}
	if _, err := ResolvePreset("not-a-provider", ""); err == nil {
		t.Fatal("unsupported preset was accepted")
	}
}
