package ui

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"sort"
	"strings"
)

//go:embed assets/*
var assetFiles embed.FS

// staticAsset is one immutable embedded Desk asset loaded at process start.
type staticAsset struct {
	body        []byte
	contentType string
}

// staticAssets caches every file under assets/ so request handlers never re-read embed.FS.
var staticAssets = loadStaticAssets()

var assetVersion = hashCachedAssets(staticAssets)

// AssetVersion identifies the complete embedded asset set for cache busting.
func AssetVersion() string {
	return assetVersion
}

// AssetURL returns the stable local URL for a versioned embedded asset.
func AssetURL(name, version string) string {
	return "/desk/assets/" + name + "?v=" + version
}

// ServeAsset writes one known embedded asset and reports whether it existed.
// Contents are served from the process-start cache, not re-read from embed.FS.
func ServeAsset(w http.ResponseWriter, r *http.Request, name string) bool {
	asset, ok := staticAssets[name]
	if !ok {
		return false
	}
	return writeStaticAsset(w, r, asset)
}

// ServeTaskAsset is the additive asset seam used by the shared shell asset
// registry when the Tasks section is composed into Desk.
func ServeTaskAsset(w http.ResponseWriter, r *http.Request, name string) bool {
	if name != "tasks.js" {
		return false
	}
	return ServeAsset(w, r, name)
}

// ServeWorkspaceAsset is the additive asset seam used when the Workspaces
// section is composed into the shared Desk shell.
func ServeWorkspaceAsset(w http.ResponseWriter, r *http.Request, name string) bool {
	if name != "workspaces.js" && name != "workspaces.css" {
		return false
	}
	return ServeAsset(w, r, name)
}

// ServeMemoryAsset is the additive asset seam used by the shared shell asset
// registry when the Memory section is composed into Desk.
func ServeMemoryAsset(w http.ResponseWriter, r *http.Request, name string) bool {
	if name != "memory.js" && name != "memory.css" {
		return false
	}
	return ServeAsset(w, r, name)
}

func writeStaticAsset(w http.ResponseWriter, r *http.Request, asset staticAsset) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return true
	}
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("Content-Type", asset.contentType)
	if r.Method != http.MethodHead {
		_, _ = w.Write(asset.body)
	}
	return true
}

func loadStaticAssets() map[string]staticAsset {
	entries, err := fs.ReadDir(assetFiles, "assets")
	if err != nil {
		panic(err)
	}
	assets := make(map[string]staticAsset, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		contents, err := assetFiles.ReadFile(path.Join("assets", name))
		if err != nil {
			panic(err)
		}
		// Defensive copy so callers cannot mutate the package cache through the map value.
		body := make([]byte, len(contents))
		copy(body, contents)
		assets[name] = staticAsset{
			body:        body,
			contentType: assetContentType(name),
		}
	}
	if len(assets) == 0 {
		panic("dashboard ui: no embedded static assets")
	}
	return assets
}

func assetContentType(name string) string {
	switch {
	case strings.HasSuffix(name, ".css"):
		return "text/css; charset=utf-8"
	case strings.HasSuffix(name, ".js"):
		return "text/javascript; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}

func hashCachedAssets(assets map[string]staticAsset) string {
	names := make([]string, 0, len(assets))
	for name := range assets {
		names = append(names, name)
	}
	sort.Strings(names)
	hash := sha256.New()
	for _, name := range names {
		asset := assets[name]
		_, _ = hash.Write([]byte(name))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write(asset.body)
	}
	return hex.EncodeToString(hash.Sum(nil))
}
