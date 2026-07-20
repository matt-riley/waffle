# Waffle Chat TUI and Local Socket Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the approved Focused Conversation Bubble Tea UI, complete chat command surface, per-session model persistence, and secure versioned Unix-socket chat client/server in Waffle.

**Architecture:** Extract presentation-neutral chat contracts and runtime behavior from the scanner REPL. Direct and Unix clients implement one Backend interface consumed by plain and Bubble Tea renderers; waffle serve owns one isolated runtime per Unix connection.

**Tech Stack:** Go 1.25.12; SQLite; charm.land/bubbletea/v2 v2.0.8; charm.land/bubbles/v2 v2.1.1; charm.land/lipgloss/v2 v2.0.5; github.com/creack/pty v1.1.24; golang.org/x/sys/unix.

**Companion Infra plan:** `../infra/docs/superpowers/plans/2026-07-20-waffle-chat-managed-host-infra.md`

## Global Constraints

- Managed chat never reads provider credentials, age identity, configuration bodies, database paths, or service-owned files in the client process.
- An explicitly selected socket fails without falling back to direct state access.
- Direct mode is the default when neither --socket nor WAFFLE_CHAT_SOCKET is present.
- Non-TTY input/output and --plain use deterministic plain mode.
- Command names match whole first words; near misses remain model messages.
- Model aliases are validated before persistence and remain attached to sessions.
- Colors respect NO_COLOR, light/dark backgrounds, and monochrome readability.
- Follow red-green-refactor: each production behavior is preceded by a failing focused test.
- Preserve unrelated untracked .gitattributes and .pnpm-store entries.

## File Structure

- internal/store/migrations/0023_session_model_alias.sql: schema addition.
- internal/session: session model persistence.
- internal/config: optional standalone chat socket.
- internal/chat: shared commands, DTOs, events, and Backend interface.
- cmd/waffle/chat_runtime.go: agent/session/workspace orchestration.
- internal/chatwire: bounded NDJSON protocol and Unix client/server.
- internal/localsocket: systemd activation, safe socket creation, peer credentials.
- cmd/waffle/chat_plain.go: deterministic non-TTY renderer.
- internal/chatui and cmd/waffle/chat_tui.go: Focused Conversation UI.
- docs/chat.md and existing help/deploy/plan docs: shipped behavior.

---

### Task 1: Persist Per-Session Model Selection and Socket Configuration

**Files:**
- Create: internal/store/migrations/0023_session_model_alias.sql
- Modify: internal/session/session.go
- Modify: internal/session/session_test.go
- Modify: internal/config/config.go
- Modify: internal/config/config_test.go

**Interfaces:**
- Produces: session.Session.ModelAlias string
- Produces: (*session.Store).SetModelAlias(context.Context, string, string) error
- Produces: config.Chat with Socket string and config.Config.Chat
- Consumes: strict config loading and ordered migrations

- [ ] **Step 1: Write failing persistence and configuration tests**

~~~go
func TestSessionModelAliasPersistsAcrossGetLatestAndList(t *testing.T) {
	ctx := context.Background()
	sessions, st := newSessionStore(t)
	defer st.Close()
	sess, err := sessions.Create(ctx, "model session")
	if err != nil { t.Fatal(err) }
	if err := sessions.SetModelAlias(ctx, sess.ID, "claude"); err != nil { t.Fatal(err) }
	got, err := sessions.Get(ctx, sess.ID)
	if err != nil || got.ModelAlias != "claude" { t.Fatalf("Get = %+v, %v", got, err) }
	latest, err := sessions.Latest(ctx)
	if err != nil || latest.ModelAlias != "claude" { t.Fatalf("Latest = %+v, %v", latest, err) }
	list, err := sessions.List(ctx, 10)
	if err != nil || len(list) != 1 || list[0].ModelAlias != "claude" {
		t.Fatalf("List = %+v, %v", list, err)
	}
}

func TestLoadChatSocketRequiresAbsoluteCleanPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, "[chat]\nsocket = \"relative.sock\"\n")
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("relative socket = %v", err)
	}
	writeFile(t, path, "[chat]\nsocket = \"/tmp/waffle-chat.sock\"\n")
	cfg, err := Load(path)
	if err != nil || cfg.Chat.Socket != "/tmp/waffle-chat.sock" {
		t.Fatalf("chat config = %+v, %v", cfg.Chat, err)
	}
}
~~~

- [ ] **Step 2: Verify RED**

~~~sh
go test ./internal/session ./internal/config -run 'TestSessionModelAlias|TestLoadChatSocket' -count=1
~~~

Expected: compile failures for ModelAlias, SetModelAlias, and Config.Chat.

- [ ] **Step 3: Add migration and persistence API**

~~~sql
ALTER TABLE sessions ADD COLUMN model_alias TEXT NOT NULL DEFAULT '';
~~~

Add ModelAlias to every Session SELECT/Scan and:

~~~go
func (s *Store) SetModelAlias(ctx context.Context, id, alias string) error {
	result, err := s.db.ExecContext(ctx,
		"UPDATE sessions SET model_alias = ?, updated_at = ? WHERE id = ?",
		strings.TrimSpace(alias), s.nowStr(), id)
	if err != nil { return fmt.Errorf("set session model alias: %w", err) }
	n, err := result.RowsAffected()
	if err != nil { return fmt.Errorf("read set-model result: %w", err) }
	if n == 0 { return ErrNotFound }
	return nil
}
~~~

- [ ] **Step 4: Add config.Chat and validation**

~~~go
type Chat struct {
	Socket string `toml:"socket"`
}
~~~

Reject non-empty socket values unless filepath.IsAbs is true, filepath.Clean equals the value, and no NUL byte occurs. Do not inspect/create the path during config load.

- [ ] **Step 5: Verify GREEN and commit**

~~~sh
go test ./internal/session ./internal/config ./internal/store -count=1
git add internal/store/migrations/0023_session_model_alias.sql internal/session internal/config
git commit -m "feat: persist chat model per session"
~~~

---

### Task 2: Define Shared Chat Types and the Command Registry

**Files:**
- Create: internal/chat/types.go
- Create: internal/chat/commands.go
- Create: internal/chat/commands_test.go

**Interfaces:**
- Produces: chat.Backend, OpenOptions, State, Event, Result, Command, ParsedCommand
- Produces: chat.ParseInput(string) (ParsedCommand, bool, error)
- Produces: chat.Commands() []Command and chat.Complete(string) []Command
- Consumes: llm.Message and llm.Usage

- [ ] **Step 1: Write failing command tests**

~~~go
func TestCommandRegistryParsesAliasesAndNearMisses(t *testing.T) {
	tests := []struct{ input string; name Name; args string; ok bool }{
		{"/help", CommandHelp, "", true}, {"/exit", CommandExit, "", true}, {"/quit", CommandExit, "", true},
		{"/model claude", CommandModel, "claude", true}, {"/models", CommandModels, "", true},
		{"/new", CommandNew, "", true}, {"/reset", CommandNew, "", true}, {"/clear", CommandNew, "", true},
		{"/sessions", CommandSessions, "", true}, {"/resume 01ABC", CommandResume, "01ABC", true},
		{"/status", CommandStatus, "", true}, {"/usage", CommandUsage, "", true},
		{"/permissions", CommandPermissions, "", true}, {"/skill audit fast", CommandSkill, "audit fast", true},
		{"/repo owner/repo", CommandRepo, "owner/repo", true}, {"/workset list", CommandWorkset, "list", true},
		{"/modelsx", "", "", false}, {"plain /model text", "", "", false},
	}
	for _, tt := range tests {
		got, ok, err := ParseInput(tt.input)
		if err != nil { t.Fatalf("ParseInput(%q): %v", tt.input, err) }
		if ok != tt.ok || got.Name != tt.name || got.Args != tt.args {
			t.Errorf("ParseInput(%q) = %+v,%v", tt.input, got, ok)
		}
	}
}

func TestCompletionIsStableAndDocumented(t *testing.T) {
	got := Complete("/mo")
	if len(got) != 2 || got[0].Name != CommandModel || got[1].Name != CommandModels {
		t.Fatalf("Complete = %+v", got)
	}
	for _, command := range Commands() {
		if command.Usage == "" || command.Description == "" { t.Fatalf("undocumented: %+v", command) }
	}
}
~~~

- [ ] **Step 2: Verify RED**

~~~sh
go test ./internal/chat -count=1
~~~

Expected: package or symbols do not exist.

- [ ] **Step 3: Implement exact shared interfaces**

~~~go
type OpenOptions struct { Continue bool; SessionID, Profile string; Capabilities []string }
type Model struct { Alias, Provider, Upstream string; Current bool }
type Session struct { ID, Title, Summary, ModelAlias string; UpdatedAt time.Time }
type UsageRow struct {
	SessionID, Period, PeriodStart string
	Requests, InputTokens, OutputTokens, ReservedTokens int
}
type PermissionView struct {
	SandboxMode string
	Allow, Deny, DenyPrefixes []string
}
type WorkItem struct { ID, Text string }
type State struct {
	SessionID, Title, ModelAlias, ModelError, ProviderLabel string
	Profile, ConnectionMode, SandboxMode, Workspace string
	History []llm.Message
	Models []Model
	Capabilities []string
}
type Event struct {
	Kind EventKind
	Text, ToolName string
	IsError bool
	ByteCount int
	Usage llm.Usage
	State *State
}
type Result struct {
	Title, Text string
	Commands []Command
	Models []Model
	Sessions []Session
	Usage []UsageRow
	Permissions *PermissionView
	Workset []WorkItem
	State *State
	Confirm, ShouldClose bool
}
type Backend interface {
	Open(context.Context, OpenOptions) (State, error)
	Turn(context.Context, string, func(Event)) error
	Command(context.Context, ParsedCommand, func(Event)) (Result, error)
	Cancel()
	Close(context.Context) error
}
~~~

Define CommandHelp, CommandExit, CommandModel, CommandModels, CommandNew, CommandSessions, CommandResume, CommandStatus, CommandUsage, CommandPermissions, CommandSkill, CommandRepo, and CommandWorkset as Name constants. Define EventTextDelta, EventToolStarted, EventToolFinished, EventNotice, EventState, and EventTurnDone. Add JSON tags to wire-visible DTO fields; never put error values in DTOs.

- [ ] **Step 4: Implement immutable registry**

Define canonical names/help for every tested command. Alias quit to exit and reset/clear to new. Parse only exact first tokens; Complete returns canonical commands in registry order.

~~~go
var commandRegistry = []Command{
	{Name: CommandHelp, Usage: "/help", Description: "show commands and keys"},
	{Name: CommandExit, Usage: "/exit", Aliases: []string{"quit"}, Description: "finish and close chat"},
	{Name: CommandModel, Usage: "/model [alias]", Description: "choose the session model"},
	{Name: CommandModels, Usage: "/models", Description: "list configured models"},
	{Name: CommandNew, Usage: "/new", Aliases: []string{"reset", "clear"}, Description: "start a new session"},
	{Name: CommandSessions, Usage: "/sessions", Description: "list recent sessions"},
	{Name: CommandResume, Usage: "/resume [session]", Description: "resume a session"},
	{Name: CommandStatus, Usage: "/status", Description: "show current runtime status"},
	{Name: CommandUsage, Usage: "/usage", Description: "show token and request usage"},
	{Name: CommandPermissions, Usage: "/permissions", Description: "show effective sandbox and tool policy"},
	{Name: CommandSkill, Usage: "/skill <name> [args]", Description: "invoke a skill"},
	{Name: CommandRepo, Usage: "/repo <owner/repo>", Description: "open a repository workspace"},
	{Name: CommandWorkset, Usage: "/workset [list|replace <id> <text>|drop <id>|clear]", Description: "inspect or correct the working set"},
}
~~~

- [ ] **Step 5: Verify GREEN and commit**

~~~sh
go test ./internal/chat -count=1
git add internal/chat
git commit -m "feat: define chat command contract"
~~~

---

### Task 3: Extract the Runtime and Implement Every Command

**Files:**
- Create: cmd/waffle/chat_runtime.go
- Create: cmd/waffle/chat_runtime_test.go
- Modify: cmd/waffle/chat_cmd.go
- Modify: cmd/waffle/chat_cmd_test.go

**Interfaces:**
- Consumes: Task 1 persistence and Task 2 chat contracts
- Produces: newChatRuntime(context.Context, config.Config, *store.Store) (*chatRuntime, error)
- Consumes: (*usage.Store).List(context.Context, string) ([]usage.Row, error)

- [ ] **Step 1: Write failing runtime tests**

~~~go
func TestChatRuntimeModelSelectionPersistsAndResumeRestoresIt(t *testing.T) {
	ctx := context.Background()
	runtime, sessions := newRuntimeFixture(t, configuredModels())
	state, err := runtime.Open(ctx, chat.OpenOptions{})
	if err != nil { t.Fatal(err) }
	if _, err := runtime.Command(ctx, chat.ParsedCommand{Name: chat.CommandModel, Args: "gpt"}, nil); err != nil {
		t.Fatal(err)
	}
	saved, _ := sessions.Get(ctx, state.SessionID)
	if saved.ModelAlias != "gpt" { t.Fatalf("saved = %+v", saved) }
	second := newRuntimeAgainstSameStore(t, runtime.cfg, sessions)
	resumed, err := second.Open(ctx, chat.OpenOptions{SessionID: state.SessionID})
	if err != nil || resumed.ModelAlias != "gpt" { t.Fatalf("resumed = %+v, %v", resumed, err) }
}

func TestChatRuntimeInvalidModelIsAtomic(t *testing.T) {
	runtime, sessions := newRuntimeFixture(t, configuredModels())
	state, _ := runtime.Open(context.Background(), chat.OpenOptions{})
	_, err := runtime.Command(context.Background(), chat.ParsedCommand{Name: chat.CommandModel, Args: "missing"}, nil)
	if err == nil || runtime.agent.Model != state.ModelAlias { t.Fatalf("model=%q err=%v", runtime.agent.Model, err) }
	saved, _ := sessions.Get(context.Background(), state.SessionID)
	if saved.ModelAlias != "" { t.Fatalf("invalid model persisted %q", saved.ModelAlias) }
}
~~~

Add cases where SetModelAlias fails and prove agent.Model remains unchanged, and where a persisted alias is removed from config and prove Open returns State with ModelError, the unavailable alias, and picker Models without silently selecting another model. Turn must reject while ModelError is set; a valid /model clears it. Add a table test for help, models, new/reset/clear, sessions, resume, status, usage, permissions, skill, repo usage failure, workset, and exit; assert typed Result shapes and exact usage messages. No-argument /model and /resume return picker data, and /new returns Confirm=true instead of interrupting an active or unsent turn. Inject reflection failure for exit/close and prove it emits a warning but still closes cleanly.

- [ ] **Step 2: Verify RED**

~~~sh
go test ./cmd/waffle -run 'TestChatRuntime' -count=1
~~~

Expected: runtime symbols are missing.

- [ ] **Step 3: Move state/orchestration into chatRuntime**

~~~go
type chatRuntime struct {
	mu sync.Mutex
	agent *agent.Agent
	agentCancel context.CancelFunc
	sessions *session.Store
	current *session.Session
	history []llm.Message
	persisted int
	cfg config.Config
	st *store.Store
	skills []skill.Skill
	profileName string
	agentCleanup func()
	wsBroker *broker.Broker
	wsURL string
	wsClient io.Closer
}
~~~

newChatRuntime stores config/session dependencies but does not construct an agent. Open validates OpenOptions.Profile, constructs the profile-specific agent/resources, resolves continue/exact session, repairs history, restores ModelAlias, and validates it before changing agent.Model. If the stored alias was removed, return an otherwise usable State with ModelError and Models so renderers can open/recommend the picker, and reject Turn until a valid model is chosen. Keep existing profile/workspace policy behavior.

- [ ] **Step 4: Implement Turn, Cancel, Close**

Turn sets title, appends user text, installs a per-turn child context, emits text/tool/usage hooks, persists completed history even on run error, clears cancel, and emits turn_done. Cancel invokes only active turn cancellation. Close performs bounded reflection and releases workspace, broker, agent, sandbox, and MCP resources once.

~~~go
func (r *chatRuntime) Turn(ctx context.Context, input string, emit func(chat.Event)) error
func (r *chatRuntime) Cancel()
func (r *chatRuntime) Close(ctx context.Context) error
~~~

Guard Turn under r.mu, reject an existing r.agentCancel, install context.WithCancel, then release the mutex before agent.Run. Use a defer to clear only the matching cancel function. Close uses sync.Once for resource cleanup and context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second) for reflection; return reflection failure only after cleanup and let renderers show it as a warning.

- [ ] **Step 5: Implement all command branches**

- help renders chat.Commands.
- model validates cfg.ResolveModel, commits SetModelAlias, and only then changes agent.Model; a database failure leaves both runtime and stored selection unchanged.
- models returns sorted aliases/provider/upstream labels.
- new preserves old history and creates a fresh session.
- sessions returns 50 recent sessions.
- resume loads/repairs before mutating state and restores model.
- status returns session/model/provider/profile/connection/sandbox/workspace.
- usage calls usage.New(st).List twice, once for currentSessionID and once with an empty filter, maps both into typed chat.UsageRow values, and supplies clearly labeled current-session and persisted aggregate totals.
- permissions returns resolved group/profile tool policy without mutation.
- skill runs the generated skill message through Turn.
- repo preserves current workspace setup but emits events instead of writing.
- workset preserves list/replace/drop/clear.
- exit reflects and returns ShouldClose.

Dispatch only parsed names:

~~~go
func (r *chatRuntime) Command(ctx context.Context, command chat.ParsedCommand, emit func(chat.Event)) (chat.Result, error) {
	switch command.Name {
	case chat.CommandHelp, chat.CommandModel, chat.CommandModels, chat.CommandNew,
		chat.CommandSessions, chat.CommandResume, chat.CommandStatus, chat.CommandUsage,
		chat.CommandPermissions, chat.CommandSkill, chat.CommandRepo, chat.CommandWorkset,
		chat.CommandExit:
		return r.runCommand(ctx, command, emit)
	default:
		return chat.Result{}, fmt.Errorf("unknown chat command %q", command.Name)
	}
}
~~~

- [ ] **Step 6: Verify GREEN and commit**

~~~sh
go test ./cmd/waffle -count=1
git add cmd/waffle/chat_runtime.go cmd/waffle/chat_runtime_test.go cmd/waffle/chat_cmd.go cmd/waffle/chat_cmd_test.go
git commit -m "feat: add stateful chat runtime commands"
~~~

---

### Task 4: Implement the Bounded Versioned Chat Protocol

**Files:**
- Create: internal/chatwire/frame.go
- Create: internal/chatwire/codec.go
- Create: internal/chatwire/codec_test.go
- Create: internal/chatwire/client.go
- Create: internal/chatwire/client_test.go
- Create: internal/chatwire/server.go
- Create: internal/chatwire/server_test.go

**Interfaces:**
- Consumes: chat.Backend, OpenOptions, ParsedCommand, Event, Result
- Produces: chatwire.ProtocolVersion = 1 and MaxFrameBytes = 1 MiB
- Produces: chatwire.Dial(context.Context, string) (*Client, error)
- Produces: chatwire.Serve(context.Context, net.Listener, Factory, AuditFunc) error
- Produces: type Factory func(context.Context) (chat.Backend, error)

- [ ] **Step 1: Write failing codec and security tests**

Round-trip open, turn, command, cancel, close, ready, event, result, error, and goodbye frames. Assert failures for version zero/wrong, unknown type, malformed JSON, and frames over 1 MiB.

Use these canaries in fake backend state/errors and assert no encoded server frame contains them:

~~~go
var canaries = []string{
	"AGE-SECRET-KEY-1TEST",
	"sk-provider-secret",
	"/var/lib/waffle/config.toml",
	"WAFFLE_AGE_IDENTITY",
}
~~~

- [ ] **Step 2: Verify RED**

~~~sh
go test ./internal/chatwire -count=1
~~~

Expected: package does not exist.

- [ ] **Step 3: Implement envelope and bounded codec**

~~~go
const ProtocolVersion = 1
const MaxFrameBytes = 1 << 20

type Frame struct {
	Version int             `json:"version"`
	Type    string          `json:"type"`
	ID      string          `json:"id,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}
type ErrorPayload struct {
	Code string `json:"code"`
	Message string `json:"message"`
}
~~~

Decoder rejects an overlong physical line before json.Unmarshal. Encoder rejects marshaled output above the same bound. Both validate direction-specific type allowlists.

- [ ] **Step 4: Implement Unix client**

Dial requires an absolute path, uses net.Dialer.DialContext("unix", path), sends open, and verifies the version. Serialize writes under a mutex, route response IDs, and expose Backend. Turn consumes events through turn_done. Cancel can write while Turn waits. Close sends close, awaits goodbye with a deadline, then closes.

~~~go
type Client struct {
	conn net.Conn
	codec *Codec
	writeMu sync.Mutex
	pending map[string]chan Frame
	closed chan struct{}
}

func Dial(ctx context.Context, path string) (*Client, error)
func (c *Client) Open(context.Context, chat.OpenOptions) (chat.State, error)
func (c *Client) Turn(context.Context, string, func(chat.Event)) error
func (c *Client) Command(context.Context, chat.ParsedCommand, func(chat.Event)) (chat.Result, error)
func (c *Client) Cancel()
func (c *Client) Close(context.Context) error
~~~

- [ ] **Step 5: Implement server connection loop**

Require open first, create exactly one Backend, return ready, and then accept requests. Run a turn in a child goroutine so cancel/close are still read. Reject a second turn with code turn_active. Serialize all writes. Convert internal errors to stable redacted codes/messages; never encode Go error chains.

~~~go
type Factory func(context.Context) (chat.Backend, error)
type AuditFunc func(context.Context, net.Conn, string)

func Serve(ctx context.Context, listener net.Listener, factory Factory, audit AuditFunc) error
func serveConn(ctx context.Context, conn net.Conn, factory Factory, audit AuditFunc)
~~~

serveConn owns a WaitGroup for the active turn, calls Backend.Cancel before waiting on close/disconnect, and always calls Backend.Close with a bounded context before returning.

- [ ] **Step 6: Test disconnect and concurrent clients**

Use temp Unix listeners. Prove disconnect cancels/closes one backend, a second stays functional, clients get isolated state, and close releases goroutines under -race.

- [ ] **Step 7: Verify GREEN and commit**

~~~sh
go test -race ./internal/chatwire -count=1
git add internal/chatwire
git commit -m "feat: add local chat wire protocol"
~~~

---

### Task 5: Add Systemd/Unix Listener and Serve Integration

**Files:**
- Create: internal/localsocket/activation_unix.go
- Create: internal/localsocket/activation_other.go
- Create: internal/localsocket/activation_test.go
- Create: internal/localsocket/peercred_linux.go
- Create: internal/localsocket/peercred_other.go
- Create: internal/localsocket/peercred_test.go
- Modify: cmd/waffle/serve_cmd.go
- Modify: cmd/waffle/serve_cmd_test.go
- Modify: config.example.toml

**Interfaces:**
- Consumes: chatwire.Serve and newChatRuntime
- Produces: localsocket.Listener(configuredPath string) (net.Listener, bool, error)
- Produces: localsocket.PeerCredentials(net.Conn) (Peer, error), with Peer PID/UID/GID/Available

- [ ] **Step 1: Write failing listener/lifecycle tests**

Cover:

- matching LISTEN_PID, LISTEN_FDS=1, LISTEN_FDNAMES=waffle-chat consumes fd 3;
- mismatched PID ignores inherited descriptors;
- multiple descriptors/wrong name fail;
- relative config fails;
- existing symlink/regular/directory path fails;
- stale socket is safely replaced;
- serve starts a configured chat socket, accepts a handshake, removes it on shutdown;
- listener errors fail serve instead of logging and continuing.

- [ ] **Step 2: Verify RED**

~~~sh
go test ./internal/localsocket ./cmd/waffle -run 'TestInherited|TestConfiguredUnix|TestServeChat' -count=1
~~~

Expected: package/listener wiring absent.

- [ ] **Step 3: Implement inherited listener**

Accept fd 3 only when systemd variables are consistent and descriptor name is waffle-chat. Convert os.NewFile through net.FileListener, close the os.File wrapper, then unset LISTEN_PID/LISTEN_FDS/LISTEN_FDNAMES. Non-Unix returns no inherited listener.

~~~go
func Listener(configuredPath string) (net.Listener, bool, error) {
	if inherited, ok, err := inheritedListener("waffle-chat"); ok || err != nil {
		return inherited, ok, err
	}
	if configuredPath == "" { return nil, false, nil }
	listener, err := configuredListener(configuredPath)
	return listener, false, err
}
~~~

- [ ] **Step 4: Implement configured socket**

Require absolute clean path. lstat existing path and remove only ModeSocket; reject symlink/regular/directory/device. Create standalone parent 0700, listen with net.ListenUnix, chmod socket 0600, and remove it on Close. Never chmod an inherited systemd listener.

~~~go
type removingListener struct { net.Listener; path string }
func (l *removingListener) Close() error {
	err := l.Listener.Close()
	removeErr := os.Remove(l.path)
	if errors.Is(removeErr, os.ErrNotExist) { removeErr = nil }
	return errors.Join(err, removeErr)
}
~~~

- [ ] **Step 5: Implement peer credentials**

On Linux call unix.GetsockoptUcred through UnixConn.SyscallConn and return numeric PID/UID/GID. Other platforms return Available=false without weakening filesystem checks.

~~~go
type Peer struct { PID int32; UID, GID uint32; Available bool }
func PeerCredentials(conn net.Conn) (Peer, error)
~~~

- [ ] **Step 6: Integrate with serve**

Acquire listener after config/store/skills. Start chatwire.Serve with a factory that creates a fresh chatRuntime over the shared store. Its AuditFunc resolves localsocket.Peer and logs only numeric PID/UID/GID plus connection lifecycle; credential lookup failure is logged without rejecting a filesystem-authorized client. Any non-context server error cancels ownership and is returned. Wait for chat server shutdown before shared resource cleanup. Register no TCP endpoint.

~~~go
listener, _, err := localsocket.Listener(cfg.Chat.Socket)
if err != nil { return fmt.Errorf("chat listener: %w", err) }
if listener != nil {
	group.Go(func() error {
		return chatwire.Serve(groupCtx, listener, runtimeFactory, auditPeer)
	})
}
~~~

- [ ] **Step 7: Verify GREEN and commit**

~~~sh
go test -race ./internal/localsocket ./internal/chatwire ./cmd/waffle -run 'TestInherited|TestConfiguredUnix|TestServeChat|TestServeStops' -count=1
git add internal/localsocket cmd/waffle/serve_cmd.go cmd/waffle/serve_cmd_test.go config.example.toml
git commit -m "feat: serve chat over a local Unix socket"
~~~

---

### Task 6: Route Backends and Preserve Plain Mode

**Files:**
- Modify: cmd/waffle/chat_cmd.go
- Create: cmd/waffle/chat_plain.go
- Create: cmd/waffle/chat_plain_test.go
- Modify: cmd/waffle/chat_cmd_test.go

**Interfaces:**
- Consumes: chat.Backend, chatwire.Dial, newChatRuntime
- Produces: chatOptions with Continue, Profile, Socket, Plain, Help
- Produces: runPlainChat(context.Context, chat.Backend, chat.OpenOptions, io.Reader, io.Writer, io.Writer) error

- [ ] **Step 1: Write failing routing tests**

Assert --socket requires absolute path and beats the environment; WAFFLE_CHAT_SOCKET selects remote; empty env selects direct; --plain forces plain; either non-TTY stream selects plain; --help prints complete chat usage and opens no backend; unavailable explicit socket includes path plus waffle.service/waffle-chat.socket guidance and never opens WAFFLE_HOME.

- [ ] **Step 2: Verify RED**

~~~sh
go test ./cmd/waffle -run 'TestChatOptions|TestChatSocketDoesNotFallback|TestPlainChat' -count=1
~~~

Expected: options/plain renderer missing.

- [ ] **Step 3: Implement option parsing/backend selection**

Usage is:

~~~text
waffle chat [-c|--continue] [--profile name] [--socket absolute-path] [--plain]
~~~

Parse without opening config. With Socket, dial only. Otherwise open local config/store/runtime. Use term.IsTerminal for os.File streams through an injectable function for tests.

Handle -h/--help before backend selection and return success after printing the exact usage and flags. This help path is also the deployment compatibility probe used by Infra.

- [ ] **Step 4: Implement plain renderer**

Retain banner, prompt, streamed text, compact tool rows, EOF close, and 1 MiB scanner bound. Parse with chat.ParseInput. Honor ShouldClose. Render typed lists in stable text without ANSI dependencies.

~~~go
func runPlainChat(ctx context.Context, backend chat.Backend, open chat.OpenOptions, in io.Reader, out, stderr io.Writer) error {
	state, err := backend.Open(ctx, open)
	if err != nil { return err }
	defer func() { _ = backend.Close(context.WithoutCancel(ctx)) }()
	return scanPlainInput(ctx, backend, state, bufio.NewScanner(in), out, stderr)
}
~~~

Set scanner.Buffer(make([]byte, 0, 64*1024), 1<<20). EventTextDelta writes text, tool events write one sanitized status row, notices/errors go to stderr, and every Result slice is formatted in registry/list order.

- [ ] **Step 5: Verify compatibility and commit**

~~~sh
go test ./cmd/waffle -run 'TestChat|TestPlain|TestSplitCommand|TestBareSkillAndRepo' -count=1
git add cmd/waffle/chat_cmd.go cmd/waffle/chat_cmd_test.go cmd/waffle/chat_plain.go cmd/waffle/chat_plain_test.go
git commit -m "refactor: share chat behavior across renderers"
~~~

---

### Task 7: Build the Focused Conversation Bubble Tea UI

**Files:**
- Create: internal/chatui/model.go
- Create: internal/chatui/update.go
- Create: internal/chatui/render.go
- Create: internal/chatui/markdown.go
- Create: internal/chatui/theme.go
- Create: internal/chatui/model_test.go
- Create: internal/chatui/render_test.go
- Create: internal/chatui/testdata/focused.golden
- Create: internal/chatui/testdata/narrow.golden
- Create: internal/chatui/testdata/monochrome.golden
- Create: cmd/waffle/chat_tui.go
- Create: cmd/waffle/chat_tui_test.go
- Modify: cmd/waffle/chat_cmd.go
- Modify: go.mod
- Modify: go.sum

**Interfaces:**
- Consumes: chat.Backend, chat.OpenOptions, chat.Event, chat.Result, chat.Commands
- Produces: chatui.New(chat.Backend, chat.OpenOptions, chatui.Options) *chatui.Model
- Produces: runTUIChat(context.Context, chat.Backend, chat.OpenOptions, io.Reader, io.Writer) error
- Uses: Bubble Tea v2 declarative tea.View, textarea.Model, viewport.Model, list.Model, and Lip Gloss v2

- [ ] **Step 1: Pin the approved terminal UI dependencies**

~~~sh
go get charm.land/bubbletea/v2@v2.0.8 charm.land/bubbles/v2@v2.1.1 charm.land/lipgloss/v2@v2.0.5
~~~

Expected: go.mod contains exactly those minimum versions and go.sum records their transitive modules.

- [ ] **Step 2: Write failing model-transition tests**

~~~go
func TestModelHandlesTurnCancelAndExit(t *testing.T) {
	backend := newFakeBackend(chat.State{SessionID: "01TEST", ModelAlias: "gpt"})
	m := New(backend, chat.OpenOptions{}, Options{Width: 100, Height: 30})
	m = updateForTest(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if !m.turnActive { t.Fatal("enter did not start a turn") }
	m = updateForTest(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})
	if backend.cancelCalls != 1 { t.Fatalf("cancel calls = %d", backend.cancelCalls) }
	m.composer.SetValue("/exit")
	m = updateForTest(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if !m.quitting { t.Fatal("/exit did not quit") }
}

func TestModelCommandPaletteAndModelOverlay(t *testing.T) {
	backend := newFakeBackend(chat.State{Models: []chat.Model{{Alias: "gpt"}, {Alias: "claude"}}})
	m := New(backend, chat.OpenOptions{}, Options{Width: 100, Height: 30})
	m.composer.SetValue("/mo")
	m = updateForTest(t, m, tea.KeyPressMsg{Code: tea.KeyTab})
	if !m.paletteVisible || len(m.palette) != 2 { t.Fatalf("palette = %+v", m.palette) }
	m.composer.SetValue("/model")
	m = updateForTest(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.overlay != overlayModels { t.Fatalf("overlay = %v", m.overlay) }
}
~~~

Also cover Ctrl+C with an active turn versus the two-press idle exit, Ctrl+D on an empty composer, multiline Alt+Enter, resize below 72 columns without scroll/focus loss, streamed deltas, tool start/done rows, backend disconnect, /help, /permissions, /sessions, no-ID /resume, and result confirmation.

- [ ] **Step 3: Verify RED**

~~~sh
go test ./internal/chatui ./cmd/waffle -run 'TestModel|TestRunTUIChat' -count=1
~~~

Expected: chatui package and runTUIChat do not exist.

- [ ] **Step 4: Implement the Bubble Tea v2 model and update loop**

~~~go
type Options struct {
	Width, Height int
	NoColor bool
}

type Model struct {
	backend chat.Backend
	open chat.OpenOptions
	state chat.State
	viewport viewport.Model
	composer textarea.Model
	messages []messageCard
	tools []toolRow
	palette []chat.Command
	overlay overlayKind
	paletteVisible, turnActive, quitting bool
	width, height int
	err error
}
~~~

Init asynchronously opens the backend. Update owns all component mutation. A submitted non-command starts one backend Turn command; Event messages stream back as Bubble Tea messages. Exact slash commands call Backend.Command. Escape and Ctrl+C cancel only an active turn; the first idle Ctrl+C arms exit, the second exits, Ctrl+D exits only when the composer is empty, and /exit closes and quits. Tab completes commands, Up/Down navigate overlays, Enter selects, and Alt+Enter inserts a newline. Backend loss preserves the transcript, marks the connection disconnected, waits for acknowledgement, and exits without retrying a possibly non-idempotent turn or falling back to direct mode.

- [ ] **Step 5: Implement responsive Focused Conversation rendering**

View returns a Bubble Tea v2 tea.View with AltScreen=true. Render:

~~~text
 Waffle  session-title                         gpt · local
 ─────────────────────────────────────────────────────────
 You
 Explain the failed deploy.

 Waffle
 The service could not parse its database URL.
   ✓ read logs   2.1 KB
   ✓ inspect unit

 ┌───────────────────────────────────────────────────────┐
 │ Ask Waffle…                                           │
 └───────────────────────────────────────────────────────┘
 /help  /model  /sessions                 Esc cancel · ↵ send
~~~

At widths below 72, stack title/model metadata and hide nonessential footer hints. Keep message cards visually distinct without boxed every-line chrome; tool rows remain one line. Render headings, lists, emphasis, links, inline code, and fenced code with small focused Lip Gloss styles in markdown.go, preserving readable plain text when color is off. /help, /models, /sessions, /permissions, and confirmation use centered bounded overlays. Request the terminal background color, select light/dark palettes from the response, disable color when Options.NoColor or NO_COLOR is set, and retain prefixes/borders in monochrome.

- [ ] **Step 6: Add deterministic render snapshots**

Seed a fixed state with user/assistant/tool/error content, render at 100x30 and 58x24, strip ANSI for structural assertions, and compare to focused.golden and narrow.golden. Render with NoColor=true and compare byte-for-byte with monochrome.golden. Golden content must assert the header, cards, compact tool row, composer, footer, and overlay clipping.

- [ ] **Step 7: Wire TTY mode and verify GREEN**

~~~go
func runTUIChat(ctx context.Context, backend chat.Backend, open chat.OpenOptions, in io.Reader, out io.Writer) error {
	m := chatui.New(backend, open, chatui.Options{NoColor: os.Getenv("NO_COLOR") != ""})
	_, err := tea.NewProgram(m, tea.WithContext(ctx), tea.WithInput(in), tea.WithOutput(out)).Run()
	return err
}
~~~

~~~sh
go test ./internal/chatui ./cmd/waffle -run 'TestModel|TestRender|TestRunTUIChat' -count=1
git add go.mod go.sum internal/chatui cmd/waffle/chat_tui.go cmd/waffle/chat_tui_test.go cmd/waffle/chat_cmd.go
git commit -m "feat: add focused conversation chat TUI"
~~~

---

### Task 8: Prove End-to-End Socket, TTY, and Secret Boundaries

**Files:**
- Create: cmd/waffle/chat_socket_integration_test.go
- Create: cmd/waffle/chat_tui_pty_test.go
- Modify: go.mod
- Modify: go.sum

**Interfaces:**
- Consumes: serve chat listener, chatwire.Client, direct/plain/TUI routing, session.ModelAlias
- Produces: regression coverage for managed no-sudo behavior, terminal interaction, cancellation, persistence, and data isolation
- Uses: github.com/creack/pty v1.1.24

- [ ] **Step 1: Pin PTY support and write the failing socket integration test**

~~~sh
go get github.com/creack/pty@v1.1.24
~~~

The test starts serve with a temporary standalone chat socket and service-owned config/store. As an ordinary client configuration, set WAFFLE_HOME to a nonexistent unreadable path and WAFFLE_AGE_IDENTITY to AGE-SECRET-KEY-1CLIENT-CANARY, then connect with --socket and --plain. Feed:

~~~text
/models
/model gpt
/status
hello socket
/exit
~~~

Assert the model changes, one streamed answer arrives, status says connection=unix, exit is clean, the server database contains model_alias=gpt, and client output never includes credential/config/database canaries.

- [ ] **Step 2: Write failing PTY tests**

Start a deterministic temporary chatwire server, then launch the real Waffle chat process against its socket under pty.StartWithSize at 100x30. Assert the Focused Conversation header/composer appears, type /help and /model, select an alias, submit a synthetic turn, resize to 58x24, cancel a delayed turn with Escape, and quit with /exit. Add backend-disconnect and SIGINT cases that assert the alternate-screen restore sequence is emitted and the process exits without a terminal hang. Bound every expect operation to five seconds and dump the sanitized screen buffer on failure.

- [ ] **Step 3: Run the new end-to-end tests**

~~~sh
go test ./cmd/waffle -run 'TestChatSocketEndToEnd|TestChatTUIPTY' -count=1
~~~

Expected: PASS against the unit-tested implementation from Tasks 1-7. If either test exposes an integration defect, stop and use superpowers:systematic-debugging; fix the responsible task's production boundary without adding protocol fields, credential access, environment bypasses, or automatic direct fallback.

- [ ] **Step 4: Run repeated race and leak-sensitive checks**

~~~sh
go test -race ./internal/chatwire ./internal/localsocket ./internal/chatui ./cmd/waffle -run 'TestChatSocketEndToEnd|TestChatTUIPTY|TestConcurrent|TestDisconnect|TestServeChat' -count=10
~~~

Expected: PASS ten times with no race, timeout, goroutine leak assertion, or secret canary in output.

- [ ] **Step 5: Commit**

~~~sh
git add go.mod go.sum cmd/waffle/chat_socket_integration_test.go cmd/waffle/chat_tui_pty_test.go cmd/waffle internal/chatwire internal/localsocket internal/chatui
git commit -m "test: cover managed chat socket and TUI"
~~~

---

### Task 9: Document, Visually Inspect, and Gate the Waffle Release

**Files:**
- Create: docs/chat.md
- Modify: README.md
- Modify: docs/deploy.md
- Modify: docs/plan.md
- Modify: config.example.toml
- Modify: cmd/waffle/main.go
- Create: cmd/waffle/main_test.go

**Interfaces:**
- Consumes: final flags, command registry, protocol behavior, systemd unit names from the companion Infra plan
- Produces: user/operator documentation and release evidence ready for the Infra rollout

- [ ] **Step 1: Write failing documentation/help assertions**

Assert top-level help documents chat flags --continue, --profile, --socket, and --plain. Add a documentation contract test that docs/chat.md contains all canonical commands and aliases from chat.Commands(), plus /run/waffle/chat.sock, waffle-chat.socket, NO_COLOR, direct mode, and explicit no-fallback behavior.

- [ ] **Step 2: Verify RED**

~~~sh
go test ./cmd/waffle -run 'TestHelp.*Chat|TestChatDocumentation' -count=1
~~~

Expected: missing flags and docs/chat.md.

- [ ] **Step 3: Write the shipped chat guide and update existing docs**

docs/chat.md must include:

- ordinary managed-host use: waffle chat, with no sudo;
- direct local mode and explicit --socket/WAFFLE_CHAT_SOCKET precedence;
- every command and alias with exact argument syntax;
- keyboard controls, multiline input, cancellation, overlays, and plain mode;
- per-session /model behavior and /resume restoration;
- service/socket troubleshooting using systemctl status waffle.service waffle-chat.socket and journalctl, never sudo waffle chat;
- security boundary: socket clients do not read service credentials/config/database and access is controlled by /run/waffle/chat.sock ownership/mode;
- rollback compatibility: old clients cannot speak to an incompatible protocol and fail with a concise version error.

Include one sanitized text terminal capture of the Focused Conversation layout generated from the accepted golden fixture; do not include provider output, identities, paths to private state, or raw test failure artifacts.

Link it from README.md. Update docs/deploy.md with the socket activation order and operator smoke command. Replace the docs/plan.md scanner-Repl/Bubble-Tea-deferred statements with the delivered TUI and plain fallback. Add commented [chat] socket configuration to config.example.toml.

- [ ] **Step 4: Verify docs/help GREEN**

~~~sh
go test ./cmd/waffle -run 'TestHelp.*Chat|TestChatDocumentation' -count=1
~~~

Expected: PASS.

- [ ] **Step 5: Perform the manual visual acceptance pass**

Run a local deterministic backend and inspect at 120x36, 80x24, and 58x24 in both dark and light terminal themes. Exercise a long streamed answer, tool success/failure, command palette, /models, /sessions, resize, Escape cancellation, multiline compose, NO_COLOR=1, and /exit. Use the accepted sanitized text capture in docs/chat.md; do not commit transient test-failure captures, binary screenshots, or credentials. Fix clipping, unreadable contrast, cursor loss, or broken focus before continuing.

- [ ] **Step 6: Run the complete Waffle gate**

~~~sh
mise run fmt
mise run test
mise run vet
mise run lint
mise run build
git diff --check
git status --short
~~~

Expected: every command exits 0; only the intentional Waffle changes plus the pre-existing untracked .gitattributes and .pnpm-store remain.

- [ ] **Step 7: Commit documentation and record the Waffle revision**

~~~sh
git add README.md docs/chat.md docs/deploy.md docs/plan.md config.example.toml cmd/waffle/main.go cmd/waffle/main_test.go
git commit -m "docs: explain secure managed chat"
git rev-parse HEAD
~~~

Record the SHA in the Infra rollout evidence. Do not declare the overall goal complete until the companion Infra plan is implemented and the managed-host smoke tests pass.
