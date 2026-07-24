package ui

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMemoryRendersSearchAttachConversationAndCancelFirstDialog(t *testing.T) {
	var rendered bytes.Buffer
	if err := Memory(ShellView{ActiveSection: "memory", AssetVersion: "test"}).Render(context.Background(), &rendered); err != nil {
		t.Fatal(err)
	}
	body := rendered.String()
	for _, required := range []string{
		`class="memory"`,
		`aria-labelledby="memory-title"`,
		`id="memory-search-form"`,
		`id="memory-query"`,
		`id="memory-session-id"`,
		`id="memory-results"`,
		`id="memory-status"`,
		`id="memory-forget-dialog"`,
		`id="memory-forget-cancel"`,
		`id="memory-forget-confirm"`,
		`section=today`,
		`Add through conversation`,
		`memory.css`,
		`memory.js`,
	} {
		if !strings.Contains(body, required) {
			t.Errorf("Memory view missing %q", required)
		}
	}
	cancel := strings.Index(body, `id="memory-forget-cancel"`)
	confirm := strings.Index(body, `id="memory-forget-confirm"`)
	if cancel < 0 || confirm < 0 || cancel > confirm {
		t.Fatalf("dialog is not cancel-first: %s", body)
	}
	if strings.Contains(strings.ToLower(body), "undo") {
		t.Fatalf("Memory view offers fake undo: %s", body)
	}
}

func TestMemoryStylesAndClientKeepDynamicContentSafe(t *testing.T) {
	cssBytes, err := assetFiles.ReadFile("assets/memory.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(cssBytes)
	for _, required := range []string{
		".memory-layout",
		".memory-hit",
		".memory-source-chip",
		".memory-hit.is-archived",
		"@media (max-width: 900px)",
		".memory-search-panel {\n    flex-basis: auto;",
	} {
		if !strings.Contains(css, required) {
			t.Errorf("Memory CSS missing %q", required)
		}
	}

	jsBytes, err := assetFiles.ReadFile("assets/memory.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(jsBytes)
	for _, required := range []string{
		"textContent",
		"replaceChildren",
		"crypto.randomUUID()",
		"X-Waffle-Desk-Token",
		"Idempotency-Key",
		"showModal",
		"setTimeout",
		"/api/v1/desk/memory",
	} {
		if !strings.Contains(js, required) {
			t.Errorf("Memory client missing %q", required)
		}
	}
	for _, forbidden := range []string{
		"innerHTML",
		"localStorage",
		"sessionStorage",
		"console.",
		"provider/delete",
		"backup/delete",
		"Undo",
	} {
		if strings.Contains(js, forbidden) {
			t.Errorf("Memory client contains forbidden %q", forbidden)
		}
	}
}

func TestServeMemoryAssetServesOnlyMemoryAssets(t *testing.T) {
	for _, name := range []string{"memory.js", "memory.css"} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/desk/assets/"+name, nil)
		if !ServeMemoryAsset(recorder, request, name) {
			t.Fatalf("%s was not served", name)
		}
		if recorder.Code != http.StatusOK ||
			recorder.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
			t.Fatalf("%s response = %d %q", name, recorder.Code, recorder.Header().Get("Cache-Control"))
		}
	}
	request := httptest.NewRequest(http.MethodGet, "/desk/assets/app.js", nil)
	if ServeMemoryAsset(httptest.NewRecorder(), request, "app.js") {
		t.Fatal("Memory asset seam claimed a shared asset")
	}
}
