package main

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/memory"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/skill"
	"github.com/matt-riley/waffle/internal/store"
)

// learnCmd runs the mine→propose→validate learning loop (#65).
// Suitable for cron: prints a digest to stdout (deliver via cron --deliver).
//
//	waffle learn
//	waffle cron add learn-daily 0 3 * * * /learn --deliver telegram:900
//
// Example system crontab entry:
//
//	0 3 * * * waffle learn >> ~/.waffle/learn.log 2>&1
//
// `/learn` is the only reserved internal cron action; arbitrary CLI commands
// are never dispatched from job prompts.
func learnCmd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	_ = stderr
	if len(args) > 0 {
		return fmt.Errorf("usage: waffle learn")
	}
	cfg, st, err := openConfigAndStore(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	return runLearn(ctx, cfg, st, stdout)
}

func runLearn(ctx context.Context, cfg config.Config, st *store.Store, stdout io.Writer) error {
	ws, err := memory.Open(memory.DefaultAgent)
	if err != nil {
		return err
	}
	sessions := session.New(st)
	l := skill.NewLearnerFromStore(st, sessions, ws)

	// Attribution uses the configured utility model alias (#65).
	model, provider, runtimeErr := learnRuntime(cfg, newModelRuntimeResolver(cfg))
	if runtimeErr == nil && provider != nil {
		l.Provider = provider
		l.Model = model
	}

	res, err := l.Run(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, "waffle learn digest")
	fmt.Fprintf(stdout, "run=%s patterns=%d proposals=%d accepted=%d rejected=%d pending=%d provider_calls=%d scanned_sessions=%d pages=%d\n",
		res.ID, len(res.Patterns), len(res.Proposals), res.Accepted, res.Rejected, res.Pending, res.ProviderCalls, res.ScannedSessions, res.Pages)
	if res.SinceAt != "" {
		fmt.Fprintf(stdout, "since=%s\n", res.SinceAt)
	}
	fmt.Fprintf(stdout, "cursor=%s/%s\n", res.Cursor.UpdatedAt, res.Cursor.SessionID)
	for _, p := range res.Patterns {
		fmt.Fprintf(stdout, "  (%d×) %s\n", p.Count, p.Class)
		if p.Attribution != "" {
			fmt.Fprintf(stdout, "      attr: %s\n", p.Attribution)
		}
		if len(p.SessionIDs) > 0 {
			fmt.Fprintf(stdout, "      evidence: %s\n", joinIDs(p.SessionIDs, 8))
		}
	}
	for _, p := range res.Proposals {
		fmt.Fprintf(stdout, "  proposal %s %s → %s", p.Surface, p.Name, p.Status)
		if p.Rationale != "" {
			fmt.Fprintf(stdout, " rationale=%q", p.Rationale)
		}
		fmt.Fprintln(stdout)
		if p.Audit != "" {
			fmt.Fprintf(stdout, "      audit: %s\n", p.Audit)
		}
	}
	if len(res.Patterns) == 0 {
		fmt.Fprintln(stdout, "no recurring failure patterns since last run")
	}
	return nil
}

func learnRuntime(cfg config.Config, runtime *modelRuntimeResolver) (string, llm.Provider, error) {
	model := runtimeUtilityModel(cfg)
	if model == "" {
		return "", nil, nil
	}
	if runtime == nil {
		runtime = newModelRuntimeResolver(cfg)
	}
	if _, _, _, err := runtime.resolve(model); err != nil {
		return "", nil, err
	}
	return model, runtime, nil
}

func learnDigest(ctx context.Context, cfg config.Config, st *store.Store) (string, error) {
	var out bytes.Buffer
	if err := runLearn(ctx, cfg, st, &out); err != nil {
		return "", err
	}
	return out.String(), nil
}

func joinIDs(ids []string, max int) string {
	if len(ids) <= max {
		return fmt.Sprintf("%v", ids)
	}
	return fmt.Sprintf("%v…(+%d)", ids[:max], len(ids)-max)
}
