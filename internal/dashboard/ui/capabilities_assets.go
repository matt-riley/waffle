package ui

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"net/http"
	"path"
)

//go:embed assets/capabilities.js assets/capabilities.css
var capabilitiesAssetFiles embed.FS

var capabilitiesAssetVersion = hashCapabilityAssets()

func CapabilitiesAssetVersion() string {
	return capabilitiesAssetVersion
}

func CapabilitiesAssetURL(name, version string) string {
	return "/desk/assets/" + name + "?v=" + version
}

// ServeCapabilitiesAsset is an additive asset seam for the shared shell
// handler. The coordinator may call it before the existing ServeAsset.
func ServeCapabilitiesAsset(w http.ResponseWriter, r *http.Request, name string) bool {
	if name != "capabilities.js" && name != "capabilities.css" {
		return false
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return true
	}
	contents, err := capabilitiesAssetFiles.ReadFile(path.Join("assets", name))
	if err != nil {
		return false
	}
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	if name == "capabilities.css" {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	}
	if r.Method != http.MethodHead {
		_, _ = w.Write(contents)
	}
	return true
}

func hashCapabilityAssets() string {
	hash := sha256.New()
	for _, name := range []string{"capabilities.css", "capabilities.js"} {
		contents, err := capabilitiesAssetFiles.ReadFile(path.Join("assets", name))
		if err != nil {
			panic(err)
		}
		_, _ = hash.Write([]byte(name))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write(contents)
	}
	return hex.EncodeToString(hash.Sum(nil))
}
