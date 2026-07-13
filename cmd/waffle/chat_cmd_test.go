package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/mcp"
	"github.com/matt-riley/waffle/internal/secret"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/store"
	"github.com/matt-riley/waffle/internal/tool"
	"github.com/matt-riley/waffle/internal/workset"
)

func TestSplitCommand(t *testing.T) {
	tests := []struct {
		line string
		cmd  string
		args string
	}{
		{"/skill foo", "/skill", "foo"},
		{"/skill foo bar baz", "/skill", "foo bar baz"},
		{"/skill", "/skill", ""},
		{"/repo owner/name", "/repo", "owner/name"},
		{"/repo", "/repo", ""},
		// Word boundary: these must NOT parse as /skill or /repo.
		{"/skills", "/skills", ""},
		{"/report the bug", "/report", "the bug"},
		{"/repository", "/repository", ""},
		// Plain messages keep their leading word intact.
		{"hello world", "hello", "world"},
		{"", "", ""},
	}
	for _, tt := range tests {
		cmd, args := splitCommand(tt.line)
		if cmd != tt.cmd || args != tt.args {
			t.Errorf("splitCommand(%q) = (%q, %q), want (%q, %q)", tt.line, cmd, args, tt.cmd, tt.args)
		}
	}
}

func TestWorksetCommandInspectsAndCorrectsPersistedState(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	sessions := session.New(st)
	sess, err := sessions.Create(ctx, "workset cli")
	if err != nil {
		t.Fatal(err)
	}
	ws := &workset.Store{DB: st.DB}
	e, err := ws.Add(ctx, sess.ID, workset.KindConstraint, "old constraint", workset.SourceModel, false)
	if err != nil {
		t.Fatal(err)
	}
	c := &chat{st: st, current: sess}
	if out, err := c.worksetCommand(ctx, "list"); err != nil || !strings.Contains(out, "old constraint") {
		t.Fatalf("list=%q err=%v", out, err)
	}
	if _, err := c.worksetCommand(ctx, "replace "+e.ID+" corrected by owner"); err != nil {
		t.Fatal(err)
	}
	entries, _ := ws.List(ctx, sess.ID)
	if len(entries) != 1 || entries[0].Body != "corrected by owner" || entries[0].Source != workset.SourceUser {
		t.Fatalf("replace persistence=%+v", entries)
	}
	if _, err := c.worksetCommand(ctx, "drop "+entries[0].ID); err != nil {
		t.Fatal(err)
	}
	if out, err := c.worksetCommand(ctx, "list"); err != nil || out != "working set is empty" {
		t.Fatalf("after drop=%q err=%v", out, err)
	}
	if _, err := ws.Add(ctx, sess.ID, workset.KindGoal, "clear me", workset.SourceUser, false); err != nil {
		t.Fatal(err)
	}
	if _, err := c.worksetCommand(ctx, "clear"); err != nil {
		t.Fatal(err)
	}
	entries, _ = ws.List(ctx, sess.ID)
	if len(entries) != 0 {
		t.Fatalf("clear persistence=%+v", entries)
	}
}

func TestChatContinueResumesCurrentSessionWorkset(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	sessions := session.New(st)
	sess, err := sessions.Create(ctx, "continued")
	if err != nil {
		t.Fatal(err)
	}
	ws := &workset.Store{DB: st.DB}
	if _, err := ws.Add(ctx, sess.ID, workset.KindGoal, "resume this exact goal", workset.SourceUser, true); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Provider.APIKey = ""
	cfg.Agent.Subagents = false
	cfg.Agent.Learn = false
	c, cleanup, err := newChat(ctx, cfg, st, true, "")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if c.current.ID != sess.ID {
		t.Fatalf("chat -c current session = %s, want %s", c.current.ID, sess.ID)
	}
	entries, err := ws.List(ctx, c.current.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Body != "resume this exact goal" {
		t.Fatalf("chat -c current working set = %+v", entries)
	}
}

func TestResetStartsEmptyWorksetAndPreservesOldSessionState(t *testing.T) {
	ctx := context.Background()
	sessions, st := newTestSessions(t)
	old, err := sessions.Create(ctx, "old")
	if err != nil {
		t.Fatal(err)
	}
	ws := &workset.Store{DB: st.DB}
	if _, err := ws.Add(ctx, old.ID, workset.KindGoal, "keep old goal", workset.SourceUser, true); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.Add(ctx, old.ID, workset.KindAssumption, "drop stale guess", workset.SourceModel, false); err != nil {
		t.Fatal(err)
	}
	c := &chat{sessions: sessions, st: st, current: old, history: []llm.Message{llm.UserText("old")}, persisted: 1}
	dropped, err := c.resetSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if dropped != 1 || c.current.ID == old.ID || len(c.history) != 0 || c.persisted != 0 {
		t.Fatalf("reset dropped=%d current=%s history=%d persisted=%d", dropped, c.current.ID, len(c.history), c.persisted)
	}
	oldEntries, err := ws.List(ctx, old.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(oldEntries) != 1 || oldEntries[0].Body != "keep old goal" {
		t.Fatalf("old session working set after reset = %+v", oldEntries)
	}
	currentEntries, err := ws.List(ctx, c.current.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(currentEntries) != 0 {
		t.Fatalf("new session working set after reset = %+v", currentEntries)
	}
}

func TestWorkspaceSessionSwitchSelectsItsWorkset(t *testing.T) {
	ctx := context.Background()
	sessions, st := newTestSessions(t)
	current, err := sessions.Create(ctx, "chat")
	if err != nil {
		t.Fatal(err)
	}
	workspaceSession, err := sessions.Create(ctx, "workspace")
	if err != nil {
		t.Fatal(err)
	}
	ws := &workset.Store{DB: st.DB}
	if _, err := ws.Add(ctx, current.ID, workset.KindGoal, "chat goal", workset.SourceUser, true); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.Add(ctx, workspaceSession.ID, workset.KindConstraint, "workspace constraint", workset.SourceSystem, true); err != nil {
		t.Fatal(err)
	}
	c := &chat{sessions: sessions, current: current}
	if err := c.switchToWorkspaceSession(ctx, workspaceSession.ID); err != nil {
		t.Fatal(err)
	}
	active, err := ws.List(ctx, c.current.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].Body != "workspace constraint" {
		t.Fatalf("active workspace working set = %+v", active)
	}
	original, err := ws.List(ctx, current.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(original) != 1 || original[0].Body != "chat goal" {
		t.Fatalf("original chat working set after switch = %+v", original)
	}
}

func TestSessionDeletionClearsStoredWorkset(t *testing.T) {
	ctx := context.Background()
	sessions, st := newTestSessions(t)
	sess, err := sessions.Create(ctx, "delete")
	if err != nil {
		t.Fatal(err)
	}
	ws := &workset.Store{DB: st.DB}
	if _, err := ws.Add(ctx, sess.ID, workset.KindFact, "delete with session", workset.SourceUser, false); err != nil {
		t.Fatal(err)
	}
	if err := sessions.Delete(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	entries, err := ws.List(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("deleted session working set = %+v", entries)
	}
}

// TestDockerCronGroupRestrictedMCP is the inspectable #77.7 gate. Its
// deterministic assertions always run; setting WAFFLE_TEST_DOCKER=1 also
// starts the planned MCP inside Docker.
func TestDockerCronGroupRestrictedMCP(t *testing.T) {
	t.Setenv("SAFE_CRON_VALUE", "allowed")
	t.Setenv("GITHUB_TOKEN", "must-not-leak")
	cfg := config.Default()
	cfg.Agent.Groups = map[string]config.AgentGroup{
		config.GroupCron: {Sandbox: "docker", Tools: config.ToolPolicy{Allow: []string{"cronmcp__echo"}}},
	}
	pol := cfg.AgentPolicy(config.GroupCron)
	if pol.Mode != "docker" {
		t.Fatalf("cron authority = %q, want docker", pol.Mode)
	}
	allowed := config.MCPServer{Name: "cronmcp", Execution: "sandbox", Groups: []string{config.GroupCron}, Tools: []string{"echo"}, Env: []string{"SAFE_CRON_VALUE"}}
	denied := config.MCPServer{Name: "denied", Execution: "sandbox", Groups: []string{config.GroupCron}, Tools: []string{"secret"}}
	toolPolicy := tool.Policy{Allow: pol.Allow, Deny: pol.Deny}
	if !mcpServerInGroup(allowed, config.GroupCron) || !mcpServerPermitted(allowed, toolPolicy) {
		t.Fatal("approved cron MCP was not eligible")
	}
	if mcpServerPermitted(denied, toolPolicy) {
		t.Fatal("fully denied cron MCP would be launched")
	}
	server := mcp.Server{Name: allowed.Name, Command: "sh", Args: []string{"-c", `while IFS= read -r line; do id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p'); case "$line" in *'"initialize"'*) printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2024-11-05","capabilities":{}}}\n' "$id";; *'"tools/list"'*) printf '{"jsonrpc":"2.0","id":%s,"result":{"tools":[]}}\n' "$id";; esac; done`}, Env: allowed.Env}
	planned, opts := mcp.PlanLaunch(server, allowed.Execution, pol.Mode, t.TempDir(), "alpine:3.20", "none")
	joined := strings.Join(planned.Args, "\x00")
	for _, want := range []string{"docker", "--network", "none", "/work", "SAFE_CRON_VALUE=allowed", "alpine:3.20"} {
		if want == "docker" {
			if planned.Command != want {
				t.Fatalf("command=%q want docker", planned.Command)
			}
		} else if !strings.Contains(joined, want) {
			t.Fatalf("planned cron sandbox missing %q: %v", want, planned.Args)
		}
	}
	if strings.Contains(joined, "GITHUB_TOKEN") || strings.Contains(joined, "must-not-leak") {
		t.Fatalf("ambient secret leaked into cron container args: %v", planned.Args)
	}
	if os.Getenv("WAFFLE_TEST_DOCKER") != "1" {
		t.Skip("deterministic cron MCP assertions passed; set WAFFLE_TEST_DOCKER=1 for container runtime")
	}
	client, err := mcp.ConnectRestricted(context.Background(), planned, opts)
	if err != nil {
		t.Fatalf("cron sandbox MCP runtime: %v", err)
	}
	defer func() { _ = client.Close() }()
	if _, err := client.Toolbox(context.Background()); err != nil {
		t.Fatalf("cron sandbox tools/list: %v", err)
	}
}

// TestSplitCommandDoesNotMisrouteNearMisses pins the issue #28 regression:
// dispatch matches on the whole leading word, so inputs that merely share a
// prefix with a command fall through to the default (plain message) case.
func TestSplitCommandDoesNotMisrouteNearMisses(t *testing.T) {
	for _, line := range []string{"/skills", "/report the bug", "/skillful advice", "/repos please"} {
		cmd, _ := splitCommand(line)
		if cmd == "/skill" || cmd == "/repo" {
			t.Errorf("splitCommand(%q) routed to %q; want fallthrough to default", line, cmd)
		}
	}
	for line, want := range map[string]string{
		"/skill foo":       "/skill",
		"/skill":           "/skill",
		"/repo owner/name": "/repo",
		"/repo":            "/repo",
	} {
		if cmd, _ := splitCommand(line); cmd != want {
			t.Errorf("splitCommand(%q) = %q, want %q", line, cmd, want)
		}
	}
}

func TestBareSkillAndRepoStillGiveUsage(t *testing.T) {
	c := &chat{}
	if _, err := c.skillMessage(""); err == nil || !strings.Contains(err.Error(), "usage: /skill") {
		t.Errorf("skillMessage(\"\") err = %v, want usage error", err)
	}
	if err := c.repoCommand(context.Background(), "", io.Discard); err == nil || !strings.Contains(err.Error(), "usage: /repo") {
		t.Errorf("repoCommand(\"\") err = %v, want usage error", err)
	}
}

func newTestSessions(t *testing.T) (*session.Store, *store.Store) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return session.New(st), st
}

func TestSwitchToWorkspaceSessionKeepsStateOnTurnsError(t *testing.T) {
	ctx := context.Background()
	sessions, st := newTestSessions(t)

	current, err := sessions.Create(ctx, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// The workspace session exists but its history won't load: a corrupt
	// turn makes Turns fail, standing in for any transient load error.
	wsSess, err := sessions.Create(ctx, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := st.DB.ExecContext(ctx, `
		INSERT INTO turns (session_id, seq, role, blocks, text, created_at)
		VALUES (?, 1, 'user', 'not json', '', ?)`,
		wsSess.ID, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("insert corrupt turn: %v", err)
	}

	c := &chat{
		sessions:  sessions,
		current:   current,
		history:   []llm.Message{llm.UserText("hello"), {Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "hi"}}}},
		persisted: 2,
	}
	if err := c.switchToWorkspaceSession(ctx, wsSess.ID); err == nil {
		t.Fatal("switchToWorkspaceSession = nil, want error")
	}
	if c.current.ID != current.ID {
		t.Errorf("current = %s, want %s (unchanged)", c.current.ID, current.ID)
	}
	if len(c.history) != 2 || c.persisted != 2 {
		t.Errorf("history = %d turns, persisted = %d, want both unchanged at 2", len(c.history), c.persisted)
	}
}

func TestSwitchToWorkspaceSessionLoadsHistory(t *testing.T) {
	ctx := context.Background()
	sessions, _ := newTestSessions(t)

	current, err := sessions.Create(ctx, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	wsSess, err := sessions.Create(ctx, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := sessions.AppendTurn(ctx, wsSess.ID, llm.UserText("earlier workspace turn")); err != nil {
		t.Fatalf("AppendTurn: %v", err)
	}

	c := &chat{sessions: sessions, current: current, history: []llm.Message{llm.UserText("old")}, persisted: 1}
	if err := c.switchToWorkspaceSession(ctx, wsSess.ID); err != nil {
		t.Fatalf("switchToWorkspaceSession: %v", err)
	}
	if c.current.ID != wsSess.ID {
		t.Errorf("current = %s, want %s", c.current.ID, wsSess.ID)
	}
	if len(c.history) != 1 || c.history[0].Text() != "earlier workspace turn" {
		t.Errorf("history = %+v, want the workspace session's turn", c.history)
	}
	if c.persisted != len(c.history) {
		t.Errorf("persisted = %d, want %d", c.persisted, len(c.history))
	}
}

func TestResolveAPIKeyRedactsEnvFallbackWithoutStore(t *testing.T) {
	t.Setenv(secret.EnvIdentity, "not-an-age-identity")
	t.Setenv(envName("anthropic"), "sk-ant-env-secret")

	key, redact, err := resolveAPIKey(config.Provider{
		Name:   "anthropic",
		APIKey: "secret://anthropic/api-key",
	})
	if err != nil {
		t.Fatalf("resolveAPIKey: %v", err)
	}
	if key != "sk-ant-env-secret" {
		t.Fatalf("key = %q, want env fallback", key)
	}
	if redact == nil {
		t.Fatal("redact = nil, want runtime redactor")
	}
	got := redact("token sk-ant-env-secret leaked")
	want := "token [redacted:anthropic/api-key] leaked"
	if got != want {
		t.Fatalf("redact = %q, want %q", got, want)
	}
}

func TestResolveAPIKeyRedactsEnvFallbackWithStore(t *testing.T) {
	t.Setenv("WAFFLE_HOME", t.TempDir())
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(secret.EnvIdentity, id.String())
	t.Setenv(envName("openai"), "sk-openai-env-secret")

	key, redact, err := resolveAPIKey(config.Provider{
		Name:   "openai",
		APIKey: "secret://openai/api-key",
	})
	if err != nil {
		t.Fatalf("resolveAPIKey: %v", err)
	}
	if key != "sk-openai-env-secret" {
		t.Fatalf("key = %q, want env fallback", key)
	}
	if redact == nil {
		t.Fatal("redact = nil, want runtime redactor")
	}
	got := redact("Authorization: Bearer sk-openai-env-secret")
	want := "Authorization: Bearer [redacted:openai/api-key]"
	if got != want {
		t.Fatalf("redact = %q, want %q", got, want)
	}
}

func TestApplyCodeIntelCapsFiltersUnapproved(t *testing.T) {
	// Empty requested → no extra denies.
	pol := applyCodeIntelCaps(tool.Policy{}, nil)
	if len(pol.Deny) != 0 {
		t.Fatalf("empty caps should not deny: %v", pol.Deny)
	}
	// Repo selects two approved IDs + an executable-looking unknown → deny the rest.
	pol = applyCodeIntelCaps(tool.Policy{}, []string{"code_find_symbol", "/bin/evil", "code_blast_radius"})
	denied := map[string]bool{}
	for _, d := range pol.Deny {
		denied[d] = true
	}
	if denied["code_find_symbol"] || denied["code_blast_radius"] {
		t.Fatalf("approved caps must not be denied: %v", pol.Deny)
	}
	if !denied["code_references"] || !denied["code_callers"] {
		t.Fatalf("non-selected codeintel tools must be denied: %v", pol.Deny)
	}
	if denied["/bin/evil"] {
		// Unapproved IDs are dropped by FilterCodeIntelCaps, not added as tool denies.
		t.Fatalf("executable path must not become a tool name deny: %v", pol.Deny)
	}
}

// TestRepoPolicyCannotSelectUnapprovedCodeIntelCaps ensures applyCodeIntelCaps
// rejects evil/unknown tool names from repo policy (#79 / #53).
func TestRepoPolicyCannotSelectUnapprovedCodeIntelCaps(t *testing.T) {
	evil := []string{
		"/bin/evil",
		"bash",
		"rm -rf /",
		"code_find_symbol; curl evil",
		"not_a_real_cap",
		"code_find_symbol", // only approved one
	}
	pol := applyCodeIntelCaps(tool.Policy{}, evil)
	denied := map[string]bool{}
	for _, d := range pol.Deny {
		denied[d] = true
	}
	// Only the approved selected cap stays available; all other codeintel tools denied.
	if denied["code_find_symbol"] {
		t.Fatalf("approved selected cap denied: %v", pol.Deny)
	}
	for _, name := range []string{"code_references", "code_callers", "code_structure", "code_blast_radius", "code_suggest_tests"} {
		if !denied[name] {
			t.Fatalf("expected deny of unselected %q; deny=%v", name, pol.Deny)
		}
	}
	for _, bad := range []string{"/bin/evil", "bash", "rm -rf /", "code_find_symbol; curl evil", "not_a_real_cap"} {
		if denied[bad] {
			t.Fatalf("unapproved name %q must not enter tool deny list as a capability: %v", bad, pol.Deny)
		}
	}
}

// TestDeniedMCPServerNotLaunched asserts that when every declared tool is
// denied by policy, the server is filtered before Connect (no process start).
func TestDeniedMCPServerNotLaunched(t *testing.T) {
	s := config.MCPServer{
		Name:  "evil",
		Tools: []string{"hack", "pwn"},
	}
	// Full deny of declared tools → not permitted → buildAgent skips launch.
	denyAll := tool.Policy{Deny: []string{"evil__hack", "evil__pwn"}}
	if mcpServerPermitted(s, denyAll) {
		t.Fatal("server with all tools denied must not be permitted for launch")
	}

	// Allow-list that omits server tools → not permitted.
	allowOnlyBash := tool.Policy{Allow: []string{"bash"}}
	if mcpServerPermitted(s, allowOnlyBash) {
		t.Fatal("allow-list without MCP tools must not permit launch")
	}

	// One allowed tool → permitted (would launch).
	allowOne := tool.Policy{Allow: []string{"evil__hack"}}
	if !mcpServerPermitted(s, allowOne) {
		t.Fatal("server with at least one permitted tool must be eligible")
	}

	// Undeclared tools remain eligible (back-compat; process may still start).
	legacy := config.MCPServer{Name: "legacy"}
	if !mcpServerPermitted(legacy, denyAll) {
		t.Fatal("undeclared-tools server remains eligible for back-compat")
	}

	// Group filter: wrong group is excluded before launch.
	scoped := config.MCPServer{Name: "scoped", Groups: []string{"cron"}, Tools: []string{"echo"}}
	if mcpServerInGroup(scoped, "main") {
		t.Fatal("server limited to cron must not be in main")
	}
	if !mcpServerInGroup(scoped, "cron") {
		t.Fatal("server limited to cron must be in cron")
	}
}
