package dashboard

import (
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

const contentSecurityPolicy = "default-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'"

// Security protects the loopback Waffle Desk listener from cross-origin requests.
type Security struct {
	token        string
	allowedHosts map[string]struct{}
	// policyAuditDB, when set, receives policy_audit rows for admitted Desk mutations.
	policyAuditDB *sql.DB
}

// NewSecurity creates a process-scoped CSRF token and derives the only Hosts
// that may address the configured loopback listener.
func NewSecurity(listen string, random io.Reader) (*Security, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return nil, fmt.Errorf("parse dashboard listen address: %w", err)
	}
	if port == "" || !isLoopbackHost(host) {
		return nil, fmt.Errorf("dashboard listen address must use a loopback host")
	}
	bytes := make([]byte, 32)
	if _, err := io.ReadFull(random, bytes); err != nil {
		return nil, fmt.Errorf("read dashboard token entropy: %w", err)
	}
	return &Security{
		token: base64.RawURLEncoding.EncodeToString(bytes),
		allowedHosts: map[string]struct{}{
			canonicalHost(net.JoinHostPort(host, port)):        {},
			canonicalHost(net.JoinHostPort("localhost", port)): {},
		},
	}, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Token returns the process-scoped request token for same-origin mutations.
func (s *Security) Token() string {
	return s.token
}

// SetPolicyAuditDB attaches the shared policy_audit database used by Desk mutations.
// Passing nil disables audit writes (tests without a store).
func (s *Security) SetPolicyAuditDB(db *sql.DB) {
	if s == nil {
		return
	}
	s.policyAuditDB = db
}

// PolicyAuditDB returns the optional policy_audit destination for mutations.
func (s *Security) PolicyAuditDB() *sql.DB {
	if s == nil {
		return nil
	}
	return s.policyAuditDB
}

// Wrap validates request metadata and applies response hardening headers.
func (s *Security) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w.Header())
		if !s.allowsRequest(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func setSecurityHeaders(header http.Header) {
	header.Set("Content-Security-Policy", contentSecurityPolicy)
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("X-Frame-Options", "DENY")
}

func (s *Security) allowsRequest(r *http.Request) bool {
	if _, ok := s.allowedHosts[canonicalHost(r.Host)]; !ok {
		return false
	}
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	return canonicalHost(parsed.Host) == canonicalHost(r.Host)
}

func canonicalHost(hostport string) string {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil || port == "" {
		return ""
	}
	return net.JoinHostPort(strings.ToLower(host), port)
}

// RequireMutation verifies the process token and an idempotency key before
// allowing a state-changing handler to execute.
func (s *Security) RequireMutation(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Waffle-Desk-Token")), []byte(s.token)) != 1 {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if r.Header.Get("Idempotency-Key") == "" {
			http.Error(w, "idempotency_key_required", http.StatusBadRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}
