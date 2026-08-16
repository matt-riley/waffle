package dashboard

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/matt-riley/waffle/internal/dashboard/ui"
)

func TestShellRendersApprovedFiveSectionNavigation(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestShellHandler(t, ui.ShellView{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/desk/", nil))

	body := rec.Body.String()
	for section, label := range map[string]string{
		"today":        "Today",
		"tasks":        "Tasks",
		"workspaces":   "Workspaces",
		"memory":       "Memory",
		"capabilities": "Capabilities",
	} {
		link := regexp.MustCompile(`<a href="/desk/\?section=` + section + `"[^>]*>` + label + `</a>`)
		if !link.MatchString(body) {
			t.Errorf("missing navigation destination link for %q", label)
		}
	}
	if strings.Contains(body, "https://") {
		t.Fatal("shell must not load external assets")
	}
}

func TestShellMobileNavigationRemovesRedundantBrandFromTabOrder(t *testing.T) {
	handler := newTestShellHandler(t, ui.ShellView{})
	asset := httptest.NewRecorder()
	handler.ServeHTTP(asset, httptest.NewRequest(http.MethodGet, "/desk/assets/app.css", nil))

	mobileBrandDisplay := regexp.MustCompile(`(?s)@media \(max-width: 768px\) \{.*?\.brand \{[^}]*display:\s*none;`)
	if !mobileBrandDisplay.Match(asset.Body.Bytes()) {
		t.Fatal("mobile navigation must remove the redundant brand link from the tab order")
	}
}

func TestShellMobileNavigationProvidesMinimumTouchTargets(t *testing.T) {
	handler := newTestShellHandler(t, ui.ShellView{})
	asset := httptest.NewRecorder()
	handler.ServeHTTP(asset, httptest.NewRequest(http.MethodGet, "/desk/assets/app.css", nil))

	mobileTargets := regexp.MustCompile(`(?s)@media \(max-width: 768px\) \{.*?\.section-links a \{[^}]*min-height:\s*2\.75rem;[^}]*\}`)
	if !mobileTargets.Match(asset.Body.Bytes()) {
		t.Fatal("mobile navigation links must provide at least 2.75rem touch targets")
	}
}

func TestShellHandlerProvidesDefaultShellView(t *testing.T) {
	security := mustSecurity(t, "127.0.0.1:8422")
	rec := httptest.NewRecorder()
	ShellHandler(security).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/desk/", nil))

	body := rec.Body.String()
	// Server-rendered rail must use neutral placeholders — never a false "Connected".
	for _, required := range []string{
		"<title>Waffle Desk</title>",
		`id="rail-status"`,
		`id="rail-connection"`,
		`id="rail-model"`,
		`id="rail-status-dot"`,
		">Connecting…<",
		">—<",
	} {
		if !strings.Contains(body, required) {
			t.Errorf("default shell missing %q", required)
		}
	}
	if strings.Contains(body, ">Connected<") {
		t.Fatal("default shell must not claim Connected before client hydration")
	}
	if strings.Contains(body, `>default<`) {
		t.Fatal("default shell must not hardcode model alias default")
	}
}

func TestShellRendersOnlyTheRequestedSection(t *testing.T) {
	handler := newTestShellHandler(t, ui.ShellView{})
	for section, marker := range map[string]string{
		"today":        `id="desk-session-title"`,
		"tasks":        `id="tasks-title"`,
		"workspaces":   `id="workspaces-title"`,
		"memory":       `id="memory-title"`,
		"capabilities": `id="capabilities-title"`,
	} {
		t.Run(section, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/desk/?section="+section, nil))
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), marker) {
				t.Fatalf("section %q = %d %q", section, rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), `data-active-section="`+section+`"`) {
				t.Fatalf("section %q does not expose its active state", section)
			}
		})
	}
}

func TestShellEscapesDynamicViewStrings(t *testing.T) {
	view := ui.ShellView{
		Title:         `Today <script>alert("title")</script>`,
		Connection:    `<img src=x onerror=alert("connection")>`,
		ModelAlias:    `<b>model</b>`,
		ActiveSection: "today",
	}
	rec := httptest.NewRecorder()
	newTestShellHandler(t, view).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/desk/", nil))

	body := rec.Body.String()
	for _, unsafe := range []string{"<script>", "<img src=x", "<b>model</b>"} {
		if strings.Contains(body, unsafe) {
			t.Errorf("shell rendered unsafe content %q", unsafe)
		}
	}
	for _, escaped := range []string{"&lt;script&gt;", "&lt;img", "&lt;b&gt;model&lt;/b&gt;"} {
		if !strings.Contains(body, escaped) {
			t.Errorf("shell did not escape %q", escaped)
		}
	}
}

func TestShellProvidesAccessibleDocumentAndRequestToken(t *testing.T) {
	security := mustSecurity(t, "127.0.0.1:8422")
	rec := httptest.NewRecorder()
	shellHandler(security, ui.ShellView{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/desk/", nil))
	body := rec.Body.String()

	for _, required := range []string{
		`<html lang="en">`,
		`href="#main-content"`,
		`<main id="main-content"`,
		`data-request-token="` + security.Token() + `"`,
		`<button type="button"`,
		`<textarea`,
	} {
		if !strings.Contains(body, required) {
			t.Errorf("shell missing %q", required)
		}
	}
	if strings.Contains(body, `<div role="button"`) {
		t.Fatal("shell must use native focusable controls")
	}
}

func TestTodayRendersConversationControlsAndContext(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestShellHandler(t, ui.ShellView{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/desk/", nil))
	body := rec.Body.String()

	for _, required := range []string{
		`id="desk-session-title"`,
		`id="desk-stale-status"`,
		`id="desk-transcript"`,
		`aria-label="Conversation transcript"`,
		`id="desk-slash-menu"`,
		`role="listbox"`,
		`<label for="desk-message">`,
		`<textarea id="desk-message"`,
		`rows="3"`,
		`<button id="desk-send" type="submit"`,
		`<button id="desk-cancel" type="button"`,
		`<label for="desk-model">`,
		`<select id="desk-model"`,
		`id="desk-skill"`,
		`id="desk-connection"`,
		`id="desk-profile"`,
		`id="desk-workspace"`,
		`class="composer-session"`,
	} {
		if !strings.Contains(body, required) {
			t.Errorf("Today view missing %q", required)
		}
	}
}

func TestShellServesVersionedEmbeddedAssets(t *testing.T) {
	handler := newTestShellHandler(t, ui.ShellView{})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/desk/", nil))
	body := rec.Body.String()

	assetURL := regexp.MustCompile(`/desk/assets/app\.css\?v=([a-f0-9]{64})`).FindStringSubmatch(body)
	if len(assetURL) != 2 {
		t.Fatalf("shell CSS URL = %q, want hashed local URL", body)
	}
	if !strings.Contains(body, `/desk/assets/app.js?v=`+assetURL[1]) {
		t.Fatal("shell JS URL must use the embedded asset version")
	}

	appModule := httptest.NewRecorder()
	handler.ServeHTTP(appModule, httptest.NewRequest(http.MethodGet, "/desk/assets/app.js?v="+assetURL[1], nil))
	if appModule.Code != http.StatusOK {
		t.Fatalf("app module status = %d, want %d", appModule.Code, http.StatusOK)
	}
	for _, required := range []string{
		`today: "today.js"`,
		`moduleURL.searchParams.set("v", version)`,
	} {
		if !strings.Contains(appModule.Body.String(), required) {
			t.Errorf("app module missing versioned Today loader %q", required)
		}
	}
	if strings.Contains(appModule.Body.String(), `tasks: "tasks.js"`) {
		t.Fatal("migrated Tasks section must not be bootstrapped by the bespoke client")
	}

	asset := httptest.NewRecorder()
	handler.ServeHTTP(asset, httptest.NewRequest(http.MethodGet, assetURL[0], nil))
	if asset.Code != http.StatusOK {
		t.Fatalf("asset status = %d, want %d", asset.Code, http.StatusOK)
	}
	if cacheControl := asset.Header().Get("Cache-Control"); cacheControl != "public, max-age=31536000, immutable" {
		t.Errorf("asset Cache-Control = %q", cacheControl)
	}
	if contentType := asset.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/css") {
		t.Errorf("asset Content-Type = %q", contentType)
	}
	if bytes.Contains(asset.Body.Bytes(), []byte("https://")) {
		t.Fatal("embedded asset must not load external resources")
	}

	todayAsset := httptest.NewRecorder()
	handler.ServeHTTP(todayAsset, httptest.NewRequest(http.MethodGet, "/desk/assets/today.js?v="+assetURL[1], nil))
	if todayAsset.Code != http.StatusOK {
		t.Fatalf("Today asset status = %d, want %d", todayAsset.Code, http.StatusOK)
	}
	if contentType := todayAsset.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/javascript") {
		t.Errorf("Today asset Content-Type = %q", contentType)
	}
	if cacheControl := todayAsset.Header().Get("Cache-Control"); cacheControl != "public, max-age=31536000, immutable" {
		t.Errorf("Today asset Cache-Control = %q", cacheControl)
	}
}

func TestShellDocumentIsNotCached(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestShellHandler(t, ui.ShellView{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/desk/", nil))
	if cacheControl := rec.Header().Get("Cache-Control"); cacheControl != "no-store" {
		t.Errorf("document Cache-Control = %q, want no-store", cacheControl)
	}
}

func newTestShellHandler(t *testing.T, view ui.ShellView) http.Handler {
	t.Helper()
	return shellHandler(mustSecurity(t, "127.0.0.1:8422"), view)
}
