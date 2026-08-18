package ui

import (
	"bytes"
	"strings"
	"testing"
)

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
			artwork, ok := WaffleEmptyStateMap[tc.key]
			if !ok {
				t.Fatalf("missing semantic empty-state map entry %q", tc.key)
			}
			if artwork.AssetName != tc.assetName || artwork.Width != tc.width || artwork.Height != tc.height {
				t.Fatalf("empty state artwork %q = %#v", tc.key, artwork)
			}
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
	primary := FragmentAction{ID: "retry", Label: "Retry", Method: "post", URL: "/retry", Target: "#tasks-list", Swap: "outerHTML"}
	secondary := FragmentAction{ID: "filter", Label: "Show active", Method: "get", URL: "/tasks?filter=active"}
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
}
