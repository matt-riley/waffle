package main

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/matt-riley/waffle/internal/memory"
	"github.com/matt-riley/waffle/internal/session"
)

func forgetCmd(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (err error) {
	if len(args) == 0 {
		return fmt.Errorf("usage: waffle forget <query>")
	}
	query := strings.Join(args, " ")
	_, st, err := openConfigAndStore(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := st.Close(); err == nil {
			err = cerr
		}
	}()
	sessions := session.New(st)
	hits, err := sessions.Search(ctx, query, 100)
	if err != nil {
		return err
	}
	ws, err := memory.Open(memory.DefaultAgent)
	if err != nil {
		return err
	}
	lines, err := ws.MatchingLines(query)
	if err != nil {
		return err
	}
	if len(hits) == 0 && len(lines) == 0 {
		fmt.Fprintln(stdout, "no matches")
		return nil
	}
	for _, h := range hits {
		fmt.Fprintf(stdout, "turn %d in session %s: %s\n", h.TurnID, h.SessionID, h.Snippet)
	}
	for _, line := range lines {
		fmt.Fprintf(stdout, "memory %s\n", line)
	}
	fmt.Fprint(stdout, "Delete these matches? [y/N] ")
	if !confirmed(stdin) {
		fmt.Fprintln(stdout, "cancelled")
		return nil
	}
	ids := make([]int64, 0, len(hits))
	for _, h := range hits {
		ids = append(ids, h.TurnID)
	}
	if err := sessions.DeleteTurns(ctx, ids); err != nil {
		return err
	}
	lineNums := make([]int, 0, len(lines))
	for _, line := range lines {
		n, _, ok := strings.Cut(line, ":")
		if ok {
			if i, err := strconv.Atoi(n); err == nil {
				lineNums = append(lineNums, i)
			}
		}
	}
	if len(lineNums) > 0 {
		if err := ws.RemoveLines(lineNums); err != nil {
			return err
		}
	}
	fmt.Fprintf(stdout, "forgot %d turn match(es) and %d memory line(s)\n", len(ids), len(lineNums))
	return nil
}
