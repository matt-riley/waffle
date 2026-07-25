package providerconfig

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
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
		{name: "authentication failed text", err: errors.New("authentication failed"), want: ProbeOutcomeAuthentication},
		{name: "unknown model", err: errors.New("provider returned 404 not found"), want: ProbeOutcomeRequestFailed},
		{name: "rate limited", err: errors.New("provider returned 429 too many requests"), want: ProbeOutcomeRequestFailed},
		{name: "upstream error", err: errors.New("provider returned 502 bad gateway"), want: ProbeOutcomeRequestFailed},
		{name: "dial failure text", err: errors.New("dial tcp 10.0.0.1:443: connect: connection refused"), want: ProbeOutcomeUnreachable},
		{name: "dns failure text", err: errors.New("lookup gateway.example: no such host"), want: ProbeOutcomeUnreachable},
		{
			name: "deadline mentions authentication proxy",
			err:  fmt.Errorf("context deadline exceeded reaching authentication proxy: %w", context.DeadlineExceeded),
			want: ProbeOutcomeUnreachable,
		},
		{
			name: "canceled",
			err:  fmt.Errorf("probe canceled: %w", context.Canceled),
			want: ProbeOutcomeUnreachable,
		},
		{
			name: "url timeout",
			err:  &url.Error{Op: "Get", URL: "https://gateway.example/v1", Err: context.DeadlineExceeded},
			want: ProbeOutcomeUnreachable,
		},
		{
			name: "op error",
			err:  &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")},
			want: ProbeOutcomeUnreachable,
		},
		{
			name: "structured http 401",
			err:  probeHTTPStatusError{status: 401, message: "rejected"},
			want: ProbeOutcomeAuthentication,
		},
		{
			name: "structured http 404",
			err:  probeStatusCodeError{status: 404, message: "missing"},
			want: ProbeOutcomeRequestFailed,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := ClassifyProbeError(testCase.err); got != testCase.want {
				t.Fatalf("ClassifyProbeError(%v) = %q, want %q", testCase.err, got, testCase.want)
			}
		})
	}
}

type probeHTTPStatusError struct {
	status  int
	message string
}

func (e probeHTTPStatusError) Error() string   { return e.message }
func (e probeHTTPStatusError) HTTPStatus() int { return e.status }

type probeStatusCodeError struct {
	status  int
	message string
}

func (e probeStatusCodeError) Error() string   { return e.message }
func (e probeStatusCodeError) StatusCode() int { return e.status }

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
