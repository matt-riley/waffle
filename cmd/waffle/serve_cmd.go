package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
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
	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/entity"
	"github.com/matt-riley/waffle/internal/gateway"
	"github.com/matt-riley/waffle/internal/intake"
	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/memory"
	"github.com/matt-riley/waffle/internal/observability"
	"github.com/matt-riley/waffle/internal/schedule"
	"github.com/matt-riley/waffle/internal/secret"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/skill"
	"github.com/matt-riley/waffle/internal/store"
	usagepkg "github.com/matt-riley/waffle/internal/usage"
	"github.com/matt-riley/waffle/internal/workset"
	"github.com/matt-riley/waffle/internal/workspace"
)

func serveCmd(ctx context.Context, stderr io.Writer) error {
	return serveCmdWithAdapterFactory(ctx, stderr, configuredAdapters)
}

type adapterFactory func(config.Config) ([]channel.Adapter, error)

// serveCmdWithAdapterFactory runs the serve command with an explicit adapter
// factory so command lifecycle tests can use an in-memory channel.
func serveCmdWithAdapterFactory(ctx context.Context, stderr io.Writer, makeAdapters adapterFactory) (err error) {
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

	statusListener, err := net.Listen("tcp", cfg.Gateway.StatusListen)
	if err != nil {
		return fmt.Errorf("status listener cannot bind %s: %w", cfg.Gateway.StatusListen, err)
	}
	obs := observability.New(st, nil)
	statusDone := make(chan error, 1)
	go func() {
		if err := observability.ServeListener(ctx, statusListener, obs); err != nil {
			slog.New(slog.NewTextHandler(stderr, nil)).Error("status listener stopped", "err", err)
			statusDone <- err
			return
		}
		statusDone <- nil
	}()
	defer func() {
		stop()
		<-statusDone
	}()

	sessions := session.New(st)
	entities := entity.New(st, sessions)

	ws, skills, err := loadWorkspaceWithStore(st)
	if err != nil {
		return err
	}
	agents, cronAgent, profilesMain, profilesGroup, profilesCron, cleanup, err := buildGatewayAgents(ctx, cfg, ws, skills, sessions)
	if err != nil {
		cleanup()
		return err
	}
	defer cleanup()

	log := slog.New(slog.NewTextHandler(stderr, nil))
	var serveBroker *broker.Broker
	if cfg.Broker.Listen != "" {
		upstreams := brokerUpstreams(cfg)
		b := broker.New(st, upstreams)
		serveBroker = b
		b.Usage = usagepkg.New(st)
		b.Limits = brokerLimits(cfg, config.GroupMain)
		go func() {
			if err := b.Serve(ctx, cfg.Broker.Listen); err != nil {
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
	// Lifecycle sweeps are owned by this serve process only. CLI commands
	// perform explicit operations but never start a background reaper.
	lifecycleCtx, lifecycleCancel := context.WithCancel(ctx)
	defer lifecycleCancel()
	wsManager := newWorkspaceManager(cfg, st, nil)
	if serveBroker != nil {
		limits := brokerLimits(cfg, config.GroupMain)
		wsManager.MintToken = func(mintCtx context.Context, sessionID string) (string, error) {
			return serveBroker.MintScoped(mintCtx, sessionID, sessionID, limits)
		}
		wsManager.RevokeSession = serveBroker.RevokeSession
		wsManager.BindGitScope = serveBroker.BindGitRepo
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
	wsStore := &workset.Store{DB: st.DB}
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
		Usage:             usagepkg.New(st),
		ReflectEveryTurns: cfg.Memory.ReflectEveryTurns,
	}

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

	// Scheduler: fire cron jobs while the gateway runs, delivering results
	// through the same channel adapters. Runs on the restricted cron agent
	// (or a named profile built against the cron tier when Job.Profile is set).
	sched := &schedule.Scheduler{
		Store: schedule.NewStore(st),
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
		Usage:  usagepkg.New(st),
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

	// Issue intake watchers: board-driven dispatch under the restricted issue
	// tier. Owned exclusively by this serve process (#48 / #51).
	intakeDone := make(chan struct{})
	go func() {
		defer close(intakeDone)
		runIntakeWatchers(lifecycleCtx, cfg, st, sessions, ws, skills, agents, serveBroker, adapterDeliverer(adapters), log)
	}()

	log.Info("waffle gateway starting", "channels", len(adapters))
	err = gw.Run(ctx)

	// Stop the scheduler and wait for its in-flight-job drain before the
	// deferred cleanup tears down the shared sandbox executor and MCP
	// clients a running cron job may still be using.
	waitForServeWorkers(stop, lifecycleCancel, schedDone, intakeDone)
	return err
}

// waitForServeWorkers preserves shutdown ordering: stop accepting/scheduling,
// then wait for cron.Stop's in-flight-job drain and intake before deferred
// cleanup closes the shared sandbox executor and MCP clients.
func waitForServeWorkers(stop, lifecycleCancel context.CancelFunc, schedDone <-chan error, intakeDone <-chan struct{}) {
	stop()
	lifecycleCancel()
	<-schedDone
	<-intakeDone
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
	for _, name := range profileNames {
		mainA, mainCloser, err := buildAgentWithProfileRuntime(ctx, cfg, ws, skills, sessions, config.GroupMain, name, runtime)
		cleanups = append(cleanups, mainCloser)
		if err != nil {
			return nil, nil, nil, nil, nil, cleanup, fmt.Errorf("profile %q (main): %w", name, err)
		}
		profilesMain[name] = mainA

		groupA, groupCloser, err := buildAgentWithProfileRuntime(ctx, cfg, ws, skills, sessions, config.GroupGroup, name, runtime)
		cleanups = append(cleanups, groupCloser)
		if err != nil {
			return nil, nil, nil, nil, nil, cleanup, fmt.Errorf("profile %q (group): %w", name, err)
		}
		profilesGroup[name] = groupA

		cronA, cronCloser, err := buildAgentWithProfileRuntime(ctx, cfg, ws, skills, sessions, config.GroupCron, name, runtime)
		cleanups = append(cleanups, cronCloser)
		if err != nil {
			return nil, nil, nil, nil, nil, cleanup, fmt.Errorf("profile %q (cron): %w", name, err)
		}
		profilesCron[name] = cronA
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
				base = "https://api.anthropic.com"
			}
			if key != "" {
				header, value = "x-api-key", key
			}
		case "openai":
			if base == "" {
				base = "https://api.openai.com"
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
