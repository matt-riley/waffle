package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/matt-riley/waffle/internal/netlock"
	"github.com/matt-riley/waffle/internal/sandbox"
	"github.com/matt-riley/waffle/internal/tool"
)

// runnerCmd is the sandbox-side entrypoint: the same waffle binary,
// bind-mounted into a container, serving tool execution over the queue
// pair. It has no access to config, secrets, or the database — only the
// queue directory and whatever the container can see.
// lockdownPhaseEnv marks that the setup phase below has already applied the
// network lockdown and dropped capabilities; it prevents re-exec loops.
const lockdownPhaseEnv = "WAFFLE_RUNNER_LOCKDOWN_APPLIED"

func runnerCmd(ctx context.Context, args []string, stderr io.Writer) error {
	// Workspace none/allowlist set WAFFLE_NET_LOCKDOWN so the runner must
	// drop the default route (keep waffle-host only) before serving (#95).
	// Fail closed: do not serve with an open default route.
	//
	// Linux capabilities are per-thread, so dropping them inside this
	// long-lived multithreaded process would leave the Go runtime's other
	// threads — and every tool they exec — holding CAP_NET_ADMIN, able to
	// re-add the routes the lockdown removed. The setup phase therefore
	// applies the lockdown, drops every capability, and re-execs: the
	// serving process starts single-threaded with an empty capability set
	// that untrusted workspace commands inherit.
	if os.Getenv(lockdownPhaseEnv) == "" {
		if err := netlock.ApplyFromEnv(os.Getenv, netlock.LockdownExceptHost); err != nil {
			return fmt.Errorf("waffle runner: %w", err)
		}
		if v := strings.TrimSpace(os.Getenv(netlock.EnvLockdown)); v == "1" || strings.EqualFold(v, "true") {
			fmt.Fprintf(stderr, "waffle runner: network lockdown active\n")
			if err := netlock.DropCapabilities(); err != nil {
				return fmt.Errorf("waffle runner: drop capabilities: %w", err)
			}
			self, err := os.Executable()
			if err != nil {
				return fmt.Errorf("waffle runner: locate self for re-exec: %w", err)
			}
			env := append(os.Environ(), lockdownPhaseEnv+"=1")
			return syscall.Exec(self, append([]string{self}, args...), env)
		}
	}

	dir, err := queueDirArg(args)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Fprintf(stderr, "waffle runner: serving queue %s\n", dir)
	r := &sandbox.Runner{Tools: tool.BuiltinsWithFetch(fetchAllowPrivateArgs(args))}
	return r.Serve(ctx, dir)
}

func fetchAllowPrivateArgs(args []string) []string {
	var entries []string
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--fetch-allow-private" {
			entries = append(entries, args[i+1])
			i++
		}
	}
	return entries
}

// sandboxCmd offers `waffle sandbox exec` — a diagnostic client for a
// running runner, and the harness for e2e-testing the queue protocol.
func sandboxCmd(ctx context.Context, args []string, stdout, stderr io.Writer) (err error) {
	if len(args) < 1 || args[0] != "exec" {
		fmt.Fprintln(stderr, "usage: waffle sandbox exec --queue <dir> <tool> <json-input>")
		return errUsage
	}
	rest := args[1:]
	dir, err := queueDirArg(rest)
	if err != nil {
		return err
	}
	// Strip the --queue flag pair to find tool + input.
	var positional []string
	for i := 0; i < len(rest); i++ {
		if rest[i] == "--queue" {
			i++
			continue
		}
		positional = append(positional, rest[i])
	}
	if len(positional) != 2 {
		return fmt.Errorf("usage: waffle sandbox exec --queue <dir> <tool> <json-input>")
	}

	client, err := sandbox.NewClient(dir)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := client.Close(); err == nil {
			err = cerr
		}
	}()

	content, isError, err := client.Exec(ctx, positional[0], json.RawMessage(positional[1]))
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, content)
	if isError {
		return fmt.Errorf("tool returned an error")
	}
	return nil
}

func queueDirArg(args []string) (string, error) {
	for i, a := range args {
		if a == "--queue" && i+1 < len(args) {
			return args[i+1], nil
		}
	}
	return "", fmt.Errorf("--queue <dir> is required")
}
