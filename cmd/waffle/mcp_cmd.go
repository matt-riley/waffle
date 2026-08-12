package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/mcp"
)

// mcpCmd is the owner-side `waffle mcp` surface (#249): interactive OAuth
// login for remote (url) MCP servers, status, and logout. Tokens are
// stored in the encrypted secret store under mcp/<server>/... — config.toml
// holds only the server's url and (optionally) secret:// references.
func mcpCmd(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		mcpUsage(stderr)
		return errUsage
	}
	switch args[0] {
	case "login":
		return mcpLogin(ctx, args[1:], stdout, stderr)
	case "logout":
		if len(args) != 2 {
			return errors.New("usage: waffle mcp logout <server>")
		}
		return mcpLogout(args[1], stdout, stderr)
	case "status":
		return mcpStatus(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		mcpUsage(stdout)
		return nil
	default:
		mcpUsage(stderr)
		return fmt.Errorf("unknown mcp command %q", args[0])
	}
}

func mcpUsage(w io.Writer) {
	fmt.Fprint(w, `Manage remote (url-based) MCP servers (#249).

Remote MCP servers are authorized with OAuth; tokens are stored in the
encrypted secret store (age-encrypted), never in config.toml.

Usage:
  waffle mcp login <server> [--scope a,b]   authorize a url server (OAuth
                                            authorization-code + PKCE, dynamic
                                            client registration)
  waffle mcp status [server...]             show token state per server
  waffle mcp logout <server>                delete the server's stored tokens

Config contract: a server must have url set (exactly one of command/url),
egress is "broker" (default for docker-mode groups) or "direct", and any
token reference is a secret:// reference. See config.example.toml.
`)
}

// mcpLogin runs the interactive OAuth authorization-code + PKCE flow for
// one configured url server: RFC 8414 metadata discovery, dynamic client
// registration (RFC 7591), browser round trip to the loopback redirect,
// code exchange, and token storage in internal/secret.
func mcpLogin(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	var scope string
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "--scope="):
			scope = strings.TrimPrefix(a, "--scope=")
		case a == "--scope":
			return errors.New("usage: waffle mcp login <server> [--scope a,b]")
		case strings.HasPrefix(a, "-"):
			return fmt.Errorf("unknown flag %q", a)
		}
	}
	rest := []string{}
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			rest = append(rest, a)
		}
	}
	if len(rest) != 1 {
		return errors.New("usage: waffle mcp login <server> [--scope a,b]")
	}
	serverName := rest[0]

	cfgPath, err := config.Path()
	if err != nil {
		return err
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	var server *config.MCPServer
	for i := range cfg.MCP {
		if cfg.MCP[i].Name == serverName {
			server = &cfg.MCP[i]
			break
		}
	}
	if server == nil || server.URL == "" {
		return fmt.Errorf("no remote (url) MCP server named %q in config; add [[mcp.server]] with url = \"...\" first", serverName)
	}

	store, err := openSecretStore()
	if err != nil {
		return fmt.Errorf("secret store: %w (run `waffle secret init` first)", err)
	}

	httpClient := &http.Client{Timeout: 30 * time.Second}
	fmt.Fprintf(stderr, "discovering OAuth metadata for %s …\n", server.URL)
	meta, err := mcp.DiscoverOAuthMetadata(ctx, server.URL, httpClient)
	if err != nil {
		return err
	}
	if meta.RegistrationEndpoint == "" {
		return fmt.Errorf("server %q does not offer dynamic client registration (no registration_endpoint in its OAuth metadata); static credentials are not supported yet", serverName)
	}

	// Loopback redirect: the browser round trip lands on 127.0.0.1.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("loopback listener: %w", err)
	}
	defer func() { _ = ln.Close() }()
	redirectURI := "http://" + ln.Addr().String() + "/callback"

	fmt.Fprintf(stderr, "registering a dynamic OAuth client (PKCE) …\n")
	reg, err := mcp.RegisterDynamicClient(ctx, meta.RegistrationEndpoint, redirectURI, httpClient)
	if err != nil {
		return err
	}

	pkce, err := mcp.NewPKCEPair()
	if err != nil {
		return err
	}
	state, err := randomState()
	if err != nil {
		return err
	}
	var scopes []string
	if scope != "" {
		scopes = strings.Split(scope, ",")
	}
	authURL, err := mcp.BuildAuthorizationURL(meta, reg.ClientID, redirectURI, state, pkce, scopes)
	if err != nil {
		return err
	}

	fmt.Fprintf(stderr, "open this URL in your browser to authorize waffle for %q:\n\n  %s\n\n", serverName, authURL)
	openBrowser(authURL)

	code, err := waitForCallback(ctx, ln, state, 5*time.Minute)
	if err != nil {
		return err
	}

	fmt.Fprintf(stderr, "exchanging the authorization code …\n")
	ts, err := mcp.ExchangeAuthorizationCode(ctx, meta.TokenEndpoint, reg.ClientID, code, redirectURI, pkce.Verifier, httpClient)
	if err != nil {
		return err
	}

	tm := &mcp.TokenManager{Store: store, Server: serverName, HTTP: httpClient}
	metaScope := ts.Scope
	if metaScope == "" {
		metaScope = strings.Join(scopes, " ")
	}
	if err := tm.Save(ts, mcp.TokenMeta{
		TokenEndpoint: meta.TokenEndpoint,
		ClientID:      reg.ClientID,
		Scope:         metaScope,
	}); err != nil {
		return fmt.Errorf("store tokens: %w", err)
	}
	fmt.Fprintf(stdout, "authorized %s; tokens stored in the encrypted secret store (mcp/%s/*) — config.toml holds references only.\n", serverName, serverName)
	fmt.Fprintf(stdout, "restart `waffle serve` (or the chat session) so the running redactor knows the new token.\n")
	return nil
}

func mcpLogout(server string, stdout, stderr io.Writer) error {
	store, err := openSecretStore()
	if err != nil {
		return fmt.Errorf("secret store: %w", err)
	}
	tm := &mcp.TokenManager{Store: store, Server: server}
	if err := tm.Clear(); err != nil {
		return fmt.Errorf("clear tokens for %q: %w", server, err)
	}
	fmt.Fprintf(stdout, "removed stored tokens for %q.\n", server)
	return nil
}

func mcpStatus(names []string, stdout, stderr io.Writer) error {
	cfgPath, err := config.Path()
	if err != nil {
		return err
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	store, err := openSecretStore()
	if err != nil {
		// Without an identity there is nothing stored: report no tokens.
		store = nil
	}
	var servers []config.MCPServer
	for _, s := range cfg.MCP {
		if s.URL != "" && (len(names) == 0 || contains(names, s.Name)) {
			servers = append(servers, s)
		}
	}
	if len(servers) == 0 {
		fmt.Fprintln(stdout, "no remote (url) MCP servers configured"+(orList(names)))
		return nil
	}
	sort.Slice(servers, func(i, j int) bool { return servers[i].Name < servers[j].Name })
	for _, s := range servers {
		fmt.Fprintf(stdout, "%-20s %s\n", s.Name, s.URL)
		if store == nil {
			fmt.Fprintln(stdout, "  tokens: none (no secret store)")
			continue
		}
		tm := &mcp.TokenManager{Store: store, Server: s.Name}
		st := tm.Status()
		switch {
		case st.Err != "":
			fmt.Fprintf(stdout, "  tokens: error: %s\n", st.Err)
		case !st.HasToken:
			fmt.Fprintf(stdout, "  tokens: none — run `waffle mcp login %s`\n", s.Name)
		default:
			fmt.Fprintf(stdout, "  tokens: ok, expires %s%s\n", st.ExpiresAt.Local().Format(time.RFC3339), scopeSuffix(st.Scope))
		}
	}
	return nil
}

func scopeSuffix(scope string) string {
	if scope == "" {
		return ""
	}
	return " (scope: " + scope + ")"
}

func orList(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return " matching " + strings.Join(names, ", ")
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func randomState() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// waitForCallback serves the loopback redirect once and returns the
// authorization code, verifying the state to prevent CSRF on the redirect.
func waitForCallback(ctx context.Context, ln net.Listener, state string, timeout time.Duration) (string, error) {
	type result struct {
		code string
		err  error
	}
	done := make(chan result, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			done <- result{err: errors.New("callback state mismatch (possible CSRF) — aborting login")}
			return
		}
		if errDesc := q.Get("error"); errDesc != "" {
			http.Error(w, "authorization denied", http.StatusForbidden)
			done <- result{err: fmt.Errorf("authorization server refused: %s (%s)", errDesc, q.Get("error_description"))}
			return
		}
		code := q.Get("code")
		if code == "" {
			http.Error(w, "no code", http.StatusBadRequest)
			done <- result{err: errors.New("callback carried no authorization code")}
			return
		}
		_, _ = io.WriteString(w, "waffle: authorization complete — you can close this tab.")
		done <- result{code: code}
	})
	srv := &http.Server{Handler: mux}
	defer func() { _ = srv.Shutdown(context.Background()) }()
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	select {
	case res := <-done:
		return res.code, res.err
	case <-time.After(timeout):
		return "", fmt.Errorf("timed out waiting for the browser callback (opened URL: see above); no token stored")
	case <-ctx.Done():
		return "", ctx.Err()
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return "", fmt.Errorf("loopback server: %w", err)
		}
		return "", errors.New("loopback server stopped before the callback")
	}
}

// openBrowser best-effort opens the authorization URL in the user's
// browser; failure only prints the URL (which was already shown).
func openBrowser(authURL string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", authURL)
	case "linux":
		cmd = exec.Command("xdg-open", authURL)
	default:
		return
	}
	_ = cmd.Start()
}
