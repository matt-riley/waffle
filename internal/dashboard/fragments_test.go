package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"html"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/dashboard/ui"
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

func TestTaskEmptyFragmentsRenderTheExactFilterMatrixAndNoArtworkOnFailure(t *testing.T) {
	cases := []struct {
		filter TaskFilter
		title  string
		body   string
		action string
	}{
		{filter: TaskFilterAll, title: "Nothing on Waffle's plate", body: "Scheduled runs and completed work will appear here.", action: "Start a conversation"},
		{filter: TaskFilterActive, title: "No active runs", body: "Nothing is running right now.", action: "View all tasks"},
		{filter: TaskFilterScheduled, title: "No schedules yet", body: "Create a schedule and Waffle can pick this up later.", action: "View all tasks"},
		{filter: TaskFilterCompleted, title: "No completed runs", body: "Finished runs will appear here.", action: "View all tasks"},
		{filter: TaskFilterAttention, title: "Nothing needs attention", body: "Waffle has no blocked or failed work to review.", action: "View all tasks"},
	}
	for _, tc := range cases {
		t.Run(string(tc.filter), func(t *testing.T) {
			component := fragmentComponent(nil, http.StatusOK, TasksSnapshot{Filter: tc.filter})
			var rendered bytes.Buffer
			if err := component.Render(t.Context(), &rendered); err != nil {
				t.Fatal(err)
			}
			body := rendered.String()
			for _, want := range []string{tc.title, tc.body, tc.action, `data-waffle-fragment="true"`} {
				expected := html.EscapeString(want)
				if want == `data-waffle-fragment="true"` {
					expected = want
				}
				if !strings.Contains(body, expected) {
					t.Errorf("empty %s fragment missing %q: %s", tc.filter, want, body)
				}
			}
			if strings.Contains(body, "No tasks match this view.") || strings.Count(body, `id="task-schedule-open"`) != 1 {
				t.Fatalf("empty %s fragment retained generic copy or lost/duplicated schedule trigger: %s", tc.filter, body)
			}
			if tc.filter == TaskFilterAll || tc.filter == TaskFilterScheduled {
				if !strings.Contains(body, "waffle-empty-curled.png") {
					t.Errorf("proven empty %s fragment lost the curled Waffle", tc.filter)
				}
			}
		})
	}

	component := fragmentComponent(nil, http.StatusOK, TasksSnapshot{Filter: TaskFilterAll, Errors: []*SectionError{{Section: OperationsSectionJobs}}})
	var rendered bytes.Buffer
	if err := component.Render(t.Context(), &rendered); err != nil {
		t.Fatal(err)
	}
	body := rendered.String()
	for _, want := range []string{"Some task evidence is unavailable", "Try again", "Waffle could not check every task source"} {
		if !strings.Contains(body, want) {
			t.Errorf("failure fragment missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, "waffle-empty-curled.png") {
		t.Fatalf("failure fragment rendered empty artwork: %s", body)
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

func TestCapabilityModelActionsAreCompactAndTruthful(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8422/api/v1/desk/capabilities?part=models", nil)
	component := fragmentComponent(request, http.StatusOK, CapabilitiesSnapshot{
		Providers: providerconfig.Listing{
			DefaultModel: "primary",
			UtilityModel: "utility",
			Models: map[string]providerconfig.ModelSummary{
				"primary": {Provider: "fixture", Model: "primary-model"},
				"utility": {Provider: "fixture", Model: "utility-model"},
				"local":   {Provider: "fixture", Model: "local-model"},
			},
		},
	})
	var rendered bytes.Buffer
	if err := component.Render(t.Context(), &rendered); err != nil {
		t.Fatal(err)
	}
	body := rendered.String()
	for _, pattern := range []string{
		`data-waffle-action-id="model-default-primary"[^>]*disabled[^>]*aria-pressed="true"[^>]*>Default</button>`,
		`data-waffle-action-id="model-utility-utility"[^>]*disabled[^>]*aria-pressed="true"[^>]*>Utility model</button>`,
		`data-waffle-action-id="model-default-local"[^>]*>Make default</button>`,
		`data-waffle-action-id="model-utility-local"[^>]*>Make utility</button>`,
	} {
		if !regexp.MustCompile(pattern).MatchString(body) {
			t.Errorf("model fragment missing pattern %q:\n%s", pattern, body)
		}
	}
	for _, actionID := range []string{"model-default-local", "model-utility-local"} {
		button := regexp.MustCompile(`<button[^>]*data-waffle-action-id="` + actionID + `"[^>]*>[^<]+</button>`).FindString(body)
		if strings.Contains(button, "aria-pressed") {
			t.Errorf("unselected model action %q unexpectedly has toggle semantics: %s", actionID, button)
		}
	}
	for _, want := range []string{`hx-post="/api/v1/desk/models/default"`, `hx-post="/api/v1/desk/models/utility"`} {
		if !strings.Contains(body, want) {
			t.Errorf("model fragment missing %q:\n%s", want, body)
		}
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
		{name: "single unknown", in: map[string]int{"mysterious": 1}, want: "1 unknown"},
		{name: "mixed unknown", in: map[string]int{"open": 1, "mysterious": 2, "closed": 1}, want: "1 open · 2 unknown · 1 closed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := workspaceSummaryLabel(tc.in); got != tc.want {
				t.Fatalf("workspaceSummaryLabel(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestWorkspacesRenderOnePrimaryAndMoreActionsByLifecycle(t *testing.T) {
	cases := []struct {
		name          string
		status        string
		session       string
		wantPrimary   string
		wantSecondary string
		wantMore      bool
	}{
		{name: "open", status: "open", session: "session-open", wantPrimary: "Open at Desk", wantSecondary: "Idle", wantMore: true},
		{name: "idle", status: "idle", session: "session-idle", wantPrimary: "Resume", wantSecondary: "Open at Desk", wantMore: true},
		{name: "failed", status: "failed", session: "session-failed", wantMore: true},
		{name: "unknown", status: "mysterious", session: "session-unknown", wantMore: true},
		{name: "closed", status: "closed", session: "session-closed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fragment := workspacesFragment(WorkspaceSnapshot{Workspaces: []WorkspaceView{{
				ID: "ws-" + tc.name, Repository: "owner/" + tc.name, SessionID: tc.session, Status: tc.status,
			}}}, nil)
			body := renderFragmentForTest(t, fragment)
			if got := strings.Count(body, `class="workspace-primary"`); got != boolCount(tc.wantPrimary != "") {
				t.Fatalf("primary count = %d, want %d: %s", got, boolCount(tc.wantPrimary != ""), body)
			}
			if tc.wantPrimary != "" && !strings.Contains(body, `>`+tc.wantPrimary+`</button>`) {
				t.Fatalf("missing primary %q: %s", tc.wantPrimary, body)
			}
			if tc.wantSecondary != "" && !strings.Contains(body, `>`+tc.wantSecondary+`</button>`) {
				t.Fatalf("missing secondary %q: %s", tc.wantSecondary, body)
			}
			if strings.Contains(body, `class="workspace-primary">Resume`) && tc.wantPrimary != "Resume" {
				t.Fatalf("unexpected Resume primary: %s", body)
			}
			if got := strings.Contains(body, `class="workspace-more-actions"`); got != tc.wantMore {
				t.Fatalf("More actions disclosure = %v, want %v: %s", got, tc.wantMore, body)
			}
			if tc.status == "mysterious" {
				if strings.Contains(body, `data-status="mysterious"`) || strings.Contains(body, `>mysterious</p>`) {
					t.Fatalf("unknown lifecycle status leaked into rendered state: %s", body)
				}
				if !strings.Contains(body, `data-status="unknown"`) || !strings.Contains(body, `>unknown</p>`) {
					t.Fatalf("unknown lifecycle status was not bounded to unknown: %s", body)
				}
			}
			if tc.wantMore {
				wantLabel := `aria-label="More actions for owner/` + tc.name + `"`
				if !strings.Contains(body, wantLabel) {
					t.Fatalf("missing accessible disclosure name %q: %s", wantLabel, body)
				}
			}
		})
	}
}

func TestWorkspacesEmptyStateUsesTheStableHeaderOpenPath(t *testing.T) {
	fragment := workspacesFragment(WorkspaceSnapshot{}, nil)
	if fragment.EmptyState == nil {
		t.Fatal("empty workspace response has no structured empty state")
	}
	if fragment.EmptyState.Title != "No guarded workspaces yet" || fragment.EmptyState.Body != "Open a repository to give Waffle a bounded place to work." {
		t.Fatalf("empty copy = %q / %q", fragment.EmptyState.Title, fragment.EmptyState.Body)
	}
	if fragment.EmptyState.PrimaryAction != nil || fragment.EmptyState.SecondaryAction != nil {
		t.Fatal("empty fragment created a duplicate workspace recovery trigger; the stable header button owns this path")
	}
	body := renderFragmentForTest(t, fragment)
	if strings.Contains(body, "workspace-empty-open") || strings.Contains(body, "data-waffle-dialog-trigger") {
		t.Fatalf("empty fragment contains a duplicate open trigger: %s", body)
	}
}

func renderFragmentForTest(t *testing.T, fragment ui.FragmentView) string {
	t.Helper()
	var body bytes.Buffer
	if err := ui.FragmentList(fragment).Render(context.Background(), &body); err != nil {
		t.Fatal(err)
	}
	return body.String()
}

func boolCount(value bool) int {
	if value {
		return 1
	}
	return 0
}

func TestCapabilitySummaries(t *testing.T) {
	models := CapabilitiesSnapshot{Providers: providerconfig.Listing{Models: map[string]providerconfig.ModelSummary{}}}
	models.Providers.DefaultModel = "primary"
	models.Providers.UtilityModel = "primary"
	models.Providers.Models = map[string]providerconfig.ModelSummary{
		"primary": {Provider: "fixture", Model: "m1"},
		"local":   {Provider: "fixture", Model: "m2"},
	}
	got := modelsSummaryLabel(models)
	if got != "Default: primary · Utility: primary · 2 aliases" {
		t.Fatalf("models summary = %q", got)
	}
	skills := skillsSummaryLabel([]CapabilitySkill{{Name: "a", Active: true}, {Name: "b"}})
	if skills != "2 skills · 1 active" {
		t.Fatalf("skills summary = %q", skills)
	}
	conns := connectionsSummaryLabel(CapabilitiesSnapshot{
		Providers: providerconfig.Listing{Providers: map[string]providerconfig.ProviderSummary{"a": {}}},
		Probes:    map[string]ConnectionProbe{"a": {Outcome: providerconfig.ProbeOutcomeSuccess}},
	})
	if conns != "1 connections · 1 healthy" {
		t.Fatalf("connections summary = %q", conns)
	}
}

func TestMemoryZeroFragmentHasOneStructuredVisibleExplanationAndOneLiveOwner(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8422/api/v1/desk/memory?query=nothing", nil)
	component := fragmentComponent(request, http.StatusOK, MemorySearchResponse{Query: "nothing"})
	var rendered bytes.Buffer
	if err := component.Render(request.Context(), &rendered); err != nil {
		t.Fatal(err)
	}
	body := rendered.String()
	if got := strings.Count(body, "No attributed memory matched that search."); got != 1 {
		t.Fatalf("zero explanation count = %d, want 1: %s", got, body)
	}
	for _, want := range []string{
		"No memory matched that search",
		"No attributed memory matched that search. Try a different phrase or add context through conversation.",
		"waffle-empty-curious.png",
		">0 results</p>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("zero fragment missing %q: %s", want, body)
		}
	}
	if strings.Count(body, `id="memory-status"`) != 1 || strings.Count(body, `id="memory-results"`) != 1 {
		t.Fatalf("zero fragment duplicated stable IDs: %s", body)
	}
	if !strings.Contains(body, `id="memory-status"`) || !strings.Contains(body, `aria-live="polite"`) {
		t.Fatalf("memory status lost its polite live-region owner: %s", body)
	}
	resultsStart := strings.Index(body, `id="memory-results"`)
	resultsEnd := strings.Index(body[resultsStart:], ">")
	if resultsStart < 0 || resultsEnd < 0 || strings.Contains(body[resultsStart:resultsStart+resultsEnd+1], "aria-live") {
		t.Fatalf("memory results became a second live owner: %s", body)
	}
}

func TestMemorySessionFragmentUsesEscapedSafeChoiceDataAndErrorRecovery(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8422/api/v1/desk/memory/sessions", nil)
	choice := MemorySessionChoice{ID: "session-full-id-1234567890", Label: "<Duplicate title>", Summary: "Summary & context", ModelAlias: "model/alias", UpdatedAt: time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC), Pinned: true}
	component := fragmentComponent(request, http.StatusOK, MemorySessionsResponse{Choices: []MemorySessionChoice{choice}})
	var rendered bytes.Buffer
	if err := component.Render(request.Context(), &rendered); err != nil {
		t.Fatal(err)
	}
	body := rendered.String()
	for _, want := range []string{
		`id="memory-session"`,
		`type="hidden"`,
		`data-session-id="session-full-id-1234567890"`,
		`data-session-summary="Summary &amp; context"`,
		`data-session-model-alias="model/alias"`,
		"Duplicate title",
		`data-session-pinned="true"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("session picker fragment missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, "<select") || strings.Contains(body, ">session-full-id-1234567890<") {
		t.Fatalf("session picker exposed a native select or full ID as primary text: %s", body)
	}

	errorComponent := fragmentComponent(request, http.StatusServiceUnavailable, errorResponse{Code: "memory_unavailable", Message: "memory request could not be completed"})
	rendered.Reset()
	if err := errorComponent.Render(request.Context(), &rendered); err != nil {
		t.Fatal(err)
	}
	errorBody := rendered.String()
	for _, want := range []string{`id="memory-session-field"`, "Conversations could not be loaded.", "Try again", `id="memory-session"`} {
		if !strings.Contains(errorBody, want) {
			t.Errorf("session picker error missing %q: %s", want, errorBody)
		}
	}
}

func TestMemoryPartialFragmentKeepsHealthyCardsAndOneNonLiveWarning(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8422/api/v1/desk/memory?query=healthy", nil)
	component := fragmentComponent(request, http.StatusOK, MemorySearchResponse{
		Query:  "healthy",
		Hits:   []MemoryHit{{Source: MemorySourceSummary, SourceID: "summary-1", Excerpt: "Healthy result"}},
		Errors: []*SectionError{newSectionError("notes", errors.New("notes unavailable"))},
	})
	var rendered bytes.Buffer
	if err := component.Render(request.Context(), &rendered); err != nil {
		t.Fatal(err)
	}
	body := rendered.String()
	for _, want := range []string{
		"Healthy result",
		"1 result(s) — some memory sources are unavailable.",
		`class="memory-partial-warning"`,
		"Some memory sources are unavailable.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("partial fragment missing %q: %s", want, body)
		}
	}
	resultsStart := strings.Index(body, `id="memory-results"`)
	resultsEnd := strings.Index(body[resultsStart:], ">")
	if strings.Contains(body, "No memory matched that search") || resultsStart < 0 || resultsEnd < 0 || strings.Contains(body[resultsStart:resultsStart+resultsEnd+1], `aria-live="polite"`) {
		t.Fatalf("partial fragment rendered an empty/live result region: %s", body)
	}
}
