package main

import (
	"context"
	"fmt"
	"io"

	usagepkg "github.com/matt-riley/waffle/internal/usage"
)

func usageCmd(ctx context.Context, args []string, out, stderr io.Writer) error {
	if len(args) > 0 && args[0] != "ls" {
		return fmt.Errorf("usage: waffle usage")
	}
	_, st, err := openConfigAndStore(ctx)
	if err != nil {
		return err
	}
	defer st.Close() //nolint:errcheck // process is exiting
	rows, err := usagepkg.New(st).List(ctx, "")
	if err != nil {
		return err
	}
	for _, r := range rows {
		fmt.Fprintf(out, "%s %s requests=%d input=%d output=%d\n", r.SessionID, r.Period, r.Requests, r.InputTokens, r.OutputTokens)
	}
	return nil
}

func pauseCmd(ctx context.Context, command string, out io.Writer) error {
	_, st, err := openConfigAndStore(ctx)
	if err != nil {
		return err
	}
	defer st.Close() //nolint:errcheck // process is exiting
	p := command == "pause"
	if err := usagepkg.New(st).SetPaused(ctx, p); err != nil {
		return err
	}
	if p {
		fmt.Fprintln(out, "waffle paused")
	} else {
		fmt.Fprintln(out, "waffle resumed")
	}
	return nil
}
