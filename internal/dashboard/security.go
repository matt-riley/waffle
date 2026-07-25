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
	"path"
	"strings"
)

const contentSecurityPolicy = "default-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'"

// tailnetServePort is the port `tailscale serve --https=443` answers on, and so
// the port implied by a proxied Host header that carries no explicit port.
const tailnetServePort = "443"

// admissionProfile identifies which boundary a request was admitted through.
type admissionProfile int

const (
	// profileNone is an unrecognized Host: always rejected.
	profileNone admissionProfile = iota
	// profileLoopback is a direct request to the loopback listener.
	profileLoopback
	// profileTailnet is a request proxied by `tailscale serve` on this host.
	profileTailnet
)

// TailnetOptions opts Desk into serving requests proxied by `tailscale serve`.
// It never changes the bind address; the listener stays loopback-only, which is
// what keeps the injected Tailscale identity headers trustworthy.
type TailnetOptions struct {
	Enabled       bool
	ServeHost     string
	AllowedLogins []string
}

// tailnetAdmission is the resolved, request-time form of TailnetOptions.
type tailnetAdmission struct {
	hosts map[string]struct{}
	// logins is a slice rather than a set so comparison can avoid early exit.
	logins []string
}

// Security protects the loopback Waffle Desk listener from cross-origin requests
// and, when a tailnet admission is configured, authenticates requests proxied to
// that listener by `tailscale serve`.
type Security struct {
	token        string
	allowedHosts map[string]struct{}
	// tailnet is nil unless the tailnet Desk path is explicitly enabled.
	tailnet *tailnetAdmission
	// policyAuditDB, when set, receives policy_audit rows for admitted Desk mutations.
	policyAuditDB *sql.DB
	// onRejectedLogin, when set, reports a rejected tailnet login so an
	// allowlist mismatch is diagnosable instead of a silent 403.
	onRejectedLogin func(login string)
}

// NewSecurity creates a process-scoped CSRF token and derives the only Hosts
// that may address the configured loopback listener. When tailnet is enabled it
// additionally derives the served MagicDNS name and its login allowlist.
func NewSecurity(listen string, tailnet TailnetOptions, random io.Reader) (*Security, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return nil, fmt.Errorf("parse dashboard listen address: %w", err)
	}
	if port == "" || !isLoopbackHost(host) {
		return nil, fmt.Errorf("dashboard listen address must use a loopback host")
	}
	admission, err := newTailnetAdmission(tailnet)
	if err != nil {
		return nil, err
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
		tailnet: admission,
	}, nil
}

// newTailnetAdmission resolves the tailnet opt-in, or returns nil when it is
// disabled. It fails closed rather than admitting an incomplete configuration.
func newTailnetAdmission(options TailnetOptions) (*tailnetAdmission, error) {
	if !options.Enabled {
		return nil, nil
	}
	if options.ServeHost == "" {
		return nil, fmt.Errorf("dashboard tailnet serve host is required")
	}
	if net.ParseIP(options.ServeHost) != nil {
		return nil, fmt.Errorf("dashboard tailnet serve host must be a MagicDNS name, not an IP address")
	}
	if _, _, err := net.SplitHostPort(options.ServeHost); err == nil {
		return nil, fmt.Errorf("dashboard tailnet serve host must not include a port")
	}
	logins := make([]string, 0, len(options.AllowedLogins))
	for _, login := range options.AllowedLogins {
		if login == "" {
			return nil, fmt.Errorf("dashboard tailnet allowed login must not be empty")
		}
		logins = append(logins, strings.ToLower(login))
	}
	if len(logins) == 0 {
		return nil, fmt.Errorf("dashboard tailnet requires at least one allowed login")
	}
	served := strings.ToLower(options.ServeHost)
	return &tailnetAdmission{
		hosts: map[string]struct{}{
			served: {},
			net.JoinHostPort(served, tailnetServePort): {},
		},
		logins: logins,
	}, nil
}

// SetLoginRejectionObserver registers a callback invoked with the sanitized
// login of a tailnet request rejected for not matching the allowlist.
func (s *Security) SetLoginRejectionObserver(observe func(login string)) {
	if s == nil {
		return
	}
	s.onRejectedLogin = observe
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
		profile := s.profileFor(r)
		if profile == profileTailnet {
			// Only meaningful on the HTTPS origin Serve terminates; emitting it
			// on http://127.0.0.1 would pin loopback to a scheme it cannot use.
			w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		}
		if !s.allowsRequest(r, profile) {
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

// profileFor resolves which admission boundary a request's Host selects. The
// loopback listener is matched first so a tailnet opt-in can never shadow it.
func (s *Security) profileFor(r *http.Request) admissionProfile {
	if _, ok := s.allowedHosts[canonicalHost(r.Host)]; ok {
		return profileLoopback
	}
	if s.tailnet != nil {
		if _, ok := s.tailnet.hosts[strings.ToLower(r.Host)]; ok {
			return profileTailnet
		}
	}
	return profileNone
}

func (s *Security) allowsRequest(r *http.Request, profile admissionProfile) bool {
	switch profile {
	case profileLoopback:
		return s.allowsLoopbackRequest(r)
	case profileTailnet:
		return s.allowsTailnetRequest(r)
	default:
		return false
	}
}

// allowsLoopbackRequest is the unchanged loopback boundary: an allowed Host, no
// cross-site fetch, and an Origin (when present) that is the same http origin.
func (s *Security) allowsLoopbackRequest(r *http.Request) bool {
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

// allowsTailnetRequest authenticates a request proxied by `tailscale serve`.
// Every condition must hold: tailscaled strips inbound copies of the identity
// headers and re-sets them, so their presence is only meaningful for traffic it
// actually proxied to this loopback listener.
func (s *Security) allowsTailnetRequest(r *http.Request) bool {
	if !isDeskPath(r.URL.Path) {
		return false
	}
	// Funnel exposes a node publicly. Serve marks those requests, and does not
	// populate identity headers for them, so reject them outright.
	if r.Header.Get("Tailscale-Funnel-Request") != "" {
		return false
	}
	if !strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		return false
	}
	// A present fetch-metadata header must describe a same-origin request. This
	// is stricter than the loopback rule: sibling MagicDNS names in the same
	// tailnet are same-site, so rejecting only "cross-site" would admit them.
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
	default:
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		parsed, err := url.Parse(origin)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
			return false
		}
		if _, ok := s.tailnet.hosts[strings.ToLower(parsed.Host)]; !ok {
			return false
		}
	}
	// Tagged devices send no login, so an empty value is always a rejection.
	login := r.Header.Get("Tailscale-User-Login")
	if login == "" {
		return false
	}
	if !s.tailnet.allowsLogin(login) {
		if s.onRejectedLogin != nil {
			s.onRejectedLogin(sanitizeLogin(login))
		}
		return false
	}
	return true
}

// allowsLogin reports whether login is allowlisted, comparing against every
// entry without an early exit.
func (a *tailnetAdmission) allowsLogin(login string) bool {
	candidate := []byte(strings.ToLower(login))
	matched := 0
	for _, allowed := range a.logins {
		matched |= subtle.ConstantTimeCompare(candidate, []byte(allowed))
	}
	return matched == 1
}

// isDeskPath reports whether a cleaned request path addresses Desk. Wrap runs
// ahead of the mux, so the path is cleaned here to stop traversal reaching
// /status or /healthz through the tailnet boundary.
func isDeskPath(requestPath string) bool {
	cleaned := path.Clean("/" + strings.TrimPrefix(requestPath, "/"))
	if cleaned == "/desk" {
		return true
	}
	return strings.HasPrefix(cleaned, "/desk/") || strings.HasPrefix(cleaned, "/api/v1/desk/")
}

// sanitizeLogin bounds and de-escapes an attacker-controlled header value so it
// cannot inject structure into a log record.
func sanitizeLogin(login string) string {
	const maxLoginLogBytes = 128
	if len(login) > maxLoginLogBytes {
		login = login[:maxLoginLogBytes]
	}
	return strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f {
			return -1
		}
		return r
	}, login)
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
