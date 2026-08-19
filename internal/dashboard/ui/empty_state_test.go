package ui

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

func TestEmptyStatePublicViewCannotOverrideApprovedArtwork(t *testing.T) {
	typeOfView := reflect.TypeOf(EmptyStateView{})
	if artwork, ok := typeOfView.FieldByName("Artwork"); ok && artwork.PkgPath == "" {
		t.Fatalf("EmptyStateView exposes mutable artwork metadata: %s", artwork.Type)
	}
}

func TestWaffleEmptyStateMapUsesApprovedDeskArtwork(t *testing.T) {
	cases := []struct {
		key       EmptyStateKey
		assetName string
		width     string
		height    string
	}{
		{key: EmptyStateTasks, assetName: "waffle-empty-curled.png", width: "480", height: "320"},
		{key: EmptyStateWorkspaces, assetName: "waffle-empty-sitting.png", width: "320", height: "320"},
		{key: EmptyStateMemory, assetName: "waffle-empty-curious.png", width: "256", height: "256"},
	}
	for _, tc := range cases {
		t.Run(string(tc.key), func(t *testing.T) {
			view, ok := NewWaffleEmptyStateView(tc.key, "Consumer-owned title", "Consumer-owned body", string(tc.key)+"-title", nil, nil)
			if !ok {
				t.Fatalf("constructor rejected known semantic key %q", tc.key)
			}
			if view.Title != "Consumer-owned title" || view.Body != "Consumer-owned body" || view.TitleID != string(tc.key)+"-title" {
				t.Fatalf("constructor lost consumer-owned copy for %q: %#v", tc.key, view)
			}
			var rendered bytes.Buffer
			if err := WaffleEmptyState(view).Render(t.Context(), &rendered); err != nil {
				t.Fatal(err)
			}
			body := rendered.String()
			for _, want := range []string{
				`class="waffle-empty-state"`,
				`role="region"`,
				`src="/desk/assets/` + tc.assetName + `?v=`,
				`alt=""`,
				`aria-hidden="true"`,
				`loading="lazy"`,
				`decoding="async"`,
				`width="` + tc.width + `"`,
				`height="` + tc.height + `"`,
				`<h2`,
				`<p`,
			} {
				if !strings.Contains(body, want) {
					t.Errorf("empty state %q missing %q: %s", tc.key, want, body)
				}
			}
			if strings.Count(body, `class="waffle-empty-state-action`) > 2 {
				t.Fatalf("empty state %q rendered more than two actions: %s", tc.key, body)
			}
		})
	}
	if _, ok := NewWaffleEmptyStateView(EmptyStateKey("unapproved"), "", "", "", nil, nil); ok {
		t.Fatal("constructor accepted an unapproved semantic artwork key")
	}
}

func TestFragmentListRendersOptionalStructuredEmptyStateOnlyWhenEmpty(t *testing.T) {
	state, ok := NewWaffleEmptyStateView(EmptyStateTasks, "Tasks title", "Tasks body", "tasks-title", nil, nil)
	if !ok {
		t.Fatal("failed to construct task empty state")
	}
	for _, tc := range []struct {
		name  string
		view  FragmentView
		found bool
	}{
		{name: "empty", view: FragmentView{ID: "empty-list", Class: "task-list", EmptyState: &state}, found: true},
		{name: "populated", view: FragmentView{ID: "populated-list", Class: "task-list", EmptyState: &state, Items: []FragmentItem{{ID: "task-1", Title: "A task"}}}, found: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rendered bytes.Buffer
			if err := FragmentList(tc.view).Render(t.Context(), &rendered); err != nil {
				t.Fatal(err)
			}
			got := strings.Contains(rendered.String(), `class="waffle-empty-state"`)
			if got != tc.found {
				t.Fatalf("structured empty state rendered = %t, want %t: %s", got, tc.found, rendered.String())
			}
		})
	}
}

func TestWaffleEmptyStateActionTiersAreExplicit(t *testing.T) {
	primary := FragmentAction{
		ID: "retry", Label: "Retry", Method: "post", URL: "/retry", Target: "#tasks-list", Swap: "outerHTML",
		Fields: []FragmentField{{Label: "session_id", Value: "session-1"}},
		Inputs: []FragmentInput{{ID: "retry-reason", Name: "reason", Type: "text", Label: "Reason", Placeholder: "Why?", Value: "later", Required: true}},
	}
	secondary := FragmentAction{
		ID: "filter", Label: "Show active", Method: "post", URL: "/tasks/filter", Target: "#tasks-list", Swap: "outerHTML",
		Fields: []FragmentField{{Label: "filter", Value: "active"}},
	}
	state, ok := NewWaffleEmptyStateView(EmptyStateTasks, "Tasks title", "Tasks body", "tasks-title", &primary, &secondary)
	if !ok {
		t.Fatal("failed to construct task empty state")
	}
	var rendered bytes.Buffer
	if err := WaffleEmptyState(state).Render(t.Context(), &rendered); err != nil {
		t.Fatal(err)
	}
	body := rendered.String()
	if strings.Count(body, `action-primary`) != 1 || strings.Count(body, `action-quiet`) != 1 {
		t.Fatalf("empty state action tiers = %s", body)
	}
	if !strings.Contains(body, `data-waffle-action-id="retry"`) || !strings.Contains(body, `data-waffle-action-id="filter"`) {
		t.Fatalf("empty state lost canonical action IDs: %s", body)
	}
	for _, want := range []string{
		`hx-post="/retry"`,
		`hx-target="#tasks-list"`,
		`hx-swap="outerHTML"`,
		`name="session_id" value="session-1"`,
		`id="retry-reason"`,
		`name="reason"`,
		`placeholder="Why?"`,
		`value="later"`,
		`required`,
		`hx-disabled-elt="this"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("empty state action missing canonical payload/submit attribute %q: %s", want, body)
		}
	}
	if strings.Count(body, `class="waffle-action-form-with-inputs"`) != 1 {
		t.Fatalf("expected only the visible-input action to receive the bounded form class: %s", body)
	}
}

func TestWaffleEmptyStateCanOmitArtworkWithoutChangingTheSemanticView(t *testing.T) {
	view, ok := NewWaffleEmptyStateView(EmptyStateTasks, "No tasks", "Nothing is queued", "tasks-title", nil, nil)
	if !ok {
		t.Fatal("failed to construct task empty state")
	}
	view.NoArtwork = true

	var rendered bytes.Buffer
	if err := WaffleEmptyState(view).Render(t.Context(), &rendered); err != nil {
		t.Fatal(err)
	}
	body := rendered.String()
	if !strings.Contains(body, `class="waffle-empty-state is-no-artwork"`) {
		t.Fatalf("artwork-free empty state lost its explicit class: %s", body)
	}
	if strings.Contains(body, `<img `) {
		t.Fatalf("artwork-free empty state rendered an image: %s", body)
	}
	for _, want := range []string{`<h2`, `No tasks`, `Nothing is queued`} {
		if !strings.Contains(body, want) {
			t.Errorf("artwork-free empty state missing %q: %s", want, body)
		}
	}
}

func TestFragmentActionFormClassOnlyVisibleInputs(t *testing.T) {
	fieldsOnly := FragmentAction{Fields: []FragmentField{{Label: "fixture", Value: "fields-only"}}}
	if got := fragmentActionFormClass(fieldsOnly); got != "" {
		t.Fatalf("fields-only action form class = %q, want compact default", got)
	}
	withInput := FragmentAction{Inputs: []FragmentInput{{ID: "note", Name: "note", Type: "text"}}}
	if got := fragmentActionFormClass(withInput); got != "waffle-action-form-with-inputs" {
		t.Fatalf("visible-input action form class = %q, want bounded input class", got)
	}
}
