package ui

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"sort"
)

//go:embed assets/*
var assetFiles embed.FS

var assetVersion = hashAssets()

// AssetVersion identifies the complete embedded asset set for cache busting.
func AssetVersion() string {
	return assetVersion
}

// AssetURL returns the stable local URL for a versioned embedded asset.
func AssetURL(name, version string) string {
	return "/desk/assets/" + name + "?v=" + version
}

// ServeAsset writes one known embedded asset and reports whether it existed.
func ServeAsset(w http.ResponseWriter, r *http.Request, name string) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return true
	}
	if name != "app.css" && name != "app.js" && name != "today.js" {
		for _, serve := range []func(http.ResponseWriter, *http.Request, string) bool{
			ServeTaskAsset,
			ServeWorkspaceAsset,
			ServeMemoryAsset,
			ServeCapabilitiesAsset,
		} {
			if serve(w, r, name) {
				return true
			}
		}
		return false
	}
	contents, err := assetFiles.ReadFile(path.Join("assets", name))
	if err != nil {
		return false
	}
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	if name == "app.css" {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	}
	if r.Method != http.MethodHead {
		_, _ = w.Write(contents)
	}
	return true
}

func hashAssets() string {
	entries, err := fs.ReadDir(assetFiles, "assets")
	if err != nil {
		panic(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	hash := sha256.New()
	for _, name := range names {
		contents, err := assetFiles.ReadFile(path.Join("assets", name))
		if err != nil {
			panic(err)
		}
		_, _ = hash.Write([]byte(name))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write(contents)
	}
	return hex.EncodeToString(hash.Sum(nil))
}
