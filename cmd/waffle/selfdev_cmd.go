package main

import (
	"context"
	"fmt"
	"io"

	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/memory"
	"github.com/matt-riley/waffle/internal/schedule"
	"github.com/matt-riley/waffle/internal/selfdev"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/skill"
	"github.com/matt-riley/waffle/internal/store"
)

// loadWorkspace opens the default workspace and its skills — shared by
// chat, serve, and cron.
func loadWorkspace() (memory.Workspace, []skill.Skill, error) {
	ws, err := memory.Open(memory.DefaultAgent)
	if err != nil {
		return memory.Workspace{}, nil, err
	}
	skills, err := skill.Discover(ws.SkillsDir())
	if err != nil {
		return memory.Workspace{}, nil, err
	}
	return ws, skills, nil
}

func doctorCmd(ctx context.Context, stdout io.Writer) error {
	checks, ok, err := selfdev.Doctor(ctx)
	if err != nil {
		return err
	}
	for _, c := range checks {
		mark := "ok  "
		if !c.OK {
			mark = "FAIL"
		}
		fmt.Fprintf(stdout, "[%s] %s", mark, c.Name)
		if c.Info != "" {
			fmt.Fprintf(stdout, " — %s", c.Info)
		}
		fmt.Fprintln(stdout)
	}
	if !ok {
		return fmt.Errorf("doctor found problems")
	}
	fmt.Fprintln(stdout, "all checks passed")
	return nil
}

func upgradeCmd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	ref := ""
	if len(args) > 0 {
		ref = args[0]
	}
	cfgPath, err := config.Path()
	if err != nil {
		return err
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	if cfg.Repo.Dir == "" {
		return fmt.Errorf("set [repo] dir in config.toml to a waffle checkout to enable upgrade")
	}

	fmt.Fprintf(stdout, "building waffle from %s", cfg.Repo.Dir)
	if ref != "" {
		fmt.Fprintf(stdout, " @ %s", ref)
	}
	fmt.Fprintln(stdout, " ...")

	path, err := selfdev.Upgrade(ctx, cfg.Repo.Dir, ref, stderr)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "upgraded %s\nprevious binary kept at %s.prev — `waffle rollback` restores it\n", path, path)
	fmt.Fprintln(stdout, "restart waffle (or `waffle serve`) to run the new code")
	return nil
}

func rollbackCmd(stdout io.Writer) error {
	path, err := selfdev.Rollback()
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "rolled back %s to the previous binary\n", path)
	return nil
}

// buildCronRunner assembles a scheduler Runner sharing the agent build path.
// It uses the cron agent group so a manual `waffle cron run` matches the tier
// the scheduler runs jobs under in `waffle serve` (restricted — host bash
// denied by default), rather than previewing them with the owner's main tier.
// cleanup stops any sandbox the agent started.
func buildCronRunner(ctx context.Context, cfg config.Config, st *store.Store) (*schedule.Runner, func(), error) {
	sessions := session.New(st)
	ws, skills, err := loadWorkspace()
	if err != nil {
		return nil, func() {}, err
	}
	a, cleanup, err := buildAgent(ctx, cfg, ws, skills, sessions, config.GroupCron)
	if err != nil {
		return nil, cleanup, err
	}
	return &schedule.Runner{Agent: a, Sessions: sessions}, cleanup, nil
}
