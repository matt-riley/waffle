// Command waffle is the personal agent binary. One executable carries every
// role — gateway, chat REPL, sandbox runner — selected by subcommand
// (docs/plan.md).
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"

	"github.com/matt-riley/waffle/internal/backup"
	"github.com/matt-riley/waffle/internal/gitcred"
	"github.com/matt-riley/waffle/internal/secret"
	"github.com/matt-riley/waffle/internal/telemetry"
)

// version is stamped at build time via -ldflags "-X main.version=...".
// When left as "dev" (plain go build / go install without ldflags),
// resolveVersion fills it from VCS build info or the module version.
var version = "dev"

func init() {
	version = resolveVersion(version)
}

// resolveVersion returns stamped when it is not the placeholder "dev".
// Otherwise it prefers a short VCS revision (with .dirty when modified),
// then the module version from go install @tag, else "dev".
func resolveVersion(stamped string) string {
	if stamped != "" && stamped != "dev" {
		return stamped
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return stamped
	}
	var rev string
	dirty := false
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev != "" {
		if len(rev) > 7 {
			rev = rev[:7]
		}
		if dirty {
			return rev + ".dirty"
		}
		return rev
	}
	if v := bi.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	return stamped
}

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
	case "backup":
		if len(args) < 2 || len(args) > 3 {
			return errors.New("usage: waffle backup <absolute-dir> [--with-identity]")
		}
		withIdentity := len(args) == 3 && args[2] == "--with-identity"
		if len(args) == 3 && !withIdentity {
			return errors.New("usage: waffle backup <absolute-dir> [--with-identity]")
		}
		var identity string
		if withIdentity {
			id, err := secret.LoadIdentity()
			if err != nil {
				return err
			}
			identity = id.String()
		}
		if err := backup.Create(ctx, args[1], withIdentity, identity); err != nil {
			return err
		}
		if !withIdentity {
			fmt.Fprintln(stdout, "backup created; export your identity separately with `waffle secret export-identity`")
		}
		return nil
	case "restore":
		if len(args) != 2 {
			return errors.New("usage: waffle restore <absolute-dir>")
		}
		return backup.Restore(ctx, args[1])
	case "chat":
		return chatCmd(ctx, args[1:], stdin, stdout, stderr)
	case "session":
		return sessionCmd(ctx, args[1:], stdin, stdout, stderr)
	case "forget":
		return forgetCmd(ctx, args[1:], stdin, stdout, stderr)
	case "usage":
		return usageCmd(ctx, args[1:], stdout, stderr)
	case "pause", "resume":
		return pauseCmd(ctx, args[0], stdout)
	case "serve":
		return serveCmd(ctx, stderr)
	case "status":
		return statusCmd(ctx, args[1:], stdout, stderr)
	case "pair":
		return pairCmd(ctx, args[1:], stdout, stderr)
	case "runner":
		return runnerCmd(ctx, args[1:], stderr)
	case "sandbox":
		return sandboxCmd(ctx, args[1:], stdout, stderr)
	case "ws":
		return wsCmd(ctx, args[1:], stdout, stderr)
	case "cron":
		return cronCmd(ctx, args[1:], stdout, stderr)
	case "doctor":
		return doctorCmd(ctx, stdout)
	case "eval":
		return evalCmd(ctx, args[1:], stdout, stderr)
	case "skills":
		return skillsCmd(ctx, args[1:], stdout, stderr)
	case "upgrade":
		return upgradeCmd(ctx, args[1:], stdout, stderr)
	case "rollback":
		return rollbackCmd(stdout)
	case "git-credential":
		op := ""
		if len(args) > 1 {
			op = args[1]
		}
		return gitcred.Run(ctx, op, stdin, stdout)
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
  serve     run the gateway (channels from config.toml)
  status    show active and recent gateway runs
  pair      approve your accounts on connected channels
  ws        manage repo workspaces (open/ls/idle/close)
  cron      manage scheduled jobs (add/ls/run/rm)
  session   list past sessions
  forget    delete confirmed conversation and memory matches
  usage     show persisted token/request usage
  pause     pause new agent runs
  resume    resume agent runs
  secret    manage the encrypted secret store
  backup    create a local state backup
  restore   validate and restore a local state backup
  doctor    run self-checks
  eval      run zero-network agent eval harness (exit 1 on failure)
  skills    skill utilities (audit mines recurring tool errors)
  upgrade   rebuild and verify waffle, then swap in the new binary
            --no-verify skips vet/tests/lint (unsafe)
  rollback  restore the previous binary
  version   print version
  help      show this help

State lives in $WAFFLE_HOME (default ~/.waffle).
`)
}
