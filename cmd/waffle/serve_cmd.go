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

	"github.com/matt-riley/waffle/internal/channel"
	"github.com/matt-riley/waffle/internal/channel/telegram"
	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/entity"
	"github.com/matt-riley/waffle/internal/gateway"
	"github.com/matt-riley/waffle/internal/memory"
	"github.com/matt-riley/waffle/internal/secret"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/skill"
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

	ws, err := memory.Open(memory.DefaultAgent)
	if err != nil {
		return err
	}
	skills, err := skill.Discover(ws.SkillsDir())
	if err != nil {
		return err
	}
	a, err := buildAgent(cfg, ws, skills, sessions)
	if err != nil {
		return err
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

	log := slog.New(slog.NewTextHandler(stderr, nil))
	gw := &gateway.Gateway{
		Agent:    a,
		Entities: entities,
		Sessions: sessions,
		Adapters: adapters,
		Log:      log,
	}
	log.Info("waffle gateway starting", "channels", len(adapters))
	return gw.Run(ctx)
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
