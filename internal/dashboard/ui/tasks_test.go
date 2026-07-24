package ui

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTasksRendersFiltersSummaryEvidenceAndScheduleForm(t *testing.T) {
	var rendered bytes.Buffer
	if err := Tasks(ShellView{ActiveSection: "tasks"}).Render(context.Background(), &rendered); err != nil {
		t.Fatal(err)
	}
	body := rendered.String()
	for _, required := range []string{
		`class="tasks"`,
		`id="tasks-attention-count"`,
		`id="tasks-list"`,
		`id="tasks-errors"`,
		`id="tasks-empty"`,
		`id="task-schedule-form"`,
		`id="task-schedule-name"`,
		`id="task-schedule-cron"`,
		`id="task-schedule-prompt"`,
		`id="task-schedule-deliver"`,
		`id="task-schedule-profile"`,
		`id="task-schedule-enabled-row"`,
		`id="task-schedule-enabled-row" for="task-schedule-enabled" hidden`,
		`data-task-filter="all"`,
		`data-task-filter="active"`,
		`data-task-filter="scheduled"`,
		`data-task-filter="completed"`,
		`data-task-filter="attention"`,
		`aria-live="polite"`,
	} {
		if !strings.Contains(body, required) {
			t.Errorf("Tasks view missing %q", required)
		}
	}
}

func TestTasksStylesProvideResponsiveCardsAndAttentionState(t *testing.T) {
	contents, err := assetFiles.ReadFile("assets/app.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(contents)
	for _, required := range []string{
		".tasks-layout",
		".task-card",
		".task-card.is-attention",
		".task-filter[aria-pressed=\"true\"]",
		"@media (max-width: 900px)",
	} {
		if !strings.Contains(css, required) {
			t.Errorf("Tasks CSS missing %q", required)
		}
	}
}

func TestServeTaskAssetServesOnlyTasksClient(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/desk/assets/tasks.js", nil)
	if !ServeTaskAsset(rec, req, "tasks.js") {
		t.Fatal("tasks.js was not served")
	}
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/javascript") {
		t.Fatalf("asset response = %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if ServeTaskAsset(httptest.NewRecorder(), req, "today.js") {
		t.Fatal("Tasks asset seam claimed an unrelated asset")
	}
}
