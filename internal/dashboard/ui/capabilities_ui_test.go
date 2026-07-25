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
	if err := Capabilities().Render(t.Context(), &rendered); err != nil {
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
		`id="capability-skill-stage-prerequisite"`,
		`id="capability-skill-local-help"`,
		`id="capability-skill-git-help"`,
		`id="capability-skill-review-expires"`,
		`Install inactive`,
		`id="capability-restart-status"`,
		`Restart required.`,
		`id="capability-connections"`,
		`id="capability-status"`,
		`role="status"`,
		`aria-live="polite"`,
		`Connection health only`,
		`Credentials, endpoints, commands, environment, and tool policy details stay private.`,
		`aria-describedby="capability-default-status"`,
		`id="capability-default-status"`,
		`aria-describedby="capability-utility-status"`,
		`id="capability-utility-status"`,
		`aria-describedby="capability-catalogue-status"`,
		`id="capability-catalogue-status"`,
		`aria-describedby="capability-model-status"`,
		`id="capability-model-status"`,
		`aria-describedby="capability-skill-stage-status"`,
		`id="capability-skill-stage-status"`,
		`aria-describedby="capability-skill-install-status"`,
		`id="capability-skill-install-status"`,
		`aria-describedby="capability-provider-status"`,
		`id="capability-provider-status"`,
		`class="capability-form-status"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("Capabilities markup missing %q:\n%s", want, body)
		}
	}
	// List containers must not announce full re-renders; status regions keep aria-live.
	for _, listID := range []string{
		`id="capability-models"`,
		`id="capability-skills"`,
		`id="capability-connections"`,
		`id="capability-catalogue-results"`,
	} {
		idx := strings.Index(body, listID)
		if idx < 0 {
			t.Fatalf("missing list container %q", listID)
		}
		// Inspect the opening tag only.
		end := strings.Index(body[idx:], ">")
		if end < 0 {
			t.Fatalf("unclosed tag for %q", listID)
		}
		openTag := body[idx : idx+end+1]
		if strings.Contains(openTag, `aria-live`) {
			t.Errorf("list container %q must not carry aria-live: %s", listID, openTag)
		}
	}
	if strings.Contains(body, "capabilities.css") || strings.Contains(body, "capabilities.js") {
		t.Error("Capabilities section must not link stylesheet/script from body; use CapabilitiesAssets in head")
	}
}

func TestCapabilitiesAssetsAreVersionedInHead(t *testing.T) {
	var rendered bytes.Buffer
	if err := CapabilitiesAssets().Render(t.Context(), &rendered); err != nil {
		t.Fatal(err)
	}
	body := rendered.String()
	version := CapabilitiesAssetVersion()
	for _, required := range []string{
		`/desk/assets/capabilities.css?v=` + version,
		`rel="stylesheet"`,
	} {
		if !strings.Contains(body, required) {
			t.Errorf("CapabilitiesAssets missing %q:\n%s", required, body)
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
		"connection cards shrink": `(?s)\.connection-card\s*\{[^}]*min-width:\s*0;`,
		"connection titles wrap":  `(?s)\.connection-card\s*>\s*strong\s*\{[^}]*max-width:\s*100%;[^}]*overflow-wrap:\s*anywhere;`,
		"connection details wrap": `(?s)\.connection-detail\s*\{[^}]*overflow-wrap:\s*anywhere;`,
		"single column mobile":    `(?s)@media \(max-width:\s*48rem\)\s*\{.*?\.capability-grid\s*\{[^}]*grid-template-columns:\s*1fr;`,
	} {
		if !regexp.MustCompile(pattern).MatchString(body) {
			t.Errorf("%s rule missing from capabilities.css", name)
		}
	}
	// Input chrome lives in the shared app.css baseline, not section CSS.
	// Field-level aria-invalid highlights are allowed.
	if regexp.MustCompile(`\.capability-panel\s+input\s*\{`).MatchString(body) {
		t.Error("capabilities.css must not redeclare input control baseline")
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
	for _, name := range []string{"capabilities.css"} {
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
