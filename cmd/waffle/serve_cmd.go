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
	"syscall"

	"github.com/matt-riley/waffle/internal/agent"
	"github.com/matt-riley/waffle/internal/broker"
	"github.com/matt-riley/waffle/internal/channel"
	"github.com/matt-riley/waffle/internal/channel/telegram"
	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/entity"
	"github.com/matt-riley/waffle/internal/gateway"
	"github.com/matt-riley/waffle/internal/memory"
	"github.com/matt-riley/waffle/internal/observability"
	"github.com/matt-riley/waffle/internal/schedule"
	"github.com/matt-riley/waffle/internal/secret"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/skill"
)

func serveCmd(ctx context.Context, stderr io.Writer) error {
	return serveCmdWithAdapterFactory(ctx, stderr, configuredAdapters)
}

type adapterFactory func(config.Config) ([]channel.Adapter, error)

// serveCmdWithAdapterFactory runs the serve command with an explicit adapter
// factory so command lifecycle tests can use an in-memory channel.
func serveCmdWithAdapterFactory(ctx context.Context, stderr io.Writer, makeAdapters adapterFactory) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, st, err := openConfigAndStore(ctx)
	if err != nil {
		return err
	}
	defer st.Close() //nolint:errcheck // process is exiting

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

	ws, skills, err := loadWorkspace()
	if err != nil {
		return err
	}
	agents, cronAgent, cleanup, err := buildGatewayAgents(ctx, cfg, ws, skills, sessions)
	if err != nil {
		cleanup()
		return err
	}
	defer cleanup()

	log := slog.New(slog.NewTextHandler(stderr, nil))
	if cfg.Broker.Listen != "" {
		upstreams := brokerUpstreams(cfg)
		b := broker.New(st, upstreams)
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

	gw := &gateway.Gateway{
		Agent:         agents[config.GroupMain],
		Agents:        agents,
		Entities:      entities,
		Sessions:      sessions,
		Adapters:      adapters,
		Log:           log,
		Observability: obs,
	}

	// Scheduler: fire cron jobs while the gateway runs, delivering results
	// through the same channel adapters. Runs on the restricted cron agent.
	sched := &schedule.Scheduler{
		Store: schedule.NewStore(st),
		Runner: &schedule.Runner{
			Agent:         cronAgent,
			Sessions:      sessions,
			Deliverer:     adapterDeliverer(adapters),
			Log:           log,
			Observability: obs,
		},
		Log: log,
	}
	schedDone := make(chan error, 1)
	go func() {
		serr := sched.Run(ctx)
		if serr != nil {
			log.Error("scheduler stopped", "err", serr)
		}
		schedDone <- serr
	}()

	log.Info("waffle gateway starting", "channels", len(adapters))
	err = gw.Run(ctx)

	// Stop the scheduler and wait for its in-flight-job drain before the
	// deferred cleanup tears down the shared sandbox executor and MCP
	// clients a running cron job may still be using.
	stop()
	<-schedDone
	return err
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
// together with the cron agent used by the scheduler. The cleanup callback
// closes successfully and partially-built agents in reverse build order.
func buildGatewayAgents(ctx context.Context, cfg config.Config, ws memory.Workspace, skills []skill.Skill, sessions *session.Store) (map[string]*agent.Agent, *agent.Agent, func(), error) {
	agents := make(map[string]*agent.Agent)
	var cleanups []func()
	cleanup := func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}

	build := func(group string) (*agent.Agent, error) {
		a, closer, err := buildAgent(ctx, cfg, ws, skills, sessions, group)
		cleanups = append(cleanups, closer)
		if err != nil {
			return nil, err
		}
		agents[group] = a
		return a, nil
	}

	if _, err := build(config.GroupMain); err != nil {
		return nil, nil, cleanup, err
	}
	cronAgent, err := build(config.GroupCron)
	if err != nil {
		return nil, nil, cleanup, err
	}

	groups := make([]string, 0, len(cfg.Agent.Groups))
	for group := range cfg.Agent.Groups {
		if group != config.GroupMain && group != config.GroupCron {
			groups = append(groups, group)
		}
	}
	sort.Strings(groups)
	for _, group := range groups {
		if _, err := build(group); err != nil {
			return nil, nil, cleanup, err
		}
	}

	return agents, cronAgent, cleanup, nil
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
	var ups []broker.Upstream
	if key, _, err := resolveAPIKey(config.Provider{Name: "anthropic", APIKey: "secret://anthropic/api-key"}); err == nil && key != "" {
		base := "https://api.anthropic.com"
		if cfg.Provider.Name == "anthropic" && cfg.Provider.BaseURL != "" {
			base = cfg.Provider.BaseURL
		}
		ups = append(ups, broker.Upstream{Name: "anthropic", BaseURL: base, Header: "x-api-key", Value: key})
	}
	if key, _, err := resolveAPIKey(config.Provider{Name: "openai", APIKey: "secret://openai/api-key"}); err == nil && key != "" {
		base := "https://api.openai.com"
		if cfg.Provider.Name == "openai" && cfg.Provider.BaseURL != "" {
			base = cfg.Provider.BaseURL
		}
		ups = append(ups, broker.Upstream{Name: "openai", BaseURL: base, Header: "Authorization", Value: "Bearer " + key})
	}
	return ups
}

// resolveSecretValue resolves a config value that may be a secret://
// reference, falling back to envVar when unset or when the store has no
// such secret. Delegates to the shared helper in internal/secret.
func resolveSecretValue(ref, envVar string) (string, error) {
	return secret.ResolveRef(ref, envVar)
}
