package ui

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestCapabilitiesComponentHasExplicitScopesAndReviewedInstall(t *testing.T) {
	var rendered bytes.Buffer
	if err := Capabilities(CapabilitiesView{
		AssetVersion: "test-version",
	}).Render(t.Context(), &rendered); err != nil {
		t.Fatal(err)
	}
	body := rendered.String()
	for _, want := range []string{
		`id="desk-capabilities"`,
		`Session`,
		`Waffle-wide default`,
		`Utility model`,
		`id="capability-catalogue-form"`,
		`id="capability-catalogue-search"`,
		`id="capability-catalogue-results"`,
		`Refresh catalogue`,
		`id="capability-provider-credential"`,
		`autocomplete="off"`,
		`id="capability-skill-review"`,
		`Install inactive`,
		`id="capability-restart-status"`,
		`Restart required.`,
		`id="capability-connections"`,
		`aria-live="polite"`,
		`Connection health only`,
		`Credentials, endpoints, commands, environment, and tool policy details stay private.`,
		`capabilities.js`,
		`capabilities.css`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("Capabilities markup missing %q:\n%s", want, body)
		}
	}
}

func TestCapabilitiesConnectionsStayReadableOnNarrowScreens(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/desk/assets/capabilities.css", nil)
	response := httptest.NewRecorder()
	if !ServeCapabilitiesAsset(response, request, "capabilities.css") {
		t.Fatal("capabilities.css was not served")
	}
	body := response.Body.String()
	for name, pattern := range map[string]string{
		"connection details wrap": `(?s)\.connection-detail\s*\{[^}]*overflow-wrap:\s*anywhere;`,
		"single column mobile":    `(?s)@media \(max-width:\s*48rem\)\s*\{.*?\.capability-grid\s*\{[^}]*grid-template-columns:\s*1fr;`,
	} {
		if !regexp.MustCompile(pattern).MatchString(body) {
			t.Errorf("%s rule missing from capabilities.css", name)
		}
	}
}

func TestTodayComponentExposesSessionSkillControls(t *testing.T) {
	var rendered bytes.Buffer
	if err := Today(ShellView{}).Render(t.Context(), &rendered); err != nil {
		t.Fatal(err)
	}
	body := rendered.String()
	for _, want := range []string{
		`id="desk-skill"`,
		`id="desk-skill-toggle"`,
		`Changes this conversation only.`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("Today markup missing %q", want)
		}
	}
}

func TestServeCapabilitiesAssetIsAdditiveAndImmutable(t *testing.T) {
	for _, name := range []string{"capabilities.js", "capabilities.css"} {
		request := httptest.NewRequest(http.MethodGet, "/desk/assets/"+name, nil)
		response := httptest.NewRecorder()
		if !ServeCapabilitiesAsset(response, request, name) {
			t.Fatalf("%s was not served", name)
		}
		if response.Code != http.StatusOK || response.Body.Len() == 0 {
			t.Fatalf("%s status=%d bytes=%d", name, response.Code, response.Body.Len())
		}
		if got := response.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
			t.Fatalf("%s cache control = %q", name, got)
		}
	}
}
