package ui

import (
	"net/http"
)

// ServeTaskAsset is the additive asset seam used by the shared shell asset
// registry when the Tasks section is composed into Desk.
func ServeTaskAsset(w http.ResponseWriter, r *http.Request, name string) bool {
	if name != "tasks.js" {
		return false
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return true
	}
	contents, err := assetFiles.ReadFile("assets/tasks.js")
	if err != nil {
		return false
	}
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	if r.Method != http.MethodHead {
		_, _ = w.Write(contents)
	}
	return true
}
