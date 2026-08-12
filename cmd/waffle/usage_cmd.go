package main

import (
	"context"
	"fmt"
	"io"

	usagepkg "github.com/matt-riley/waffle/internal/usage"
)

func usageCmd(ctx context.Context, args []string, out, stderr io.Writer) (err error) {
	args, jsonOut := takeJSONFlag(args)
	if len(args) > 1 || (len(args) == 1 && args[0] != "ls") {
		return fmt.Errorf("usage: waffle usage [--json]")
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
	rows, err := usagepkg.New(st).List(ctx, "")
	if err != nil {
		return err
	}
	if jsonOut {
		outRows := make([]usageRowJSON, 0, len(rows))
		for _, r := range rows {
			outRows = append(outRows, usageRowToJSON(r))
		}
		return writeJSON(out, outRows)
	}
	for _, r := range rows {
		fmt.Fprintf(out, "%s %s requests=%d input=%d cache_write=%d cache_read=%d output=%d reserved=%d\n", r.SessionID, r.Period, r.Requests, r.InputTokens, r.CacheCreationInputTokens, r.CacheReadInputTokens, r.OutputTokens, r.ReservedTokens)
	}
	return nil
}

// usageRowJSON is the machine-readable shape for `waffle usage --json`.
type usageRowJSON struct {
	SessionID                string `json:"session_id"`
	Period                   string `json:"period"`
	PeriodStart              string `json:"period_start"`
	Requests                 int    `json:"requests"`
	InputTokens              int    `json:"input_tokens"`
	CacheCreationInputTokens int    `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int    `json:"cache_read_input_tokens"`
	OutputTokens             int    `json:"output_tokens"`
	ReservedTokens           int    `json:"reserved_tokens"`
}

func usageRowToJSON(r usagepkg.Row) usageRowJSON {
	return usageRowJSON{
		SessionID:                r.SessionID,
		Period:                   r.Period,
		PeriodStart:              r.PeriodStart,
		Requests:                 r.Requests,
		InputTokens:              r.InputTokens,
		CacheCreationInputTokens: r.CacheCreationInputTokens,
		CacheReadInputTokens:     r.CacheReadInputTokens,
		OutputTokens:             r.OutputTokens,
		ReservedTokens:           r.ReservedTokens,
	}
}

func pauseCmd(ctx context.Context, command string, out io.Writer) (err error) {
	_, st, err := openConfigAndStore(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := st.Close(); err == nil {
			err = cerr
		}
	}()
	p := command == "pause"
	if err = usagepkg.New(st).SetPaused(ctx, p); err != nil {
		return err
	}
	if p {
		fmt.Fprintln(out, "waffle paused")
	} else {
		fmt.Fprintln(out, "waffle resumed")
	}
	return nil
}
