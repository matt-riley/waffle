package main

import (
	"context"
	"fmt"
	"io"

	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/llm/anthropicp"
	"github.com/matt-riley/waffle/internal/llm/openaip"
	"github.com/matt-riley/waffle/internal/memory"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/skill"
)

// learnCmd runs the mine→propose→validate learning loop (#65).
// Suitable for cron: prints a digest to stdout (deliver via cron --deliver).
//
//	waffle learn
//	waffle cron add learn-daily 0 3 * * * "ignored"  # prefer: shell cron → waffle learn
//
// Example system crontab entry:
//
//	0 3 * * * waffle learn >> ~/.waffle/learn.log 2>&1
//
// Or a waffle cron job whose prompt is a no-op and a host wrapper runs learn;
// the digest on stdout is the deliverable.
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

	ws, err := memory.Open(memory.DefaultAgent)
	if err != nil {
		return err
	}
	sessions := session.New(st)
	l := skill.NewLearnerFromStore(st, sessions, ws)

	// Attribution uses utility model when configured (#65).
	if cfg.Provider.UtilityModel != "" {
		apiKey, _, err := resolveAPIKey(cfg.Provider)
		if err == nil && apiKey != "" {
			var provider llm.Provider
			switch cfg.Provider.Name {
			case "openai":
				base := cfg.Provider.BaseURL
				if base == "" {
					base = "https://api.openai.com/v1"
				}
				provider = openaip.New(apiKey, base)
			default:
				provider = anthropicp.New(apiKey, cfg.Provider.BaseURL)
			}
			l.Provider = provider
			l.Model = cfg.Provider.UtilityModel
		}
	}

	res, err := l.Run(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, "waffle learn digest")
	fmt.Fprintf(stdout, "run=%s patterns=%d proposals=%d accepted=%d rejected=%d provider_calls=%d\n",
		res.ID, len(res.Patterns), len(res.Proposals), res.Accepted, res.Rejected, res.ProviderCalls)
	if res.SinceAt != "" {
		fmt.Fprintf(stdout, "since=%s\n", res.SinceAt)
	}
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
		fmt.Fprintf(stdout, "  proposal %s %s → %s\n", p.Surface, p.Name, p.Status)
		if p.Audit != "" {
			fmt.Fprintf(stdout, "      audit: %s\n", p.Audit)
		}
	}
	if len(res.Patterns) == 0 {
		fmt.Fprintln(stdout, "no recurring failure patterns since last run")
	}
	return nil
}

func joinIDs(ids []string, max int) string {
	if len(ids) <= max {
		return fmt.Sprintf("%v", ids)
	}
	return fmt.Sprintf("%v…(+%d)", ids[:max], len(ids)-max)
}
