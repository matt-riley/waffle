package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/secret"
)

const defaultMainSystemPrompt = "You are the owner's personal assistant."

// setupMainProfileHeaderRE matches an explicit [agent.profile.main] table header.
var setupMainProfileHeaderRE = regexp.MustCompile(`(?m)^\s*\[agent\.profile\.main\]\s*(?:#.*)?$`)

func setupCmd(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) > 0 {
		switch args[0] {
		case "help", "-h", "--help":
			setupUsage(stdout)
			return nil
		default:
			setupUsage(stderr)
			return fmt.Errorf("unknown setup option %q", args[0])
		}
	}

	fmt.Fprintln(stdout, "Waffle setup — first-run configuration")
	fmt.Fprintln(stdout)

	// Step 1: secret-store identity.
	if _, err := secret.LoadIdentity(); errors.Is(err, secret.ErrNoIdentity) {
		fmt.Fprintln(stdout, "→ Creating secret-store identity…")
		if err := secretInit(false, stdout); err != nil {
			return fmt.Errorf("setup secret init: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("setup check identity: %w", err)
	} else {
		fmt.Fprintln(stdout, "→ Secret-store identity already configured")
	}

	// Step 2: provider connection + models.
	hasProvider, err := setupHasProvider()
	if err != nil {
		return fmt.Errorf("setup check provider: %w", err)
	}
	if hasProvider {
		fmt.Fprintln(stdout, "→ Provider already configured")
	} else {
		fmt.Fprintln(stdout, "→ Adding a model provider…")
		// Reuse the existing guided provider-add path so credentials stay on
		// stdin / 0600 key files (no argv or env secret channels).
		if err := providerAddGuided(ctx, stdin, stdout, stderr); err != nil {
			return fmt.Errorf("setup provider add: %w", err)
		}
	}

	// Step 3: minimal [agent.profile.main] when missing.
	hasMain, err := setupHasMainProfile()
	if err != nil {
		return fmt.Errorf("setup check profile: %w", err)
	}
	if hasMain {
		fmt.Fprintln(stdout, "→ agent.profile.main already configured")
	} else {
		fmt.Fprintln(stdout, "→ Configuring agent.profile.main…")
		system, err := promptLineNoReadAhead(stdin, stderr, "System prompt for profile main", defaultMainSystemPrompt)
		if err != nil {
			return fmt.Errorf("setup profile prompt: %w", err)
		}
		if strings.TrimSpace(system) == "" {
			system = defaultMainSystemPrompt
		}
		if err := writeMainProfile(system); err != nil {
			return fmt.Errorf("setup write profile: %w", err)
		}
		fmt.Fprintln(stdout, "wrote [agent.profile.main]")
	}

	// Step 4: summary.
	modelAlias, err := setupDefaultModelAlias()
	if err != nil {
		return fmt.Errorf("setup summary: %w", err)
	}
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "Setup complete.")
	if modelAlias != "" {
		fmt.Fprintf(stdout, "Active model alias: %s\n", modelAlias)
	} else {
		fmt.Fprintln(stdout, "Active model alias: (none — set a default with waffle provider model activate <alias>)")
	}
	fmt.Fprintln(stdout, "Next: waffle chat")
	return nil
}

func setupUsage(w io.Writer) {
	fmt.Fprint(w, `First-run setup: secret identity, provider, and starter profile.

Usage:
  waffle setup

Runs interactively:
  1. secret init (skipped when an identity already exists)
  2. provider add guided (skipped when a provider connection exists)
  3. ensure [agent.profile.main] (skipped when already present)
  4. print the active model alias and how to start chat

Credentials are read the same way as waffle provider add (hidden stdin or a
0600 key file). API keys are never accepted as command-line values or env vars.
`)
}

func setupHasProvider() (bool, error) {
	path, err := config.Path()
	if err != nil {
		return false, err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return false, err
	}
	return len(cfg.Providers) > 0, nil
}

func setupHasMainProfile() (bool, error) {
	path, err := config.Path()
	if err != nil {
		return false, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if setupMainProfileHeaderRE.Match(raw) {
		return true, nil
	}
	// Also treat a decoded main profile as present (covers non-canonical forms
	// that config.Load accepts).
	cfg, err := config.Load(path)
	if err != nil {
		return false, err
	}
	if cfg.Agent.Profiles == nil {
		return false, nil
	}
	_, ok := cfg.Agent.Profiles["main"]
	return ok, nil
}

func setupDefaultModelAlias() (string, error) {
	path, err := config.Path()
	if err != nil {
		return "", err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return "", err
	}
	if alias := strings.TrimSpace(cfg.Agent.DefaultModel); alias != "" {
		return alias, nil
	}
	// Prefer a stable first alias when no default is set yet.
	for alias := range cfg.Models {
		return alias, nil
	}
	return "", nil
}

// writeMainProfile appends a minimal [agent.profile.main] block without
// rewriting unrelated config sections. Existing main profiles must be skipped
// by the caller via setupHasMainProfile.
func writeMainProfile(system string) error {
	path, err := config.Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if setupMainProfileHeaderRE.Match(raw) {
		return errors.New("agent.profile.main already present")
	}

	var b strings.Builder
	if len(raw) > 0 {
		b.Write(raw)
		if raw[len(raw)-1] != '\n' {
			b.WriteByte('\n')
		}
		if !strings.HasSuffix(b.String(), "\n\n") {
			b.WriteByte('\n')
		}
	}
	b.WriteString("[agent.profile.main]\n")
	b.WriteString("system = ")
	b.WriteString(strconv.Quote(system))
	b.WriteByte('\n')
	b.WriteString("model = \"default\"\n")
	b.WriteString("sandbox = \"host\"\n")

	// Stage + rename so a partial write cannot leave a truncated config.toml.
	dir := filepath.Dir(path)
	stage, err := os.CreateTemp(dir, ".setup-profile-*")
	if err != nil {
		return fmt.Errorf("stage profile config: %w", err)
	}
	stagePath := stage.Name()
	defer func() { _ = os.Remove(stagePath) }()
	if _, err := stage.WriteString(b.String()); err != nil {
		_ = stage.Close()
		return fmt.Errorf("write staged profile config: %w", err)
	}
	if err := stage.Chmod(0o600); err != nil {
		_ = stage.Close()
		return fmt.Errorf("chmod staged profile config: %w", err)
	}
	if err := stage.Sync(); err != nil {
		_ = stage.Close()
		return fmt.Errorf("sync staged profile config: %w", err)
	}
	if err := stage.Close(); err != nil {
		return fmt.Errorf("close staged profile config: %w", err)
	}
	if err := os.Rename(stagePath, path); err != nil {
		return fmt.Errorf("commit profile config: %w", err)
	}

	// Validate the result still loads as config.
	if _, err := config.Load(path); err != nil {
		return fmt.Errorf("validate profile config: %w", err)
	}
	return nil
}
