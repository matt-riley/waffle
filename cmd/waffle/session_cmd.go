package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/matt-riley/waffle/internal/session"
)

func sessionCmd(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
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
	case "rm":
		if len(args) != 2 {
			return errors.New("usage: waffle session rm <id>")
		}
		_, st, err := openConfigAndStore(ctx)
		if err != nil {
			return err
		}
		defer st.Close() //nolint:errcheck
		fmt.Fprintf(stdout, "Delete session %s and all its turns? [y/N] ", args[1])
		if !confirmed(stdin) {
			fmt.Fprintln(stdout, "cancelled")
			return nil
		}
		if err := session.New(st).Delete(ctx, args[1]); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "session deleted")
		return nil
	case "help", "-h", "--help":
		fmt.Fprintln(stdout, "usage: waffle session ls | waffle session rm <id>")
		return nil
	default:
		fmt.Fprintln(stderr, "usage: waffle session ls | waffle session rm <id>")
		return fmt.Errorf("unknown session command %q", sub)
	}
}

func confirmed(r io.Reader) bool {
	var answer string
	_, _ = fmt.Fscan(r, &answer)
	answer = strings.TrimSpace(answer)
	return strings.EqualFold(answer, "y") || strings.EqualFold(answer, "yes")
}
