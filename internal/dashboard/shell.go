package dashboard

import (
	"net/http"
	"strings"

	"github.com/matt-riley/waffle/internal/dashboard/ui"
)

// ShellHandler returns the additive Waffle Desk document and embedded asset handler.
// Callers mount it at /desk/ inside the existing secured dashboard boundary.
func ShellHandler(security *Security) http.Handler {
	return shellHandler(security, ui.ShellView{
		Title:         "Waffle Desk",
		ActiveSection: "today",
		// Neutral placeholders until shared shell JS hydrates live state.
		// Never ship a false "Connected" before the client confirms health.
		Connection: "Connecting…",
		ModelAlias: "—",
	})
}

func shellHandler(security *Security, view ui.ShellView) http.Handler {
	view = withShellDefaults(view, security.Token())
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/desk/":
			w.Header().Set("Cache-Control", "no-store")
			requestView := view
			requestView.ActiveSection = deskSection(r.URL.Query()["section"])
			if err := ui.Shell(requestView).Render(r.Context(), w); err != nil {
				http.Error(w, "render Waffle Desk shell", http.StatusInternalServerError)
			}
		case strings.HasPrefix(r.URL.Path, "/desk/assets/"):
			if !ui.ServeAsset(w, r, strings.TrimPrefix(r.URL.Path, "/desk/assets/")) {
				http.NotFound(w, r)
			}
		default:
			http.NotFound(w, r)
		}
	})
}

func deskSection(values []string) string {
	if len(values) != 1 {
		return "today"
	}
	switch values[0] {
	case "today", "tasks", "workspaces", "memory", "capabilities":
		return values[0]
	default:
		return "today"
	}
}

func withShellDefaults(view ui.ShellView, requestToken string) ui.ShellView {
	if view.Title == "" {
		view.Title = "Waffle Desk"
	}
	if view.ActiveSection == "" {
		view.ActiveSection = "today"
	}
	if view.Connection == "" {
		view.Connection = "Connecting…"
	}
	if view.ModelAlias == "" {
		view.ModelAlias = "—"
	}
	view.RequestToken = requestToken
	view.AssetVersion = ui.AssetVersion()
	return view
}
