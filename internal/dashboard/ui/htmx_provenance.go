package ui

import (
	"crypto/sha256"
	"encoding/hex"
)

const (
	// HtmxVersion and HtmxSHA256 describe the exact offline runtime shipped in
	// assets/htmx.min.js. The URL is documentation-only; serving never reaches
	// the network.
	HtmxVersion = "2.0.7"
	HtmxSHA256  = "6cf37d968150607c38666e3b73d66bd3522ef44b247cd15f17b7539cf8b032ab"
	HtmxSource  = "https://cdn.jsdelivr.net/npm/htmx.org@2.0.7/dist/htmx.min.js"
)

func HtmxAssetDigest() string {
	asset, ok := staticAssets["htmx.min.js"]
	if !ok {
		return ""
	}
	digest := sha256.Sum256(asset.body)
	return hex.EncodeToString(digest[:])
}

func HtmxAssetProvenance() (version, source, digest string) {
	return HtmxVersion, HtmxSource, HtmxSHA256
}
