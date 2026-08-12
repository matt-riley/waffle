package agentbuild

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/broker"
	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/mcp"
	"github.com/matt-riley/waffle/internal/store"
)

// fakeRemoteMCP is a minimal in-process streamable-HTTP MCP server for
// agentbuild tests (zero real network).
func fakeRemoteMCP(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" && r.Method == http.MethodPost {
			// OAuth refresh endpoint: returns a fresh token set so an
			// in-window refresh completes through the broker egress.
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"access_token":"fresh-access-777","refresh_token":"fresh-refresh-777","token_type":"bearer","expires_in":3600,"scope":"read"}`)
			return
		}
		var req struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		reply := func(result string) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":%s}`, req.ID, result)
		}
		switch req.Method {
		case "initialize":
			reply(`{"protocolVersion":"2025-03-26","capabilities":{},"serverInfo":{"name":"fake","version":"0"}}`)
		case "tools/list":
			reply(`{"tools":[{"name":"echo","description":"echo","inputSchema":{"type":"object"}}]}`)
		case "tools/call":
			reply(`{"content":[{"type":"text","text":"pong"}],"isError":false}`)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		default:
			http.Error(w, "unknown method", http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func remoteMCPServer(name, url, egress string, groups ...string) config.MCPServer {
	s := config.MCPServer{Name: name, URL: url, Egress: egress, Groups: groups}
	return s
}

// TestRemoteServerGroupsDefaultToMainTierOnly: stdio servers default an empty
// groups list to "all groups"; remote servers default to main only, and
// the cron/issue/group tiers must be named explicitly (#249).
func TestRemoteServerGroupsDefaultToMainTierOnly(t *testing.T) {
	srv := fakeRemoteMCP(t)
	server := remoteMCPServer("github", srv.URL, "")

	unlisted := []string{config.GroupCron, config.GroupIssue, config.GroupGroup, "custom-tier"}
	for _, group := range unlisted {
		if RemoteServerInGroup(server, group) {
			t.Fatalf("remote server without groups must not reach tier %q", group)
		}
	}
	if !RemoteServerInGroup(server, config.GroupMain) {
		t.Fatal("remote server without groups must be available to main")
	}

	explicit := remoteMCPServer("github", srv.URL, "", config.GroupCron, config.GroupMain)
	if !RemoteServerInGroup(explicit, config.GroupCron) {
		t.Fatal("explicitly named cron tier must be allowed")
	}
	if RemoteServerInGroup(explicit, config.GroupIssue) {
		t.Fatal("unnamed issue tier must stay denied")
	}

	// stdio servers keep the historical all-groups default.
	stdio := config.MCPServer{Name: "local", Command: "/bin/true"}
	if !ServerInGroup(stdio, config.GroupCron) {
		t.Fatal("stdio server with empty groups must keep all-groups default")
	}
}

// TestConnectRemoteMCPRefusesUnsafePosturesNamingServer: docker-mode groups cannot use direct egress
// (unaudited side channel) and need a live broker for broker egress; token
// references need a secret store. All refusals name the server.
func TestConnectRemoteMCPRefusesUnsafePosturesNamingServer(t *testing.T) {
	srv := fakeRemoteMCP(t)
	b := &Builder{}

	cases := []struct {
		name        string
		server      config.MCPServer
		sandboxMode string
		egress      *mcp.RemoteEgress
		wantErr     string
	}{
		{
			name:        "docker group + direct egress refused",
			server:      remoteMCPServer("direct", srv.URL, "direct"),
			sandboxMode: "docker",
			wantErr:     "egress=direct is refused for docker-mode group",
		},
		{
			name:        "docker group + default egress without broker refused",
			server:      remoteMCPServer("nobroker", srv.URL, ""),
			sandboxMode: "docker",
			wantErr:     "egress=broker requires the gateway credential broker",
		},
		{
			name:        "broker egress with nil egress wiring refused",
			server:      remoteMCPServer("nobroker2", srv.URL, "broker"),
			sandboxMode: "host",
			wantErr:     "egress=broker requires the gateway credential broker",
		},
		{
			name:        "token ref without secret store refused",
			server:      config.MCPServer{Name: "tok", URL: srv.URL, Token: "secret://mcp/tok/access-token"},
			sandboxMode: "host",
			wantErr:     "token references the secret store but none is available",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, err := b.connectRemoteMCP(context.Background(), tc.server, "main", tc.sandboxMode)
			if err == nil {
				t.Fatal("connectRemoteMCP succeeded, want refusal")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestDockerModeRemoteMCPRoutesThroughBrokerWithAuditAndRedaction: a docker-mode group's remote
// MCP server with default egress connects through the broker, produces
// audit rows, and its tools reach the toolbox. The redactor built from the
// secret store scrubs the server's token from tool output.
func TestDockerModeRemoteMCPRoutesThroughBrokerWithAuditAndRedaction(t *testing.T) {
	origin := fakeRemoteMCP(t)
	_, originPort, _ := net.SplitHostPort(origin.Listener.Addr().String())

	st, err := store.Open(context.Background(), t.TempDir()+"/waffle.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	b := broker.New(st, nil)
	b.DialEgress = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	b.SetEgress([]broker.EgressTarget{{Host: "localhost", BaseURL: "http://localhost:" + originPort}})
	front := httptest.NewServer(b)
	t.Cleanup(front.Close)

	secrets := mapStore{}
	if err := secrets.Set(mcp.TokenSecretName("github"), "mcp_access_token_secret_abcdef"); err != nil {
		t.Fatal(err)
	}
	egress := &mcp.RemoteEgress{
		ProxyURL: front.URL + "/egress",
		MintToken: func(ctx context.Context, group string) (string, error) {
			return b.Mint(ctx, "mcp-egress:"+group)
		},
	}
	builder := &Builder{Secrets: secrets, RemoteEgress: egress}
	server := remoteMCPServer("github", "http://localhost/", "") // default → broker for docker
	tb, closer, redact, err := builder.connectRemoteMCP(context.Background(), server, "main", "docker")
	if err != nil {
		t.Fatalf("connectRemoteMCP: %v", err)
	}
	t.Cleanup(func() { _ = closer(context.Background()) })

	defs := tb.Defs()
	if len(defs) != 1 || defs[0].Name != "github__echo" {
		t.Fatalf("defs = %+v", defs)
	}
	out, err := tb.Run(context.Background(), "github__echo", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "pong" {
		t.Fatalf("out = %q", out)
	}

	// Audit rows: the docker-mode traffic traversed the broker.
	var egressRows int
	rows, err := st.DB.Query(`SELECT action FROM broker_audit WHERE session LIKE 'mcp-egress:%'`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var action string
		_ = rows.Scan(&action)
		if action == "egress" {
			egressRows++
		}
	}
	if egressRows < 2 {
		t.Fatalf("egress audit rows = %d, want >= 2", egressRows)
	}

	// Redaction: a tool result echoing the token must be scrubbed.
	if redact == nil {
		t.Fatal("connectRemoteMCP returned no redactor")
	}
	if got := redact("result: " + "mcp_access_token_secret_abcdef"); strings.Contains(got, "mcp_access_token_secret_abcdef") {
		t.Fatalf("token reached the model: %q", got)
	}
}

// mapStore is a minimal in-memory secret.Store.
type mapStore map[string]string

func (m mapStore) Get(name string) (string, error) {
	v, ok := m[name]
	if !ok {
		return "", fmt.Errorf("secret %q not found", name)
	}
	return v, nil
}
func (m mapStore) Set(name, value string) error { m[name] = value; return nil }
func (m mapStore) Delete(name string) error     { delete(m, name); return nil }
func (m mapStore) List() ([]string, error) {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out, nil
}

// TestHostModeRemoteMCPConnectsDirectlyWithoutBroker: a host-mode group with direct egress
// reaches the server directly; no broker involved.
func TestHostModeRemoteMCPConnectsDirectlyWithoutBroker(t *testing.T) {
	srv := fakeRemoteMCP(t)
	builder := &Builder{}
	server := remoteMCPServer("github", srv.URL, "direct")
	tb, closer, _, err := builder.connectRemoteMCP(context.Background(), server, "main", "host")
	if err != nil {
		t.Fatalf("connectRemoteMCP: %v", err)
	}
	t.Cleanup(func() { _ = closer(context.Background()) })
	if defs := tb.Defs(); len(defs) != 1 {
		t.Fatalf("defs = %+v", defs)
	}
	out, err := tb.Run(context.Background(), "github__echo", json.RawMessage(`{}`))
	if err != nil || out != "pong" {
		t.Fatalf("Run: %v %q", err, out)
	}
}

// TestDockerModeOAuthRefreshRoutesThroughBrokerEgress: a docker-mode
// group whose stored OAuth token is inside its refresh window must refresh
// through the broker egress proxy — the TokenManager's client is built
// from the same egress configuration as the MCP connection (#249). The
// token endpoint is reachable only via the broker (its host:port exists
// only behind the allowlist rewrite), so a refresh that bypassed the
// proxy would fail and the connection would fail closed; the audit rows
// prove the refresh traversed the broker.
func TestDockerModeOAuthRefreshRoutesThroughBrokerEgress(t *testing.T) {
	origin := fakeRemoteMCP(t)
	_, originPort, _ := net.SplitHostPort(origin.Listener.Addr().String())

	st, err := store.Open(context.Background(), t.TempDir()+"/waffle.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	b := broker.New(st, nil)
	b.DialEgress = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	b.SetEgress([]broker.EgressTarget{{Host: "localhost", BaseURL: "http://localhost:" + originPort}})
	front := httptest.NewServer(b)
	t.Cleanup(front.Close)

	// Token state exactly as `waffle mcp login` stores it, with the token
	// expiring inside the refresh window so the first MCP call refreshes.
	secrets := mapStore{}
	expires := time.Now().Add(2 * time.Minute).UTC().Format(time.RFC3339)
	if err := secrets.Set("mcp/github/access-token", "old-access-123456"); err != nil {
		t.Fatal(err)
	}
	if err := secrets.Set("mcp/github/refresh-token", "refresh-1"); err != nil {
		t.Fatal(err)
	}
	if err := secrets.Set("mcp/github/token-meta", fmt.Sprintf(
		`{"expires_at":%q,"token_type":"bearer","scope":"read","token_endpoint":"http://localhost/token","client_id":"client-1"}`, expires)); err != nil {
		t.Fatal(err)
	}

	egress := &mcp.RemoteEgress{
		ProxyURL: front.URL + "/egress",
		MintToken: func(ctx context.Context, group string) (string, error) {
			return b.Mint(ctx, "mcp-egress:"+group)
		},
	}
	builder := &Builder{Secrets: secrets, RemoteEgress: egress}
	server := remoteMCPServer("github", "http://localhost/", "") // default → broker for docker
	tb, closer, _, err := builder.connectRemoteMCP(context.Background(), server, "main", "docker")
	if err != nil {
		t.Fatalf("connectRemoteMCP: %v", err)
	}
	t.Cleanup(func() { _ = closer(context.Background()) })

	// The refresh completed (through the broker) and was persisted.
	if got := secrets["mcp/github/access-token"]; got != "fresh-access-777" {
		t.Fatalf("stored access token = %q, want the refreshed value (refresh must have traversed the broker)", got)
	}
	out, err := tb.Run(context.Background(), "github__echo", json.RawMessage(`{}`))
	if err != nil || out != "pong" {
		t.Fatalf("Run: %v %q", err, out)
	}

	// Audit rows: the token refresh is broker egress traffic like any other.
	rows, err := st.DB.Query(`SELECT action, detail FROM broker_audit WHERE session LIKE 'mcp-egress:%'`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var tokenRefreshes int
	for rows.Next() {
		var action, detail string
		_ = rows.Scan(&action, &detail)
		if action == "egress" && strings.Contains(detail, "/token") {
			tokenRefreshes++
		}
	}
	if tokenRefreshes < 1 {
		t.Fatal("no broker audit row for the OAuth refresh; refresh bypassed the broker")
	}
}
