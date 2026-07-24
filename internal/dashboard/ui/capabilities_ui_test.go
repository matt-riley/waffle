package ui

import (
	"bytes"
	"net/http"
	"net/http/httptest"
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
		`id="capability-provider-credential"`,
		`autocomplete="off"`,
		`id="capability-skill-review"`,
		`Install inactive`,
		`id="capability-restart-status"`,
		`capabilities.js`,
		`capabilities.css`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("Capabilities markup missing %q:\n%s", want, body)
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
