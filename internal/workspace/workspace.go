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
	neturl "net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/matt-riley/waffle/internal/id"
	"github.com/matt-riley/waffle/internal/sandbox"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/store"
)

// Workspace is one repo workspace.
type Workspace struct {
	ID        string
	Repo      string // owner/name
	URL       string
	Image     string
	Container string
	Volume    string
	SessionID string
	Status    string // open | idle | closed
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Statuses.
const (
	StatusOpen   = "open"
	StatusIdle   = "idle"
	StatusClosed = "closed"
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
	// Network for workspace containers; cloning needs egress, so default
	// bridge.
	Network string
	// BrokerURL as reachable from inside containers, plus a token minter.
	BrokerURL     string
	MintToken     func(ctx context.Context, sessionID string) (string, error)
	RevokeSession func(sessionID string)
	// ExecTimeout bounds one in-container command.
	ExecTimeout time.Duration

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

var repoRE = regexp.MustCompile(`^[\w.-]+/[\w.-]+$`)

// normalizeRepo accepts "owner/name" or a full https URL.
func normalizeRepo(arg string) (repo, url string, err error) {
	arg = strings.TrimSuffix(strings.TrimSpace(arg), ".git")
	if repoRE.MatchString(arg) {
		return arg, "https://github.com/" + arg + ".git", nil
	}
	if strings.HasPrefix(arg, "https://") {
		u, err := neturl.Parse(arg)
		if err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" {
			repoPath := strings.Trim(u.Path, "/")
			if repoRE.MatchString(repoPath) {
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
	repo, url, err := normalizeRepo(repoArg)
	if err != nil {
		return nil, nil, err
	}
	if err := m.ensureActiveRepoIndex(ctx); err != nil {
		return nil, nil, err
	}
	if existing, err := m.ForRepo(ctx, repo); err == nil {
		return m.Resume(ctx, existing.ID)
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
		ID:        wsID,
		Repo:      repo,
		URL:       url,
		Image:     m.DefaultImage,
		SessionID: sess.ID,
		Status:    StatusOpen,
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
	if err := m.Runtime.StartWorkspace(ctx, m.containerOpts(ws, token)); err != nil {
		m.revokeSession(sess.ID)
		return nil, nil, err
	}
	client, err := sandbox.NewClient(m.queueDir(ws.ID))
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
				return nil, nil, fmt.Errorf("adopt devcontainer image %q: %w", img, err)
			}
		}
	}

	if _, err := m.DB.ExecContext(ctx, `
		INSERT INTO workspaces (id, repo, url, image, container, volume, session_id, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ws.ID, ws.Repo, ws.URL, ws.Image, ws.Container, ws.Volume, ws.SessionID, ws.Status, now(), now()); err != nil {
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

func (m *Manager) containerOpts(ws *Workspace, token string) ContainerOpts {
	return ContainerOpts{
		Name:      ws.Container,
		Image:     ws.Image,
		Volume:    ws.Volume,
		QueueDir:  m.queueDir(ws.ID),
		Network:   m.Network,
		BrokerURL: m.BrokerURL,
		Token:     token,
	}
}

func (m *Manager) queueDir(id string) string { return filepath.Join(m.QueueRoot, id) }

// setup configures git and clones the repo inside the container.
func (m *Manager) setup(ctx context.Context, client *sandbox.Client, ws *Workspace) error {
	steps := []string{
		"git config --global credential.helper '!waffle git-credential'",
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

// isUniqueConstraintError reports whether err is the expected SQLite
// UNIQUE constraint violation from our partial active-repo index (used
// to detect concurrent Open races and resume the winner).
func isUniqueConstraintError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	// Only treat as the expected race if it's a UNIQUE violation on the
	// workspaces.repo partial index (not other constraints like CHECK or FK).
	return strings.Contains(s, "unique constraint failed") &&
		(strings.Contains(s, "workspaces.repo") || strings.Contains(s, "idx_workspaces_repo_active"))
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
	return m.setStatus(ctx, id, StatusIdle)
}

// Resume restarts an idle workspace's container and reconnects the queue.
func (m *Manager) Resume(ctx context.Context, id string) (*Workspace, *sandbox.Client, error) {
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
	client, err := sandbox.NewClient(m.queueDir(ws.ID))
	if err != nil {
		return nil, nil, err
	}
	return ws, client, nil
}

// CloseReport says what Close found before tearing down.
type CloseReport struct {
	Dirty    string // non-empty git status --porcelain output
	Unpushed string // non-empty log of commits ahead of upstream
}

// Close tears the workspace down. When force is false and the tree is
// dirty or has unpushed commits, it refuses and reports instead.
func (m *Manager) Close(ctx context.Context, id string, force bool) (*CloseReport, error) {
	ws, err := m.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if ws.Status == StatusClosed {
		return nil, nil
	}

	report := &CloseReport{}
	wasIdle := ws.Status == StatusIdle
	if !force {
		var client *sandbox.Client
		if wasIdle {
			resumed, resumedClient, err := m.Resume(ctx, id)
			if err != nil {
				return report, fmt.Errorf("resume workspace for safety check: %w", err)
			}
			ws = resumed
			client = resumedClient
		} else {
			client, err = sandbox.NewClient(m.queueDir(ws.ID))
			if err != nil {
				return report, fmt.Errorf("connect workspace for safety check: %w", err)
			}
		}

		report.Dirty, err = m.bashOutput(ctx, client, "cd /work/repo && git status --porcelain")
		if err == nil {
			report.Unpushed, err = m.bashOutput(ctx, client, "cd /work/repo && if git rev-parse --abbrev-ref '@{upstream}' >/dev/null 2>&1; then git log --oneline '@{upstream}'..HEAD; else git log --oneline HEAD --not --remotes; fi")
		}
		_ = client.Close()
		if err != nil {
			if wasIdle {
				if idleErr := m.Idle(ctx, id); idleErr != nil {
					return report, fmt.Errorf("inspect workspace before close: %w (also failed to restore idle: %v)", err, idleErr)
				}
			}
			return report, fmt.Errorf("inspect workspace before close: %w", err)
		}
		if report.Dirty != "" || report.Unpushed != "" {
			if wasIdle {
				if idleErr := m.Idle(ctx, id); idleErr != nil {
					return report, fmt.Errorf("workspace %s has unsaved work (close with force to discard; also failed to restore idle: %v)", id, idleErr)
				}
			}
			return report, fmt.Errorf("workspace %s has unsaved work (close with force to discard)", id)
		}
	}

	if err := m.Runtime.RemoveContainer(ctx, ws.Container); err != nil {
		return report, err
	}
	if err := m.Runtime.RemoveVolume(ctx, ws.Volume); err != nil {
		return report, err
	}
	m.revokeSession(ws.SessionID)
	return report, m.setStatus(ctx, id, StatusClosed)
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
		SELECT id, repo, url, image, container, volume, session_id, status, created_at, updated_at
		FROM workspaces WHERE id = ?`, id))
}

// ForRepo finds the non-closed workspace for a repo.
func (m *Manager) ForRepo(ctx context.Context, repo string) (*Workspace, error) {
	return m.scanOne(m.DB.QueryRowContext(ctx, `
		SELECT id, repo, url, image, container, volume, session_id, status, created_at, updated_at
		FROM workspaces WHERE repo = ? AND status != 'closed'`, repo))
}

// List returns all workspaces, newest first.
func (m *Manager) List(ctx context.Context) ([]Workspace, error) {
	rows, err := m.DB.QueryContext(ctx, `
		SELECT id, repo, url, image, container, volume, session_id, status, created_at, updated_at
		FROM workspaces ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // read-only cursor
	var out []Workspace
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
	err := row.Scan(&ws.ID, &ws.Repo, &ws.URL, &ws.Image, &ws.Container, &ws.Volume,
		&ws.SessionID, &ws.Status, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("workspace not found")
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
	return &ws, nil
}
