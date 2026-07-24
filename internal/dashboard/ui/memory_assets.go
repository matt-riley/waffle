package ui

import "net/http"

// ServeMemoryAsset is the additive asset seam used by the shared shell asset
// registry when the Memory section is composed into Desk.
func ServeMemoryAsset(w http.ResponseWriter, r *http.Request, name string) bool {
	if name != "memory.js" && name != "memory.css" {
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
	if name == "memory.css" {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	}
	if r.Method != http.MethodHead {
		_, _ = w.Write(contents)
	}
	return true
}
