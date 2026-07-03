// Command waffle is the personal agent binary. One executable carries every
// role — gateway, TUI, sandbox runner — selected by subcommand (docs/plan.md).
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/matt-riley/waffle/internal/telemetry"
)

// version is stamped at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		if !errors.Is(err, errUsage) {
			fmt.Fprintln(os.Stderr, "waffle:", err)
		}
		os.Exit(1)
	}
}

// errUsage marks errors whose message has already been shown via usage text.
var errUsage = errors.New("usage")

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		usage(stderr)
		return errUsage
	}

	shutdown, err := telemetry.Setup(ctx, "waffle", version)
	if err != nil {
		return fmt.Errorf("telemetry: %w", err)
	}
	defer func() {
		if err := shutdown(ctx); err != nil {
			fmt.Fprintln(stderr, "waffle: telemetry shutdown:", err)
		}
	}()

	switch args[0] {
	case "version":
		fmt.Fprintln(stdout, "waffle", version)
		return nil
	case "secret":
		return secretCmd(args[1:], stdin, stdout, stderr)
	case "chat":
		return chatCmd(ctx, args[1:], stdin, stdout, stderr)
	case "session":
		return sessionCmd(ctx, args[1:], stdout, stderr)
	case "serve":
		return errors.New("serve is not implemented yet (phase 3, see docs/plan.md)")
	case "help", "-h", "--help":
		usage(stdout)
		return nil
	default:
		usage(stderr)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `waffle — a personal AI agent

Usage:
  waffle <command> [arguments]

Commands:
  chat      interactive terminal session (-c continues the last session)
  session   list past sessions
  serve     run the gateway (phase 3)
  secret    manage the encrypted secret store
  version   print version
  help      show this help

State lives in $WAFFLE_HOME (default ~/.waffle).
`)
}
