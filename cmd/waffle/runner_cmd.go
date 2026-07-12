package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/matt-riley/waffle/internal/sandbox"
	"github.com/matt-riley/waffle/internal/tool"
)

// runnerCmd is the sandbox-side entrypoint: the same waffle binary,
// bind-mounted into a container, serving tool execution over the queue
// pair. It has no access to config, secrets, or the database — only the
// queue directory and whatever the container can see.
func runnerCmd(ctx context.Context, args []string, stderr io.Writer) error {
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
func sandboxCmd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
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
	defer client.Close() //nolint:errcheck // process is exiting

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
