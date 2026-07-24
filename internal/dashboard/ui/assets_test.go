package ui

import (
	"bytes"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path"
	"sync"
	"testing"
)

func TestStaticAssetsCachedAtInit(t *testing.T) {
	required := []string{
		"app.css",
		"app.js",
		"today.js",
		"tasks.js",
		"workspaces.css",
		"workspaces.js",
		"memory.css",
		"memory.js",
		"capabilities.css",
		"capabilities.js",
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
	// Mutating the embed FS is impossible; prove the serve path reads staticAssets,
	// not embed.FS, by swapping the cache entry and serving it.
	const name = "app.css"
	original, ok := staticAssets[name]
	if !ok {
		t.Fatal("app.css missing from cache")
	}
	t.Cleanup(func() { staticAssets[name] = original })

	marker := []byte("/* cache-hit-marker */")
	staticAssets[name] = staticAsset{
		body:        marker,
		contentType: "text/css; charset=utf-8",
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/desk/assets/"+name, nil)
	if !ServeAsset(rec, req, name) {
		t.Fatal("ServeAsset returned false for known asset")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !bytes.Equal(rec.Body.Bytes(), marker) {
		t.Fatalf("served body = %q, want cache marker (prove no embed.FS re-read)", rec.Body.Bytes())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/css; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Errorf("Cache-Control = %q", cc)
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
		{"tasks", ServeTaskAsset, "tasks.js"},
		{"workspaces", ServeWorkspaceAsset, "workspaces.css"},
		{"memory", ServeMemoryAsset, "memory.js"},
		{"capabilities", ServeCapabilitiesAsset, "capabilities.js"},
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

type errString string

func (e errString) Error() string { return string(e) }
