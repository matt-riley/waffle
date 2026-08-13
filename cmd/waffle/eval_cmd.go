package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/matt-riley/waffle/internal/eval"
)

var evalRegistry = eval.Registry

// evalCmd runs the zero-network eval harness (#63). Exits 1 on any failure
// via main's os.Exit when this returns a non-nil error.
//
//	waffle eval            run deterministic offline cases
//	waffle eval --history  show recent SQLite-recorded runs
//
// Live tier: set WAFFLE_EVAL_LIVE=1 (still skipped without a configured
// provider). Results are recorded in the waffle DB when available.
func evalCmd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) > 0 {
		switch args[0] {
		case "--history":
			if len(args) != 1 {
				return fmt.Errorf("usage: waffle eval --history")
			}
			return evalHistoryCmd(ctx, stdout)
		case "-h", "--help", "help":
			fmt.Fprintln(stdout, "usage: waffle eval | waffle eval --history")
			fmt.Fprintln(stdout, "  deterministic offline agent checks (zero network)")
			fmt.Fprintln(stdout, "  live tier: WAFFLE_EVAL_LIVE=1 (skipped without provider)")
			return nil
		default:
			return fmt.Errorf("usage: waffle eval | waffle eval --history")
		}
	}

	started := time.Now().UTC()
	cases := append([]eval.Case{}, evalRegistry()...)
	cases = append(cases, eval.LiveRegistry()...)

	// Live tier, github_pr (#241): exercise the tool end to end against the
	// real GitHub API. Opt-in like the provider tier: WAFFLE_EVAL_LIVE=1,
	// WAFFLE_EVAL_GITHUB_REPO=owner/repo, and a configured [github.app].
	cfg, st, cfgErr := openConfigAndStore(ctx)
	if cfgErr == nil {
		app, appErr := newGitHubApp(cfg)
		if appErr != nil {
			fmt.Fprintf(stderr, "waffle eval: github app: %v\n", appErr)
		}
		if live := eval.LiveGitHubPRCase(app); live != nil {
			cases = append(cases, *live)
		}
	}
	report := eval.Run(ctx, cases)

	// Best-effort history persistence; missing home/db must not fail CI.
	if cfgErr == nil {
		_ = eval.RecordRun(ctx, st.DB, version, started, report)
		_ = st.Close()
	}

	fmt.Fprint(stdout, report.Text)
	if report.Failed > 0 {
		return fmt.Errorf("%d eval case(s) failed", report.Failed)
	}
	return nil
}

func evalHistoryCmd(ctx context.Context, stdout io.Writer) error {
	_, st, err := openConfigAndStore(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	hist, err := eval.ListHistory(ctx, st.DB, 20)
	if err != nil {
		return err
	}
	eval.FormatHistory(stdout, hist)
	return nil
}
