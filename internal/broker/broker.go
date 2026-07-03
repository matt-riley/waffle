// Package broker is the host-side credential broker (docs/plan.md,
// "Secret management"). One rule: raw keys exist only here. Sandboxes hold
// short-lived wk_ session tokens and talk to the broker, which injects the
// real credential upstream. Phase 4 ships the LLM face (Anthropic and
// OpenAI-compatible passthrough); the git and egress faces arrive with
// repo workspaces (phase 5).
package broker

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/matt-riley/waffle/internal/store"
)

// Upstream is one provider the broker can front.
type Upstream struct {
	// Name routes requests: the broker serves /<name>/<upstream path>.
	Name string
	// BaseURL of the real provider (e.g. https://api.anthropic.com).
	BaseURL string
	// Header is the auth header to inject ("x-api-key" or "Authorization").
	Header string
	// Value is the real credential (for Authorization: pass "Bearer ...").
	Value string
}

// Broker mints session tokens and proxies authenticated requests.
type Broker struct {
	audit     *sql.DB
	upstreams map[string]*httputil.ReverseProxy

	mu     sync.Mutex
	tokens map[string]string // token → session id
}

// New builds a broker over the given upstreams; st may be nil to skip
// audit persistence (tests).
func New(st *store.Store, upstreams []Upstream) *Broker {
	b := &Broker{
		upstreams: map[string]*httputil.ReverseProxy{},
		tokens:    map[string]string{},
	}
	if st != nil {
		b.audit = st.DB
	}
	for _, u := range upstreams {
		target, err := url.Parse(u.BaseURL)
		if err != nil {
			continue
		}
		header, value := u.Header, u.Value
		proxy := &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(target)
				pr.Out.Host = target.Host
				// Strip the sandbox's token, inject the real key. This is
				// the only place a raw credential touches a request.
				pr.Out.Header.Del("Authorization")
				pr.Out.Header.Del("X-Api-Key")
				pr.Out.Header.Set(header, value)
			},
		}
		b.upstreams[u.Name] = proxy
	}
	return b
}

// Mint issues a wk_ session token bound to sessionID.
func (b *Broker) Mint(ctx context.Context, sessionID string) string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic(err) // crypto/rand failing is not a recoverable state
	}
	token := "wk_" + hex.EncodeToString(raw[:])
	b.mu.Lock()
	b.tokens[token] = sessionID
	b.mu.Unlock()
	b.record(ctx, token, sessionID, "mint", "")
	return token
}

// Revoke invalidates a token (session ended).
func (b *Broker) Revoke(token string) {
	b.mu.Lock()
	delete(b.tokens, token)
	b.mu.Unlock()
}

// session resolves a bearer token to its session, or "".
func (b *Broker) session(token string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tokens[token]
}

// ServeHTTP implements the broker's HTTP face: /<upstream>/<path>, bearer
// wk_ token required.
func (b *Broker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	sessionID := ""
	if strings.HasPrefix(token, "wk_") {
		sessionID = b.session(token)
	}
	if sessionID == "" {
		b.record(r.Context(), token, "", "denied", r.URL.Path)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	name, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	proxy, ok := b.upstreams[name]
	if !ok {
		http.Error(w, "unknown upstream", http.StatusNotFound)
		return
	}
	b.record(r.Context(), token, sessionID, "proxy", name+"/"+rest)
	r.URL.Path = "/" + rest
	proxy.ServeHTTP(w, r)
}

// Serve runs the broker's HTTP listener until ctx ends.
func (b *Broker) Serve(ctx context.Context, listen string) error {
	srv := &http.Server{
		Addr:              listen,
		Handler:           b,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	err := srv.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

func (b *Broker) record(ctx context.Context, token, sessionID, action, detail string) {
	if b.audit == nil {
		return
	}
	prefix := token
	if len(prefix) > 11 {
		prefix = prefix[:11]
	}
	_, _ = b.audit.ExecContext(ctx, `
		INSERT INTO broker_audit (at, token_prefix, session, action, detail)
		VALUES (?, ?, ?, ?, ?)`,
		time.Now().UTC().Format(time.RFC3339Nano), prefix, sessionID, action, detail)
}
