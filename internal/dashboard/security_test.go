package dashboard

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewSecurityCreatesRawURLSafeTokenAndAllowsConfiguredHosts(t *testing.T) {
	random := bytes.NewReader(bytes.Repeat([]byte{0xff}, 32))
	security, err := NewSecurity("127.0.0.1:8422", random)
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

func mustSecurity(t *testing.T, listen string) *Security {
	t.Helper()
	security, err := NewSecurity(listen, bytes.NewReader(bytes.Repeat([]byte{1}, 32)))
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
