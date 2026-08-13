package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/matt-riley/waffle/internal/agent"
	"github.com/matt-riley/waffle/internal/agentbuild"
	"github.com/matt-riley/waffle/internal/apiface"
	"github.com/matt-riley/waffle/internal/broker"
	chatpkg "github.com/matt-riley/waffle/internal/chat"
	"github.com/matt-riley/waffle/internal/chatwire"
	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/gitcred"
	"github.com/matt-riley/waffle/internal/mcp"
	"github.com/matt-riley/waffle/internal/memory"
	"github.com/matt-riley/waffle/internal/secret"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/skill"
	"github.com/matt-riley/waffle/internal/store"
	"github.com/matt-riley/waffle/internal/tool"
	"golang.org/x/term"
)

const (
	chatUsageLine = "waffle chat [-c|--continue] [--profile name] [--socket absolute-path] [--plain]"
	chatUsage     = "Usage: " + chatUsageLine + "\n\n" +
		"Options:\n" +
		"  -c, --continue         continue the latest session\n" +
		"      --profile name     use an agent profile\n" +
		"      --socket path      connect to an absolute Unix socket path\n" +
		"      --plain            use deterministic plain-text mode\n" +
		"  -h, --help             show this help\n"
)

// chat remains an alias for focused legacy tests. All behavior is owned by
// chatRuntime; no renderer-specific state lives here.
type chat = chatRuntime

type chatOptions struct {
	Continue bool
	Profile  string
	Socket   string
	Plain    bool
	Help     bool
}

func parseChatOptions(args []string, socketEnv string) (chatOptions, error) {
	for _, arg := range args {
		if arg == "-h" || arg == "--help" {
			return chatOptions{Help: true}, nil
		}
	}

	var options chatOptions
	explicitSocket := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-c" || arg == "--continue":
			options.Continue = true
		case arg == "--profile":
			if i+1 >= len(args) {
				return chatOptions{}, fmt.Errorf("usage: %s", chatUsageLine)
			}
			i++
			options.Profile = strings.TrimSpace(args[i])
		case strings.HasPrefix(arg, "--profile="):
			options.Profile = strings.TrimSpace(strings.TrimPrefix(arg, "--profile="))
		case arg == "--socket":
			explicitSocket = true
			if i+1 >= len(args) {
				return chatOptions{}, errors.New("chat socket path must be absolute")
			}
			i++
			options.Socket = args[i]
		case strings.HasPrefix(arg, "--socket="):
			explicitSocket = true
			options.Socket = strings.TrimPrefix(arg, "--socket=")
		case arg == "--plain":
			options.Plain = true
		default:
			return chatOptions{}, fmt.Errorf("usage: %s", chatUsageLine)
		}
	}
	if !explicitSocket && socketEnv != "" {
		options.Socket = socketEnv
	}
	if options.Socket != "" && !filepath.IsAbs(options.Socket) {
		return chatOptions{}, fmt.Errorf("chat socket path %q must be absolute", options.Socket)
	}
	if explicitSocket && options.Socket == "" {
		return chatOptions{}, errors.New("chat socket path must be absolute")
	}
	return options, nil
}

func shouldRunPlain(options chatOptions, stdin io.Reader, stdout io.Writer, isTerminal func(int) bool) bool {
	if options.Plain {
		return true
	}
	inFile, inputIsFile := stdin.(*os.File)
	outFile, outputIsFile := stdout.(*os.File)
	if !inputIsFile || !outputIsFile {
		return true
	}
	return !isTerminal(int(inFile.Fd())) || !isTerminal(int(outFile.Fd()))
}

func chatCmd(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (err error) {
	options, err := parseChatOptions(args, os.Getenv("WAFFLE_CHAT_SOCKET"))
	if err != nil {
		return err
	}
	if options.Help {
		_, err := io.WriteString(stdout, chatUsage)
		return err
	}

	backend, cleanup, err := openChatBackend(ctx, options)
	if err != nil {
		return err
	}
	defer func() {
		if cleanupErr := cleanup(); err == nil {
			err = cleanupErr
		}
	}()

	open := chatpkg.OpenOptions{
		Continue: options.Continue,
		Profile:  options.Profile,
	}
	if shouldRunPlain(options, stdin, stdout, term.IsTerminal) {
		return runPlainChat(ctx, backend, open, stdin, stdout, stderr)
	}
	return runTUIChat(ctx, backend, open, stdin, stdout)
}

func openChatBackend(ctx context.Context, options chatOptions) (chatpkg.Backend, func() error, error) {
	if options.Socket != "" {
		backend, err := chatwire.Dial(ctx, options.Socket)
		if err != nil {
			return nil, func() error { return nil }, fmt.Errorf(
				"connect to chat socket %q: %w; check waffle.service and waffle-chat.socket",
				options.Socket, err,
			)
		}
		return withConnectionMode(backend, "unix"), func() error { return nil }, nil
	}

	cfg, st, err := openConfigAndStore(ctx)
	if err != nil {
		return nil, func() error { return nil }, err
	}
	if len(cfg.Providers) == 0 {
		_ = st.Close()
		return nil, func() error { return nil }, errors.New("no provider configured; run `waffle setup` to get started")
	}

	backend, err := newChatRuntime(ctx, cfg, st)
	if err != nil {
		_ = st.Close()
		return nil, func() error { return nil }, err
	}
	return backend, st.Close, nil
}

type connectionModeBackend struct {
	chatpkg.Backend
	mode string
}

func withConnectionMode(backend chatpkg.Backend, mode string) chatpkg.Backend {
	return &connectionModeBackend{Backend: backend, mode: mode}
}

func (b *connectionModeBackend) Open(ctx context.Context, options chatpkg.OpenOptions) (chatpkg.State, error) {
	state, err := b.Backend.Open(ctx, options)
	state.ConnectionMode = b.mode
	return state, err
}

func (b *connectionModeBackend) Turn(ctx context.Context, input string, emit func(chatpkg.Event)) error {
	return b.Backend.Turn(ctx, input, b.withModeEmitter(emit))
}

func (b *connectionModeBackend) Command(ctx context.Context, command chatpkg.ParsedCommand, emit func(chatpkg.Event)) (chatpkg.Result, error) {
	result, err := b.Backend.Command(ctx, command, b.withModeEmitter(emit))
	if result.State != nil {
		state := *result.State
		state.ConnectionMode = b.mode
		result.State = &state
	}
	return result, err
}

func (b *connectionModeBackend) withModeEmitter(emit func(chatpkg.Event)) func(chatpkg.Event) {
	if emit == nil {
		return nil
	}
	return func(event chatpkg.Event) {
		if event.State != nil {
			state := *event.State
			state.ConnectionMode = b.mode
			event.State = &state
		}
		emit(event)
	}
}

// splitCommand splits an input line into its leading word and the trimmed
// remainder, so dispatch matches whole commands only — "/skills" is not
// "/skill" and "/report" is not "/repo".
func splitCommand(line string) (cmd, args string) {
	cmd, args, _ = strings.Cut(line, " ")
	return cmd, strings.TrimSpace(args)
}

func skillNames(skills []skill.Skill) string {
	if len(skills) == 0 {
		return "none"
	}
	names := make([]string, len(skills))
	for i, s := range skills {
		names[i] = s.Name
	}
	return strings.Join(names, ", ")
}

func openConfigAndStore(ctx context.Context) (config.Config, *store.Store, error) {
	cfgPath, err := config.Path()
	if err != nil {
		return config.Config{}, nil, err
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return config.Config{}, nil, err
	}
	dbPath, err := config.DBPath()
	if err != nil {
		return config.Config{}, nil, err
	}
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		return config.Config{}, nil, err
	}
	return cfg, st, nil
}

// apiBrokerWiring carries the running credential broker into agent builds
// so per-face API tools (internal/apiface, #254) and web_search (#245) can be
// offered. The zero value disables them; surfaces that run a broker (serve,
// ws) set it before any agent is built.
type apiBrokerWiring struct {
	broker *broker.Broker
	url    string
	faces  []apiface.Face
	redact func(string) string
	// search is the effective web_search provider metadata, or nil when no
	// [search] config exists (the tool is not offered).
	search *tool.WebSearchSpec
}

// buildAgent assembles the agent for an agent group (docs/plan.md trust
// tiering): the group's resolved policy decides where tools run (host vs
// docker) and which tools it may use. The returned cleanup stops any sandbox
// container; call it when done (it is never nil). The assembly itself lives
// in internal/agentbuild; these wrappers only supply package-main wiring
// (model runtime resolver, GitHub App construction) (#287).
func buildAgent(ctx context.Context, cfg config.Config, ws memory.Workspace, skills []skill.Skill, sessions *session.Store, group string, api apiBrokerWiring) (*agent.Agent, func(), error) {
	return buildAgentWithProfileRuntime(ctx, cfg, ws, skills, sessions, group, "", newModelRuntimeResolver(cfg), nil, api)
}

// buildAgentWithProfile is buildAgent with an optional named profile (#71).
func buildAgentWithProfile(ctx context.Context, cfg config.Config, ws memory.Workspace, skills []skill.Skill, sessions *session.Store, group, profileName string, api apiBrokerWiring) (*agent.Agent, func(), error) {
	return buildAgentWithProfileRuntime(ctx, cfg, ws, skills, sessions, group, profileName, newModelRuntimeResolver(cfg), nil, api)
}

type agentCleanupContext func(context.Context) error

func cleanupWithoutContext(cleanup agentCleanupContext) func() {
	return func() {
		if cleanup != nil {
			_ = cleanup(context.Background())
		}
	}
}

func buildAgentWithProfileContext(ctx context.Context, cfg config.Config, ws memory.Workspace, skills []skill.Skill, sessions *session.Store, group, profileName string, api apiBrokerWiring) (*agent.Agent, agentCleanupContext, error) {
	return buildAgentWithProfileRuntimeContext(ctx, cfg, ws, skills, sessions, group, profileName, newModelRuntimeResolver(cfg), nil, api)
}

func buildAgentWithProfileRuntime(ctx context.Context, cfg config.Config, ws memory.Workspace, skills []skill.Skill, sessions *session.Store, group, profileName string, runtime *modelRuntimeResolver, remoteEgress *mcp.RemoteEgress, api apiBrokerWiring) (*agent.Agent, func(), error) {
	built, cleanup, err := buildAgentWithProfileRuntimeContext(ctx, cfg, ws, skills, sessions, group, profileName, runtime, remoteEgress, api)
	return built, cleanupWithoutContext(cleanup), err
}

// buildAgentWithProfileRuntimeContext is the single entry point behind every
// agent construction in the binary: chat, serve, intake, and selfdev all
// call through here, and it delegates the composition to internal/agentbuild.
// remoteEgress is nil outside serve (or without a broker), which refuses
// remote MCP for docker-mode groups at build (#249).
func buildAgentWithProfileRuntimeContext(ctx context.Context, cfg config.Config, ws memory.Workspace, skills []skill.Skill, sessions *session.Store, group, profileName string, runtime *modelRuntimeResolver, remoteEgress *mcp.RemoteEgress, api apiBrokerWiring) (*agent.Agent, agentCleanupContext, error) {
	secrets, _ := secret.TryOpen() // nil without an identity; remote MCP tokens fail closed then
	builder := &agentbuild.Builder{
		Config:       cfg,
		Sessions:     sessions,
		Workspace:    ws,
		Skills:       skills,
		Runtime:      runtime,
		GitHubApp:    func() (*gitcred.App, error) { return newGitHubApp(cfg) },
		Secrets:      secrets,
		RemoteEgress: remoteEgress,
		Broker:       api.broker,
		BrokerURL:    api.url,
		APIFaces:     api.faces,
		APIRedact:    api.redact,
		Search:       api.search,
	}
	built, cleanup, err := builder.Build(ctx, group, profileName)
	if err != nil {
		return nil, agentCleanupContext(func(context.Context) error { return nil }), err
	}
	return built, agentCleanupContext(cleanup), nil
}

// resolveAPIKey turns the configured api_key into a real key: secret://
// references go through the secret store; an empty value falls back to the
// provider's conventional env var (which the Anthropic SDK also reads on
// its own). It also returns the redaction function when the store opens.
func resolveAPIKey(p config.Provider) (string, func(string) string, error) {
	key, err := secret.ResolveRef(p.APIKey, envName(p.Name))
	if err != nil {
		return "", nil, err
	}
	if key == "" && secret.IsRef(p.APIKey) {
		// No secret store (or notfound with no env) and ref was specified:
		// the ResolveRef for notfound case already errors with hint; this
		// path catches the no-store + empty-env case for the specific msg.
		return "", nil, fmt.Errorf("api_key is %q but no secret store is available: run `waffle secret init`, or set %s", p.APIKey, envName(p.Name))
	}
	if key == "" {
		key = envKey(p.Name)
	}
	// build redactor using conventional name even for env fallbacks
	store, _ := secret.TryOpen() // ignore err; redaction is best-effort here
	redact, _ := secret.RedactorFor(store, providerSecretName(p.Name), key)
	// RedactorFor errs only on store problems; swallow to nil like before
	return key, redact, nil
}

func envName(provider string) string {
	if provider == "openai" {
		return "OPENAI_API_KEY"
	}
	return "ANTHROPIC_API_KEY"
}

func envKey(provider string) string { return os.Getenv(envName(provider)) }

func providerSecretName(provider string) string {
	if provider == "openai" {
		return "openai/api-key"
	}
	return "anthropic/api-key"
}
