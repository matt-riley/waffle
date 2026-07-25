package ui

import (
	"bytes"
	"context"
	"regexp"
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
		`id="task-schedule-deliver-clear"`,
		`id="task-schedule-deliver-clear-row"`,
		`id="task-schedule-profile"`,
		`id="task-schedule-profile-clear"`,
		`id="task-schedule-profile-clear-row"`,
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
	// List container must not announce full re-renders; status regions keep aria-live.
	listIdx := strings.Index(body, `id="tasks-list"`)
	if listIdx < 0 {
		t.Fatal("missing tasks-list")
	}
	end := strings.Index(body[listIdx:], ">")
	openTag := body[listIdx : listIdx+end+1]
	if strings.Contains(openTag, `aria-live`) {
		t.Errorf("tasks-list must not carry aria-live: %s", openTag)
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

func TestAppCSSIncludesInputInSharedControlBaseline(t *testing.T) {
	contents, err := assetFiles.ReadFile("assets/app.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(contents)
	for _, required := range []string{
		"font: inherit",
		"input:disabled",
		`input:where(:not([type="checkbox"]):not([type="radio"]):not([type="range"]):not([type="file"]):not([type="hidden"]):not([type="button"]):not([type="submit"]):not([type="reset"]))`,
	} {
		if !strings.Contains(css, required) {
			t.Errorf("app.css control baseline missing %q", required)
		}
	}
	// font: inherit selector list must include input.
	if !regexp.MustCompile(`(?s)a,\s*button,\s*select,\s*textarea,\s*input\s*\{[^}]*font:\s*inherit`).MatchString(css) {
		t.Error("input must share font: inherit with a, button, select, textarea")
	}
}
