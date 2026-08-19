package ui

import (
	"bytes"
	"image/png"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"strings"
	"sync"
	"testing"
)

func TestStaticAssetsCachedAtInit(t *testing.T) {
	required := []string{
		"app.css",
		"app.js",
		"theme-boot.js",
		"today.js",
		"session-presentation.mjs",
		"dictate.js",
		"workspaces.css",
		"memory.css",
		"memory.js",
		"capabilities.css",
		"posture.css",
		"posture.js",
		"profiles.css",
		"profiles.js",
		"setup.css",
		"setup.js",
		"htmx.min.js",
		"waffle-htmx.js",
	}
	for _, name := range required {
		asset, ok := staticAssets[name]
		if !ok {
			t.Fatalf("staticAssets missing %q after init", name)
		}
		if len(asset.body) == 0 {
			t.Fatalf("staticAssets[%q] body is empty", name)
		}
		if asset.contentType == "" {
			t.Fatalf("staticAssets[%q] content type is empty", name)
		}
		// Bytes must match the embedded FS exactly (cache is a faithful load-once snapshot).
		want, err := assetFiles.ReadFile(path.Join("assets", name))
		if err != nil {
			t.Fatalf("embed.FS ReadFile(%q): %v", name, err)
		}
		if !bytes.Equal(asset.body, want) {
			t.Fatalf("cached body for %q does not match embed.FS", name)
		}
	}
}

func TestServeAssetUsesProcessStartCache(t *testing.T) {
	// Prove the serve helper writes whatever asset it is given (local map/value,
	// no mutation of the package-level staticAssets cache — review #165).
	marker := staticAsset{
		body:        []byte("/* cache-hit-marker */"),
		contentType: "text/css; charset=utf-8",
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/desk/assets/app.css", nil)
	if !writeStaticAsset(rec, req, marker) {
		t.Fatal("writeStaticAsset returned false")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !bytes.Equal(rec.Body.Bytes(), marker.body) {
		t.Fatalf("served body = %q, want local cache marker", rec.Body.Bytes())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/css; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Errorf("Cache-Control = %q", cc)
	}

	// ServeAsset must serve the process-start cache entry, not re-read embed.FS.
	// Comparing response bytes to staticAssets proves the shipped serve path.
	const name = "app.css"
	cached, ok := staticAssets[name]
	if !ok {
		t.Fatal("app.css missing from process-start cache")
	}
	served := httptest.NewRecorder()
	if !ServeAsset(served, httptest.NewRequest(http.MethodGet, "/desk/assets/"+name, nil), name) {
		t.Fatal("ServeAsset returned false for known asset")
	}
	if !bytes.Equal(served.Body.Bytes(), cached.body) {
		t.Fatal("ServeAsset body does not match process-start cache")
	}
}

func TestServeAssetCoversAllEmbeddedFiles(t *testing.T) {
	entries, err := fs.ReadDir(assetFiles, "assets")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/desk/assets/"+name, nil)
			if !ServeAsset(rec, req, name) {
				t.Fatalf("ServeAsset(%q) returned false", name)
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}
			want := staticAssets[name].body
			if !bytes.Equal(rec.Body.Bytes(), want) {
				t.Fatalf("body length %d != cache length %d", rec.Body.Len(), len(want))
			}
		})
	}
}

func TestServeAssetUnknownAndMethod(t *testing.T) {
	rec := httptest.NewRecorder()
	if ServeAsset(rec, httptest.NewRequest(http.MethodGet, "/desk/assets/nope.js", nil), "nope.js") {
		t.Fatal("unknown asset should return false")
	}

	rec = httptest.NewRecorder()
	if !ServeAsset(rec, httptest.NewRequest(http.MethodPost, "/desk/assets/app.js", nil), "app.js") {
		t.Fatal("known asset with bad method should still be handled")
	}
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

func TestServeAssetConcurrentReads(t *testing.T) {
	const goroutines = 32
	var wg sync.WaitGroup
	errCh := make(chan error, goroutines)
	want := staticAssets["app.js"].body
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			if !ServeAsset(rec, httptest.NewRequest(http.MethodGet, "/desk/assets/app.js", nil), "app.js") {
				errCh <- errString("ServeAsset false")
				return
			}
			if !bytes.Equal(rec.Body.Bytes(), want) {
				errCh <- errString("body mismatch under concurrency")
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}

func TestSectionAssetSeamsUseSharedCache(t *testing.T) {
	cases := []struct {
		name  string
		serve func(http.ResponseWriter, *http.Request, string) bool
		file  string
	}{
		{"workspaces", ServeWorkspaceAsset, "workspaces.css"},
		{"memory", ServeMemoryAsset, "memory.css"},
		{"capabilities", ServeCapabilitiesAsset, "capabilities.css"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			if !tc.serve(rec, httptest.NewRequest(http.MethodGet, "/desk/assets/"+tc.file, nil), tc.file) {
				t.Fatal("seam returned false")
			}
			if !bytes.Equal(rec.Body.Bytes(), staticAssets[tc.file].body) {
				t.Fatal("seam did not serve cached body")
			}
		})
	}
}

func TestAssetVersionStableAndNonEmpty(t *testing.T) {
	v := AssetVersion()
	if len(v) != 64 {
		t.Fatalf("AssetVersion length = %d, want 64 hex chars", len(v))
	}
	if AssetVersion() != v {
		t.Fatal("AssetVersion must be stable")
	}
	if CapabilitiesAssetVersion() == "" {
		t.Fatal("CapabilitiesAssetVersion empty")
	}
}

func TestWaffleDeskDerivativesAreEmbeddedServedAndSanitized(t *testing.T) {
	want := map[string][2]int{
		"waffle-mark-sitting.png":  {128, 128},
		"waffle-empty-curled.png":  {480, 320},
		"waffle-empty-sitting.png": {320, 320},
		"waffle-empty-curious.png": {256, 256},
	}
	approvedSources := map[string]string{
		"waffle-mark-sitting.png":  "assets/brand/waffle/poses/sitting.png",
		"waffle-empty-curled.png":  "assets/brand/waffle/poses/curled.png",
		"waffle-empty-sitting.png": "assets/brand/waffle/poses/sitting.png",
		"waffle-empty-curious.png": "assets/brand/waffle/canon/expressions/curious.png",
	}
	var total int
	for name, dimensions := range want {
		source := path.Join("..", "..", "..", approvedSources[name])
		if _, err := os.Stat(source); err != nil {
			t.Fatalf("approved source master for %s missing at %s: %v", name, source, err)
		}
		asset, ok := staticAssets[name]
		if !ok {
			t.Fatalf("Desk derivative %q is not embedded", name)
		}
		if asset.contentType != "image/png" {
			t.Errorf("%s content type = %q, want image/png", name, asset.contentType)
		}
		response := httptest.NewRecorder()
		if !ServeAsset(response, httptest.NewRequest(http.MethodGet, AssetURL(name, AssetVersion()), nil), name) {
			t.Fatalf("ServeAsset(%q) returned false", name)
		}
		if response.Header().Get("Content-Type") != "image/png" {
			t.Errorf("served %s Content-Type = %q", name, response.Header().Get("Content-Type"))
		}
		if response.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
			t.Errorf("served %s Cache-Control = %q", name, response.Header().Get("Cache-Control"))
		}
		config, err := png.DecodeConfig(bytes.NewReader(response.Body.Bytes()))
		if err != nil {
			t.Fatalf("decode %s: %v", name, err)
		}
		if config.Width != dimensions[0] || config.Height != dimensions[1] {
			t.Errorf("%s dimensions = %dx%d, want %dx%d", name, config.Width, config.Height, dimensions[0], dimensions[1])
		}
		decoded, err := png.Decode(bytes.NewReader(response.Body.Bytes()))
		if err != nil {
			t.Fatalf("decode pixels %s: %v", name, err)
		}
		corners := [][2]int{{0, 0}, {config.Width - 1, 0}, {0, config.Height - 1}, {config.Width - 1, config.Height - 1}}
		for _, corner := range corners {
			_, _, _, alpha := decoded.At(corner[0], corner[1]).RGBA()
			if alpha != 0 {
				t.Errorf("%s lost transparent corner alpha at %v", name, corner)
			}
		}
		chunks, err := pngChunkTypes(response.Body.Bytes())
		if err != nil {
			t.Fatalf("parse PNG chunks %s: %v", name, err)
		}
		for _, chunk := range chunks {
			switch chunk {
			case "tEXt", "zTXt", "iTXt", "eXIf", "iCCP":
				t.Errorf("%s contains private PNG metadata chunk %s", name, chunk)
			}
		}
		mutated := make(map[string]staticAsset, len(staticAssets))
		for assetName, cached := range staticAssets {
			body := append([]byte(nil), cached.body...)
			mutated[assetName] = staticAsset{body: body, contentType: cached.contentType}
		}
		mutatedBody := append([]byte(nil), asset.body...)
		mutatedBody[len(mutatedBody)-1] ^= 1
		mutated[name] = staticAsset{body: mutatedBody, contentType: asset.contentType}
		if hashCachedAssets(mutated) == AssetVersion() {
			t.Errorf("AssetVersion did not change when derivative %s changed", name)
		}
		total += len(asset.body)
	}
	if total > 350*1024 {
		t.Fatalf("Desk derivatives total %d bytes, want <= 358400", total)
	}
}

func pngChunkTypes(data []byte) ([]string, error) {
	const signatureLength = 8
	if len(data) < signatureLength || !bytes.Equal(data[:signatureLength], []byte{137, 80, 78, 71, 13, 10, 26, 10}) {
		return nil, errString("invalid PNG signature")
	}
	chunks := make([]string, 0, 4)
	for offset := signatureLength; offset < len(data); {
		if len(data)-offset < 12 {
			return nil, errString("truncated PNG chunk")
		}
		length := int(data[offset])<<24 | int(data[offset+1])<<16 | int(data[offset+2])<<8 | int(data[offset+3])
		end := offset + 12 + length
		if length < 0 || end > len(data) {
			return nil, errString("PNG chunk exceeds file")
		}
		chunks = append(chunks, string(data[offset+4:offset+8]))
		offset = end
	}
	return chunks, nil
}

func TestHtmxRuntimeIsPinnedAndEmbedded(t *testing.T) {
	if got := HtmxAssetDigest(); got != HtmxSHA256 {
		t.Fatalf("htmx digest = %q, want pinned %q", got, HtmxSHA256)
	}
	version, source, digest := HtmxAssetProvenance()
	if version != HtmxVersion || digest != HtmxSHA256 || source == "" {
		t.Fatalf("unexpected htmx provenance: version=%q source=%q digest=%q", version, source, digest)
	}
	body := string(staticAssets["htmx.min.js"].body)
	for _, forbidden := range []string{"cdn.jsdelivr.net", "unpkg.com", "ajax.googleapis.com"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("vendored htmx runtime references external host %q", forbidden)
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }
