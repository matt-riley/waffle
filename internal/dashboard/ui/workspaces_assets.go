package ui

import "net/http"

// ServeWorkspaceAsset is the additive asset seam used when the Workspaces
// section is composed into the shared Desk shell.
func ServeWorkspaceAsset(w http.ResponseWriter, r *http.Request, name string) bool {
	var contentType string
	switch name {
	case "workspaces.js":
		contentType = "text/javascript; charset=utf-8"
	case "workspaces.css":
		contentType = "text/css; charset=utf-8"
	default:
		return false
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return true
	}
	contents, err := assetFiles.ReadFile("assets/" + name)
	if err != nil {
		return false
	}
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("Content-Type", contentType)
	if r.Method != http.MethodHead {
		_, _ = w.Write(contents)
	}
	return true
}
