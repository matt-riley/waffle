package main

import (
	"context"
	"fmt"
	"io"

	"github.com/matt-riley/waffle/internal/memory"
	"github.com/matt-riley/waffle/internal/skill"
)

// skillsCmd implements skill utilities: audit, activate, ls|list (#65).
func skillsCmd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	_ = stderr
	if len(args) == 0 {
		return fmt.Errorf("usage: waffle skills <audit|activate|ls|list [--json]>")
	}
	switch args[0] {
	case "audit":
		return skillsAuditCmd(ctx, args[1:], stdout)
	case "activate":
		return skillsActivateCmd(ctx, args[1:], stdout)
	case "ls", "list":
		return skillsListCmd(ctx, args[1:], stdout)
	default:
		return fmt.Errorf("usage: waffle skills <audit|activate|ls|list [--json]>")
	}
}

func skillsAuditCmd(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) > 0 {
		return fmt.Errorf("usage: waffle skills audit")
	}
	// Delegate to the full learn loop (mine since last run + digest).
	return learnCmd(ctx, nil, stdout, io.Discard)
}

func skillsActivateCmd(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: waffle skills activate <name>")
	}
	name := args[0]
	_, st, err := openConfigAndStore(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	ws, err := memory.Open(memory.DefaultAgent)
	if err != nil {
		return err
	}
	if err := skill.ActivateSkill(ctx, st.DB, ws, name); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "activated skill %q — now listed in the skills index\n", name)
	return nil
}

func skillsListCmd(ctx context.Context, args []string, stdout io.Writer) error {
	args, jsonOut := takeJSONFlag(args)
	if len(args) > 0 {
		return fmt.Errorf("usage: waffle skills ls|list [--json]")
	}
	_, st, err := openConfigAndStore(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	ws, err := memory.Open(memory.DefaultAgent)
	if err != nil {
		return err
	}
	all, err := skill.Discover(ws.SkillsDir())
	if err != nil {
		return err
	}
	active, err := skill.DiscoverActive(ws.SkillsDir(), st.DB)
	if err != nil {
		return err
	}
	activeSet := map[string]bool{}
	for _, s := range active {
		activeSet[s.Name] = true
	}
	if jsonOut {
		out := make([]skillJSON, 0, len(all))
		for _, s := range all {
			out = append(out, skillJSON{
				Name:        s.Name,
				Description: s.Description,
				Active:      activeSet[s.Name],
			})
		}
		return writeJSON(stdout, out)
	}
	if len(all) == 0 {
		fmt.Fprintln(stdout, "no skills")
		return nil
	}
	for _, s := range all {
		stLabel := "inactive"
		if activeSet[s.Name] {
			stLabel = "active"
		}
		fmt.Fprintf(stdout, "%s  %-10s  %s\n", s.Name, stLabel, s.Description)
	}
	return nil
}

// skillJSON is the machine-readable shape for `waffle skills ls --json`.
type skillJSON struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Active      bool   `json:"active"`
}
