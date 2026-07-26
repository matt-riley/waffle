package ui

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
)

var capabilitiesAssetVersion = hashCapabilityAssets()

func CapabilitiesAssetVersion() string {
	return capabilitiesAssetVersion
}

func CapabilitiesAssetURL(name, version string) string {
	return "/desk/assets/" + name + "?v=" + version
}

// ServeCapabilitiesAsset is an additive asset seam for the shared shell
// handler. Contents come from the process-start static asset cache.
func ServeCapabilitiesAsset(w http.ResponseWriter, r *http.Request, name string) bool {
	if name != "capabilities.css" {
		return false
	}
	return ServeAsset(w, r, name)
}

func hashCapabilityAssets() string {
	hash := sha256.New()
	for _, name := range []string{"capabilities.css"} {
		asset, ok := staticAssets[name]
		if !ok {
			panic("dashboard ui: missing capabilities asset " + name)
		}
		_, _ = hash.Write([]byte(name))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write(asset.body)
	}
	return hex.EncodeToString(hash.Sum(nil))
}
