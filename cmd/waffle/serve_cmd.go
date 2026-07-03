package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/matt-riley/waffle/internal/broker"
	"github.com/matt-riley/waffle/internal/channel"
	"github.com/matt-riley/waffle/internal/channel/telegram"
	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/entity"
	"github.com/matt-riley/waffle/internal/gateway"
	"github.com/matt-riley/waffle/internal/schedule"
	"github.com/matt-riley/waffle/internal/secret"
	"github.com/matt-riley/waffle/internal/session"
)

func serveCmd(ctx context.Context, stderr io.Writer) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, st, err := openConfigAndStore(ctx)
	if err != nil {
		return err
	}
	defer st.Close() //nolint:errcheck // process is exiting

	sessions := session.New(st)
	entities := entity.New(st, sessions)

	ws, skills, err := loadWorkspace()
	if err != nil {
		return err
	}
	a, cleanup, err := buildAgent(ctx, cfg, ws, skills, sessions)
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

	var adapters []channel.Adapter
	if cfg.Channel.Telegram.Enabled {
		token, err := resolveSecretValue(cfg.Channel.Telegram.Token, "TELEGRAM_BOT_TOKEN")
		if err != nil {
			return fmt.Errorf("telegram: %w", err)
		}
		if token == "" {
			return errors.New("telegram enabled but no token: store one with `waffle secret set telegram/bot-token` or set TELEGRAM_BOT_TOKEN")
		}
		adapters = append(adapters, telegram.New(token, cfg.Channel.Telegram.BaseURL))
	}

	gw := &gateway.Gateway{
		Agent:    a,
		Entities: entities,
		Sessions: sessions,
		Adapters: adapters,
		Log:      log,
	}

	// Scheduler: fire cron jobs while the gateway runs, delivering results
	// through the same channel adapters.
	sched := &schedule.Scheduler{
		Store: schedule.NewStore(st),
		Runner: &schedule.Runner{
			Agent:     a,
			Sessions:  sessions,
			Deliverer: adapterDeliverer(adapters),
			Log:       log,
		},
		Log: log,
	}
	go func() {
		if err := sched.Run(ctx); err != nil {
			log.Error("scheduler stopped", "err", err)
		}
	}()

	log.Info("waffle gateway starting", "channels", len(adapters))
	return gw.Run(ctx)
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
// such secret.
func resolveSecretValue(ref, envVar string) (string, error) {
	if ref == "" {
		return os.Getenv(envVar), nil
	}
	if !secret.IsRef(ref) {
		return ref, nil
	}
	id, err := secret.LoadIdentity()
	if err != nil {
		return os.Getenv(envVar), nil //nolint:nilerr // no store → env fallback is the contract
	}
	path, err := config.SecretsPath()
	if err != nil {
		return "", err
	}
	value, err := secret.Resolve(secret.OpenFile(path, id), ref)
	if errors.Is(err, secret.ErrNotFound) {
		if v := os.Getenv(envVar); v != "" {
			return v, nil
		}
		return "", fmt.Errorf("%w — store it with: printf '%%s' VALUE | waffle secret set %s", err, strings.TrimPrefix(ref, "secret://"))
	}
	return value, err
}
