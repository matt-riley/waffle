package ui

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWorkspacesRendersLifecycleControlsAndNativeDialogs(t *testing.T) {
	var rendered bytes.Buffer
	if err := Workspaces(ShellView{ActiveSection: "workspaces"}).Render(context.Background(), &rendered); err != nil {
		t.Fatal(err)
	}
	body := rendered.String()
	for _, required := range []string{
		`class="workspaces"`,
		`id="workspaces-title"`,
		`id="workspaces-list"`,
		`id="workspaces-errors"`,
		`id="workspaces-empty"`,
		`id="workspace-open-button"`,
		`<dialog id="workspace-open-dialog"`,
		`id="workspace-open-form"`,
		`for="workspace-repository"`,
		`id="workspace-repository"`,
		`for="workspace-profile"`,
		`id="workspace-profile"`,
		`id="workspace-open-cancel"`,
		`<dialog id="workspace-close-dialog"`,
		`id="workspace-close-dirty"`,
		`id="workspace-close-unpushed"`,
		`id="workspace-close-cancel"`,
		`id="workspace-close-confirm"`,
		`aria-live="polite"`,
	} {
		if !strings.Contains(body, required) {
			t.Errorf("Workspaces view missing %q", required)
		}
	}
	for _, forbidden := range []string{"force close", `name="force"`, `id="workspace-force"`} {
		if strings.Contains(strings.ToLower(body), forbidden) {
			t.Errorf("Workspaces view contains forbidden force control %q", forbidden)
		}
	}
}

func TestWorkspaceAssetsAreAdditiveVersionedAndResponsive(t *testing.T) {
	var rendered bytes.Buffer
	if err := WorkspaceAssets(ShellView{AssetVersion: "asset-version"}).Render(context.Background(), &rendered); err != nil {
		t.Fatal(err)
	}
	body := rendered.String()
	for _, required := range []string{
		`/desk/assets/workspaces.css?v=asset-version`,
		`/desk/assets/workspaces.js?v=asset-version`,
		`type="module"`,
	} {
		if !strings.Contains(body, required) {
			t.Errorf("WorkspaceAssets missing %q", required)
		}
	}

	contents, err := assetFiles.ReadFile("assets/workspaces.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(contents)
	for _, required := range []string{
		".workspaces-grid",
		".workspace-card",
		".workspace-status",
		".workspace-evidence",
		":focus-visible",
		"@media (max-width: 900px)",
		"@media (prefers-reduced-motion: reduce)",
	} {
		if !strings.Contains(css, required) {
			t.Errorf("Workspaces CSS missing %q", required)
		}
	}
}

func TestServeWorkspaceAssetClaimsOnlyWorkspaceAssets(t *testing.T) {
	for _, test := range []struct {
		name        string
		contentType string
	}{
		{name: "workspaces.js", contentType: "text/javascript"},
		{name: "workspaces.css", contentType: "text/css"},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/desk/assets/"+test.name, nil)
		if !ServeWorkspaceAsset(rec, req, test.name) {
			t.Fatalf("%s was not served", test.name)
		}
		if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), test.contentType) {
			t.Fatalf("%s response = %d %q", test.name, rec.Code, rec.Header().Get("Content-Type"))
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/desk/assets/today.js", nil)
	if ServeWorkspaceAsset(httptest.NewRecorder(), req, "today.js") {
		t.Fatal("Workspace asset seam claimed an unrelated asset")
	}
}
