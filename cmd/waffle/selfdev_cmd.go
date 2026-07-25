package main

import (
	"context"
	"fmt"
	"io"

	"github.com/matt-riley/waffle/internal/agent"
	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/memory"
	"github.com/matt-riley/waffle/internal/schedule"
	"github.com/matt-riley/waffle/internal/selfdev"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/skill"
	"github.com/matt-riley/waffle/internal/store"
)

// loadWorkspace opens the default workspace and its skills — shared by
// chat, serve, and cron. Only active skills are indexed (#65); inactive
// distill/learn skills stay out of the system prompt until activated.
func loadWorkspace() (memory.Workspace, []skill.Skill, error) {
	return loadWorkspaceWithStore(nil)
}

// loadWorkspaceWithStore is loadWorkspace using skill_status overrides when st is set.
func loadWorkspaceWithStore(st *store.Store) (memory.Workspace, []skill.Skill, error) {
	ws, err := memory.Open(memory.DefaultAgent)
	if err != nil {
		return memory.Workspace{}, nil, err
	}
	var skills []skill.Skill
	if st != nil {
		if err := skill.RecoverPendingSkillUninstalls(context.Background(), st.DB, ws, st.SkillLifecycleGuard()); err != nil {
			return memory.Workspace{}, nil, fmt.Errorf("recover pending skill uninstall: %w", err)
		}
		skills, err = skill.DiscoverActive(ws.SkillsDir(), st.DB)
	} else {
		skills, err = skill.DiscoverActive(ws.SkillsDir(), nil)
	}
	if err != nil {
		return memory.Workspace{}, nil, err
	}
	return ws, skills, nil
}

func doctorCmd(ctx context.Context, stdout io.Writer) error {
	fmt.Fprintf(stdout, "waffle %s\n", version)
	checks, ok, err := selfdev.Doctor(ctx)
	if err != nil {
		return err
	}
	// MCP execution authorities are reported as first-class doctor checks
	// (selfdev.Doctor, #77 / #29) — not duplicated here.
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

const upgradeUsage = `Usage: waffle upgrade [ref] [--no-verify]
  Upgrade the waffle binary from the configured [repo] dir.
  ref           optional git commit/branch/tag to build
  --no-verify   skip vet/tests/lint (unsafe)
  -h, --help    show this help
`

func upgradeCmd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	ref, noVerify, help, err := parseUpgradeArgs(args)
	if err != nil {
		return err
	}
	if help {
		fmt.Fprint(stdout, upgradeUsage)
		return nil
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

	verify := cfg.Selfdev.Verify && !noVerify
	if noVerify {
		fmt.Fprintln(stderr, "warning: --no-verify is unsafe; vet, tests, and lint are being skipped")
	}
	path, err := selfdev.UpgradeWithOptions(ctx, cfg.Repo.Dir, ref, stderr, verify, cfg.Selfdev.Approval, cfg.Selfdev.Protected)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "upgraded %s\nprevious binary kept at %s.prev — `waffle rollback` restores it\n", path, path)
	fmt.Fprintln(stdout, "restart waffle (or `waffle serve`) to run the new code")
	return nil
}

func parseUpgradeArgs(args []string) (ref string, noVerify bool, help bool, err error) {
	for _, arg := range args {
		switch arg {
		case "-h", "--help", "help":
			return "", false, true, nil
		case "--no-verify":
			noVerify = true
			continue
		}
		if ref != "" {
			return "", false, false, fmt.Errorf("upgrade accepts at most one ref")
		}
		ref = arg
	}
	return ref, noVerify, false, nil
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
// Named profiles from config are pre-built against the cron tier so Job.Profile
// binds the same way as scheduled runs (#71). cleanup stops any sandboxes.
func buildCronRunner(ctx context.Context, cfg config.Config, st *store.Store) (*schedule.Runner, func(), error) {
	sessions := session.New(st)
	ws, skills, err := loadWorkspace()
	if err != nil {
		return nil, func() {}, err
	}
	var cleanups []func()
	cleanup := func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}
	a, closer, err := buildAgent(ctx, cfg, ws, skills, sessions, config.GroupCron)
	cleanups = append(cleanups, closer)
	if err != nil {
		return nil, cleanup, err
	}
	byProfile := make(map[string]*agent.Agent)
	for name := range cfg.Agent.Profiles {
		if name == "" || name == "main" {
			continue
		}
		pa, pCloser, err := buildAgentWithProfile(ctx, cfg, ws, skills, sessions, config.GroupCron, name)
		cleanups = append(cleanups, pCloser)
		if err != nil {
			return nil, cleanup, fmt.Errorf("profile %q (cron): %w", name, err)
		}
		byProfile[name] = pa
	}
	return &schedule.Runner{Agent: a, AgentsByProfile: byProfile, Sessions: sessions}, cleanup, nil
}
