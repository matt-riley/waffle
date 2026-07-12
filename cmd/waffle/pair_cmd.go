package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/matt-riley/waffle/internal/entity"
	"github.com/matt-riley/waffle/internal/session"
)

func pairCmd(ctx context.Context, args []string, stdout, stderr io.Writer) (err error) {
	sub := "ls"
	if len(args) > 0 {
		sub = args[0]
	}

	_, st, err := openConfigAndStore(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := st.Close(); err == nil {
			err = cerr
		}
	}()
	entities := entity.New(st, session.New(st))

	switch sub {
	case "ls":
		pairings, err := entities.Pairings(ctx)
		if err != nil {
			return err
		}
		if len(pairings) == 0 {
			fmt.Fprintln(stdout, "no pending pairings")
			return nil
		}
		for _, p := range pairings {
			fmt.Fprintf(stdout, "%s  %s sender=%s (%s)  since %s\n",
				p.Code, p.Channel, p.ExternalID, p.SenderName, p.CreatedAt.Format("2006-01-02 15:04"))
		}
		return nil
	case "approve":
		if len(args) < 2 {
			return fmt.Errorf("usage: waffle pair approve <code> [name]")
		}
		code := strings.ToUpper(args[1])
		name := strings.Join(args[2:], " ")
		id, err := entities.Approve(ctx, code, name)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "paired: %s on %s is now recognized as you\n", displayName(id), id.Channel)
		return nil
	case "help", "-h", "--help":
		pairUsage(stdout)
		return nil
	default:
		pairUsage(stderr)
		return fmt.Errorf("unknown pair command %q", sub)
	}
}

func displayName(id *entity.Identity) string {
	if id.Name != "" {
		return id.Name
	}
	return id.ExternalID
}

func pairUsage(w io.Writer) {
	fmt.Fprint(w, `Approve your own accounts on connected channels. waffle is single-owner:
approval happens here on the host, which is the proof of ownership.

Usage:
  waffle pair ls                      list pending pairing requests
  waffle pair approve <code> [name]   recognize a sender as you
`)
}
