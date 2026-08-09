// Package workspace implements repo workspaces (docs/plan.md, "Repo
// workspaces"): "work on owner/repo" becomes a container + named volume
// dedicated to that repository, bound to a session. The container never
// holds a durable credential — git auth goes through `waffle
// git-credential` to the host broker.
package workspace

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	neturl "net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/matt-riley/waffle/internal/hooks"
	"github.com/matt-riley/waffle/internal/id"
	"github.com/matt-riley/waffle/internal/repopolicy"
	"github.com/matt-riley/waffle/internal/sandbox"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/store"
	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// ErrWorkspaceNotFound is returned by lookups (Get, ForRepo) when no
// matching workspace exists. Callers must distinguish it from transient
// DB errors (e.g. SQLITE_BUSY) so they don't treat a flaky read as
// "no workspace" and churn duplicate containers/volumes.
var ErrWorkspaceNotFound = errors.New("workspace not found")

// ErrWorkspaceAlreadyClosed distinguishes an inspection that observed a
// closed row from a clean, closeable workspace.
var ErrWorkspaceAlreadyClosed = errors.New("workspace already closed")

type lifecycleLockRegistry struct {
	mu      sync.Mutex
	entries map[string]*lifecycleLockEntry
}

type lifecycleLockEntry struct {
	mu   sync.Mutex
	refs int
}

type lifecycleLock struct {
	registry *lifecycleLockRegistry
	key      string
	entry    *lifecycleLockEntry
}

// workspaceLifecycleLocks coordinates lifecycle transitions across Manager
// instances in this process. Its references count both current holders and
// waiters so an entry cannot be retired during lock handoff.
var workspaceLifecycleLocks lifecycleLockRegistry

func (r *lifecycleLockRegistry) lock(key string) *lifecycleLock {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entries == nil {
		r.entries = make(map[string]*lifecycleLockEntry)
	}
	entry := r.entries[key]
	if entry == nil {
		entry = &lifecycleLockEntry{}
		r.entries[key] = entry
	}
	entry.refs++
	return &lifecycleLock{registry: r, key: key, entry: entry}
}

func (l *lifecycleLock) Lock() {
	l.entry.mu.Lock()
}

func (l *lifecycleLock) Unlock() {
	l.entry.mu.Unlock()

	l.registry.mu.Lock()
	defer l.registry.mu.Unlock()
	l.entry.refs--
	if l.entry.refs == 0 {
		delete(l.registry.entries, l.key)
	}
}

// Workspace is one repo workspace.
type Workspace struct {
	ID         string
	Repo       string // owner/name
	URL        string
	Image      string
	Container  string
	Volume     string
	SessionID  string
	Status     string // open | idle | closed | failed
	CreatedAt  time.Time
	UpdatedAt  time.Time
	LastActive time.Time
	// Profile is an optional named agent profile for runs in this workspace (#71).
	// Empty uses the caller's default (main / chat --profile).
	Profile string
	// HookLog accumulates hook stdout/stderr for session debuggability (#54).
	HookLog []hooks.Result
}

// Statuses.
const (
	StatusOpen   = "open"
	StatusIdle   = "idle"
	StatusClosed = "closed"
	StatusFailed = "failed"
)

// Manager owns workspace lifecycle.
type Manager struct {
	DB       *sql.DB
	Sessions *session.Store
	Runtime  Runtime
	// QueueRoot is where per-workspace queue dirs live.
	QueueRoot string
	// DefaultImage when the repo has no devcontainer (or before we can
	// look).
	DefaultImage string
	// Network for workspace containers when egress is full; default bridge.
	Network string
	// Egress is none (default), allowlist, or full.
	// none/allowlist attach to WorkspaceBrokerNetwork so the host broker is
	// reachable via waffle-host:host-gateway (Docker network mode "none"
	// cannot reach the host). full uses Network/bridge for open egress.
	// allowlist (and none, when ProxyURL is set) point HTTP(S)_PROXY at the
	// host broker; none with an empty broker allowlist denies non-broker HTTPS.
	Egress          string
	EgressAllowlist []string
	ProxyURL        string
	// RunnerBinary is a linux build of waffle to bind-mount as the
	// container entrypoint; empty uses the running binary (linux hosts only).
	RunnerBinary string
	Memory       string
	CPUs         float64
	PIDs         int
	Disk         string
	// BrokerURL as reachable from inside containers, plus a token minter.
	BrokerURL     string
	MintToken     func(ctx context.Context, sessionID string) (string, error)
	RevokeSession func(sessionID string)
	// BindGitScope records, before the initial clone, which repo a session's
	// git credentials are scoped to. The workspaces row is written only after
	// the clone succeeds, so the broker needs this earlier binding to avoid
	// refusing the clone's own credential request.
	BindGitScope func(sessionID, repo string)
	// AllowGitHost, when set, adds host to the broker egress allowlist so
	// git clone/fetch through HTTP_PROXY works under egress=none (which
	// otherwise denies all hosts). Called with the repo URL host at open (#95).
	AllowGitHost func(host string)
	// ExecTimeout bounds one in-container command.
	ExecTimeout time.Duration
	// Hooks are host-configured lifecycle commands (#54). Merged with
	// repo-declared hooks from WAFFLE.md when present.
	Hooks hooks.Config
	// IdleTimeout is the host idle duration; repo policy may only shorten it (#53).
	IdleTimeout time.Duration
	// InspectionProbeInterval is how often an idle-workspace inspection polls
	// for a fresh runner heartbeat. Zero uses inspectionRunnerProbeInterval;
	// tests shorten it to drive the state machine without wall-clock waits
	// (same pattern as Reaper's Now and Client's detection-window fields).
	InspectionProbeInterval time.Duration
	// PolicyCache reloads host-side WAFFLE.md by mtime when Root is set (#53).
	// Container workspaces load policy via cat /work/repo; this is for tests
	// and host-path binds.
	PolicyCache *repopolicy.Cache
	// lastPolicy is the most recently applied repo policy for this manager
	// (inspectable in tests; chat/intake also receive the Policy return value).
	lastPolicy *repopolicy.Policy

	// activityMu guards activity, the in-process fallback record of when a
	// workspace was last used. Entries exist only while last_active is known
	// to be stale, because its write failed (#260).
	activityMu sync.Mutex
	activity   map[string]time.Time

	// ensureOnce ensures the active-repo index is created only once per
	// Manager (process lifetime) to avoid repeated DDL in hot path.
	ensureOnce sync.Once
	// ensureErr holds any error from the one-time index creation so that
	// subsequent calls continue to surface the failure.
	ensureErr error
}

// NewManager wires a Manager with defaults.
func NewManager(st *store.Store, sessions *session.Store, rt Runtime, queueRoot string) *Manager {
	return &Manager{
		DB:           st.DB,
		Sessions:     sessions,
		Runtime:      rt,
		QueueRoot:    queueRoot,
		DefaultImage: "buildpack-deps:bookworm-scm",
		Network:      "bridge",
		ExecTimeout:  10 * time.Minute,
	}
}

var repoSegmentRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// ValidRepository reports whether repository has the exact "owner/name"
// shape accepted throughout workspace and Desk requests: two non-empty
// segments, neither "." nor "..", drawn from a conservative character set.
func ValidRepository(repository string) bool {
	if len(repository) == 0 || len(repository) > 255 || strings.TrimSpace(repository) != repository {
		return false
	}
	parts := strings.Split(repository, "/")
	if len(parts) != 2 {
		return false
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || !repoSegmentRE.MatchString(part) {
			return false
		}
	}
	return true
}

// normalizeRepo accepts "owner/name" or a full https URL.
// gitHostFromURL returns the lowercase hostname of an https git URL, or "".
func gitHostFromURL(raw string) string {
	u, err := neturl.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

func normalizeRepo(arg string) (repo, url string, err error) {
	arg = strings.TrimSuffix(strings.TrimSpace(arg), ".git")
	if ValidRepository(arg) {
		return arg, "https://github.com/" + arg + ".git", nil
	}
	if strings.HasPrefix(arg, "https://") {
		u, err := neturl.Parse(arg)
		if err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" {
			repoPath := strings.Trim(u.Path, "/")
			if ValidRepository(repoPath) {
				return repoPath, "https://" + u.Host + "/" + repoPath + ".git", nil
			}
		}
	}
	return "", "", fmt.Errorf("can't parse repo %q (want owner/name or an https URL)", arg)
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// ensureActiveRepoIndex installs (idempotently) a partial UNIQUE index so
// that concurrent Open calls for the same repo cannot both succeed in
// inserting a non-closed row. This fixes the TOCTOU in check-then-create.
// It runs only once per Manager lifetime (see ensureOnce) to avoid repeated
// DDL on every Open; new DBs get it from migrations.
func (m *Manager) ensureActiveRepoIndex(ctx context.Context) error {
	m.ensureOnce.Do(func() {
		if _, e := m.DB.ExecContext(ctx, `
			CREATE UNIQUE INDEX IF NOT EXISTS idx_workspaces_repo_active
			ON workspaces(repo) WHERE status != 'closed'
		`); e != nil {
			m.ensureErr = fmt.Errorf("ensure active repo index: %w", e)
		}
	})
	return m.ensureErr
}

// Open creates a workspace for repoArg: container + volume + session, repo
// cloned inside via the broker-backed credential helper. If a workspace
// for the repo already exists (open or idle), it is resumed instead.
func (m *Manager) Open(ctx context.Context, repoArg string) (*Workspace, *sandbox.Client, error) {
	return m.OpenWithProfile(ctx, repoArg, "")
}

// OpenWithProfile is Open with an optional named agent profile (#71).
// profile is stored on new workspaces; resume preserves any existing bind.
// Repo-owned WAFFLE.md cannot set or widen this profile.
func (m *Manager) OpenWithProfile(ctx context.Context, repoArg, profile string) (*Workspace, *sandbox.Client, error) {
	repo, url, err := normalizeRepo(repoArg)
	if err != nil {
		return nil, nil, err
	}
	if err := m.ensureActiveRepoIndex(ctx); err != nil {
		return nil, nil, err
	}
	switch existing, err := m.ForRepo(ctx, repo); {
	case err == nil:
		return m.Resume(ctx, existing.ID)
	case !errors.Is(err, ErrWorkspaceNotFound):
		// A transient DB error (e.g. SQLITE_BUSY) is not "no workspace";
		// creating a fresh one here would hit the partial unique index or
		// churn a duplicate container/volume.
		return nil, nil, fmt.Errorf("look up workspace for %s: %w", repo, err)
	}

	sess, err := m.Sessions.Create(ctx, "workspace "+repo)
	if err != nil {
		return nil, nil, err
	}
	wsID, err := id.New("ws-")
	if err != nil {
		return nil, nil, fmt.Errorf("new workspace id: %w", err)
	}
	ws := &Workspace{
		ID:         wsID,
		Repo:       repo,
		URL:        url,
		Image:      m.DefaultImage,
		SessionID:  sess.ID,
		Status:     StatusOpen,
		LastActive: time.Now().UTC(),
		Profile:    strings.TrimSpace(profile),
	}
	ws.Container = "waffle-" + ws.ID
	ws.Volume = "waffle-" + ws.ID

	token := ""
	if m.MintToken != nil {
		token, err = m.MintToken(ctx, sess.ID)
		if err != nil {
			return nil, nil, fmt.Errorf("mint token for workspace: %w", err)
		}
	}
	// Scope git credentials to this repo before the clone runs; the durable
	// workspaces row is written only after setup succeeds (revoked on any
	// failure below via revokeSession, which also clears this binding).
	if m.BindGitScope != nil {
		m.BindGitScope(sess.ID, repo)
	}
	// Under none (and when proxy is used), allow this repo's git host through
	// the broker so clone succeeds while other hosts stay denied (#95).
	if m.AllowGitHost != nil {
		if host := gitHostFromURL(url); host != "" {
			m.AllowGitHost(host)
		}
	}
	if err := m.Runtime.StartWorkspace(ctx, m.containerOpts(ws, token)); err != nil {
		m.revokeSession(sess.ID)
		return nil, nil, err
	}
	client, err := m.newClient(ws)
	if err != nil {
		m.revokeSession(sess.ID)
		return nil, nil, err
	}

	if err := m.setup(ctx, client, ws); err != nil {
		_ = client.Close()
		_ = m.Runtime.RemoveContainer(ctx, ws.Container)
		_ = m.Runtime.RemoveVolume(ctx, ws.Volume)
		m.revokeSession(sess.ID)
		return nil, nil, err
	}

	// devcontainer adoption: if the repo names an image, restart the
	// container on it. The volume and queue survive; the runner resumes.
	if img := m.devcontainerImage(ctx, client, ws); img != "" && img != ws.Image {
		if err := m.Runtime.RemoveContainer(ctx, ws.Container); err == nil {
			ws.Image = img
			if err := m.Runtime.StartWorkspace(ctx, m.containerOpts(ws, token)); err != nil {
				// The original container is already gone; mirror the
				// setup-failure cleanup so a failed adoption does not leak
				// the volume, queue state, or broker session token (#283).
				_ = client.Close()
				_ = m.Runtime.RemoveVolume(ctx, ws.Volume)
				m.revokeSession(sess.ID)
				return nil, nil, fmt.Errorf("adopt devcontainer image %q: %w", img, err)
			}
		}
	}

	// Repo policy: present-but-unparsable is fatal at open (#53). Applies
	// tighten-only egress/idle/hooks before after_create runs.
	prevEgress := m.Egress
	if _, err := m.loadAndApplyRepoPolicy(ctx, client); err != nil {
		_ = client.Close()
		_ = m.Runtime.RemoveContainer(ctx, ws.Container)
		_ = m.Runtime.RemoveVolume(ctx, ws.Volume)
		m.revokeSession(sess.ID)
		return nil, nil, err
	}
	// If policy tightened egress after the clone, restart the container so
	// the running network posture matches (clone may have needed bridge).
	if egressNetwork(prevEgress) != egressNetwork(m.Egress) {
		_ = client.Close()
		if err := m.Runtime.RemoveContainer(ctx, ws.Container); err != nil {
			_ = m.Runtime.RemoveVolume(ctx, ws.Volume)
			m.revokeSession(sess.ID)
			return nil, nil, fmt.Errorf("restart for policy egress: %w", err)
		}
		if err := m.Runtime.StartWorkspace(ctx, m.containerOpts(ws, token)); err != nil {
			_ = m.Runtime.RemoveVolume(ctx, ws.Volume)
			m.revokeSession(sess.ID)
			return nil, nil, fmt.Errorf("restart for policy egress: %w", err)
		}
		client, err = m.newClient(ws)
		if err != nil {
			_ = m.Runtime.RemoveContainer(ctx, ws.Container)
			_ = m.Runtime.RemoveVolume(ctx, ws.Volume)
			m.revokeSession(sess.ID)
			return nil, nil, err
		}
	}

	// after_create hooks run inside the container; failure marks the
	// workspace failed and refuses to hand it out as usable (#54).
	if res, err := m.runHook(ctx, client, hooks.AfterCreate, ws.ID, ws.SessionID); err != nil {
		ws.Status = StatusFailed
		ws.HookLog = append(ws.HookLog, res)
		_ = client.Close()
		_ = m.Runtime.RemoveContainer(ctx, ws.Container)
		_ = m.Runtime.RemoveVolume(ctx, ws.Volume)
		m.revokeSession(sess.ID)
		return nil, nil, err
	} else if res.Output != "" || res.Err != nil {
		ws.HookLog = append(ws.HookLog, res)
	}

	if _, err := m.DB.ExecContext(ctx, `
		INSERT INTO workspaces (id, repo, url, image, container, volume, session_id, status, created_at, updated_at, last_active, profile)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ws.ID, ws.Repo, ws.URL, ws.Image, ws.Container, ws.Volume, ws.SessionID, ws.Status, now(), now(), ws.LastActive.UTC().Format(time.RFC3339Nano), ws.Profile); err != nil {
		// Concurrent Open raced us (or other insert error); clean up our
		// side effects.
		_ = client.Close()
		_ = m.Runtime.RemoveContainer(ctx, ws.Container)
		_ = m.Runtime.RemoveVolume(ctx, ws.Volume)
		m.revokeSession(ws.SessionID)
		if isUniqueConstraintError(err) {
			// Expected race on the active-repo index: resume the winner.
			if existing, err2 := m.ForRepo(ctx, repo); err2 == nil {
				return m.Resume(ctx, existing.ID)
			}
		}
		// Real DB error (disk full, locked, schema, etc.) or no winner found:
		// return original error after cleanup.
		return nil, nil, err
	}
	return ws, client, nil
}

// WorkspaceBrokerNetwork is the user-defined Docker bridge used by
// none/allowlist workspace containers so --add-host waffle-host:host-gateway
// can reach the host credential broker (#95). Docker network mode "none"
// has no host route.
const WorkspaceBrokerNetwork = "waffle-ws"

// egressNetwork is the docker network mode implied by an egress posture.
// none/allowlist use a dedicated bridge (not Docker "none") so the host
// broker remains reachable; full uses the default bridge for open egress.
func egressNetwork(egress string) string {
	switch egress {
	case "full":
		return "bridge"
	default: // "", "none", "allowlist"
		return WorkspaceBrokerNetwork
	}
}

func (m *Manager) containerOpts(ws *Workspace, token string) ContainerOpts {
	egress := m.Egress
	if egress == "" {
		egress = "none"
	}
	network := egressNetwork(egress)
	if egress == "full" && m.Network != "" {
		network = m.Network
	}
	proxy := ""
	// allowlist routes HTTP(S) through the broker proxy. none also points
	// proxy-aware apps at the broker so non-broker HTTPS fails under an
	// empty/deny-all allowlist; raw TCP on the bridge may still reach the
	// internet — see docs/deploy.md.
	if egress == "allowlist" || egress == "none" {
		proxy = m.ProxyURL
		if proxy == "" && m.BrokerURL != "" {
			proxy = strings.TrimRight(m.BrokerURL, "/") + "/egress"
		}
	}
	return ContainerOpts{
		Name:        ws.Container,
		Image:       ws.Image,
		Volume:      ws.Volume,
		QueueDir:    m.queueDir(ws.ID),
		Network:     network,
		BrokerURL:   m.BrokerURL,
		Token:       token,
		SelfPath:    m.RunnerBinary,
		Memory:      m.Memory,
		CPUs:        m.CPUs,
		PIDs:        m.PIDs,
		Disk:        m.Disk,
		ProxyURL:    proxy,
		ProxyToken:  token,
		NetLockdown: egress == "none" || egress == "allowlist",
	}
}

func (m *Manager) queueDir(id string) string { return filepath.Join(m.QueueRoot, id) }

// setup configures git and clones the repo inside the container.
func (m *Manager) setup(ctx context.Context, client *sandbox.Client, ws *Workspace) error {
	steps := []string{
		"git config --global credential.helper '!waffle git-credential'",
		// Git omits the repository path from credential requests unless this is
		// set, so the helper is asked only for a host. The broker scopes every
		// credential to the one repo a session is bound to and refuses a
		// request whose path does not match -- with the path absent that check
		// can never pass, and the clone fails with "refusing credentials for".
		"git config --global credential.useHttpPath true",
		"git config --global user.name waffle && git config --global user.email waffle@localhost",
		fmt.Sprintf("git clone -- %s /work/repo", shellQuote(ws.URL)),
	}
	for _, cmd := range steps {
		if err := m.bash(ctx, client, cmd); err != nil {
			return fmt.Errorf("workspace setup: %w", err)
		}
	}
	return nil
}

func (m *Manager) bash(ctx context.Context, client *sandbox.Client, cmd string) error {
	ctx, cancel := context.WithTimeout(ctx, m.ExecTimeout)
	defer cancel()
	input, _ := json.Marshal(map[string]any{"command": cmd, "timeout_seconds": 570})
	out, isError, err := client.Exec(ctx, "bash", input)
	if err != nil {
		return err
	}
	if isError {
		return fmt.Errorf("%s: %s", cmd, strings.TrimSpace(out))
	}
	return nil
}

// loadAndApplyRepoPolicy reads WAFFLE.md/AGENT.md from the container (or
// PolicyCache when set), fails on unparsable content, and tightens manager
// egress/idle/hooks (#53). Absent policy leaves manager settings unchanged.
func (m *Manager) loadAndApplyRepoPolicy(ctx context.Context, client *sandbox.Client) (*repopolicy.Policy, error) {
	if m.PolicyCache != nil {
		p, err := m.PolicyCache.Get()
		if err != nil {
			return nil, fmt.Errorf("repo policy: %w", err)
		}
		if p != nil {
			m.applyPolicy(p)
		}
		return p, nil
	}
	if client == nil {
		return nil, nil
	}
	raw, err := m.bashOutput(ctx, client, "cat /work/repo/WAFFLE.md 2>/dev/null || cat /work/repo/AGENT.md 2>/dev/null || true")
	if err != nil {
		return nil, nil // best-effort read; missing file is fine
	}
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	p, err := repopolicy.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("repo policy: %w", err)
	}
	m.applyPolicy(p)
	return p, nil
}

// applyPolicy tightens host egress/idle and merges hooks from repo policy.
// Tool allow/deny is applied by chat/intake callers (they own the tool policy).
func (m *Manager) applyPolicy(p *repopolicy.Policy) {
	if p == nil {
		return
	}
	m.lastPolicy = p
	if p.Egress != "" {
		m.Egress = repopolicy.TightenEgress(m.Egress, p.Egress)
	}
	if p.IdleTimeout != "" {
		if d, err := time.ParseDuration(p.IdleTimeout); err == nil {
			m.IdleTimeout = repopolicy.TightenIdle(m.IdleTimeout, d)
		}
	}
	repo := hooks.Config{
		AfterCreate:  p.Hooks.AfterCreate,
		BeforeRun:    p.Hooks.BeforeRun,
		AfterRun:     p.Hooks.AfterRun,
		BeforeRemove: p.Hooks.BeforeRemove,
	}
	if p.Hooks.Timeout != "" {
		if d, err := time.ParseDuration(p.Hooks.Timeout); err == nil {
			repo.Timeout = d
		}
	}
	m.Hooks = hooks.Merge(m.Hooks, repo)
}

// LastPolicy returns the most recently applied repo policy, if any.
func (m *Manager) LastPolicy() *repopolicy.Policy { return m.lastPolicy }

// LoadRepoPolicy is the public entry for chat /repo and intake to load policy
// from an open workspace client (or PolicyCache).
func (m *Manager) LoadRepoPolicy(ctx context.Context, client *sandbox.Client) (*repopolicy.Policy, error) {
	return m.loadAndApplyRepoPolicy(ctx, client)
}

// hookConfig merges host hooks with a repo-declared WAFFLE.md/AGENT.md, if readable
// from the container at /work/repo. After loadAndApplyRepoPolicy, m.Hooks already
// includes repo hooks; still re-read so Resume/Close paths stay current.
func (m *Manager) hookConfig(ctx context.Context, client *sandbox.Client) hooks.Config {
	cfg := m.Hooks
	if client == nil {
		return cfg
	}
	raw, err := m.bashOutput(ctx, client, "cat /work/repo/WAFFLE.md 2>/dev/null || cat /work/repo/AGENT.md 2>/dev/null || true")
	if err != nil || strings.TrimSpace(raw) == "" {
		return cfg
	}
	p, err := repopolicy.Parse(raw)
	if err != nil {
		// Open already failed on unparsable; at hook time keep host hooks only.
		return cfg
	}
	repo := hooks.Config{
		AfterCreate:  p.Hooks.AfterCreate,
		BeforeRun:    p.Hooks.BeforeRun,
		AfterRun:     p.Hooks.AfterRun,
		BeforeRemove: p.Hooks.BeforeRemove,
	}
	if p.Hooks.Timeout != "" {
		if d, err := time.ParseDuration(p.Hooks.Timeout); err == nil {
			repo.Timeout = d
		}
	}
	return hooks.Merge(cfg, repo)
}

// runHook executes one lifecycle hook inside the workspace container and
// persists stdout/stderr to hook_logs for session debuggability (#54).
func (m *Manager) runHook(ctx context.Context, client *sandbox.Client, point hooks.Point, workspaceID, sessionID string) (hooks.Result, error) {
	cfg := m.hookConfig(ctx, client)
	ex := hooks.ClientExecutor{Exec: func(ctx context.Context, name string, input json.RawMessage) (string, bool, error) {
		return client.Exec(ctx, name, input)
	}}
	res := hooks.Run(ctx, ex, cfg, point)
	m.persistHookLog(ctx, workspaceID, sessionID, res)
	if res.Err != nil && hooks.Fatal(point) {
		return res, res.Err
	}
	return res, nil
}

func (m *Manager) persistHookLog(ctx context.Context, workspaceID, sessionID string, res hooks.Result) {
	if m.DB == nil || (res.Output == "" && res.Err == nil) {
		return
	}
	hid, err := id.New("hook-")
	if err != nil {
		return
	}
	errText := ""
	if res.Err != nil {
		errText = res.Err.Error()
	}
	_, _ = m.DB.ExecContext(ctx, `
		INSERT INTO hook_logs (id, workspace_id, session_id, point, output, error, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		hid, workspaceID, sessionID, string(res.Point), res.Output, errText, now())
}

// RunHook is the public entry for before_run / after_run during issue intake.
func (m *Manager) RunHook(ctx context.Context, client *sandbox.Client, point hooks.Point) (hooks.Result, error) {
	return m.runHook(ctx, client, point, "", "")
}

// RunHookFor records hook output against a workspace/session (#54).
func (m *Manager) RunHookFor(ctx context.Context, client *sandbox.Client, point hooks.Point, workspaceID, sessionID string) (hooks.Result, error) {
	return m.runHook(ctx, client, point, workspaceID, sessionID)
}

func (m *Manager) bashOutput(ctx context.Context, client *sandbox.Client, cmd string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, m.ExecTimeout)
	defer cancel()
	input, _ := json.Marshal(map[string]any{"command": cmd})
	out, isError, err := client.Exec(ctx, "bash", input)
	if err != nil {
		return "", err
	}
	if isError {
		return "", fmt.Errorf("%s: %s", cmd, strings.TrimSpace(out))
	}
	return strings.TrimSpace(out), nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// isMissingFileError reports whether err represents an expected
// "file not found" during best-effort container inspection (e.g. no
// .devcontainer.json). Other errors are real failures (permissions,
// I/O, etc).
func isMissingFileError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	// Strict match for expected "file not found" from cat/exec in the
	// container (e.g. no .devcontainer.json). Broad "not found" can
	// hide real errors like "command not found" or permission denied.
	return strings.Contains(msg, "no such file or directory") ||
		(strings.Contains(msg, "cat:") && strings.Contains(msg, "no such file or directory"))
}

// isUniqueConstraintError reports whether err is a SQLite UNIQUE
// constraint violation (used to detect concurrent Open races on the
// partial active-repo index and resume the winner). It checks the
// driver's typed error code first (SQLITE_CONSTRAINT_UNIQUE) so we
// don't depend on driver error-message wording, with a message
// substring check only as a fallback for wrapped or non-typed errors.
func isUniqueConstraintError(err error) bool {
	if err == nil {
		return false
	}
	var se *sqlite.Error
	if errors.As(err, &se) {
		return se.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE
	}
	return strings.Contains(strings.ToLower(err.Error()), "unique constraint failed")
}

// devcontainerImage reads .devcontainer/devcontainer.json from the cloned
// repo and returns its "image", or "". It now uses bashOutput so that
// "no such file" (ENOENT) can be distinguished from other errors.
func (m *Manager) devcontainerImage(ctx context.Context, client *sandbox.Client, ws *Workspace) string {
	raw, err := m.bashOutput(ctx, client, "cat /work/repo/.devcontainer/devcontainer.json")
	if err != nil {
		// expected "no .devcontainer" vs unexpected exec error
		if isMissingFileError(err) {
			return ""
		}
		// other error (e.g. permission, container problem): treat as no image
		// (best-effort path; would log at debug if logger were attached)
		return ""
	}
	var dc struct {
		Image string `json:"image"`
	}
	if err := json.Unmarshal([]byte(raw), &dc); err != nil {
		return ""
	}
	return dc.Image
}

// Idle stops the container; the volume (and queue) persist.
func (m *Manager) Idle(ctx context.Context, id string) error {
	lock := workspaceLifecycleLock(id)
	lock.Lock()
	defer lock.Unlock()
	return m.idle(ctx, id)
}

func (m *Manager) idle(ctx context.Context, id string) error {
	ws, err := m.Get(ctx, id)
	if err != nil {
		return err
	}
	if ws.Status != StatusOpen {
		return fmt.Errorf("workspace %s is %s, not open", id, ws.Status)
	}
	if err := m.Runtime.StopContainer(ctx, ws.Container); err != nil {
		return err
	}
	m.revokeSession(ws.SessionID)
	// The container is stopped, so in-process activity no longer protects it.
	m.forgetActivity(id)
	return m.setStatus(ctx, id, StatusIdle)
}

// Resume restarts an idle workspace's container and reconnects the queue.
func (m *Manager) Resume(ctx context.Context, id string) (*Workspace, *sandbox.Client, error) {
	lock := workspaceLifecycleLock(id)
	lock.Lock()
	defer lock.Unlock()
	return m.resume(ctx, id)
}

func (m *Manager) resume(ctx context.Context, id string) (*Workspace, *sandbox.Client, error) {
	ws, err := m.Get(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if ws.Status == StatusClosed {
		return nil, nil, fmt.Errorf("workspace %s is closed", id)
	}

	if m.MintToken != nil {
		token, err := m.MintToken(ctx, ws.SessionID)
		if err != nil {
			return nil, nil, fmt.Errorf("mint token for resume: %w", err)
		}
		if err := m.Runtime.RemoveContainer(ctx, ws.Container); err != nil {
			return nil, nil, fmt.Errorf("replace workspace container: %w", err)
		}
		if err := m.Runtime.StartWorkspace(ctx, m.containerOpts(ws, token)); err != nil {
			_ = m.setStatus(ctx, id, StatusIdle)
			return nil, nil, fmt.Errorf("restart workspace with refreshed credentials: %w", err)
		}
		if err := m.setStatus(ctx, id, StatusOpen); err != nil {
			return nil, nil, err
		}
		ws.Status = StatusOpen
	} else if ws.Status == StatusIdle {
		if err := m.Runtime.StartContainer(ctx, ws.Container); err != nil {
			return nil, nil, err
		}
		if err := m.setStatus(ctx, id, StatusOpen); err != nil {
			return nil, nil, err
		}
		ws.Status = StatusOpen
	}
	client, err := m.newClient(ws)
	if err != nil {
		return nil, nil, err
	}
	// Re-load repo policy on resume so mtime changes apply without serve
	// restart (#53). Unparsable remains fatal.
	if _, err := m.loadAndApplyRepoPolicy(ctx, client); err != nil {
		_ = client.Close()
		return nil, nil, err
	}
	if err := m.Touch(ctx, id); err != nil {
		_ = client.Close()
		return nil, nil, err
	}
	return ws, client, nil
}

// Touch records activity so serve-owned lifecycle sweeps can distinguish an
// active workspace from one that has been forgotten.
func (m *Manager) Touch(ctx context.Context, id string) error {
	_, err := m.DB.ExecContext(ctx, `UPDATE workspaces SET last_active = ?, updated_at = ? WHERE id = ?`, now(), now(), id)
	return err
}

func (m *Manager) newClient(ws *Workspace) (*sandbox.Client, error) {
	client, err := sandbox.NewClient(m.queueDir(ws.ID))
	if err != nil {
		return nil, err
	}
	client.OnActivity = func() { m.noteActivity(context.Background(), ws.ID, ws.Repo) }
	return client, nil
}

// noteActivity records a workspace command. A Touch that fails — SQLite busy,
// a closed handle during a shutdown race — used to be discarded silently, and
// the stored last_active then aged past the idle timeout until the next sweep
// stopped the container in the middle of real work (#260). The failure is now
// logged, and the activity is kept in memory so the sweep can tell "not used"
// apart from "used, but the write failed".
func (m *Manager) noteActivity(ctx context.Context, id, repo string) {
	// Callbacks for one workspace can overlap, and they do not finish in the
	// order they started. Both updates below are keyed on when this activity
	// happened, so a slower older write can never overwrite a newer one.
	at := time.Now().UTC()
	if err := m.Touch(ctx, id); err != nil {
		m.recordActivity(id, at)
		slog.Default().WarnContext(ctx, "workspace activity write failed; idle sweeps fall back to in-process activity",
			"workspace", id, "repo", repo, "err", err)
		return
	}
	// last_active covers this activity, so the fallback for it is no longer
	// needed — but a later activity whose write failed keeps its own.
	m.clearActivityUpTo(id, at)
}

// recordActivity remembers activity whose last_active write failed, keeping
// the newest of any overlapping callbacks.
func (m *Manager) recordActivity(id string, at time.Time) {
	m.activityMu.Lock()
	defer m.activityMu.Unlock()
	if m.activity == nil {
		m.activity = make(map[string]time.Time)
	}
	if existing, ok := m.activity[id]; ok && existing.After(at) {
		return
	}
	m.activity[id] = at
}

// clearActivityUpTo drops the fallback only when a successful write covers it.
// A record newer than at belongs to activity this write did not persist, so
// erasing it would let the reaper idle a workspace that is still in use.
func (m *Manager) clearActivityUpTo(id string, at time.Time) {
	m.activityMu.Lock()
	defer m.activityMu.Unlock()
	if existing, ok := m.activity[id]; ok && existing.After(at) {
		return
	}
	delete(m.activity, id)
}

// ActiveSince reports whether this process used the workspace at or after
// since despite failing to record it. It implements ActivityProbe so a sweep
// can corroborate a stale last_active before stopping a container.
func (m *Manager) ActiveSince(id string, since time.Time) bool {
	if m == nil {
		return false
	}
	m.activityMu.Lock()
	defer m.activityMu.Unlock()
	last, ok := m.activity[id]
	return ok && !last.Before(since)
}

// forgetActivity drops the in-process record for a workspace whose container
// is gone (idled or closed): there is nothing left to protect from the reaper,
// and the entry would otherwise outlive the workspace it describes.
func (m *Manager) forgetActivity(id string) {
	m.activityMu.Lock()
	delete(m.activity, id)
	m.activityMu.Unlock()
}

// newInspectionClient connects to a workspace without recording activity.
// Inspection must not make an open or idle workspace look recently used.
func (m *Manager) newInspectionClient(ws *Workspace) (*sandbox.Client, error) {
	return sandbox.NewClient(m.queueDir(ws.ID))
}

const (
	// inspectionRunnerReadyTimeout matches sandbox's supported 60-second
	// cold-start allowance for the first runner heartbeat.
	inspectionRunnerReadyTimeout  = time.Minute
	inspectionRunnerProbeInterval = 100 * time.Millisecond
	inspectionRestoreTimeout      = 30 * time.Second
)

// waitForInspectionRunner waits for a heartbeat created after an idle
// container restart. The queue persists across idle periods, so an old stale
// heartbeat must not make the first inspection command declare the newly
// started runner dead.
func (m *Manager) waitForInspectionRunner(ctx context.Context, ws *Workspace, startedAt time.Time) error {
	// Open through the queue's own opener: a bare sql.Open inherits no
	// busy_timeout, so reading this file while the runner is mid-write returns
	// SQLITE_BUSY immediately instead of waiting.
	db, err := sandbox.OpenQueueReader(filepath.Join(m.queueDir(ws.ID), "outbound.db"))
	if err != nil {
		return fmt.Errorf("open inspection runner heartbeat: %w", err)
	}
	defer func() { _ = db.Close() }()

	tickerInterval := m.InspectionProbeInterval
	if tickerInterval <= 0 {
		tickerInterval = inspectionRunnerProbeInterval
	}
	ticker := time.NewTicker(tickerInterval)
	defer ticker.Stop()
	return waitForInspectionHeartbeat(ctx, startedAt, inspectionRunnerReadyTimeout,
		func(ctx context.Context) (time.Time, error) {
			var raw string
			err := db.QueryRowContext(ctx,
				`SELECT created_at FROM results WHERE request_id = ?`, -1).Scan(&raw)
			if errors.Is(err, sql.ErrNoRows) {
				return time.Time{}, nil
			}
			if err != nil {
				return time.Time{}, err
			}
			heartbeat, err := time.Parse(time.RFC3339Nano, raw)
			if err != nil {
				return time.Time{}, nil
			}
			return heartbeat, nil
		}, ticker.C)
}

// waitForInspectionHeartbeat contains the bounded wait independently of the
// queue read. Keeping those seams separate makes the supported cold-start
// window deterministic to test without delaying the suite.
func waitForInspectionHeartbeat(ctx context.Context, startedAt time.Time, timeout time.Duration, heartbeat func(context.Context) (time.Time, error), ticks <-chan time.Time) error {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		heartbeatAt, err := heartbeat(waitCtx)
		switch {
		case err == nil:
			if heartbeatAt.After(startedAt) {
				return nil
			}
		case sandbox.IsBusyErr(err):
			// The runner holds the queue mid-write. This loop exists to wait, so
			// contention is "not yet", not a failed inspection: retry until the
			// deadline rather than abandoning a closeable workspace.
		default:
			return fmt.Errorf("read inspection runner heartbeat: %w", err)
		}

		select {
		case <-ticks:
		case <-waitCtx.Done():
			return fmt.Errorf("wait for restarted workspace runner heartbeat: %w", waitCtx.Err())
		}
	}
}

// CloseReport says what Close found before tearing down.
type CloseReport struct {
	Dirty    string // non-empty git status --porcelain output
	Unpushed string // non-empty log of commits ahead of upstream
}

// ErrWorkspaceNotRunning is returned by read-only inspections that refuse to
// start a container implicitly. Idle, failed, and closed workspaces report it
// so a status read can never change lifecycle state.
var ErrWorkspaceNotRunning = errors.New("workspace is not running")

// GitStatus is the read-only git projection for a running workspace. It
// deliberately excludes remotes and paths: only branch, counts, and the last
// commit subject cross this boundary.
type GitStatus struct {
	Branch     string // current branch, or "" when detached
	Detached   bool
	DirtyFiles int
	Tracking   bool // an upstream branch is configured
	Ahead      int
	Behind     int
	CommitSHA  string // abbreviated SHA of HEAD, "" for an empty repository
	Subject    string // subject line of HEAD
}

// gitStatusScript is one read-only shell pass over the repository. Every
// command is a query: nothing fetches, writes refs, or mutates the tree.
const gitStatusScript = `cd /work/repo && ` +
	// symbolic-ref, not rev-parse --abbrev-ref: it still names the branch on a
	// freshly cloned repo with no commits, and fails only when HEAD really is
	// detached.
	`printf 'branch\t%s\n' "$(git symbolic-ref --short HEAD 2>/dev/null || echo HEAD)" && ` +
	`printf 'dirty\t%s\n' "$(git status --porcelain 2>/dev/null | wc -l | tr -d ' ')" && ` +
	`if git rev-parse --abbrev-ref '@{upstream}' >/dev/null 2>&1; then ` +
	`printf 'tracking\t1\n'; printf 'counts\t%s\n' "$(git rev-list --left-right --count '@{upstream}...HEAD' 2>/dev/null)"; ` +
	`else printf 'tracking\t0\n'; fi && ` +
	`printf 'sha\t%s\n' "$(git rev-parse --short HEAD 2>/dev/null || true)" && ` +
	`printf 'subject\t%s\n' "$(git log -1 --format=%s 2>/dev/null || true)"`

// InspectGit reads git state from an already-running workspace. It never
// starts, stops, or transitions a workspace: an idle or closed workspace
// returns ErrWorkspaceNotRunning instead (#181). The per-workspace lifecycle
// lock is held so a status read cannot interleave with a close.
func (m *Manager) InspectGit(ctx context.Context, id string) (*GitStatus, error) {
	lock := workspaceLifecycleLock(id)
	lock.Lock()
	defer lock.Unlock()

	ws, err := m.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if ws.Status != StatusOpen {
		return nil, ErrWorkspaceNotRunning
	}

	client, err := m.newInspectionClient(ws)
	if err != nil {
		return nil, fmt.Errorf("connect workspace for git status: %w", err)
	}
	defer func() { _ = client.Close() }()

	out, err := m.bashOutput(ctx, client, gitStatusScript)
	if err != nil {
		return nil, fmt.Errorf("read workspace git status: %w", err)
	}
	return parseGitStatus(out), nil
}

func parseGitStatus(out string) *GitStatus {
	status := &GitStatus{}
	for _, line := range strings.Split(out, "\n") {
		key, value, found := strings.Cut(strings.TrimRight(line, "\r"), "\t")
		if !found {
			continue
		}
		value = strings.TrimSpace(value)
		switch key {
		case "branch":
			if value == "HEAD" || value == "" {
				status.Detached = true
				continue
			}
			status.Branch = value
		case "dirty":
			status.DirtyFiles, _ = strconv.Atoi(value)
		case "tracking":
			status.Tracking = value == "1"
		case "counts":
			// git rev-list --left-right --count upstream...HEAD prints
			// "<behind>\t<ahead>": left is upstream-only, right is HEAD-only.
			behind, ahead, ok := strings.Cut(value, "\t")
			if !ok {
				behind, ahead, ok = strings.Cut(value, " ")
			}
			if !ok {
				continue
			}
			status.Behind, _ = strconv.Atoi(strings.TrimSpace(behind))
			status.Ahead, _ = strconv.Atoi(strings.TrimSpace(ahead))
		case "sha":
			status.CommitSHA = value
		case "subject":
			status.Subject = value
		}
	}
	return status
}

// InspectClose gathers the evidence Close needs without changing workspace
// lifecycle state. Idle workspaces are temporarily started and restored to
// their stopped state without refreshing credentials or timestamps.
func (m *Manager) InspectClose(ctx context.Context, id string) (report *CloseReport, err error) {
	return m.InspectCloseGuarded(ctx, id, nil)
}

// InspectCloseGuarded keeps the per-workspace lifecycle lock held through
// accept. This lets a caller atomically accept inspection evidence, for
// example by issuing a short-lived confirmation token, before another actor
// can idle, resume, inspect, or close the same workspace. accept must not call
// workspace lifecycle methods for this ID.
func (m *Manager) InspectCloseGuarded(
	ctx context.Context,
	id string,
	accept func(*CloseReport) error,
) (report *CloseReport, err error) {
	lock := workspaceLifecycleLock(id)
	lock.Lock()
	defer lock.Unlock()

	report, err = m.inspectClose(ctx, id)
	if err != nil {
		return report, err
	}
	if accept != nil {
		if err := accept(report); err != nil {
			return report, err
		}
	}
	return report, nil
}

func (m *Manager) inspectClose(ctx context.Context, id string) (report *CloseReport, err error) {
	// Preview and standalone inspection always restore idle workspaces so a
	// read-only check cannot leave containers running.
	return m.inspectCloseOpt(ctx, id, true)
}

// inspectCloseOpt gathers close evidence. When restoreIdle is false and the
// workspace was idle, a clean inspection leaves the container running so a
// subsequent close can reuse that single start (#148). Dirty/error outcomes
// always restore idle when this call started the container.
func (m *Manager) inspectCloseOpt(ctx context.Context, id string, restoreIdle bool) (report *CloseReport, err error) {
	ws, err := m.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	report = &CloseReport{}
	if ws.Status == StatusClosed {
		return report, ErrWorkspaceAlreadyClosed
	}

	wasIdle := ws.Status == StatusIdle
	if wasIdle {
		startedAt := time.Now().UTC()
		if err := m.Runtime.StartContainer(ctx, ws.Container); err != nil {
			return report, fmt.Errorf("start idle workspace for inspection: %w", err)
		}
		// Defer is registered only after a successful start, so restore always
		// applies to a container we started here.
		defer func() {
			// Restore stopped state when the caller asked, or when this
			// inspection cannot hand a live container to teardown.
			shouldRestore := restoreIdle
			if !restoreIdle {
				if err != nil || report.Dirty != "" || report.Unpushed != "" {
					shouldRestore = true
				}
			}
			if !shouldRestore {
				return
			}
			restoreCtx, cancel := context.WithTimeout(context.Background(), inspectionRestoreTimeout)
			defer cancel()
			if restoreErr := m.Runtime.StopContainer(restoreCtx, ws.Container); restoreErr != nil {
				if err != nil {
					err = errors.Join(err, fmt.Errorf("restore idle workspace: %w", restoreErr))
					return
				}
				err = fmt.Errorf("restore idle workspace after inspection: %w", restoreErr)
			}
		}()
		if err := m.waitForInspectionRunner(ctx, ws, startedAt); err != nil {
			return report, err
		}
	}

	client, err := m.newInspectionClient(ws)
	if err != nil {
		return report, fmt.Errorf("connect workspace for inspection: %w", err)
	}
	defer func() { _ = client.Close() }()

	report.Dirty, err = m.bashOutput(ctx, client, "cd /work/repo && git status --porcelain")
	if err != nil {
		return report, fmt.Errorf("inspect workspace before close: %w", err)
	}
	report.Unpushed, err = m.bashOutput(ctx, client, "cd /work/repo && if git rev-parse --abbrev-ref '@{upstream}' >/dev/null 2>&1; then git log --oneline '@{upstream}'..HEAD; else git log --oneline HEAD --not --remotes; fi")
	if err != nil {
		return report, fmt.Errorf("inspect workspace before close: %w", err)
	}
	return report, nil
}

// Close tears the workspace down. When force is false and the tree is
// dirty or has unpushed commits, it refuses and reports instead.
// before_remove runs best-effort after safety checks and before teardown.
func (m *Manager) Close(ctx context.Context, id string, force bool) (*CloseReport, error) {
	report, _, err := m.CloseTransition(ctx, id, force)
	return report, err
}

// CloseTransition closes a workspace and reports whether this invocation
// performed the durable non-closed to closed transition. Existing Close
// callers retain their no-op compatibility for rows already closed.
func (m *Manager) CloseTransition(ctx context.Context, id string, force bool) (*CloseReport, bool, error) {
	lock := workspaceLifecycleLock(id)
	lock.Lock()
	defer lock.Unlock()

	ws, err := m.Get(ctx, id)
	if err != nil {
		return nil, false, err
	}
	if ws.Status == StatusClosed {
		return nil, false, nil
	}

	report := &CloseReport{}
	wasIdle := ws.Status == StatusIdle
	// Idle clean close reuses the single container start from inspection so we
	// do not StartContainer a second time (#148). Trade-off: this path does
	// not call resume/MintToken, so before_remove runs against the existing
	// inspection container credentials (already best-effort; hooks that need
	// a refreshed broker token should not rely on this path).
	idleKeptRunning := false
	if !force {
		report, err = m.inspectCloseOpt(ctx, id, !wasIdle)
		if err != nil {
			return report, false, err
		}
		if report.Dirty != "" || report.Unpushed != "" {
			return report, false, fmt.Errorf("workspace %s has unsaved work (close with force to discard)", id)
		}
		idleKeptRunning = wasIdle
	}

	var client *sandbox.Client
	switch {
	case idleKeptRunning:
		// Inspection already started the idle container and left it running.
		// Prefer reusing it over resume (no second start / no MintToken).
		client, err = m.newInspectionClient(ws)
		if err != nil {
			return report, false, fmt.Errorf("connect workspace for safety check: %w", err)
		}
	case wasIdle:
		resumed, resumedClient, err := m.resume(ctx, id)
		if err != nil {
			if force {
				// Force-close without a live container when resume fails.
				goto teardown
			}
			return report, false, fmt.Errorf("resume workspace for safety check: %w", err)
		}
		ws = resumed
		client = resumedClient
	default:
		client, err = m.newClient(ws)
		if err != nil {
			if force {
				goto teardown
			}
			return report, false, fmt.Errorf("connect workspace for safety check: %w", err)
		}
	}
	defer func() {
		if client != nil {
			_ = client.Close()
		}
	}()

	// before_remove is best-effort and must not block teardown (#54).
	if client != nil {
		_, _ = m.runHook(ctx, client, hooks.BeforeRemove, ws.ID, ws.SessionID)
		_ = client.Close()
		client = nil
	}

teardown:
	if err := m.Runtime.RemoveContainer(ctx, ws.Container); err != nil {
		return report, false, err
	}
	if err := m.Runtime.RemoveVolume(ctx, ws.Volume); err != nil {
		return report, false, err
	}
	m.revokeSession(ws.SessionID)
	// The container and volume are gone, so any fallback activity record for
	// this workspace is now unreachable state.
	m.forgetActivity(id)
	if err := m.setStatus(ctx, id, StatusClosed); err != nil {
		return report, false, err
	}
	return report, true, nil
}

func workspaceLifecycleLock(id string) *lifecycleLock {
	return workspaceLifecycleLocks.lock(id)
}

func (m *Manager) revokeSession(sessionID string) {
	if m.RevokeSession != nil {
		m.RevokeSession(sessionID)
	}
}

func (m *Manager) setStatus(ctx context.Context, id, status string) error {
	_, err := m.DB.ExecContext(ctx,
		`UPDATE workspaces SET status = ?, updated_at = ? WHERE id = ?`, status, now(), id)
	return err
}

// Get loads one workspace.
func (m *Manager) Get(ctx context.Context, id string) (*Workspace, error) {
	return m.scanOne(m.DB.QueryRowContext(ctx, `
		SELECT id, repo, url, image, container, volume, session_id, status, created_at, updated_at, last_active, profile
		FROM workspaces WHERE id = ?`, id))
}

// ForRepo finds the non-closed, non-failed workspace for a repo.
func (m *Manager) ForRepo(ctx context.Context, repo string) (*Workspace, error) {
	return m.scanOne(m.DB.QueryRowContext(ctx, `
		SELECT id, repo, url, image, container, volume, session_id, status, created_at, updated_at, last_active, profile
		FROM workspaces WHERE repo = ? AND status NOT IN ('closed', 'failed')`, repo))
}

// ForSession finds the non-closed workspace bound to a session, or
// ErrWorkspaceNotFound. Used by the broker's git-credential face to scope a
// session's credentials to exactly the repo it opened (docs/plan.md: a
// compromised session "cannot read another repo").
func (m *Manager) ForSession(ctx context.Context, sessionID string) (*Workspace, error) {
	return m.scanOne(m.DB.QueryRowContext(ctx, `
		SELECT id, repo, url, image, container, volume, session_id, status, created_at, updated_at, last_active, profile
		FROM workspaces WHERE session_id = ? AND status != 'closed'`, sessionID))
}

// List returns all workspaces, newest first.
func (m *Manager) List(ctx context.Context) (out []Workspace, err error) {
	rows, err := m.DB.QueryContext(ctx, `
		SELECT id, repo, url, image, container, volume, session_id, status, created_at, updated_at, last_active, profile
		FROM workspaces ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer func() {
		if cerr := rows.Close(); err == nil {
			err = cerr
		}
	}()
	for rows.Next() {
		ws, err := m.scanOne(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *ws)
	}
	return out, rows.Err()
}

type scanner interface{ Scan(dest ...any) error }

func (m *Manager) scanOne(row scanner) (*Workspace, error) {
	var ws Workspace
	var created, updated string
	var active string
	err := row.Scan(&ws.ID, &ws.Repo, &ws.URL, &ws.Image, &ws.Container, &ws.Volume,
		&ws.SessionID, &ws.Status, &created, &updated, &active, &ws.Profile)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrWorkspaceNotFound
	}
	if err != nil {
		return nil, err
	}
	var parseErr error
	if ws.CreatedAt, parseErr = time.Parse(time.RFC3339Nano, created); parseErr != nil && created != "" {
		return nil, fmt.Errorf("parse workspace created_at: %w", parseErr)
	}
	if ws.UpdatedAt, parseErr = time.Parse(time.RFC3339Nano, updated); parseErr != nil && updated != "" {
		return nil, fmt.Errorf("parse workspace updated_at: %w", parseErr)
	}
	if ws.LastActive, parseErr = time.Parse(time.RFC3339Nano, active); parseErr != nil && active != "" {
		return nil, fmt.Errorf("parse workspace last_active: %w", parseErr)
	}
	if active == "" {
		ws.LastActive = ws.UpdatedAt
	}
	return &ws, nil
}
