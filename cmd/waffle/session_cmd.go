package main

import (
	"context"
	"fmt"
	"io"

	"github.com/matt-riley/waffle/internal/session"
)

func sessionCmd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	sub := "ls"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "ls":
		_, st, err := openConfigAndStore(ctx)
		if err != nil {
			return err
		}
		defer st.Close() //nolint:errcheck // read-only use
		sessions, err := session.New(st).List(ctx, 20)
		if err != nil {
			return err
		}
		if len(sessions) == 0 {
			fmt.Fprintln(stdout, "no sessions yet — start one with: waffle chat")
			return nil
		}
		for _, s := range sessions {
			fmt.Fprintf(stdout, "%s  %s\n", s.ID, s.Title)
			if s.Summary != "" {
				fmt.Fprintf(stdout, "    %s\n", s.Summary)
			}
		}
		return nil
	case "help", "-h", "--help":
		fmt.Fprintln(stdout, "usage: waffle session ls")
		return nil
	default:
		fmt.Fprintln(stderr, "usage: waffle session ls")
		return fmt.Errorf("unknown session command %q", sub)
	}
}
