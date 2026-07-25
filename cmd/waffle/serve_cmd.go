package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/matt-riley/waffle/internal/agent"
	"github.com/matt-riley/waffle/internal/broker"
	"github.com/matt-riley/waffle/internal/channel"
	"github.com/matt-riley/waffle/internal/channel/telegram"
	chatpkg "github.com/matt-riley/waffle/internal/chat"
	"github.com/matt-riley/waffle/internal/chatwire"
	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/dashboard"
	"github.com/matt-riley/waffle/internal/entity"
	"github.com/matt-riley/waffle/internal/gateway"
	"github.com/matt-riley/waffle/internal/intake"
	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/llm/anthropicp"
	"github.com/matt-riley/waffle/internal/llm/openaip"
	"github.com/matt-riley/waffle/internal/localsocket"
	"github.com/matt-riley/waffle/internal/memory"
	"github.com/matt-riley/waffle/internal/observability"
	"github.com/matt-riley/waffle/internal/providerconfig"
	"github.com/matt-riley/waffle/internal/schedule"
	"github.com/matt-riley/waffle/internal/secret"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/skill"
	"github.com/matt-riley/waffle/internal/store"
	usagepkg "github.com/matt-riley/waffle/internal/usage"
	"github.com/matt-riley/waffle/internal/workset"
	"github.com/matt-riley/waffle/internal/workspace"
)

const serveUsage = "Usage: waffle serve\n\n" +
	"Start the Waffle gateway daemon (Telegram, chat socket, cron, lifecycle).\n" +
	"Configuration is read from $WAFFLE_HOME/config.toml (default ~/.waffle).\n\n" +
	"Options:\n" +
	"  -h, --help    show this help\n"

func serveCmd(ctx context.Context, args []string, stderr io.Writer) error {
	return serveCmdWithAdapterFactory(ctx, args, stderr, configuredAdapters)
}

type adapterFactory func(config.Config) ([]channel.Adapter, error)

var serveChat = chatwire.Serve
var openChatListener = localsocket.Listener
var dashboardRandom io.Reader = rand.Reader

// serveCmdWithAdapterFactory runs the serve command with an explicit adapter
// factory so command lifecycle tests can use an in-memory channel.
func serveCmdWithAdapterFactory(ctx context.Context, args []string, stderr io.Writer, makeAdapters adapterFactory) (err error) {
	// Resolve help before any ownership, listeners, or background work (#127).
	for _, arg := range args {
		if arg == "-h" || arg == "--help" {
			_, err := io.WriteString(stderr, serveUsage)
			return err
		}
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancelOwnership := context.WithCancel(ctx)
	defer cancelOwnership()
	owner, err := acquireServeOwner(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if releaseErr := owner.Release(); err == nil {
			err = releaseErr
		}
	}()
	ownerLoss := make(chan error, 1)
	go func() {
		select {
		case loss := <-owner.Errors():
			ownerLoss <- loss
			cancelOwnership()
		case <-ctx.Done():
		}
	}()
	defer func() {
		select {
		case loss := <-ownerLoss:
			err = fmt.Errorf("serve ownership lost: %w", loss)
		default:
		}
	}()

	cfg, st, err := openConfigAndStore(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := st.Close(); err == nil {
			err = cerr
		}
	}()
	log := slog.New(slog.NewTextHandler(stderr, nil))
	sessionOwners := newChatSessionOwners()
	runtimeFactory := func(runtimeCtx context.Context) (chatpkg.Backend, error) {
		runtime, runtimeErr := newChatRuntime(runtimeCtx, cfg, st)
		if runtimeErr == nil {
			runtime.sessionOwners = sessionOwners
		}
		return runtime, runtimeErr
	}

	statusListener, err := net.Listen("tcp", cfg.Gateway.StatusListen)
	if err != nil {
		return fmt.Errorf("status listener cannot bind %s: %w", cfg.Gateway.StatusListen, err)
	}
	statusListener = &cachedCloseListener{Listener: statusListener}
	defer func() { _ = statusListener.Close() }()
	obs := observability.New(st, nil)
	statusMux := http.NewServeMux()
	observability.RegisterRoutes(statusMux, obs)
	statusHandler := http.Handler(statusMux)
	var dashboardSecurity *dashboard.Security
	var dashboardHub *dashboard.EventHub
	var dashboardClients *dashboard.ChatClients
	var dashboardGeneration string
	if cfg.Dashboard.Enabled {
		security, err := dashboard.NewSecurity(cfg.Gateway.StatusListen, dashboard.TailnetOptions{
			Enabled:       cfg.Dashboard.Tailnet.Enabled,
			ServeHost:     cfg.Dashboard.Tailnet.ServeHost,
			AllowedLogins: cfg.Dashboard.Tailnet.AllowedLogins,
		}, dashboardRandom)
		if err != nil {
			return fmt.Errorf("dashboard security: %w", err)
		}
		// Desk mutations write to the shared policy_audit table (#152).
		security.SetPolicyAuditDB(st.DB)
		if cfg.Dashboard.Tailnet.Enabled {
			// Log the login, never the allowlist: a mismatch is otherwise an
			// unexplained 403 on a surface with no other diagnostic.
			security.SetLoginRejectionObserver(func(login string) {
				log.Warn("desk tailnet login rejected", "login", login)
			})
			log.Info("desk tailnet access enabled",
				"serve_host", cfg.Dashboard.Tailnet.ServeHost,
				"allowed_logins", len(cfg.Dashboard.Tailnet.AllowedLogins))
		}
		dashboardSecurity = security
		dashboardGeneration, err = newDashboardProcessGeneration(dashboardRandom)
		if err != nil {
			return err
		}
		dashboardHub = dashboard.NewEventHub(256)
		dashboardClients = dashboard.NewChatClients(runtimeFactory, rand.Reader)
		dashboardClients.SetEventHub(dashboardHub)
		secretStore, openErr := secret.TryOpen()
		if openErr != nil {
			log.Warn("dashboard chat secret redactor unavailable", "err", openErr)
		} else if redactor, redactorErr := secret.NewRedactor(secretStore); redactorErr != nil {
			log.Warn("dashboard chat secret redactor unavailable", "err", redactorErr)
		} else {
			dashboardClients.SetRedactor(redactor.Redact)
		}
	}

	sessions := session.New(st)
	entities := entity.New(st, sessions)
	scheduleStore := schedule.NewStore(st)
	usageStore := usagepkg.New(st)
	wsStore := &workset.Store{DB: st.DB}

	ws, skills, err := loadWorkspaceWithStore(st)
	if err != nil {
		return err
	}
	notesIndex := &memory.NotesIndex{DB: st.DB}
	ws.Notes = notesIndex
	if err := notesIndex.SyncWorkspace(ctx, memory.DefaultAgent, ws); err != nil {
		return fmt.Errorf("sync memory search index: %w", err)
	}
	chatListener, _, err := openChatListener(cfg.Chat.Socket)
	if err != nil {
		return fmt.Errorf("chat listener: %w", err)
	}
	if chatListener != nil {
		chatListener = &cachedCloseListener{Listener: chatListener}
		defer func() {
			closeErr := chatListener.Close()
			if err == nil && closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
				err = fmt.Errorf("chat listener cleanup: %w", closeErr)
			}
		}()
	}
	agents, cronAgent, profilesMain, profilesGroup, profilesCron, cleanup, err := buildGatewayAgents(ctx, cfg, ws, skills, sessions)
	if err != nil {
		cleanup()
		return err
	}
	defer cleanup()

	var serveBroker *broker.Broker
	var brokerDone <-chan struct{}
	if cfg.Broker.Listen != "" {
		// Bind synchronously so a busy address fails startup instead of
		// logging "credential broker up" and continuing with a dead broker (#99).
		ln, err := net.Listen("tcp", cfg.Broker.Listen)
		if err != nil {
			return fmt.Errorf("credential broker cannot bind %s: %w", cfg.Broker.Listen, err)
		}
		upstreams := brokerUpstreams(cfg)
		b := broker.New(st, upstreams)
		if err := configureWorkspaceBroker(cfg, st, b); err != nil {
			_ = ln.Close()
			return err
		}
		serveBroker = b
		b.Usage = usageStore
		b.Limits = brokerLimits(cfg, config.GroupMain)
		done := make(chan struct{})
		brokerDone = done
		go func() {
			defer close(done)
			if err := b.ServeListener(ctx, ln); err != nil {
				log.Error("broker stopped", "err", err)
			}
		}()
		log.Info("credential broker up", "listen", cfg.Broker.Listen, "upstreams", len(upstreams))
	}

	adapters, err := makeAdapters(cfg)
	if err != nil {
		return err
	}
	for _, adapter := range adapters {
		obs.RegisterAdapter(adapter.Name())
		if telegramAdapter, ok := adapter.(*telegram.Adapter); ok {
			telegramAdapter.SetPollObserver(func() { obs.MarkAdapter(telegramAdapter.Name()) })
		}
	}
	chatDone := make(chan error, 1)
	if chatListener == nil {
		chatDone <- nil
	} else {
		audit := newChatAudit(log, localsocket.PeerCredentials)
		go func() {
			serveErr := serveChat(ctx, chatListener, runtimeFactory, audit)
			if serveErr != nil && !errors.Is(serveErr, context.Canceled) {
				cancelOwnership()
			}
			chatDone <- serveErr
		}()
	}
	// Lifecycle sweeps are owned by this serve process only. CLI commands
	// perform explicit operations but never start a background reaper.
	lifecycleCtx, lifecycleCancel := context.WithCancel(ctx)
	defer lifecycleCancel()
	wsManager := newWorkspaceManager(cfg, st, serveBroker)
	configureServeWorkspaceManager(cfg, wsManager, serveBrokerURL(cfg.Broker.Listen))

	gw := &gateway.Gateway{
		Agent:             agents[config.GroupMain],
		Agents:            agents,
		Profiles:          profilesMain,
		GroupProfiles:     profilesGroup,
		Entities:          entities,
		Sessions:          sessions,
		Adapters:          adapters,
		Log:               log,
		Observability:     obs,
		Usage:             usageStore,
		ReflectEveryTurns: cfg.Memory.ReflectEveryTurns,
	}
	gatewayDone := make(chan error, 1)
	go func() {
		log.Info("waffle gateway starting", "channels", len(adapters))
		gatewayDone <- gw.Run(ctx)
	}()
	gatewayWaited := false
	defer func() {
		if gatewayWaited {
			return
		}
		stop()
		<-gatewayDone
	}()

	// Start the scheduler before deferred provider transactions are finalized:
	// /healthz is not ready until the scheduler's initial reconciliation tick
	// and configured adapters have had an opportunity to report a healthy poll.
	sched := &schedule.Scheduler{
		Store: scheduleStore,
		Runner: &schedule.Runner{
			Agent:           cronAgent,
			AgentsByProfile: profilesCron,
			Sessions:        sessions,
			Deliverer:       adapterDeliverer(adapters),
			Log:             log,
			Observability:   obs,
			Learn: func(ctx context.Context) (string, error) {
				return learnDigest(ctx, cfg, st)
			},
		},
		Log:    log,
		Usage:  usageStore,
		Health: obs,
		Policy: schedule.RetryPolicy{
			MaxAttempts:  cfg.Jobs.MaxAttempts,
			BaseBackoff:  mustDuration(cfg.Jobs.BaseBackoff),
			MaxBackoff:   mustDuration(cfg.Jobs.MaxBackoff),
			StallTimeout: mustDuration(cfg.Jobs.StallTimeout),
		},
	}
	schedDone := make(chan error, 1)
	go func() {
		serr := sched.Run(ctx)
		if serr != nil {
			log.Error("scheduler stopped", "err", serr)
		}
		schedDone <- serr
	}()
	schedulerWaited := false
	defer func() {
		if schedulerWaited {
			return
		}
		stop()
		<-schedDone
	}()

	var dashboardProviders *providerconfig.Manager
	if cfg.Dashboard.Enabled {
		dashboardProviders, err = defaultDashboardProviderManager()
		if err != nil {
			return fmt.Errorf("dashboard provider manager: %w", err)
		}
		catalogue, err := defaultProviderCatalogue()
		if err != nil {
			return fmt.Errorf("dashboard provider catalogue: %w", err)
		}
		capabilities, err := newDashboardCapabilities(cfg, st, ws, sessions, dashboardProviders, catalogue)
		if err != nil {
			return fmt.Errorf("dashboard capabilities: %w", err)
		}
		operations := &dashboard.Operations{
			Runs:       obs,
			Jobs:       scheduleStore,
			Workspaces: wsManager,
			Sessions:   sessions,
			Notes:      notesIndex,
			Workset:    wsStore,
			Usage:      usageStore,
			Previews:   dashboard.NewPreviewStore(time.Now, rand.Reader),
			Events:     dashboardHub,
			Now:        time.Now,
		}
		restart := dashboardRestartScheduler()
		dashboard.RegisterRoutes(statusMux, dashboard.APIConfig{
			Observability:   obs,
			Security:        dashboardSecurity,
			Hub:             dashboardHub,
			ChatClients:     dashboardClients,
			Idempotency:     dashboard.NewIdempotencyStore(time.Now, 512, 10*time.Minute),
			Operations:      operations,
			Schedules:       scheduleStore,
			Memory:          ws,
			WorkspaceEgress: cfg.Workspace.Egress,
			Capabilities:    capabilities,
			Restart:         restart,
			RestartOutcome: func(outcome dashboard.RestartScheduleOutcome) {
				log.Info("dashboard restart outcome",
					"scheduled", outcome.Scheduled,
					"code", outcome.Code,
					"message", outcome.Message,
				)
			},
			Version:           version,
			ProcessGeneration: dashboardGeneration,
			Now:               time.Now,
		})
		dashboard.RegisterConnectionsRoutes(statusMux, dashboard.NewConnectionSource(cfg, obs))
		statusHandler = dashboardSecurity.Wrap(statusHandler)
	}
	statusDone := make(chan error, 1)
	go func() {
		if err := observability.ServeHandler(ctx, statusListener, statusHandler); err != nil {
			log.Error("status listener stopped", "err", err)
			statusDone <- err
			return
		}
		statusDone <- nil
	}()
	defer func() {
		stop()
		<-statusDone
	}()
	if dashboardClients != nil {
		defer func() {
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer shutdownCancel()
			if shutdownErr := dashboardClients.Shutdown(shutdownCtx); err == nil && shutdownErr != nil {
				err = fmt.Errorf("dashboard chat shutdown: %w", shutdownErr)
			}
		}()
	}
	if dashboardProviders != nil {
		if err := dashboardProviders.FinalizeDeferred(ctx); err != nil {
			return fmt.Errorf("finalize deferred dashboard capability transaction: %w", err)
		}
	}
	idleTimeout := parseOptionalDuration(cfg.Workspace.IdleTimeout)
	if idleTimeout > 0 {
		wsManager.IdleTimeout = idleTimeout
	}
	closeTTL := parseOptionalDuration(cfg.Workspace.CloseTTL)
	wsReaper := &workspace.Reaper{Manager: wsManager, IdleTimeout: idleTimeout, CloseTTL: closeTTL, Notify: func(notifyCtx context.Context, ws workspace.Workspace, msg string) error {
		target, ok, err := entities.TargetForSession(notifyCtx, ws.SessionID)
		if err != nil {
			return err
		}
		if ok {
			if err := adapterDeliverer(adapters).Deliver(notifyCtx, target, msg); err != nil {
				return err
			}
		} else {
			log.Warn("workspace retained after TTL", "repo", ws.Repo, "message", msg)
		}
		return nil
	}}
	retention := session.RetentionSweep{Store: sessions, Retain: parseOptionalDuration(cfg.Store.Retain)}
	go func() {
		tick := time.NewTicker(time.Minute)
		defer tick.Stop()
		for {
			select {
			case <-lifecycleCtx.Done():
				return
			case <-tick.C:
				if err := wsReaper.Sweep(lifecycleCtx); err != nil {
					log.Error("workspace lifecycle sweep failed", "err", err)
				}
				if _, err := retention.Sweep(lifecycleCtx); err != nil {
					log.Error("retention sweep failed", "err", err)
				}
				// Working-set maintenance: drop unpinned model assumptions older than 7d (#70).
				if n, err := wsStore.DropStaleAssumptionsAll(lifecycleCtx, 7*24*time.Hour); err != nil {
					log.Error("working set maintenance failed", "err", err)
				} else if n > 0 {
					log.Info("working set maintenance", "dropped", n)
				}
			}
		}
	}()

	// Idle reflection: summarize sessions that went quiet without a finish pass (#59).
	// reflect_after = "0" or empty disables; when armed, holds the same group
	// lock as message handling (skip if the conversation is busy).
	if after := parseOptionalDuration(cfg.Memory.ReflectAfter); after > 0 {
		every := parseOptionalDuration(cfg.Memory.ReflectEvery)
		if every <= 0 {
			every = 5 * time.Minute
		}
		mainAgent := agents[config.GroupMain]
		reflector := &session.IdleReflector{
			Sessions: sessions,
			After:    after,
			Every:    every,
			OnError:  func(err error) { log.Warn("idle reflection", "err", err) },
			TryLockSession: func(lockCtx context.Context, sessionID string) (func(), bool) {
				return gw.TryLockSession(lockCtx, sessionID)
			},
			Provider: func() (llm.Provider, string) {
				if mainAgent == nil {
					return nil, ""
				}
				model := mainAgent.Model
				if mainAgent.UtilityModel != "" {
					model = mainAgent.UtilityModel
				}
				return mainAgent.Provider, model
			},
		}
		go reflector.Loop(lifecycleCtx)
		log.Info("idle reflection armed", "after", after, "every", every)
	}

	// Issue intake watchers: board-driven dispatch under the restricted issue
	// tier. Owned exclusively by this serve process (#48 / #51).
	intakeDone := make(chan struct{})
	go func() {
		defer close(intakeDone)
		runIntakeWatchers(lifecycleCtx, cfg, st, sessions, ws, skills, agents, serveBroker, adapterDeliverer(adapters), log)
	}()

	err = <-gatewayDone
	gatewayWaited = true

	// Stop the scheduler and wait for its in-flight-job drain before the
	// deferred cleanup tears down the shared sandbox executor and MCP
	// clients a running cron job may still be using. Also join the credential
	// broker so a fast restart does not hit "address already in use" (#109).
	chatErr := waitForServeWorkers(stop, lifecycleCancel, schedDone, intakeDone, chatDone, brokerDone)
	schedulerWaited = true
	if chatErr != nil && !errors.Is(chatErr, context.Canceled) {
		return fmt.Errorf("chat server: %w", chatErr)
	}
	return err
}

// waitForServeWorkers preserves shutdown ordering: stop accepting/scheduling,
// then wait for cron.Stop's in-flight-job drain, intake, chat, and the
// credential broker (when started) before deferred cleanup closes the shared
// sandbox executor and MCP clients. brokerDone may be nil when the broker was
// not started.
func waitForServeWorkers(stop, lifecycleCancel context.CancelFunc, schedDone <-chan error, intakeDone <-chan struct{}, chatDone <-chan error, brokerDone <-chan struct{}) error {
	stop()
	lifecycleCancel()
	<-schedDone
	<-intakeDone
	chatErr := <-chatDone
	if brokerDone != nil {
		<-brokerDone
	}
	return chatErr
}

type peerCredentialLookup func(net.Conn) (localsocket.Peer, error)

type cachedCloseListener struct {
	net.Listener
	once sync.Once
	err  error
}

func (l *cachedCloseListener) Close() error {
	l.once.Do(func() { l.err = l.Listener.Close() })
	return l.err
}

// newChatAudit intentionally records only lifecycle and numeric kernel
// identity. Credential lookup failures are useful operational signals, but
// their raw errors may contain connection or host details and are not logged.
func newChatAudit(log *slog.Logger, lookup peerCredentialLookup) chatwire.AuditFunc {
	return func(_ context.Context, conn net.Conn, event string) {
		peer, err := lookup(conn)
		if err != nil || !peer.Available {
			log.Warn("chat connection", "event", event, "peer_credentials", "unavailable")
			return
		}
		log.Info("chat connection", "event", event, "pid", peer.PID, "uid", peer.UID, "gid", peer.GID)
	}
}

func runIntakeWatchers(ctx context.Context, cfg config.Config, st *store.Store, sessions *session.Store, memWS memory.Workspace, skills []skill.Skill, agents map[string]*agent.Agent, b *broker.Broker, deliver schedule.Deliverer, log *slog.Logger) {
	if len(cfg.Intake.GitHub) == 0 {
		<-ctx.Done()
		return
	}
	issueAgent, issueCleanup, err := ensureIssueAgent(ctx, cfg, memWS, skills, sessions, agents)
	if err != nil {
		log.Error("issue agent build failed", "err", err)
		<-ctx.Done()
		return
	}
	defer issueCleanup()

	var brokerURL string
	if b != nil && cfg.Broker.Listen != "" {
		if _, port, err := net.SplitHostPort(cfg.Broker.Listen); err == nil {
			brokerURL = "http://waffle-host:" + port
		}
	}
	disp := &issueDispatcher{
		cfg: cfg, st: st, sessions: sessions, skills: skills, memWS: memWS,
		broker: b, brokerURL: brokerURL, agent: issueAgent, log: log,
	}
	claims := &intake.ClaimStore{DB: st.DB}
	var wg sync.WaitGroup
	for _, w := range cfg.Intake.GitHub {
		wc := intake.WatchConfig{
			Repo:           w.Repo,
			Label:          w.Label,
			MaxConcurrency: w.MaxConcurrency,
			Deliver:        w.Deliver,
			PollInterval:   parseOptionalDuration(w.PollInterval),
		}
		if wc.MaxConcurrency < 1 {
			wc.MaxConcurrency = 1
		}
		token, _ := resolveSecretValue(w.Token, "GITHUB_TOKEN")
		tr := &intake.GitHubTracker{Token: token}
		if cfg.GitHub.App.BaseURL != "" {
			// Tests / GHES: reuse app base as API root when set.
			tr.BaseURL = strings.TrimRight(cfg.GitHub.App.BaseURL, "/")
		}
		watcher := &intake.Watcher{
			Config:     wc,
			Tracker:    tr,
			Claims:     claims,
			Dispatcher: disp,
			Deliverer:  deliver,
			Log:        log.With("intake_repo", w.Repo),
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			log.Info("issue intake watcher starting", "repo", w.Repo, "label", w.Label)
			if err := watcher.Run(ctx); err != nil && ctx.Err() == nil {
				log.Error("issue intake watcher stopped", "repo", w.Repo, "err", err)
			}
		}()
	}
	wg.Wait()
}

func mustDuration(s string) time.Duration {
	d, _ := config.ParseDuration(s)
	return d
}

func parseOptionalDuration(s string) time.Duration {
	if s == "" || s == "0" {
		return 0
	}
	d, _ := config.ParseDuration(s)
	return d
}

func configuredAdapters(cfg config.Config) ([]channel.Adapter, error) {
	var adapters []channel.Adapter
	if cfg.Channel.Telegram.Enabled {
		token, err := resolveSecretValue(cfg.Channel.Telegram.Token, "TELEGRAM_BOT_TOKEN")
		if err != nil {
			return nil, fmt.Errorf("telegram: %w", err)
		}
		if token == "" {
			return nil, errors.New("telegram enabled but no token: store one with `waffle secret set telegram/bot-token` or set TELEGRAM_BOT_TOKEN")
		}
		adapters = append(adapters, telegram.New(token, cfg.Channel.Telegram.BaseURL))
	}
	return adapters, nil
}

// buildGatewayAgents constructs every agent tier the gateway can route to,
// the cron agent used by the scheduler, and named profile agents for channel
// (main + multiparty group) and cron surfaces (#71). The cleanup callback
// closes successfully and partially-built agents in reverse build order.
func buildGatewayAgents(ctx context.Context, cfg config.Config, ws memory.Workspace, skills []skill.Skill, sessions *session.Store) (
	agents map[string]*agent.Agent,
	cronAgent *agent.Agent,
	profilesMain, profilesGroup, profilesCron map[string]*agent.Agent,
	cleanup func(),
	err error,
) {
	return buildGatewayAgentsWithRuntime(ctx, cfg, ws, skills, sessions, newModelRuntimeResolver(cfg))
}

func buildGatewayAgentsWithRuntime(ctx context.Context, cfg config.Config, ws memory.Workspace, skills []skill.Skill, sessions *session.Store, runtime *modelRuntimeResolver) (
	agents map[string]*agent.Agent,
	cronAgent *agent.Agent,
	profilesMain, profilesGroup, profilesCron map[string]*agent.Agent,
	cleanup func(),
	err error,
) {
	agents = make(map[string]*agent.Agent)
	profilesMain = make(map[string]*agent.Agent)
	profilesGroup = make(map[string]*agent.Agent)
	profilesCron = make(map[string]*agent.Agent)
	var cleanups []func()
	cleanup = func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}

	build := func(group string) (*agent.Agent, error) {
		a, closer, err := buildAgentWithProfileRuntime(ctx, cfg, ws, skills, sessions, group, "", runtime)
		cleanups = append(cleanups, closer)
		if err != nil {
			return nil, err
		}
		agents[group] = a
		return a, nil
	}

	if _, err := build(config.GroupMain); err != nil {
		return nil, nil, nil, nil, nil, cleanup, err
	}
	cronAgent, err = build(config.GroupCron)
	if err != nil {
		return nil, nil, nil, nil, nil, cleanup, err
	}
	// Group chats always get the restricted multi-party tier (#34).
	if _, err := build(config.GroupGroup); err != nil {
		return nil, nil, nil, nil, nil, cleanup, err
	}
	// Issue intake uses the restricted issue tier (#51); build it when any
	// watcher is configured so toolbox policy is ready before the first tick.
	if len(cfg.Intake.GitHub) > 0 {
		if _, err := build(config.GroupIssue); err != nil {
			return nil, nil, nil, nil, nil, cleanup, err
		}
	}

	groups := make([]string, 0, len(cfg.Agent.Groups))
	for group := range cfg.Agent.Groups {
		if group != config.GroupMain && group != config.GroupCron && group != config.GroupIssue && group != config.GroupGroup {
			groups = append(groups, group)
		}
	}
	sort.Strings(groups)
	for _, group := range groups {
		if _, err := build(group); err != nil {
			return nil, nil, nil, nil, nil, cleanup, err
		}
	}

	// Named profiles for main, multiparty group, and cron surfaces (#71).
	// Built once at serve start so binds only select, never rebuild mid-run.
	// Group-tier profiles inherit the restricted multiparty toolbox so a
	// channel bind cannot widen past #34 trust tiering.
	profileNames := make([]string, 0, len(cfg.Agent.Profiles))
	for name := range cfg.Agent.Profiles {
		if name == "" || name == "main" {
			continue
		}
		profileNames = append(profileNames, name)
	}
	sort.Strings(profileNames)
	profileTiers := []struct {
		group string
		label string
		dest  map[string]*agent.Agent
	}{
		{config.GroupMain, "main", profilesMain},
		{config.GroupGroup, "group", profilesGroup},
		{config.GroupCron, "cron", profilesCron},
	}
	for _, name := range profileNames {
		for _, tier := range profileTiers {
			a, closer, err := buildAgentWithProfileRuntime(ctx, cfg, ws, skills, sessions, tier.group, name, runtime)
			cleanups = append(cleanups, closer)
			if err != nil {
				return nil, nil, nil, nil, nil, cleanup, fmt.Errorf("profile %q (%s): %w", name, tier.label, err)
			}
			tier.dest[name] = a
		}
	}

	return agents, cronAgent, profilesMain, profilesGroup, profilesCron, cleanup, nil
}

// adapterDeliverer routes a job's "channel:chat_id" target to the matching
// channel adapter.
type adapterDeliverer []channel.Adapter

func (ads adapterDeliverer) Deliver(ctx context.Context, target, text string) error {
	name, chatID, ok := schedule.ParseTarget(target)
	if !ok {
		return fmt.Errorf("bad delivery target %q (want channel:chat_id)", target)
	}
	for _, a := range ads {
		if a.Name() == name {
			return a.Send(ctx, chatID, text)
		}
	}
	return fmt.Errorf("no channel %q for delivery", name)
}

// brokerUpstreams assembles the LLM upstreams the broker can front, using
// the same key resolution as the agent itself.
func brokerUpstreams(cfg config.Config) []broker.Upstream {
	return brokerUpstreamsWithSecretResolver(cfg, resolveProviderConnectionSecret)
}

func brokerUpstreamsWithSecretResolver(cfg config.Config, secrets secretResolver) []broker.Upstream {
	source := cfg.ProviderRegistrySource()
	connections := cfg.Providers
	legacyFallback := source == config.ProviderRegistryLegacy
	if source == config.ProviderRegistryNone && cfg.Provider.Name != "" {
		legacyFallback = true
		connections = map[string]config.ProviderConnection{
			"default": {
				Type:      cfg.Provider.Name,
				APIKey:    cfg.Provider.APIKey,
				BaseURL:   cfg.Provider.BaseURL,
				MaxTokens: cfg.Provider.MaxTokens,
			},
		}
	}
	names := make([]string, 0, len(connections))
	for name := range connections {
		names = append(names, name)
	}
	sort.Strings(names)
	ups := make([]broker.Upstream, 0, len(names))
	for _, name := range names {
		connection := connections[name]
		if legacyFallback && connection.APIKey == "" {
			connection.APIKey = envKey(connection.Type)
		}
		key, _, err := secrets(connection)
		if err != nil {
			continue
		}
		base := connection.BaseURL
		header, value := "", ""
		switch connection.Type {
		case "anthropic":
			if base == "" {
				base = anthropicp.DefaultBaseURL
			}
			if key != "" {
				header, value = "x-api-key", key
			}
		case "openai":
			if base == "" {
				base = openaip.DefaultBaseURL
			}
			if key != "" {
				header, value = "Authorization", "Bearer "+key
			}
		default:
			continue
		}
		ups = append(ups, broker.Upstream{Name: name, BaseURL: base, Header: header, Value: value})
	}
	return ups
}

// resolveSecretValue resolves a config value that may be a secret://
// reference, falling back to envVar when unset or when the store has no
// such secret. Delegates to the shared helper in internal/secret.
func resolveSecretValue(ref, envVar string) (string, error) {
	return secret.ResolveRef(ref, envVar)
}
