package dashboard

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/matt-riley/waffle/internal/dashboard/ui"
	"github.com/matt-riley/waffle/internal/providerconfig"
	"github.com/matt-riley/waffle/internal/skillinstall"
	"github.com/matt-riley/waffle/internal/workset"
)

// fragmentResponseWriter marks a response as an HTML negotiation while
// retaining the coordinator-owned AfterResponse boundary used by restart
// mutations.
type fragmentResponseWriter struct {
	http.ResponseWriter
	request *http.Request
}

func (w fragmentResponseWriter) WantsHTML() bool { return true }

func (w fragmentResponseWriter) FragmentRequest() *http.Request { return w.request }

type fragmentAfterResponseWriter struct {
	fragmentResponseWriter
	after AfterResponseWriter
}

func (w fragmentAfterResponseWriter) AfterResponse(callback func() RestartScheduleOutcome) {
	w.after.AfterResponse(callback)
}

type wantsHTMLWriter interface {
	WantsHTML() bool
	FragmentRequest() *http.Request
}

func negotiateFragments(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !wantsHTMLRequest(r) {
			w.Header().Add("Vary", "Accept")
			w.Header().Add("Vary", "HX-Request")
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Vary", "Accept, HX-Request")
		marked := fragmentResponseWriter{ResponseWriter: w, request: r}
		if after, ok := w.(AfterResponseWriter); ok {
			next.ServeHTTP(fragmentAfterResponseWriter{fragmentResponseWriter: marked, after: after}, r)
			return
		}
		next.ServeHTTP(marked, r)
	})
}

func wantsHTMLRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	accept := strings.ToLower(r.Header.Get("Accept"))
	// JSON is the explicit compatibility contract, even when an intermediary
	// or client also forwards HX-Request.
	if strings.Contains(accept, "application/json") {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("HX-Request")), "true") {
		return true
	}
	return strings.Contains(accept, "text/html") && !strings.Contains(accept, "application/json")
}

func writeNegotiatedValue(w http.ResponseWriter, status int, value any) bool {
	marked, ok := w.(wantsHTMLWriter)
	if !ok || !marked.WantsHTML() {
		return false
	}
	if r := marked.FragmentRequest(); r != nil && r.Method != http.MethodGet && status >= 200 && status < 300 {
		w.Header().Set("HX-Trigger", "waffle:refresh")
	}
	if response, ok := value.(WorkspaceMutationResponse); ok && response.TodayURL != "" && status >= 200 && status < 300 {
		if r := marked.FragmentRequest(); r != nil && strings.HasSuffix(r.URL.Path, "/select") {
			w.Header().Set("HX-Redirect", response.TodayURL)
		}
	}
	component := fragmentComponent(marked.FragmentRequest(), status, value)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := component.Render(markedRequestContext(marked), w); err != nil {
		return true
	}
	return true
}

func markedRequestContext(marked wantsHTMLWriter) context.Context {
	if request := marked.FragmentRequest(); request != nil {
		return request.Context()
	}
	return context.Background()
}

func fragmentComponent(r *http.Request, status int, value any) templ.Component {
	if response, ok := value.(errorResponse); ok {
		return ui.FragmentStatus(ui.FragmentStatusView{Message: response.Message, Error: true})
	}
	if response, ok := value.(*errorResponse); ok && response != nil {
		return ui.FragmentStatus(ui.FragmentStatusView{Message: response.Message, Error: true})
	}
	switch typed := value.(type) {
	case CapabilitiesSnapshot:
		part := "all"
		if r != nil {
			part = r.URL.Query().Get("part")
		}
		fragment := capabilityFragment(typed, part)
		fragment.GetURL = "/api/v1/desk/capabilities?part=" + url.QueryEscape(part)
		fragment.Trigger = "waffle:refresh from:body"
		return ui.FragmentList(fragment)
	case CapabilityCatalogueView:
		return ui.FragmentList(catalogueFragment(typed))
	case TasksSnapshot:
		filter := "all"
		if r != nil && r.URL.Query().Get("filter") != "" {
			filter = r.URL.Query().Get("filter")
		}
		fragment := tasksFragment(typed)
		fragment.GetURL = "/api/v1/desk/tasks?filter=" + url.QueryEscape(filter)
		fragment.Trigger = "waffle:refresh from:body"
		return ui.FragmentList(fragment)
	case WorkspaceSnapshot:
		fragment := workspacesFragment(typed, nil)
		fragment.GetURL = "/api/v1/desk/workspaces"
		fragment.Trigger = "waffle:refresh from:body"
		return ui.FragmentList(fragment)
	case WorkspaceFragmentSnapshot:
		fragment := workspacesFragment(typed.Snapshot, typed.Git)
		fragment.GetURL = "/api/v1/desk/workspaces"
		fragment.Trigger = "waffle:refresh from:body"
		return ui.FragmentList(fragment)
	case WorkspaceClosePreview:
		return ui.WorkspaceCloseDialog(ui.WorkspaceCloseDialogView{
			Repository:   typed.Workspace.Repository,
			Dirty:        emptyValue(typed.Dirty, "Clean"),
			Unpushed:     emptyValue(typed.Unpushed, "None"),
			PreviewURL:   "/api/v1/desk/workspaces/" + url.PathEscape(typed.Workspace.ID) + "/close",
			PreviewToken: typed.PreviewToken,
			Eligible:     typed.Eligible,
			FocusID:      "workspace-close-" + typed.Workspace.ID,
		})
	case WorkspaceGitView:
		return ui.FragmentList(workspaceGitFragment(typed))
	case MemorySearchResponse:
		query := typed.Query
		if query == "" && r != nil {
			query = r.URL.Query().Get("query")
		}
		fragment := memoryFragment(typed.Hits, query, typed.Errors)
		fragment.GetURL = "/api/v1/desk/memory?query=" + url.QueryEscape(query)
		fragment.Trigger = "waffle:refresh from:body"
		return ui.FragmentList(fragment)
	case MemorySessionsResponse:
		options := make([]ui.MemorySessionOption, 0, len(typed.Choices))
		for _, choice := range typed.Choices {
			label := strings.TrimSpace(choice.Label)
			recency := memorySessionRecency(choice.UpdatedAt)
			if recency != "" {
				label += " · " + recency
			}
			if choice.Pinned {
				label += " · Pinned"
			}
			if id := shortSessionID(choice.ID); id != "" {
				label += " · " + id
			}
			options = append(options, ui.MemorySessionOption{ID: choice.ID, Label: label})
		}
		return ui.MemorySessionPicker(ui.MemorySessionPickerView{Choices: options})
	case MemoryForgetPreview:
		return ui.MemoryForgetDialog(ui.MemoryForgetDialogView{
			NoteID:       typed.Note.SourceID,
			Excerpt:      typed.Note.Excerpt,
			Scope:        typed.Scope,
			Excludes:     typed.Excludes,
			PreviewURL:   "/api/v1/desk/memory/" + url.PathEscape(typed.Note.SourceID) + "/forget",
			PreviewToken: typed.PreviewToken,
			FocusID:      "memory-forget-" + typed.Note.SourceID,
		})
	case MemoryAttachResponse:
		return ui.FragmentStatus(ui.FragmentStatusView{Message: "Memory reference attached to the session."})
	case MemoryForgetResult:
		return ui.FragmentStatus(ui.FragmentStatusView{Message: "Waffle-owned note archived."})
	case WorkspaceMutationResponse:
		return ui.FragmentStatus(ui.FragmentStatusView{Message: "Workspace state updated."})
	case WorkspaceCloseConflict:
		return ui.FragmentStatus(ui.FragmentStatusView{Message: typed.Message, Error: true})
	case TaskMutationResponse:
		return ui.FragmentStatus(ui.FragmentStatusView{Message: "Schedule saved."})
	case capabilityMutationResponse:
		message := "Capability change accepted."
		if typed.RestartRequired {
			message = "Capability change accepted; waiting for restart."
		}
		return ui.FragmentStatus(ui.FragmentStatusView{Message: message, RestartRequired: typed.RestartRequired})
	case CapabilityProviderTestResult:
		message, failed := capabilityProbeMessage(string(typed.Outcome))
		return ui.FragmentStatus(ui.FragmentStatusView{Message: message, Error: failed})
	case ConnectionTestResult:
		// A per-connection check re-renders the connections list so the card
		// health and last-check time update in place (#463).
		return ui.FragmentList(capabilityFragment(typed.Snapshot, "connections"))
	case skillinstall.Manifest:
		files := make([]ui.SkillReviewFileView, 0, len(typed.Files))
		for _, file := range typed.Files {
			files = append(files, ui.SkillReviewFileView{Path: file.Path, Size: strconv.FormatInt(file.Size, 10), SHA256: file.SHA256, Preview: file.Preview})
		}
		return ui.SkillReview(ui.SkillReviewView{
			Name: typed.Name, Description: emptyValue(typed.Description, "No description provided."), SourceRef: typed.SourceRef,
			ContentDigest: typed.ContentDigest, Files: files, AuditPassed: typed.Audit.Passed,
			AuditTitle: func() string {
				if typed.Audit.Passed {
					return "Audit passed"
				}
				return "Audit failed — do not install"
			}(),
			AuditMessage: func() string {
				if typed.Audit.Passed {
					return "No review flags were raised."
				}
				return "The source could not pass the safety review."
			}(),
			Flags:   typed.Audit.Flags,
			StageID: typed.StageID, ExpiresAt: typed.ExpiresAt.Format("2006-01-02T15:04:05Z07:00"),
		})
	case CapabilitySkill:
		// This fragment is all the Desk UI shows for the operation, so a
		// committed install with no audit row has to say so here or the
		// operator never learns of it (#297).
		if strings.HasSuffix(typed.InstallDisposition, InstallDispositionUnaudited) {
			return ui.FragmentStatus(ui.FragmentStatusView{
				Message: "Skill installed, but its policy audit record was not written; check the host log.",
			})
		}
		return ui.FragmentStatus(ui.FragmentStatusView{Message: "Skill operation completed."})
	case struct{}:
		return ui.FragmentStatus(ui.FragmentStatusView{Message: "Request completed."})
	default:
		if status >= 400 {
			return ui.FragmentStatus(ui.FragmentStatusView{Message: "The Desk request could not be completed.", Error: true})
		}
		return ui.FragmentStatus(ui.FragmentStatusView{Message: "Request completed."})
	}
}

func capabilityFragment(snapshot CapabilitiesSnapshot, part string) ui.FragmentView {
	view := ui.FragmentView{ID: "capability-" + part, Class: "capability-list", Empty: "No capability records are available."}
	switch part {
	case "models":
		view.ID = "capability-models"
		view.Empty = "No model aliases are enrolled."
		aliases := make([]string, 0, len(snapshot.Providers.Models))
		for alias := range snapshot.Providers.Models {
			aliases = append(aliases, alias)
		}
		sort.Strings(aliases)
		for _, alias := range aliases {
			model := snapshot.Providers.Models[alias]
			kind := "model"
			roles := make([]string, 0, 2)
			if alias == snapshot.Providers.DefaultModel {
				roles = append(roles, "Waffle-wide default")
			}
			if alias == snapshot.Providers.UtilityModel {
				roles = append(roles, "Utility model")
			}
			if len(roles) > 0 {
				kind = strings.Join(roles, " / ")
			}
			item := ui.FragmentItem{ID: alias, Class: "capability-card", Kind: kind, Title: alias, Fields: []ui.FragmentField{{Label: "Connection", Value: model.Provider}, {Label: "Provider model", Value: model.Model}}}
			item.Actions = append(item.Actions,
				ui.FragmentAction{ID: "model-default-" + alias, Label: "Make default", URL: "/api/v1/desk/models/default", Target: "#capability-default-status", Swap: "innerHTML", Fields: []ui.FragmentField{{Label: "alias", Value: alias}}},
				ui.FragmentAction{ID: "model-utility-" + alias, Label: "Make utility", URL: "/api/v1/desk/models/utility", Target: "#capability-utility-status", Swap: "innerHTML", Fields: []ui.FragmentField{{Label: "alias", Value: alias}}},
			)
			view.Items = append(view.Items, item)
		}
		view.OptionLists = capabilityModelOptions(snapshot, aliases)
	case "skills":
		view.ID = "capability-skills"
		view.Empty = "No reviewed skills are installed."
		for _, skill := range snapshot.Skills {
			status := "inactive"
			if skill.Active {
				status = "active"
			}
			detail := skill.Description
			if skill.Missing && skill.Attached {
				detail = "Missing from the library — this session still references it"
			} else if skill.Active {
				detail = "Active" + suffixDetail(detail)
			} else {
				detail = "Installed inactive — review before activation" + suffixDetail(detail)
			}
			item := ui.FragmentItem{ID: skill.Name, Class: "capability-card", Kind: status, Title: skill.Name, Detail: detail}
			if skill.Missing && skill.Attached {
				item.Detail = "Missing from the library — detach or reinstall before this session can use it."
			} else if skill.Active {
				item.Actions = append(item.Actions, ui.FragmentAction{ID: "skill-deactivate-" + skill.Name, Label: "Deactivate", URL: "/api/v1/desk/skills/" + url.PathEscape(skill.Name) + "/deactivate", Target: "#capability-skills", Swap: "outerHTML"})
			} else {
				item.Actions = append(item.Actions,
					ui.FragmentAction{ID: "skill-activate-" + skill.Name, Label: "Activate", URL: "/api/v1/desk/skills/" + url.PathEscape(skill.Name) + "/activate", Target: "#capability-skills", Swap: "outerHTML"},
					ui.FragmentAction{ID: "skill-uninstall-" + skill.Name, Label: "Uninstall", URL: "/api/v1/desk/skills/" + url.PathEscape(skill.Name) + "/uninstall", Target: "#capability-skills", Swap: "outerHTML"},
				)
			}
			view.Items = append(view.Items, item)
		}
		view.TextSwaps = append(view.TextSwaps,
			ui.FragmentTextSwap{ID: "capability-skill-local-help", Text: capabilitySourceHelp("Allowed local roots", snapshot.SkillSources.LocalRoots), Source: "local", Available: len(snapshot.SkillSources.LocalRoots) > 0},
			ui.FragmentTextSwap{ID: "capability-skill-git-help", Text: capabilitySourceHelp("Allowed Git hosts", snapshot.SkillSources.GitHosts), Source: "git", Available: len(snapshot.SkillSources.GitHosts) > 0},
		)
	case "connections":
		view.ID = "capability-connections"
		view.Empty = "No enrolled connections."
		connections := make([]string, 0, len(snapshot.Providers.Providers))
		for name := range snapshot.Providers.Providers {
			connections = append(connections, name)
		}
		sort.Strings(connections)
		for _, name := range connections {
			provider := snapshot.Providers.Providers[name]
			probe, probed := snapshot.Probes[name]
			health, healthClass := connectionHealth(probe, probed)
			// Endpoint values stay private per the surface copy (#463); the
			// card leads with health and translates stored configuration.
			item := ui.FragmentItem{
				ID: name, Class: "connection-card" + healthClass, Kind: health, Title: name,
				Fields: []ui.FragmentField{
					{Label: "Compatibility protocol", Value: connectionProtocolLabel(provider.Type)},
					{Label: "Maximum tokens", Value: connectionMaxTokensLabel(provider.MaxTokens)},
					{Label: "Last check", Value: connectionLastCheckLabel(probe, probed)},
				},
			}
			if probed && probe.Outcome != providerconfig.ProbeOutcomeSuccess {
				item.Detail, _ = capabilityProbeMessage(string(probe.Outcome))
			}
			item.Actions = append(item.Actions, ui.FragmentAction{ID: "connection-check-" + name, Label: "Check connection", URL: "/api/v1/desk/providers/" + url.PathEscape(name) + "/test", Target: "#capability-connections", Swap: "outerHTML"})
			view.Items = append(view.Items, item)
		}
	default:
		view.ID = "capability-models"
		view.Items = append(view.Items, capabilityFragment(snapshot, "models").Items...)
	}
	return view
}

func suffixDetail(detail string) string {
	if strings.TrimSpace(detail) == "" {
		return ""
	}
	return " — " + detail
}

func capabilityModelOptions(snapshot CapabilitiesSnapshot, aliases []string) []ui.FragmentOptionList {
	providers := make([]string, 0, len(snapshot.Providers.Providers))
	for name := range snapshot.Providers.Providers {
		providers = append(providers, name)
	}
	sort.Strings(providers)
	options := make([]ui.FragmentOption, 0, len(aliases))
	for _, alias := range aliases {
		options = append(options, ui.FragmentOption{Value: alias, Label: alias})
	}
	providerOptions := make([]ui.FragmentOption, 0, len(providers))
	for _, name := range providers {
		providerOptions = append(providerOptions, ui.FragmentOption{Value: name, Label: name})
	}
	return []ui.FragmentOptionList{
		{ID: "capability-default-alias", Name: "alias", Required: true, Options: options},
		{ID: "capability-utility-alias", Name: "alias", Required: true, Options: options},
		{ID: "capability-catalogue-connection", Name: "connection", Required: true, Options: providerOptions},
		{ID: "capability-model-connection", Name: "connection_name", Required: true, Options: providerOptions},
	}
}

func capabilitySourceHelp(label string, values []string) string {
	if len(values) == 0 {
		return label + ": none configured; this source is disabled."
	}
	return label + ": " + strings.Join(values, ", ")
}

func capabilityProbeMessage(outcome string) (string, bool) {
	switch outcome {
	case "success":
		return "Connection test succeeded.", false
	case "authentication_failed":
		return "Connection test authentication failed; check the credential.", true
	case "request_failed":
		return "Connection test reached the endpoint, but the request was rejected; check the model ID.", true
	case "unreachable":
		return "Connection test could not reach the endpoint.", true
	default:
		return "Connection test could not be completed.", true
	}
}

func catalogueFragment(view CapabilityCatalogueView) ui.FragmentView {
	fragment := ui.FragmentView{ID: "capability-catalogue-results", Class: "capability-list", Empty: "No catalogue models matched this connection.", Status: view.Warning}
	for index, model := range view.Models {
		title := model.DisplayName
		if title == "" {
			title = model.ID
		}
		item := ui.FragmentItem{ID: model.ID, Class: "catalogue-card", Kind: model.Owner, Title: title, Detail: catalogueModelDetail(model), Fields: []ui.FragmentField{{Label: "Model ID", Value: model.ID}, {Label: "Context window", Value: formatInt64(model.ContextWindow)}}}
		if model.EnrolledAlias == "" {
			item.Actions = append(item.Actions, ui.FragmentAction{
				ID: "catalogue-add-" + strconv.Itoa(index), Label: "Add as alias", URL: "/api/v1/desk/models", Target: "#capability-model-status", Swap: "innerHTML",
				Fields: []ui.FragmentField{{Label: "connection_name", Value: view.Connection}, {Label: "upstream_model", Value: model.ID}},
				Inputs: []ui.FragmentInput{{ID: "capability-catalogue-alias-" + strconv.Itoa(index), Name: "alias", Type: "text", Label: "Alias", Placeholder: "e.g. provider-model", Value: model.AliasSuggestion, Required: true}},
			})
		}
		fragment.Items = append(fragment.Items, item)
	}
	return fragment
}

func catalogueModelDetail(model CapabilityCatalogueModel) string {
	if model.EnrolledAlias != "" {
		return "Enrolled as " + model.EnrolledAlias + "."
	}
	return ""
}

// connectionHealth renders the trustworthy health summary for a connection
// card: unchecked until the first probe, then the classified outcome (#463).
func connectionHealth(probe ConnectionProbe, probed bool) (string, string) {
	if !probed {
		return "Unchecked", " is-unchecked"
	}
	switch probe.Outcome {
	case providerconfig.ProbeOutcomeSuccess:
		return "Healthy", " is-healthy"
	case providerconfig.ProbeOutcomeAuthentication, providerconfig.ProbeOutcomeUnreachable:
		return "Failed", " is-failed"
	default:
		return "Degraded", " is-degraded"
	}
}

// connectionProtocolLabel explains that a provider type is the compatibility
// protocol/driver rather than naming the remote service (#463).
func connectionProtocolLabel(protocol string) string {
	switch strings.ToLower(strings.TrimSpace(protocol)) {
	case "openai":
		return "OpenAI-compatible driver"
	case "anthropic":
		return "Anthropic-compatible driver"
	case "":
		return "Not reported"
	default:
		return strings.TrimSpace(protocol) + "-compatible driver"
	}
}

// connectionMaxTokensLabel renders an unset limit as a provider default
// instead of a misleading zero-token cap (#463).
func connectionMaxTokensLabel(tokens int) string {
	if tokens <= 0 {
		return "Provider default"
	}
	return formatInt(tokens)
}

func connectionLastCheckLabel(probe ConnectionProbe, probed bool) string {
	if !probed {
		return "Never"
	}
	age := time.Since(probe.CheckedAt)
	if age < 0 {
		age = 0
	}
	switch {
	case age < time.Minute:
		return "Just now"
	case age < time.Hour:
		return fmt.Sprintf("%d minutes ago", int(age.Minutes()))
	default:
		return probe.CheckedAt.Format("2006-01-02 15:04")
	}
}

// tasksAttentionLabel renders the settled attention chip truthfully: the
// initial page state only ever claims to be checking, and a settled fragment
// reports the real count or a distinct error state (#456).
func attentionEvidenceFailed(errors []*SectionError) bool {
	for _, err := range errors {
		if err != nil && (err.Section == OperationsSectionJobs || err.Section == OperationsSectionRuns) {
			return true
		}
	}
	return false
}

func tasksAttentionLabel(count int, hasErrors bool) string {
	if hasErrors {
		return "Attention unavailable"
	}
	switch count {
	case 0:
		return "No tasks need attention"
	case 1:
		return "1 task needs attention"
	default:
		return fmt.Sprintf("%d tasks need attention", count)
	}
}

func tasksFragment(snapshot TasksSnapshot) ui.FragmentView {
	fragment := ui.FragmentView{ID: "tasks-list", Class: "task-list", Empty: "No tasks match this view.", Filters: taskFilterFragments(snapshot.Filter)}
	attentionClass := "tasks-attention"
	attentionFailed := attentionEvidenceFailed(snapshot.Errors)
	if len(snapshot.Errors) > 0 {
		fragment.Status = "Some task evidence is temporarily unavailable."
	}
	if attentionFailed {
		attentionClass += " is-error"
	}
	fragment.TextSwaps = append(fragment.TextSwaps, ui.FragmentTextSwap{
		ID:    "tasks-attention-count",
		Class: attentionClass,
		Text:  tasksAttentionLabel(snapshot.AttentionCount, attentionFailed),
	})
	for _, task := range snapshot.Tasks {
		kind := task.Kind + " / " + task.Source
		title := task.Name
		if title == "" {
			title = task.EvidenceLabel
		}
		item := ui.FragmentItem{ID: task.ID, DataTaskID: task.ID, Class: "task-card", Kind: kind, Title: title, Fields: []ui.FragmentField{{Label: "State", Value: emptyValue(task.Phase, emptyValue(task.Outcome, "scheduled"))}, {Label: "Profile", Value: emptyValue(task.Profile, "default")}, {Label: "Evidence", Value: task.EvidenceLabel}}}
		if task.Kind == TaskKindSchedule {
			item.DataTaskName = task.Name
			item.DataTaskCron = task.Cron
			item.DataTaskPrompt = task.Prompt
			item.DataTaskDeliver = task.Deliver
			item.DataTaskProfile = task.Profile
			item.DataTaskEnabled = task.Enabled
			item.DataTaskRedactedFields = strings.Join(task.RedactedFields, ",")
			// The human schedule leads; the raw cron stays inspectable in the
			// advanced editor (#460).
			item.Fields = append(item.Fields,
				ui.FragmentField{Label: "Schedule", Value: emptyValue(task.HumanCron, task.Cron)},
				ui.FragmentField{Label: "Next run", Value: emptyValue(taskTimeLabel(task.NextRun), "—")},
			)
			item.Actions = append(item.Actions, ui.FragmentAction{ID: "task-edit-" + task.ID, Label: "Edit schedule", Method: "edit"})
		}
		if task.OpenAtDesk && task.SessionID != "" {
			item.Actions = append(item.Actions, ui.FragmentAction{Label: "Open at Desk", Method: "get", URL: "/desk/?section=today&session_id=" + url.QueryEscape(task.SessionID)})
		}
		fragment.Items = append(fragment.Items, item)
	}
	return fragment
}

func taskFilterFragments(active TaskFilter) []ui.FragmentFilter {
	filters := []struct {
		name  TaskFilter
		label string
	}{
		{name: TaskFilterAll, label: "All"},
		{name: TaskFilterActive, label: "Active"},
		{name: TaskFilterScheduled, label: "Scheduled"},
		{name: TaskFilterCompleted, label: "Completed"},
		{name: TaskFilterAttention, label: "Attention"},
	}
	result := make([]ui.FragmentFilter, 0, len(filters))
	for _, filter := range filters {
		result = append(result, ui.FragmentFilter{ID: "task-filter-" + string(filter.name), Name: string(filter.name), URL: "/api/v1/desk/tasks?filter=" + url.QueryEscape(string(filter.name)), Label: filter.label, Pressed: filter.name == active})
	}
	return result
}

func workspacesFragment(snapshot WorkspaceSnapshot, git map[string]WorkspaceGitView) ui.FragmentView {
	fragment := ui.FragmentView{ID: "workspaces-list", Class: "workspaces-grid", Empty: "No guarded workspaces are open."}
	summary := map[string]int{}
	for _, workspace := range snapshot.Workspaces {
		summary[workspace.Status]++
	}
	fragment.TextSwaps = append(fragment.TextSwaps, ui.FragmentTextSwap{
		ID:    "workspaces-summary",
		Class: "workspaces-summary",
		Text:  workspaceSummaryLabel(summary),
	})
	for _, workspace := range snapshot.Workspaces {
		// Truthful human empty states: never a blank definition row (#462).
		fields := []ui.FragmentField{
			{Label: "Profile", Value: emptyValue(workspace.Profile, "No profile")},
			{Label: "Network", Value: emptyValue(workspace.Egress, "No network")},
		}
		item := ui.FragmentItem{ID: workspace.ID, DataWorkspaceID: workspace.ID, Class: "workspace-card", Kind: workspace.Status, Title: workspace.Repository, Fields: fields}
		if status, ok := git[workspace.ID]; ok && workspace.Status != "closed" {
			item.DetailClass = "workspace-git"
			item.Detail = workspaceGitDetail(status)
		}
		if workspace.Status == "open" {
			item.Actions = append(item.Actions, ui.FragmentAction{Class: "workspace-primary", Label: "Open at Desk", URL: "/api/v1/desk/workspaces/" + url.PathEscape(workspace.ID) + "/select", Target: "#workspaces-list", Swap: "innerHTML"})
			item.Actions = append(item.Actions, ui.FragmentAction{Label: "Idle", URL: "/api/v1/desk/workspaces/" + url.PathEscape(workspace.ID) + "/idle", Target: "#workspaces-list", Swap: "innerHTML"})
		}
		if workspace.Status == "idle" {
			item.Actions = append(item.Actions, ui.FragmentAction{Label: "Open at Desk", URL: "/api/v1/desk/workspaces/" + url.PathEscape(workspace.ID) + "/select", Target: "#workspaces-list", Swap: "innerHTML"})
			item.Actions = append(item.Actions, ui.FragmentAction{Class: "workspace-primary", Label: "Resume", URL: "/api/v1/desk/workspaces/" + url.PathEscape(workspace.ID) + "/resume", Target: "#workspaces-list", Swap: "innerHTML"})
		}
		if workspace.SessionID != "" {
			item.Actions = append(item.Actions, ui.FragmentAction{Method: "copy", Value: workspace.SessionID, Label: "Copy session ID"})
		}
		if workspace.Status != "closed" {
			item.Actions = append(item.Actions, ui.FragmentAction{ID: "workspace-close-" + workspace.ID, Class: "workspace-danger-action", Label: "Review close", URL: "/api/v1/desk/workspaces/" + url.PathEscape(workspace.ID) + "/close-preview", Target: "#workspace-close-dialog", Swap: "outerHTML"})
		}
		fragment.Items = append(fragment.Items, item)
	}
	return fragment
}

func workspaceSummaryLabel(summary map[string]int) string {
	order := []string{"open", "idle", "failed", "closed"}
	parts := make([]string, 0, len(order))
	for _, status := range order {
		count := summary[status]
		if count == 0 {
			continue
		}
		// Statuses are adjectives here ("2 open", "1 idle"), never pluralised.
		parts = append(parts, fmt.Sprintf("%d %s", count, status))
	}
	if len(parts) == 0 {
		return "No guarded workspaces are open."
	}
	return strings.Join(parts, " · ")
}

func workspaceGitFragment(view WorkspaceGitView) ui.FragmentView {
	detail := view.Reason
	if view.Available {
		detail = view.Branch + ": " + view.Subject
	}
	return ui.FragmentView{ID: "workspace-git-" + view.WorkspaceID, Class: "workspace-git", Empty: "Git status unavailable.", Items: []ui.FragmentItem{{ID: view.WorkspaceID, Class: "workspace-git-card", Title: "Git status", Detail: detail}}}
}

func memoryFragment(hits []MemoryHit, query string, sectionErrors []*SectionError) ui.FragmentView {
	empty := "No attributed memory matched that search."
	if len(sectionErrors) > 0 && len(hits) == 0 {
		empty = "Memory search could not be completed right now."
	}
	fragment := ui.FragmentView{ID: "memory-results", Class: "memory-results", Empty: empty}
	// The status line is swapped with every search so the initial instruction
	// never coexists with settled results (#458).
	fragment.TextSwaps = append(fragment.TextSwaps, ui.FragmentTextSwap{
		ID:    "memory-status",
		Class: "memory-status",
		Text:  memoryStatusMessage(len(hits), sectionErrors),
	})
	for _, hit := range hits {
		fields := []ui.FragmentField{
			{Label: "Source", Value: emptyValue(hit.Provenance, "—")},
			{Label: "Time", Value: emptyValue(memoryTimeLabel(hit.Timestamp), "—")},
			{Label: "Source ID", Value: hit.SourceID},
		}
		item := ui.FragmentItem{
			ID: hit.SourceID, Class: "memory-hit", Kind: memorySourceLabel(hit.Source),
			Excerpt: hit.Excerpt, ExcerptLong: len(hit.Excerpt) > memoryExcerptClampBytes,
			Fields:  fields,
			Actions: []ui.FragmentAction{{ID: "memory-attach-" + hit.SourceID, Label: "Attach to session", URL: "/api/v1/desk/memory/attach", Target: "#memory-attach-status", Swap: "innerHTML", Include: "#memory-session", Fields: []ui.FragmentField{{Label: "query", Value: query}, {Label: "source", Value: hit.Source}, {Label: "source_id", Value: hit.SourceID}}}},
		}
		if hit.Archived {
			item.Class += " is-archived"
		}
		if hit.Source == MemorySourceNote && !hit.Archived {
			item.Actions = append(item.Actions, ui.FragmentAction{ID: "memory-forget-" + hit.SourceID, Label: "Forget…", URL: "/api/v1/desk/memory/" + url.PathEscape(hit.SourceID) + "/forget-preview", Target: "#memory-forget-dialog", Swap: "outerHTML", Fields: []ui.FragmentField{{Label: "query", Value: query}}})
		}
		fragment.Items = append(fragment.Items, item)
	}
	return fragment
}

const memoryExcerptClampBytes = 180

func memoryStatusMessage(hits int, sectionErrors []*SectionError) string {
	switch {
	case len(sectionErrors) > 0 && hits == 0:
		return "Memory search is unavailable right now."
	case len(sectionErrors) > 0:
		return fmt.Sprintf("%d result(s) — some memory sources are unavailable.", hits)
	case hits == 0:
		return "No attributed memory matched that search."
	case hits == 1:
		return "1 result"
	default:
		return fmt.Sprintf("%d results", hits)
	}
}

func shortSessionID(id string) string {
	id = strings.TrimSpace(id)
	if len(id) <= 8 {
		return id
	}
	return id[len(id)-8:]
}

func memorySessionRecency(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	age := time.Since(value)
	if age < 0 {
		age = 0
	}
	switch {
	case age < time.Hour:
		return "updated moments ago"
	case age < 24*time.Hour:
		return fmt.Sprintf("updated %d hours ago", int(age.Hours()))
	default:
		return "updated " + value.Format("2 Jan 2006")
	}
}

func memoryTimeLabel(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.Format("2 Jan 2006 15:04")
}

func memorySourceLabel(source string) string {
	switch source {
	case MemorySourceNote:
		return "Note"
	case MemorySourceSummary:
		return "Summary"
	case MemorySourceTurn:
		return "Turn"
	default:
		return source
	}
}

func workspaceGitDetail(view WorkspaceGitView) string {
	if !view.Available {
		return emptyValue(view.Reason, "Git status is unavailable for this workspace.")
	}
	branch := view.Branch
	if view.Detached {
		branch = "Detached HEAD"
	}
	branch = emptyValue(branch, "Unknown branch")
	dirty := "Clean"
	if view.Dirty {
		label := "files"
		if view.DirtyFiles == 1 {
			label = "file"
		}
		dirty = strconv.Itoa(view.DirtyFiles) + " uncommitted " + label
	}
	tracking := "No upstream branch"
	if view.Tracking {
		tracking = strconv.Itoa(view.Ahead) + " ahead · " + strconv.Itoa(view.Behind) + " behind"
	}
	commit := "No commits yet"
	if view.CommitSHA != "" {
		commit = view.CommitSHA + " " + view.Subject
	}
	return "Branch: " + branch + " · Working tree: " + dirty + " · Tracking: " + tracking + " · Last commit: " + commit
}

func emptyValue(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func formatInt(value int) string { return strconv.Itoa(value) }

func formatInt64(value int64) string { return strconv.FormatInt(value, 10) }

// Keep the response type named so both JSON and fragment paths share the same
// public object instead of one path inventing a second payload shape.
type MemorySearchResponse struct {
	Query  string          `json:"query"`
	Hits   []MemoryHit     `json:"hits"`
	Errors []*SectionError `json:"errors"`
}

// MemorySessionsResponse is the bounded, credential-free session list that
// powers the memory attach picker (#459).
type MemorySessionsResponse struct {
	Choices []MemorySessionChoice `json:"choices"`
}

type MemoryAttachResponse struct {
	Entry *workset.Entry `json:"entry"`
}

type TaskMutationResponse struct {
	Task TaskView `json:"task"`
}

type WorkspaceFragmentSnapshot struct {
	Snapshot WorkspaceSnapshot           `json:"snapshot"`
	Git      map[string]WorkspaceGitView `json:"git"`
}

func taskTimeLabel(value *time.Time) string {
	if value == nil || value.IsZero() {
		return ""
	}
	return value.Format("2 Jan 2006 15:04")
}
