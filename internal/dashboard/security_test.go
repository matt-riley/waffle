package dashboard

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewSecurityCreatesRawURLSafeTokenAndAllowsConfiguredHosts(t *testing.T) {
	random := bytes.NewReader(bytes.Repeat([]byte{0xff}, 32))
	security, err := NewSecurity("127.0.0.1:8422", TailnetOptions{}, random)
	if err != nil {
		t.Fatalf("NewSecurity() error = %v", err)
	}
	if want := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0xff}, 32)); security.Token() != want {
		t.Errorf("Token() = %q, want %q", security.Token(), want)
	}

	for _, host := range []string{"127.0.0.1:8422", "localhost:8422"} {
		t.Run(host, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "http://"+host+"/status", nil)
			req.Host = host
			security.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})).ServeHTTP(rec, req)
			if rec.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
			}
		})
	}
}

func TestSecurityRejectsInvalidRequestMetadata(t *testing.T) {
	security := mustSecurity(t, "127.0.0.1:8422")
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })

	tests := []struct {
		name   string
		host   string
		origin string
		fetch  string
	}{
		{name: "invalid host", host: "127.0.0.1:9999"},
		{name: "foreign origin", host: "127.0.0.1:8422", origin: "https://attacker.example"},
		{name: "cross-site fetch", host: "127.0.0.1:8422", fetch: "cross-site"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8422/status", nil)
			req.Host = tt.host
			req.Header.Set("Origin", tt.origin)
			req.Header.Set("Sec-Fetch-Site", tt.fetch)
			security.Wrap(next).ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
			}
			assertSecurityHeaders(t, rec)
		})
	}
}

func TestSecurityRejectsCrossSiteMutation(t *testing.T) {
	security := mustSecurity(t, "127.0.0.1:8422")
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8422/api/v1/desk/test", nil)
	req.Host = "127.0.0.1:8422"
	req.Header.Set("Origin", "https://attacker.example")
	rec := httptest.NewRecorder()

	security.Wrap(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q, want no CORS header", got)
	}
}

func TestSecurityAllowsSameOriginGETAndAddsSecurityHeaders(t *testing.T) {
	security := mustSecurity(t, "127.0.0.1:8422")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8422/status", nil)
	req.Host = "127.0.0.1:8422"
	req.Header.Set("Origin", "http://127.0.0.1:8422")
	security.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	assertSecurityHeaders(t, rec)
}

func TestRequireMutationRequiresTokenAndIdempotencyKey(t *testing.T) {
	security := mustSecurity(t, "127.0.0.1:8422")
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })

	tests := []struct {
		name     string
		token    string
		key      string
		wantCode int
	}{
		{name: "missing token", key: "key", wantCode: http.StatusForbidden},
		{name: "wrong token", token: "wrong", key: "key", wantCode: http.StatusForbidden},
		{name: "missing key", token: security.Token(), wantCode: http.StatusBadRequest},
		{name: "accepted", token: security.Token(), key: "key", wantCode: http.StatusNoContent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8422/api/v1/desk/test", nil)
			req.Host = "127.0.0.1:8422"
			req.Header.Set("X-Waffle-Desk-Token", tt.token)
			req.Header.Set("Idempotency-Key", tt.key)
			security.RequireMutation(next).ServeHTTP(rec, req)
			if rec.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantCode)
			}
		})
	}
}

// tailnetTestOptions is the serve host and login of the managed deployment this
// boundary was designed against: a MagicDNS name and a non-email SSO login.
func tailnetTestOptions() TailnetOptions {
	return TailnetOptions{
		Enabled:       true,
		ServeHost:     "waffle.tail848095.ts.net",
		AllowedLogins: []string{"matt-riley@github"},
	}
}

// newTailnetRequest builds a request shaped the way `tailscale serve` proxies
// one to the loopback listener.
func newTailnetRequest(method, target string) *http.Request {
	req := httptest.NewRequest(method, "http://127.0.0.1:8422"+target, nil)
	req.Host = "waffle.tail848095.ts.net"
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "waffle.tail848095.ts.net")
	req.Header.Set("Tailscale-User-Login", "matt-riley@github")
	return req
}

func TestSecurityAdmitsProxiedTailnetDeskRequest(t *testing.T) {
	security := mustTailnetSecurity(t, "127.0.0.1:8422", tailnetTestOptions())
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })

	tests := []struct {
		name     string
		target   string
		host     string
		mutate   func(*http.Request)
		wantHSTS bool
	}{
		{name: "shell", target: "/desk/", wantHSTS: true},
		{name: "shell without trailing slash", target: "/desk", wantHSTS: true},
		{name: "asset", target: "/desk/assets/app.js?v=1", wantHSTS: true},
		{name: "api read", target: "/api/v1/desk/bootstrap", wantHSTS: true},
		{name: "event stream", target: "/api/v1/desk/events?after=0", wantHSTS: true},
		{name: "explicit https port in host", target: "/desk/", host: "waffle.tail848095.ts.net:443", wantHSTS: true},
		{name: "host case is normalized", target: "/desk/", host: "Waffle.Tail848095.TS.NET", wantHSTS: true},
		{
			name:   "same origin https",
			target: "/desk/",
			mutate: func(r *http.Request) {
				r.Header.Set("Origin", "https://waffle.tail848095.ts.net")
				r.Header.Set("Sec-Fetch-Site", "same-origin")
			},
			wantHSTS: true,
		},
		{
			name:     "typed navigation reports no fetch site",
			target:   "/desk/",
			mutate:   func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "none") },
			wantHSTS: true,
		},
		{
			name:     "login match ignores case",
			target:   "/desk/",
			mutate:   func(r *http.Request) { r.Header.Set("Tailscale-User-Login", "Matt-Riley@GitHub") },
			wantHSTS: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := newTailnetRequest(http.MethodGet, tt.target)
			if tt.host != "" {
				req.Host = tt.host
			}
			if tt.mutate != nil {
				tt.mutate(req)
			}
			rec := httptest.NewRecorder()
			security.Wrap(next).ServeHTTP(rec, req)
			if rec.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
			}
			assertSecurityHeaders(t, rec)
			if got := rec.Header().Get("Strict-Transport-Security"); tt.wantHSTS && got == "" {
				t.Error("Strict-Transport-Security header is missing on the tailnet origin")
			}
		})
	}
}

func TestSecurityRejectsProxiedTailnetRequestMetadata(t *testing.T) {
	security := mustTailnetSecurity(t, "127.0.0.1:8422", tailnetTestOptions())
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })

	tests := []struct {
		name   string
		target string
		mutate func(*http.Request)
	}{
		{name: "status is not a desk path", target: "/status"},
		{name: "healthz is not a desk path", target: "/healthz"},
		{name: "root is not a desk path", target: "/"},
		{name: "connections api is not a desk path", target: "/api/v1/desk-connections"},
		{name: "traversal out of desk", target: "/api/v1/desk/../../status"},
		{name: "traversal out of shell", target: "/desk/../status"},
		{
			name:   "funnel request",
			target: "/desk/",
			mutate: func(r *http.Request) { r.Header.Set("Tailscale-Funnel-Request", "true") },
		},
		{
			name:   "missing forwarded proto",
			target: "/desk/",
			mutate: func(r *http.Request) { r.Header.Del("X-Forwarded-Proto") },
		},
		{
			name:   "plaintext forwarded proto",
			target: "/desk/",
			mutate: func(r *http.Request) { r.Header.Set("X-Forwarded-Proto", "http") },
		},
		{
			name:   "tagged device sends no login",
			target: "/desk/",
			mutate: func(r *http.Request) { r.Header.Del("Tailscale-User-Login") },
		},
		{
			name:   "login outside the allowlist",
			target: "/desk/",
			mutate: func(r *http.Request) { r.Header.Set("Tailscale-User-Login", "someone-else@github") },
		},
		{
			name:   "cross site fetch",
			target: "/desk/",
			mutate: func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") },
		},
		{
			name:   "sibling magicdns name is same-site but not same-origin",
			target: "/desk/",
			mutate: func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-site") },
		},
		{
			name:   "foreign https origin",
			target: "/desk/",
			mutate: func(r *http.Request) { r.Header.Set("Origin", "https://attacker.example") },
		},
		{
			name:   "plaintext origin on the tailnet host",
			target: "/desk/",
			mutate: func(r *http.Request) { r.Header.Set("Origin", "http://waffle.tail848095.ts.net") },
		},
		{
			name:   "origin carrying a path",
			target: "/desk/",
			mutate: func(r *http.Request) { r.Header.Set("Origin", "https://waffle.tail848095.ts.net/evil") },
		},
		{
			name:   "unserved magicdns host",
			target: "/desk/",
			mutate: func(r *http.Request) { r.Host = "drop-2.tail848095.ts.net" },
		},
		{
			name:   "unexpected port on the served host",
			target: "/desk/",
			mutate: func(r *http.Request) { r.Host = "waffle.tail848095.ts.net:8443" },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := newTailnetRequest(http.MethodGet, tt.target)
			if tt.mutate != nil {
				tt.mutate(req)
			}
			rec := httptest.NewRecorder()
			security.Wrap(next).ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
			}
			assertSecurityHeaders(t, rec)
		})
	}
}

func TestSecurityRejectsTailnetHostWhenOptInDisabled(t *testing.T) {
	security := mustSecurity(t, "127.0.0.1:8422")
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	rec := httptest.NewRecorder()

	security.Wrap(next).ServeHTTP(rec, newTailnetRequest(http.MethodGet, "/desk/"))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if got := rec.Header().Get("Strict-Transport-Security"); got != "" {
		t.Errorf("Strict-Transport-Security = %q, want no header when the tailnet path is disabled", got)
	}
}

func TestSecurityKeepsLoopbackBoundaryWhenTailnetEnabled(t *testing.T) {
	security := mustTailnetSecurity(t, "127.0.0.1:8422", tailnetTestOptions())
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })

	// /status stays reachable over loopback for `waffle status` and health checks.
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8422/status", nil)
	req.Host = "127.0.0.1:8422"
	req.Header.Set("Origin", "http://127.0.0.1:8422")
	rec := httptest.NewRecorder()
	security.Wrap(next).ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("loopback /status status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	if got := rec.Header().Get("Strict-Transport-Security"); got != "" {
		t.Errorf("Strict-Transport-Security = %q, want no header on the loopback origin", got)
	}

	// Loopback callers are not required to present a Tailscale identity.
	plain := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8422/desk/", nil)
	plain.Host = "localhost:8422"
	plainRec := httptest.NewRecorder()
	security.Wrap(next).ServeHTTP(plainRec, plain)
	if plainRec.Code != http.StatusNoContent {
		t.Fatalf("loopback /desk/ status = %d, want %d", plainRec.Code, http.StatusNoContent)
	}
}

func TestSecurityStillRequiresTokenForTailnetMutation(t *testing.T) {
	security := mustTailnetSecurity(t, "127.0.0.1:8422", tailnetTestOptions())
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })

	tests := []struct {
		name     string
		token    string
		key      string
		wantCode int
	}{
		{name: "authenticated identity is not a mutation credential", key: "key", wantCode: http.StatusForbidden},
		{name: "wrong token", token: "wrong", key: "key", wantCode: http.StatusForbidden},
		{name: "missing idempotency key", token: security.Token(), wantCode: http.StatusBadRequest},
		{name: "accepted", token: security.Token(), key: "key", wantCode: http.StatusNoContent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := newTailnetRequest(http.MethodPost, "/api/v1/desk/chat/turn")
			req.Header.Set("X-Waffle-Desk-Token", tt.token)
			req.Header.Set("Idempotency-Key", tt.key)
			rec := httptest.NewRecorder()
			security.Wrap(security.RequireMutation(next)).ServeHTTP(rec, req)
			if rec.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantCode)
			}
		})
	}
}

func TestSecurityReportsRejectedTailnetLoginOnce(t *testing.T) {
	security := mustTailnetSecurity(t, "127.0.0.1:8422", tailnetTestOptions())
	var observed []string
	security.SetLoginRejectionObserver(func(login string) { observed = append(observed, login) })
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })

	req := newTailnetRequest(http.MethodGet, "/desk/")
	req.Header.Set("Tailscale-User-Login", "intruder@github\nforged=line")
	rec := httptest.NewRecorder()
	security.Wrap(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if len(observed) != 1 {
		t.Fatalf("observed logins = %v, want exactly one", observed)
	}
	if strings.ContainsAny(observed[0], "\n\r") {
		t.Errorf("observed login %q retained control characters", observed[0])
	}
	if want := "intruder@githubforged=line"; observed[0] != want {
		t.Errorf("observed login = %q, want %q", observed[0], want)
	}
}

func TestNewSecurityRejectsIncompleteTailnetOptIn(t *testing.T) {
	tests := []struct {
		name    string
		tailnet TailnetOptions
	}{
		{name: "missing serve host", tailnet: TailnetOptions{Enabled: true, AllowedLogins: []string{"matt-riley@github"}}},
		{
			name:    "serve host is an ip",
			tailnet: TailnetOptions{Enabled: true, ServeHost: "100.64.0.1", AllowedLogins: []string{"matt-riley@github"}},
		},
		{
			name:    "serve host carries a port",
			tailnet: TailnetOptions{Enabled: true, ServeHost: "waffle.tail848095.ts.net:443", AllowedLogins: []string{"matt-riley@github"}},
		},
		{name: "no allowed logins", tailnet: TailnetOptions{Enabled: true, ServeHost: "waffle.tail848095.ts.net"}},
		{
			name:    "empty allowed login",
			tailnet: TailnetOptions{Enabled: true, ServeHost: "waffle.tail848095.ts.net", AllowedLogins: []string{""}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewSecurity("127.0.0.1:8422", tt.tailnet, bytes.NewReader(bytes.Repeat([]byte{1}, 32))); err == nil {
				t.Fatal("NewSecurity() error = nil, want a failed-closed error")
			}
		})
	}
}

func mustSecurity(t *testing.T, listen string) *Security {
	t.Helper()
	return mustTailnetSecurity(t, listen, TailnetOptions{})
}

func mustTailnetSecurity(t *testing.T, listen string, tailnet TailnetOptions) *Security {
	t.Helper()
	security, err := NewSecurity(listen, tailnet, bytes.NewReader(bytes.Repeat([]byte{1}, 32)))
	if err != nil {
		t.Fatalf("NewSecurity() error = %v", err)
	}
	return security
}

func assertSecurityHeaders(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if got := rec.Header().Get("Content-Security-Policy"); got == "" {
		t.Error("Content-Security-Policy header is missing")
	}
	for header, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "no-referrer",
		"X-Frame-Options":        "DENY",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
}
