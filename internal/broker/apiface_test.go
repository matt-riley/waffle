package broker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/secret"
	"github.com/matt-riley/waffle/internal/store"
	"github.com/matt-riley/waffle/internal/usage"
)

// apiTestCredential is a credential-shaped value used throughout: long
// enough to survive the redactor's minimum length, and never a real key.
const apiTestCredential = "supersecret-weather-key-12345"

// newAPITestBroker builds a broker with one weather face and an optional
// redactor covering apiTestCredential.
func newAPITestBroker(t *testing.T, st *store.Store, faces []APIFace) *Broker {
	t.Helper()
	b := New(st, nil)
	b.SetAPIFaces(faces)
	redactor, err := secret.NewRedactorWith(nil, secret.NamedValue{Name: "api/weather", Value: apiTestCredential})
	if err != nil {
		t.Fatal(err)
	}
	b.Redact = redactor.Redact
	b.RedactOverlap = redactor.MaxLen()
	return b
}

func apiFaceFixture(t *testing.T, upstreamURL string) APIFace {
	t.Helper()
	return APIFace{
		Name:    "weather",
		BaseURL: upstreamURL,
		Header:  "x-api-key",
		Value:   apiTestCredential,
		Methods: []string{"GET", "POST"},
		Paths:   []string{"/v1/weather", "/v1/alerts"},
	}
}

func mintFaces(t *testing.T, b *Broker, session string, faces ...string) string {
	t.Helper()
	token, err := b.MintScopedFaces(context.Background(), session, session, usage.Limits{}, faces)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func mintFacesLimits(t *testing.T, b *Broker, session string, limits usage.Limits, faces ...string) string {
	t.Helper()
	token, err := b.MintScopedFaces(context.Background(), session, session, limits, faces)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// apiNoRedirect is the shared test client: redirects are observed, never
// followed, so a test can assert the broker returns the 3xx un-followed and
// no credential appears anywhere in it.
var apiNoRedirect = &http.Client{
	CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

func apiDo(t *testing.T, front *httptest.Server, token, method, path string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, front.URL+path, strings.NewReader(`{"q":1}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := apiNoRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp, string(body)
}

func TestAPIFaceInjectsCredentialAndReturnsAuthenticatedResponse(t *testing.T) {
	var gotKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-Api-Key")
		if r.URL.Path != "/v1/weather/today" {
			t.Errorf("upstream path = %q, want /v1/weather/today", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "" {
			t.Errorf("caller Authorization leaked upstream: %q", r.Header.Get("Authorization"))
		}
		_, _ = io.WriteString(w, `{"temp":21}`)
	}))
	defer upstream.Close()

	b := newAPITestBroker(t, nil, []APIFace{apiFaceFixture(t, upstream.URL)})
	token := mintFaces(t, b, "sess", "weather")
	front := httptest.NewServer(b)
	defer front.Close()

	resp, body := apiDo(t, front, token, http.MethodGet, "/api/weather/v1/weather/today")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body=%q", resp.StatusCode, body)
	}
	if gotKey != apiTestCredential {
		t.Fatalf("upstream X-Api-Key = %q, want the real credential injected host-side", gotKey)
	}
	if strings.Contains(body, apiTestCredential) {
		t.Fatalf("response body leaked the credential: %q", body)
	}
}

func TestAPIFaceAuthorizationHeaderValueWithBearer(t *testing.T) {
	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `ok`)
	}))
	defer upstream.Close()

	face := APIFace{
		Name: "search", BaseURL: upstream.URL, Header: "Authorization", Value: "Bearer " + apiTestCredential,
		Methods: []string{"GET"}, Paths: []string{"/v1"},
	}
	b := newAPITestBroker(t, nil, []APIFace{face})
	token := mintFaces(t, b, "sess", "search")
	front := httptest.NewServer(b)
	defer front.Close()

	resp, _ := apiDo(t, front, token, http.MethodGet, "/api/search/v1/query")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if gotAuth != "Bearer "+apiTestCredential {
		t.Fatalf("Authorization = %q", gotAuth)
	}
}

// TestAPIFaceCredentialNeverLeavesHost is the containment acceptance test:
// a sandboxed caller receives an authenticated response but cannot obtain
// the credential by any request shape — the face's own token-echo endpoint,
// a redirect to an attacker-controlled host, and error paths.
func TestAPIFaceCredentialNeverLeavesHost(t *testing.T) {
	attackerHits := make(chan string, 8)
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attackerHits <- fmt.Sprintf("%s %s x-api-key=%q authorization=%q",
			r.Method, r.URL.String(), r.Header.Get("X-Api-Key"), r.Header.Get("Authorization"))
		_, _ = io.WriteString(w, `attacker`)
	}))
	defer attacker.Close()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/echo":
			// Token-echo endpoint: reflect every header and the body.
			w.Header().Set("Content-Type", "application/json")
			body, _ := io.ReadAll(r.Body)
			_, _ = io.WriteString(w, fmt.Sprintf(`{"x-api-key":%q,"authorization":%q,"body":%q}`,
				r.Header.Get("X-Api-Key"), r.Header.Get("Authorization"), string(body)))
		case "/v1/redirect":
			http.Redirect(w, r, attacker.URL+"/stolen", http.StatusFound)
		case "/v1/error":
			// Error path: the upstream echoes the credential in the body of
			// a failure response.
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, `{"error":"bad key `+apiTestCredential+`"}`)
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{}`)
		}
	}))
	defer upstream.Close()

	echoFace := apiFaceFixture(t, upstream.URL)
	echoFace.Paths = []string{"/v1"}
	b := newAPITestBroker(t, nil, []APIFace{echoFace})
	token := mintFaces(t, b, "sess", "weather")
	front := httptest.NewServer(b)
	defer front.Close()

	// 1. Token-echo endpoint: the response must be scrubbed.
	resp, body := apiDo(t, front, token, http.MethodPost, "/api/weather/v1/echo?x="+apiTestCredential)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("echo status = %d", resp.StatusCode)
	}
	if strings.Contains(body, apiTestCredential) {
		t.Fatalf("token-echo response leaked the credential: %q", body)
	}

	// 2. Redirect: the broker must not follow it, so the attacker never
	// receives a request, and the 3xx body/headers must not carry the key.
	resp, body = apiDo(t, front, token, http.MethodGet, "/api/weather/v1/redirect")
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("redirect status = %d, want 302 (un-followed)", resp.StatusCode)
	}
	if strings.Contains(body, apiTestCredential) || strings.Contains(resp.Header.Get("Location"), apiTestCredential) {
		t.Fatalf("redirect response leaked the credential: body=%q location=%q", body, resp.Header.Get("Location"))
	}

	// 3. Error path: the failure body echoing the credential is scrubbed.
	resp, body = apiDo(t, front, token, http.MethodGet, "/api/weather/v1/error")
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("error status = %d", resp.StatusCode)
	}
	if strings.Contains(body, apiTestCredential) {
		t.Fatalf("error response leaked the credential: %q", body)
	}

	select {
	case hit := <-attackerHits:
		t.Fatalf("attacker received a request: %s", hit)
	default:
	}

	// 4. A second request aimed at the attacker's path prefix on the same
	// face cannot reach the attacker either (host pinned to base_url).
	resp, _ = apiDo(t, front, token, http.MethodGet, "/api/weather/v1/x")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pinned-host status = %d", resp.StatusCode)
	}
	select {
	case hit := <-attackerHits:
		t.Fatalf("attacker received a request after pinned-host call: %s", hit)
	default:
	}
}

func TestAPIFaceRedactsCredentialFromAuditRows(t *testing.T) {
	st := openStore(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))
	defer upstream.Close()

	b := newAPITestBroker(t, st, []APIFace{apiFaceFixture(t, upstream.URL)})
	token := mintFaces(t, b, "sess-audit", "weather")
	front := httptest.NewServer(b)
	defer front.Close()

	// Allowed request with a credential-shaped path, denied request with a
	// credential-shaped method.
	if resp, _ := apiDo(t, front, token, http.MethodGet, "/api/weather/v1/weather/"+apiTestCredential); resp.StatusCode != http.StatusOK {
		t.Fatalf("allowed status = %d", resp.StatusCode)
	}
	if resp, _ := apiDo(t, front, token, http.MethodDelete, "/api/weather/v1/weather"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("denied status = %d", resp.StatusCode)
	}

	rows, err := st.DB.Query(`SELECT session, action, detail FROM broker_audit WHERE session='sess-audit' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	type row struct{ session, action, detail string }
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.session, &r.action, &r.detail); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	var sawAllowed, sawDenied bool
	for _, r := range got {
		if strings.Contains(r.detail, apiTestCredential) {
			t.Fatalf("audit row leaked the credential: %+v", r)
		}
		if r.session != "sess-audit" {
			t.Fatalf("audit row session = %q", r.session)
		}
		switch r.action {
		case "api":
			if strings.Contains(r.detail, "weather") {
				sawAllowed = true
			}
		case "denied":
			if strings.Contains(r.detail, "weather") {
				sawDenied = true
			}
		}
	}
	if !sawAllowed {
		t.Fatalf("no allowed api audit row: %+v", got)
	}
	if !sawDenied {
		t.Fatalf("no denied api audit row: %+v", got)
	}
}

func TestAPIFaceTokenExpiryAtDefaultTokenTTLBoundary(t *testing.T) {
	// AC: every request requires a valid wk_ token; expiry is enforced at
	// the 24h broker.DefaultTokenTTL boundary.
	clock := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))
	defer upstream.Close()

	b := newAPITestBroker(t, nil, []APIFace{apiFaceFixture(t, upstream.URL)})
	b.Now = func() time.Time { return clock }
	token := mintFaces(t, b, "sess-exp", "weather")
	front := httptest.NewServer(b)
	defer front.Close()

	// Just before mint+DefaultTokenTTL the token is still valid.
	clock = clock.Add(DefaultTokenTTL - time.Nanosecond)
	if resp, _ := apiDo(t, front, token, http.MethodGet, "/api/weather/v1/weather"); resp.StatusCode != http.StatusOK {
		t.Fatalf("status before TTL boundary = %d, want 200", resp.StatusCode)
	}
	// At exactly mint+DefaultTokenTTL the token has expired (expiresAt is
	// not After now) and the face refuses the request.
	clock = clock.Add(time.Nanosecond)
	if resp, _ := apiDo(t, front, token, http.MethodGet, "/api/weather/v1/weather"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status at TTL boundary = %d, want 401", resp.StatusCode)
	}
}

func TestAPIFaceDenyByDefaultForEveryTier(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))
	defer upstream.Close()
	calls := 0

	b := newAPITestBroker(t, nil, []APIFace{apiFaceFixture(t, upstream.URL)})
	front := httptest.NewServer(b)
	defer front.Close()

	// A plain Mint (workspace default path) carries no grants.
	token, err := b.Mint(context.Background(), "sess-plain")
	if err != nil {
		t.Fatal(err)
	}
	if resp, _ := apiDo(t, front, token, http.MethodGet, "/api/weather/v1/weather"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("un-granted status = %d, want 403", resp.StatusCode)
	}
	if calls != 0 {
		t.Fatalf("upstream called %d times for an un-granted face", calls)
	}

	// A grant for a different face does not open this one.
	otherToken := mintFaces(t, b, "sess-other", "search")
	if resp, _ := apiDo(t, front, otherToken, http.MethodGet, "/api/weather/v1/weather"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("other-face-grant status = %d, want 403", resp.StatusCode)
	}

	// An unknown grant name is dropped at mint, never granted.
	unknownToken := mintFaces(t, b, "sess-unknown", "does-not-exist")
	if resp, _ := apiDo(t, front, unknownToken, http.MethodGet, "/api/weather/v1/weather"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unknown-grant status = %d, want 403", resp.StatusCode)
	}
}

func TestAPIFaceMethodAllowlistEnforced(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))
	defer upstream.Close()
	face := apiFaceFixture(t, upstream.URL)
	face.Methods = []string{"GET"}
	b := newAPITestBroker(t, nil, []APIFace{face})
	token := mintFaces(t, b, "sess-methods", "weather")
	front := httptest.NewServer(b)
	defer front.Close()

	tests := []struct {
		method string
		want   int
	}{
		{http.MethodGet, http.StatusOK},
		{http.MethodPost, http.StatusForbidden},
		{http.MethodPut, http.StatusForbidden},
		{http.MethodPatch, http.StatusForbidden},
		{http.MethodDelete, http.StatusForbidden},
		{http.MethodHead, http.StatusForbidden},
		{http.MethodOptions, http.StatusForbidden},
	}
	for _, tc := range tests {
		resp, _ := apiDo(t, front, token, tc.method, "/api/weather/v1/weather")
		if resp.StatusCode != tc.want {
			t.Errorf("%s status = %d, want %d", tc.method, resp.StatusCode, tc.want)
		}
	}
}

func TestAPIFacePathPrefixAndTraversalRefused(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.URL.Path)
	}))
	defer upstream.Close()
	b := newAPITestBroker(t, nil, []APIFace{apiFaceFixture(t, upstream.URL)})
	token := mintFaces(t, b, "sess-paths", "weather")
	front := httptest.NewServer(b)
	defer front.Close()

	tests := []struct {
		name string
		path string
		want int
	}{
		{"allowed exact prefix", "/api/weather/v1/weather", http.StatusOK},
		{"allowed prefix child", "/api/weather/v1/weather/today?units=c", http.StatusOK},
		{"allowed second prefix", "/api/weather/v1/alerts", http.StatusOK},
		{"prefix boundary not fuzzy", "/api/weather/v1/weatherx", http.StatusForbidden},
		{"unlisted path", "/api/weather/v1/forecast", http.StatusForbidden},
		{"no path", "/api/weather", http.StatusForbidden},
		{"dot dot", "/api/weather/v1/weather/../admin", http.StatusForbidden},
		{"encoded dot", "/api/weather/v1/%2e%2e/admin", http.StatusForbidden},
		{"encoded slash", "/api/weather/v1/weather%2f..%2fadmin", http.StatusForbidden},
		{"double-encoded dot", "/api/weather/v1/%252e%252e/admin", http.StatusForbidden},
		{"double-encoded slash", "/api/weather/v1/weather%252f..%252fadmin", http.StatusForbidden},
		{"encoded backslash", "/api/weather/v1/weather%5c..%5cadmin", http.StatusForbidden},
		{"literal backslash", "/api/weather/v1/weather\\..\\admin", http.StatusForbidden},
	}
	for _, tc := range tests {
		resp, body := apiDo(t, front, token, http.MethodGet, tc.path)
		if resp.StatusCode != tc.want {
			t.Errorf("%s: status = %d, want %d (body %q)", tc.name, resp.StatusCode, tc.want, body)
		}
	}
}

func TestAPIFaceUnknownFace404DoesNotDisclose(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))
	defer upstream.Close()
	b := newAPITestBroker(t, nil, []APIFace{apiFaceFixture(t, upstream.URL)})
	token := mintFaces(t, b, "sess-404", "weather")
	front := httptest.NewServer(b)
	defer front.Close()

	bodyA := ""
	bodyB := ""
	for _, path := range []string{"/api/nope/v1/x", "/api/also-missing/v1/x", "/api/", "/api"} {
		resp, body := apiDo(t, front, token, http.MethodGet, path)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s status = %d, want 404", path, resp.StatusCode)
		}
		if bodyA == "" {
			bodyA = body
		}
		if body != bodyA {
			t.Fatalf("%s body = %q, want identical non-disclosing body %q", path, body, bodyA)
		}
		_ = bodyB
	}
	// Unauthenticated requests to an unknown face are 401 (auth comes first)
	// and never disclose faces either.
	req, _ := http.NewRequest(http.MethodGet, front.URL+"/api/nope/v1/x", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || strings.Contains(string(raw), "weather") {
		t.Fatalf("unauthenticated unknown face: status=%d body=%q", resp.StatusCode, raw)
	}
}

func TestAPIFaceRedirectNeverLeavesBaseURL(t *testing.T) {
	var attackerHits atomic.Int32
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attackerHits.Add(1)
		_, _ = io.WriteString(w, "attacker saw "+r.Header.Get("X-Api-Key"))
	}))
	defer attacker.Close()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, attacker.URL+"/capture", http.StatusTemporaryRedirect)
	}))
	defer upstream.Close()

	b := newAPITestBroker(t, nil, []APIFace{apiFaceFixture(t, upstream.URL)})
	token := mintFaces(t, b, "sess-redir", "weather")
	front := httptest.NewServer(b)
	defer front.Close()

	resp, body := apiDo(t, front, token, http.MethodGet, "/api/weather/v1/weather")
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want the un-followed 307", resp.StatusCode)
	}
	if strings.Contains(body, apiTestCredential) || strings.Contains(resp.Header.Get("Location"), apiTestCredential) {
		t.Fatalf("redirect leaked the credential: body=%q location=%q", body, resp.Header.Get("Location"))
	}
	time.Sleep(100 * time.Millisecond)
	if hits := attackerHits.Load(); hits != 0 {
		t.Fatalf("attacker received %d requests", hits)
	}
}

func TestAPIFacePausedAndUsageGatesApply(t *testing.T) {
	st := openStore(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))
	defer upstream.Close()

	b := newAPITestBroker(t, st, []APIFace{apiFaceFixture(t, upstream.URL)})
	b.Usage = usage.New(st)
	b.Limits = usage.Limits{RequestsPerHour: 1}
	b.Now = func() time.Time { return time.Date(2026, 7, 13, 8, 0, 0, 0, time.UTC) }
	token := mintFacesLimits(t, b, "sess-gates", usage.Limits{RequestsPerHour: 1}, "weather")
	front := httptest.NewServer(b)
	defer front.Close()

	// Paused waffle cannot call third-party APIs through faces.
	if err := b.Usage.SetPaused(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if resp, _ := apiDo(t, front, token, http.MethodGet, "/api/weather/v1/weather"); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("paused status = %d, want 429", resp.StatusCode)
	}
	if err := b.Usage.SetPaused(context.Background(), false); err != nil {
		t.Fatal(err)
	}

	// Usage.Check: the shared hourly request budget applies to faces.
	if resp, _ := apiDo(t, front, token, http.MethodGet, "/api/weather/v1/weather"); resp.StatusCode != http.StatusOK {
		t.Fatalf("first status = %d, want 200", resp.StatusCode)
	}
	if resp, _ := apiDo(t, front, token, http.MethodGet, "/api/weather/v1/weather"); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want 429 (hourly budget exhausted)", resp.StatusCode)
	}
}

func TestAPIFaceConcurrentSessionsNeverCrossCredentials(t *testing.T) {
	// Two faces with distinct credentials, two sessions each granted one
	// face, concurrent requests: every upstream request must carry its own
	// face's credential and its own session's nonce, and every caller must
	// get its own response back.
	type observed struct {
		key   string
		nonce string
	}
	blue := make(chan observed, 4)
	red := make(chan observed, 4)
	newUpstream := func(name string, got chan observed) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got <- observed{key: r.Header.Get("X-Api-Key"), nonce: r.Header.Get("X-Nonce")}
			_, _ = io.WriteString(w, r.Header.Get("X-Nonce"))
		}))
	}
	blueUp := newUpstream("blue", blue)
	defer blueUp.Close()
	redUp := newUpstream("red", red)
	defer redUp.Close()

	blueFace := apiFaceFixture(t, blueUp.URL)
	blueFace.Name = "weather"
	blueFace.Value = "supersecret-blue-key-0001"
	redFace := apiFaceFixture(t, redUp.URL)
	redFace.Name = "search"
	redFace.Value = "supersecret-red-key-0002"

	b := newAPITestBroker(t, nil, []APIFace{blueFace, redFace})
	blueToken := mintFaces(t, b, "sess-blue", "weather")
	redToken := mintFaces(t, b, "sess-red", "search")
	front := httptest.NewServer(b)
	defer front.Close()

	start := make(chan struct{})
	call := func(token, face, nonce string, got chan observed) <-chan string {
		done := make(chan string, 1)
		go func() {
			<-start
			req, _ := http.NewRequest(http.MethodGet, front.URL+"/api/"+face+"/v1/weather?q="+nonce, nil)
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("X-Nonce", nonce)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				done <- "err:" + err.Error()
				return
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			done <- string(body)
		}()
		return done
	}
	// Both sessions hit their faces concurrently, twice each.
	results := []<-chan string{
		call(blueToken, "weather", "nonce-blue-1", blue),
		call(redToken, "search", "nonce-red-1", red),
		call(blueToken, "weather", "nonce-blue-2", blue),
		call(redToken, "search", "nonce-red-2", red),
	}
	close(start)
	for i, done := range results {
		want := "nonce-" + map[int]string{0: "blue-1", 1: "red-1", 2: "blue-2", 3: "red-2"}[i]
		if got := <-done; got != want {
			t.Fatalf("call %d got body %q, want %q (responses crossed sessions)", i, got, want)
		}
	}
	for i := 0; i < 2; i++ {
		o := <-blue
		if o.key != blueFace.Value {
			t.Fatalf("blue upstream saw key %q, want %q (credentials crossed)", o.key, blueFace.Value)
		}
	}
	for i := 0; i < 2; i++ {
		o := <-red
		if o.key != redFace.Value {
			t.Fatalf("red upstream saw key %q, want %q (credentials crossed)", o.key, redFace.Value)
		}
	}
}

func TestAPIFaceClientCancellationAndUpstreamFailure(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer func() {
		close(release)
		upstream.Close()
	}()

	b := newAPITestBroker(t, nil, []APIFace{apiFaceFixture(t, upstream.URL)})
	token := mintFaces(t, b, "sess-cancel", "weather")
	front := httptest.NewServer(b)
	defer front.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, front.URL+"/api/weather/v1/weather", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	done := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("upstream not started")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("client abort returned nil error")
		}
	case <-time.After(time.Second):
		t.Fatal("client abort did not return")
	}

	// A failing upstream surfaces a redacted error body, and the broker
	// itself keeps serving.
	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `boom `+apiTestCredential)
	}))
	defer fail.Close()
	failFace := apiFaceFixture(t, fail.URL)
	failFace.Methods = []string{"GET"}
	b2 := newAPITestBroker(t, nil, []APIFace{failFace})
	token2 := mintFaces(t, b2, "sess-fail", "weather")
	front2 := httptest.NewServer(b2)
	defer front2.Close()
	resp, body := apiDo(t, front2, token2, http.MethodGet, "/api/weather/v1/weather")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("failure status = %d, want 500", resp.StatusCode)
	}
	if strings.Contains(body, apiTestCredential) {
		t.Fatalf("failure body leaked the credential: %q", body)
	}
}

func TestAPIFaceAbsentConfigRoutes404(t *testing.T) {
	b := New(nil, nil)
	token, err := b.Mint(context.Background(), "sess-none")
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(b)
	defer front.Close()
	if resp, _ := apiDo(t, front, token, http.MethodGet, "/api/weather/v1/x"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestAPIFaceRedactsCredentialFromLogLines(t *testing.T) {
	// A proxy transport error can carry the request URL, which the caller
	// controls. The face proxy's ErrorLog routes every line through the
	// broker's redactor before it reaches the process log.
	var logged bytes.Buffer
	handler := slog.NewTextHandler(&logged, nil)
	prev := slog.Default()
	slog.SetDefault(slog.New(handler))
	defer slog.SetDefault(prev)

	redactor, err := secret.NewRedactorWith(nil, secret.NamedValue{Name: "api/weather", Value: apiTestCredential})
	if err != nil {
		t.Fatal(err)
	}
	w := redactingLogWriter{redact: redactor.Redact}
	line := "http: proxy error: GET http://api.example.com/v1/" + apiTestCredential + ": connection refused"
	if _, err := w.Write([]byte(line)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logged.String(), apiTestCredential) {
		t.Fatalf("log line leaked the credential: %q", logged.String())
	}
	if !strings.Contains(logged.String(), "[redacted:api/weather]") {
		t.Fatalf("log line not redacted: %q", logged.String())
	}
}
