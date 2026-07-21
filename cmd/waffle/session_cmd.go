package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/entity"
	"github.com/matt-riley/waffle/internal/session"
)

func sessionCmd(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (err error) {
	args, jsonOut := takeJSONFlag(args)
	sub := "ls"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "ls", "list":
		if len(args) > 1 {
			return errors.New("usage: waffle session ls|list [--json]")
		}
		_, st, openErr := openConfigAndStore(ctx)
		if openErr != nil {
			return openErr
		}
		defer func() {
			if cerr := st.Close(); err == nil {
				err = cerr
			}
		}()
		sessions, listErr := session.New(st).List(ctx, 20)
		if listErr != nil {
			return listErr
		}
		if jsonOut {
			out := make([]sessionJSON, 0, len(sessions))
			for _, s := range sessions {
				out = append(out, sessionToJSON(s))
			}
			return writeJSON(stdout, out)
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
	case "rm", "remove":
		if len(args) != 2 {
			return errors.New("usage: waffle session rm|remove <id>")
		}
		_, st, openErr := openConfigAndStore(ctx)
		if openErr != nil {
			return openErr
		}
		defer func() {
			if cerr := st.Close(); err == nil {
				err = cerr
			}
		}()
		fmt.Fprintf(stdout, "Delete session %s and all its turns? [y/N] ", args[1])
		if !confirmed(stdin) {
			fmt.Fprintln(stdout, "cancelled")
			return nil
		}
		if delErr := session.New(st).Delete(ctx, args[1]); delErr != nil {
			return delErr
		}
		fmt.Fprintln(stdout, "session deleted")
		return nil
	case "profile":
		// waffle session profile <channel:chat|chat_id> <name|clear>
		if len(args) != 3 {
			return errors.New("usage: waffle session profile <channel:chat_id|chat_id> <profile-name>")
		}
		cfg, st, openErr := openConfigAndStore(ctx)
		if openErr != nil {
			return openErr
		}
		defer func() {
			if cerr := st.Close(); err == nil {
				err = cerr
			}
		}()
		name := strings.TrimSpace(args[2])
		if name == "" || name == "-" || name == "clear" {
			name = ""
		} else {
			if !config.ValidProfileName(name) {
				return fmt.Errorf("invalid profile name %q (slug [a-z0-9-] max %d)", name, config.ProfileNameMax)
			}
			if _, ok := cfg.Profile(name); !ok {
				return fmt.Errorf("unknown agent profile %q", name)
			}
		}
		entities := entity.New(st, session.New(st))
		if setErr := entities.SetProfileByChat(ctx, args[1], name); setErr != nil {
			return setErr
		}
		if name == "" {
			fmt.Fprintf(stdout, "cleared profile on %s\n", args[1])
		} else {
			fmt.Fprintf(stdout, "bound profile %q to %s\n", name, args[1])
		}
		return nil
	case "help", "-h", "--help":
		fmt.Fprintln(stdout, "usage: waffle session ls|list [--json] | waffle session rm|remove <id> | waffle session profile <chat> <name>")
		return nil
	default:
		fmt.Fprintln(stderr, "usage: waffle session ls|list [--json] | waffle session rm|remove <id> | waffle session profile <chat> <name>")
		return fmt.Errorf("unknown session command %q", sub)
	}
}

// sessionJSON is the machine-readable shape for `waffle session ls --json`.
type sessionJSON struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Summary string `json:"summary,omitempty"`
}

func sessionToJSON(s session.Session) sessionJSON {
	return sessionJSON{ID: s.ID, Title: s.Title, Summary: s.Summary}
}

func confirmed(r io.Reader) bool {
	var answer string
	_, _ = fmt.Fscan(r, &answer)
	answer = strings.TrimSpace(answer)
	return strings.EqualFold(answer, "y") || strings.EqualFold(answer, "yes")
}
