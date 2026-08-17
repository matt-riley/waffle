package dashboard

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
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
	if got := jsonResponse.Header()["Vary"]; !reflect.DeepEqual(got, []string{"Accept", "HX-Request"}) {
		t.Fatalf("JSON Vary = %#v, want additive Accept and HX-Request values", got)
	}
	var payload CapabilitiesSnapshot
	if err := json.Unmarshal(jsonResponse.Body.Bytes(), &payload); err != nil {
		t.Fatalf("JSON fallback: %v", err)
	}
}

func TestFragmentNegotiationSetsCombinedHTMLVaryBeforeHandler(t *testing.T) {
	var seen string
	handler := negotiateFragments(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		seen = w.Header().Get("Vary")
		writeJSON(w, http.StatusOK, CapabilitiesSnapshot{})
	}))

	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8422/api/v1/desk/capabilities", nil)
	request.Header.Set("Accept", "text/html")
	handler.ServeHTTP(httptest.NewRecorder(), request)

	if seen != "Accept, HX-Request" {
		t.Fatalf("Vary visible to HTML handler = %q, want combined header", seen)
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

func TestTaskFragmentCarriesEditStateAndFilterButtonsAsOOBSwaps(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8422/api/v1/desk/tasks?filter=scheduled", nil)
	component := fragmentComponent(request, http.StatusOK, TasksSnapshot{
		Filter: TaskFilterScheduled,
		Tasks: []TaskView{{
			ID: "job-1", Kind: TaskKindSchedule, Name: "Morning", Cron: "0 9 * * *", Prompt: "Brief", Profile: "reviewer", Enabled: true,
			RedactedFields: []string{"prompt"}, EvidenceLabel: "Scheduled",
		}},
	})
	var rendered bytes.Buffer
	if err := component.Render(t.Context(), &rendered); err != nil {
		t.Fatal(err)
	}
	body := rendered.String()
	for _, want := range []string{
		`id="tasks-list"`,
		`data-task-id="job-1"`,
		`data-task-redacted-fields="prompt"`,
		`data-waffle-task-edit="true"`,
		`Edit schedule`,
		`id="task-filter-scheduled"`,
		`aria-pressed="true"`,
		`id="task-filter-all"`,
		`aria-pressed="false"`,
		`hx-swap-oob="outerHTML"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("task fragment missing %q: %s", want, body)
		}
	}
}

func TestCapabilityFragmentsKeepCatalogueActionsAndSkillSourceState(t *testing.T) {
	catalogue := fragmentComponent(nil, http.StatusOK, CapabilityCatalogueView{
		Connection: "primary",
		Models:     []CapabilityCatalogueModel{{ID: "vendor/model", DisplayName: "Vendor Model", AliasSuggestion: "vendor-model"}},
	})
	var catalogueBody bytes.Buffer
	if err := catalogue.Render(t.Context(), &catalogueBody); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`id="capability-catalogue-results"`,
		`Add as alias`,
		`name="connection_name" value="primary"`,
		`name="upstream_model" value="vendor/model"`,
		`name="alias"`,
		`value="vendor-model"`,
	} {
		if !strings.Contains(catalogueBody.String(), want) {
			t.Errorf("catalogue fragment missing %q: %s", want, catalogueBody.String())
		}
	}

	skillRequest := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8422/api/v1/desk/capabilities?part=skills", nil)
	skills := fragmentComponent(skillRequest, http.StatusOK, CapabilitiesSnapshot{SkillSources: CapabilitySkillSources{LocalRoots: []string{"/allowed"}}})
	var skillsBody bytes.Buffer
	if err := skills.Render(t.Context(), &skillsBody); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(skillsBody.String(), `data-waffle-skill-source="local" data-waffle-source-available="true"`) {
		t.Fatalf("skill fragment did not carry source availability: %s", skillsBody.String())
	}
}

func TestCapabilityProviderTestFailureRendersAnErrorFragment(t *testing.T) {
	component := fragmentComponent(nil, http.StatusOK, CapabilityProviderTestResult{Outcome: "authentication_failed"})
	var rendered bytes.Buffer
	if err := component.Render(t.Context(), &rendered); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rendered.String(), `data-waffle-error="true"`) || !strings.Contains(rendered.String(), "authentication failed") {
		t.Fatalf("provider test failure = %s", rendered.String())
	}
}

// TestSkillFragmentReportsInstallWithoutAuditRecord covers #297: the Desk UI
// only ever renders this fragment for a skill operation, so a committed
// install whose policy_audit row was lost must say so here.
func TestSkillFragmentReportsInstallWithoutAuditRecord(t *testing.T) {
	tests := []struct {
		name        string
		disposition string
		want        string
	}{
		{name: "clean install", disposition: "committed", want: "Skill operation completed."},
		{name: "lost audit row", disposition: "committed" + InstallDispositionUnaudited, want: "policy audit record was not written"},
		{
			name:        "lost audit row after provenance repair",
			disposition: "committed_with_provenance_repair" + InstallDispositionUnaudited,
			want:        "policy audit record was not written",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := negotiateFragments(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusOK, CapabilitySkill{Name: "reviewed-skill", InstallDisposition: test.disposition})
			}))
			request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8422/api/v1/desk/capabilities/skills/install", nil)
			request.Header.Set("Accept", "text/html")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)

			if response.Code != http.StatusOK {
				t.Fatalf("status = %d", response.Code)
			}
			if !strings.Contains(response.Body.String(), test.want) {
				t.Fatalf("fragment = %s, want it to contain %q", response.Body.String(), test.want)
			}
		})
	}
}

func TestConnectionCardLabels(t *testing.T) {
	probed := ConnectionProbe{Outcome: providerconfig.ProbeOutcomeSuccess, CheckedAt: time.Now()}
	recent := ConnectionProbe{Outcome: providerconfig.ProbeOutcomeSuccess, CheckedAt: time.Now().Add(-2 * time.Minute)}
	cases := []struct {
		name string
		got  string
		want string
	}{
		{name: "unchecked health", got: func() string { h, _ := connectionHealth(ConnectionProbe{}, false); return h }(), want: "Unchecked"},
		{name: "unchecked class", got: func() string { _, c := connectionHealth(ConnectionProbe{}, false); return c }(), want: " is-unchecked"},
		{name: "healthy health", got: func() string { h, _ := connectionHealth(probed, true); return h }(), want: "Healthy"},
		{name: "healthy class", got: func() string { _, c := connectionHealth(probed, true); return c }(), want: " is-healthy"},
		{name: "failed health", got: func() string {
			h, _ := connectionHealth(ConnectionProbe{Outcome: providerconfig.ProbeOutcomeUnreachable}, true)
			return h
		}(), want: "Failed"},
		{name: "degraded health", got: func() string {
			h, _ := connectionHealth(ConnectionProbe{Outcome: providerconfig.ProbeOutcomeRequestFailed}, true)
			return h
		}(), want: "Degraded"},
		{name: "protocol openai", got: connectionProtocolLabel("openai"), want: "OpenAI-compatible driver"},
		{name: "protocol empty", got: connectionProtocolLabel(""), want: "Not reported"},
		{name: "tokens default", got: connectionMaxTokensLabel(0), want: "Provider default"},
		{name: "tokens set", got: connectionMaxTokensLabel(4000), want: "4000"},
		{name: "last check never", got: connectionLastCheckLabel(ConnectionProbe{}, false), want: "Never"},
		{name: "last check just now", got: connectionLastCheckLabel(probed, true), want: "Just now"},
		{name: "last check minutes", got: connectionLastCheckLabel(recent, true), want: "2 minutes ago"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Fatalf("got %q, want %q", tc.got, tc.want)
			}
		})
	}
}

func TestWorkspaceSummaryLabel(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]int
		want string
	}{
		{name: "empty", in: map[string]int{}, want: "No guarded workspaces are open."},
		{name: "single open", in: map[string]int{"open": 1}, want: "1 open"},
		{name: "plural idle", in: map[string]int{"idle": 2}, want: "2 idle"},
		{name: "mixed", in: map[string]int{"open": 1, "idle": 2, "closed": 1}, want: "1 open · 2 idle · 1 closed"},
		{name: "failed", in: map[string]int{"failed": 1, "open": 1}, want: "1 open · 1 failed"},
		{name: "attention ignored", in: map[string]int{"attention": 1, "open": 1}, want: "1 open"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := workspaceSummaryLabel(tc.in); got != tc.want {
				t.Fatalf("workspaceSummaryLabel(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
