package apiface

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/tool"
)

const cred = "supersecret-tool-key-777"

func testFaces() []Face {
	return []Face{
		{Name: "weather", Methods: []string{"GET"}, Paths: []string{"/v1/weather"}},
		{Name: "search", Methods: []string{"GET", "POST"}, Paths: []string{"/v1/search"}},
	}
}

func testClient(brokerURL string, mint func(context.Context, string, []string) (string, error)) *Client {
	return &Client{
		Faces:     testFaces(),
		BrokerURL: brokerURL,
		Mint:      mint,
		Revoke:    func(string) {},
		Redact: func(s string) string {
			return strings.ReplaceAll(s, cred, "[redacted]")
		},
	}
}

func TestToolsForGrantsOnlyLiteralAllowEntries(t *testing.T) {
	c := testClient("http://broker", nil)
	faces := c.ToolsFor(tool.Policy{})
	if len(faces) != 0 {
		t.Fatalf("empty allow list offered %d tools, want 0 (deny by default)", len(faces))
	}
	faces = c.ToolsFor(tool.Policy{Allow: []string{"*"}})
	if len(faces) != 0 {
		t.Fatalf("wildcard allow offered %d tools, want 0 (deny by default)", len(faces))
	}
	faces = c.ToolsFor(tool.Policy{Allow: []string{"api_weather"}})
	if len(faces) != 1 || faces[0].Def().Name != "api_weather" {
		t.Fatalf("literal grant offered %d tools: %v", len(faces), faces)
	}
	// Deny always wins.
	faces = c.ToolsFor(tool.Policy{Allow: []string{"api_weather", "api_search"}, Deny: []string{"api_weather"}})
	names := map[string]bool{}
	for _, f := range faces {
		names[f.Def().Name] = true
	}
	if !names["api_search"] || names["api_weather"] {
		t.Fatalf("deny did not win: %v", names)
	}
}

func TestFaceToolDefNamesSchemaAndDescription(t *testing.T) {
	c := testClient("http://broker", nil)
	tools := c.ToolsFor(tool.Policy{Allow: []string{"api_weather", "api_search"}})
	var weather llm.Tool
	for _, tl := range tools {
		if tl.Def().Name == "api_weather" {
			weather = tl.Def()
		}
	}
	if weather.Name != "api_weather" {
		t.Fatalf("no api_weather tool: %+v", weather)
	}
	if !strings.Contains(weather.Description, "weather") || !strings.Contains(weather.Description, "GET") ||
		!strings.Contains(weather.Description, "/v1/weather") || !strings.Contains(weather.Description, "untrusted data") {
		t.Fatalf("description = %q", weather.Description)
	}
	var schema struct {
		Properties map[string]struct {
			Enum []string `json:"enum"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(weather.InputSchema, &schema); err != nil {
		t.Fatal(err)
	}
	if len(schema.Properties["method"].Enum) != 1 || schema.Properties["method"].Enum[0] != "GET" {
		t.Fatalf("method enum = %v", schema.Properties["method"].Enum)
	}
	if len(schema.Required) != 2 || schema.Required[0] != "method" || schema.Required[1] != "path" {
		t.Fatalf("required = %v", schema.Required)
	}
}

func TestFaceToolRunMintsCallsAndRevokes(t *testing.T) {
	var (
		mu         sync.Mutex
		minted     []string
		revoked    []string
		gotAuth    string
		gotPath    string
		gotMethod  string
		gotBody    string
		gotSession string
	)
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotAuth, gotPath, gotMethod = r.Header.Get("Authorization"), r.URL.Path, r.Method
		mu.Unlock()
		body := make([]byte, 16)
		n, _ := r.Body.Read(body)
		gotBody = string(body[:n])
		_, _ = w.Write([]byte(`{"temp":21}`))
	}))
	defer broker.Close()

	client := testClient(broker.URL, func(ctx context.Context, sessionID string, faces []string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		gotSession = sessionID
		minted = append(minted, faces...)
		return "wk_testtoken", nil
	})
	tool_ := client.ToolsFor(tool.Policy{Allow: []string{"api_weather"}})[0]

	ctx := session.WithSession(context.Background(), "sess-1")
	out, err := tool_.Run(ctx, json.RawMessage(`{"method":"GET","path":"/v1/weather/today","body":"{\"q\":1}"}`))
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(out, "HTTP 200") || !strings.Contains(out, `{"temp":21}`) {
		t.Fatalf("out = %q", out)
	}
	if gotSession != "sess-1" {
		t.Fatalf("session = %q", gotSession)
	}
	if len(minted) != 1 || minted[0] != "weather" {
		t.Fatalf("minted faces = %v", minted)
	}
	if gotAuth != "Bearer wk_testtoken" {
		t.Fatalf("broker auth = %q", gotAuth)
	}
	if gotPath != "/api/weather/v1/weather/today" || gotMethod != "GET" {
		t.Fatalf("broker saw %s %s", gotMethod, gotPath)
	}
	if gotBody != `{"q":1}` {
		t.Fatalf("body = %q", gotBody)
	}
	if len(revoked) != 0 {
		t.Fatalf("revoked = %v", revoked)
	}
}

func TestFaceToolRunRevokesAfterCall(t *testing.T) {
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`ok`))
	}))
	defer broker.Close()
	var revoked []string
	client := testClient(broker.URL, func(context.Context, string, []string) (string, error) { return "wk_x", nil })
	client.Revoke = func(token string) { revoked = append(revoked, token) }
	tool_ := client.ToolsFor(tool.Policy{Allow: []string{"api_weather"}})[0]
	ctx := session.WithSession(context.Background(), "sess-2")
	if _, err := tool_.Run(ctx, json.RawMessage(`{"method":"GET","path":"/v1/weather"}`)); err != nil {
		t.Fatal(err)
	}
	if len(revoked) != 1 || revoked[0] != "wk_x" {
		t.Fatalf("revoked = %v", revoked)
	}
}

func TestFaceToolRunRedactsOutputAndErrors(t *testing.T) {
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/weather/v1/weather/echo" {
			_, _ = w.Write([]byte(`token ` + cred))
			return
		}
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`boom ` + cred))
	}))
	defer broker.Close()
	client := testClient(broker.URL, func(context.Context, string, []string) (string, error) { return "wk_x", nil })
	tool_ := client.ToolsFor(tool.Policy{Allow: []string{"api_weather"}})[0]
	ctx := session.WithSession(context.Background(), "sess-3")

	out, err := tool_.Run(ctx, json.RawMessage(`{"method":"GET","path":"/v1/weather/echo"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, cred) {
		t.Fatalf("output leaked the credential: %q", out)
	}
	if !strings.Contains(out, "[redacted]") {
		t.Fatalf("output not redacted: %q", out)
	}

	_, err = tool_.Run(ctx, json.RawMessage(`{"method":"GET","path":"/v1/weather/nope"}`))
	if err == nil {
		t.Fatal("expected error for refused call")
	}
	if strings.Contains(err.Error(), cred) {
		t.Fatalf("error leaked the credential: %q", err.Error())
	}
}

func TestFaceToolRunNeverFollowsRedirects(t *testing.T) {
	attackerHits := 0
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attackerHits++
		_, _ = w.Write([]byte("attacker saw " + r.Header.Get("Authorization")))
	}))
	defer attacker.Close()

	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, attacker.URL+"/steal", http.StatusFound)
	}))
	defer broker.Close()
	client := testClient(broker.URL, func(context.Context, string, []string) (string, error) { return "wk_x", nil })
	tool_ := client.ToolsFor(tool.Policy{Allow: []string{"api_weather"}})[0]
	ctx := session.WithSession(context.Background(), "sess-4")
	out, err := tool_.Run(ctx, json.RawMessage(`{"method":"GET","path":"/v1/weather"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "302") {
		t.Fatalf("out = %q, want the un-followed 302", out)
	}
	if attackerHits != 0 {
		t.Fatalf("attacker received %d requests (token leaked via redirect)", attackerHits)
	}
}

func TestFaceToolRunRejectsBadInputAndMissingSession(t *testing.T) {
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`ok`))
	}))
	defer broker.Close()
	client := testClient(broker.URL, func(context.Context, string, []string) (string, error) { return "wk_x", nil })
	tool_ := client.ToolsFor(tool.Policy{Allow: []string{"api_weather"}})[0]

	ctx := session.WithSession(context.Background(), "sess-5")
	for _, input := range []string{
		`{"path":"/v1/weather"}`,
		`{"method":"GET"}`,
		`{"method":"GET","path":"v1/weather"}`,
		`not json`,
	} {
		if _, err := tool_.Run(ctx, json.RawMessage(input)); err == nil {
			t.Fatalf("input %q accepted", input)
		}
	}
	// No session id in context: refuse.
	if _, err := tool_.Run(context.Background(), json.RawMessage(`{"method":"GET","path":"/v1/weather"}`)); err == nil {
		t.Fatal("missing session accepted")
	}
	// Mint failure surfaces as a redacted error.
	failClient := testClient(broker.URL, func(context.Context, string, []string) (string, error) {
		return "", context.Canceled
	})
	failTool := failClient.ToolsFor(tool.Policy{Allow: []string{"api_weather"}})[0]
	if _, err := failTool.Run(ctx, json.RawMessage(`{"method":"GET","path":"/v1/weather"}`)); err == nil {
		t.Fatal("mint failure not surfaced")
	}
}
