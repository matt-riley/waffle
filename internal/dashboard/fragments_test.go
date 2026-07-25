package dashboard

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/providerconfig"
)

func TestFragmentNegotiationEscapesHTMLAndKeepsJSONFallback(t *testing.T) {
	handler := negotiateFragments(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, CapabilitiesSnapshot{Providers: providerconfig.Listing{Models: map[string]providerconfig.ModelSummary{"<unsafe>": {Provider: "local", Model: "model"}}}})
	}))

	htmlRequest := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8422/api/v1/desk/capabilities?part=models", nil)
	htmlRequest.Header.Set("Accept", "text/html")
	html := httptest.NewRecorder()
	handler.ServeHTTP(html, htmlRequest)
	if html.Code != http.StatusOK || !strings.HasPrefix(html.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("HTML response = %d %q", html.Code, html.Header().Get("Content-Type"))
	}
	if strings.Contains(html.Body.String(), "<unsafe>") || !strings.Contains(html.Body.String(), "&lt;unsafe&gt;") {
		t.Fatalf("fragment did not escape model alias: %s", html.Body.String())
	}
	if !strings.Contains(html.Body.String(), `id="capability-default-alias"`) || !strings.Contains(html.Body.String(), `hx-swap-oob="outerHTML"`) {
		t.Fatalf("model fragment did not carry picker option swaps: %s", html.Body.String())
	}
	if got := html.Header().Get("Vary"); got != "Accept, HX-Request" {
		t.Fatalf("HTML Vary = %q", got)
	}

	jsonRequest := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8422/api/v1/desk/capabilities?part=models", nil)
	jsonRequest.Header.Set("Accept", "application/json")
	jsonResponse := httptest.NewRecorder()
	handler.ServeHTTP(jsonResponse, jsonRequest)
	if jsonResponse.Code != http.StatusOK || !strings.HasPrefix(jsonResponse.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("JSON response = %d %q", jsonResponse.Code, jsonResponse.Header().Get("Content-Type"))
	}
	var payload CapabilitiesSnapshot
	if err := json.Unmarshal(jsonResponse.Body.Bytes(), &payload); err != nil {
		t.Fatalf("JSON fallback: %v", err)
	}
}

func TestFragmentNegotiationHonorsExplicitJSONWithHXRequest(t *testing.T) {
	handler := negotiateFragments(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, CapabilitiesSnapshot{})
	}))
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8422/api/v1/desk/capabilities", nil)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("HX-Request", "true")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if got := response.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("explicit JSON with HX-Request = %q", got)
	}
}

func TestFragmentMutationPreservesHTMLHeadersAcrossReplay(t *testing.T) {
	security := mustSecurity(t, "127.0.0.1:8422")
	store := NewIdempotencyStore(nil, 8, time.Minute)
	calls := 0
	handler := NewMutationHandler(security, store, 1024, negotiateFragments(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		writeJSON(w, http.StatusAccepted, capabilityMutationResponse{RestartRequired: true})
	})))

	var firstBody []byte
	for attempt := 0; attempt < 2; attempt++ {
		recording := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8422/api/v1/desk/models/default", bytes.NewBufferString(`{"alias":"safe"}`))
		request.Host = "127.0.0.1:8422"
		request.Header.Set("Accept", "text/html")
		request.Header.Set("X-Waffle-Desk-Token", security.Token())
		request.Header.Set("Idempotency-Key", "fragment-replay")
		handler.ServeHTTP(recording, request)
		if recording.Code != http.StatusAccepted || !strings.HasPrefix(recording.Header().Get("Content-Type"), "text/html") {
			t.Fatalf("attempt %d = %d %q", attempt, recording.Code, recording.Header().Get("Content-Type"))
		}
		if attempt == 0 {
			firstBody = append([]byte(nil), recording.Body.Bytes()...)
		} else if !bytes.Equal(firstBody, recording.Body.Bytes()) {
			t.Fatal("idempotent HTML replay changed the fragment body")
		}
	}
	if calls != 1 {
		t.Fatalf("mutation calls = %d, want one first execution", calls)
	}
}

func TestFragmentMutationPreservesHTMLErrorAndJSONContentTypes(t *testing.T) {
	security := mustSecurity(t, "127.0.0.1:8422")
	store := NewIdempotencyStore(nil, 8, time.Minute)
	handler := NewMutationHandler(security, store, 1024, negotiateFragments(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if wantsHTMLRequest(r) {
			writeJSON(w, http.StatusConflict, errorResponse{Code: "provider_locked", Message: "provider configuration is locked — retry"})
			return
		}
		writeJSON(w, http.StatusConflict, errorResponse{Code: "provider_locked", Message: "provider configuration is locked — retry"})
	})))

	for attempt := 0; attempt < 2; attempt++ {
		recording := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8422/api/v1/desk/models/default", bytes.NewBufferString(`{"alias":"safe"}`))
		request.Host = "127.0.0.1:8422"
		request.Header.Set("Accept", "text/html")
		request.Header.Set("X-Waffle-Desk-Token", security.Token())
		request.Header.Set("Idempotency-Key", "fragment-error-replay")
		handler.ServeHTTP(recording, request)
		if recording.Code != http.StatusConflict || !strings.HasPrefix(recording.Header().Get("Content-Type"), "text/html") || !strings.Contains(recording.Body.String(), "provider configuration is locked") {
			t.Fatalf("HTML error attempt %d = %d %q %q", attempt, recording.Code, recording.Header().Get("Content-Type"), recording.Body.String())
		}
	}

	jsonHandler := negotiateFragments(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusConflict, errorResponse{Code: "invalid_request", Message: "request is invalid"})
	}))
	jsonRequest := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8422/api/v1/desk/capabilities", nil)
	jsonRequest.Header.Set("Accept", "application/json")
	jsonResponse := httptest.NewRecorder()
	jsonHandler.ServeHTTP(jsonResponse, jsonRequest)
	if got := jsonResponse.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("JSON error Content-Type = %q", got)
	}
}
