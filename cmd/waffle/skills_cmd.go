package main

import (
	"context"
	"fmt"
	"io"

	"github.com/matt-riley/waffle/internal/memory"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/skill"
)

// skillsCmd implements skill utilities. Currently: audit (#65).
func skillsCmd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	_ = stderr
	if len(args) == 0 {
		return fmt.Errorf("usage: waffle skills audit")
	}
	switch args[0] {
	case "audit":
		return skillsAuditCmd(ctx, args[1:], stdout)
	default:
		return fmt.Errorf("usage: waffle skills audit")
	}
}

func skillsAuditCmd(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) > 0 {
		return fmt.Errorf("usage: waffle skills audit")
	}
	cfg, st, err := openConfigAndStore(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	sessions := session.New(st)
	patterns, err := skill.MineToolErrors(ctx, sessions, 30)
	if err != nil {
		return err
	}
	if len(patterns) == 0 {
		fmt.Fprintln(stdout, "no recurring tool-error patterns found")
		return nil
	}
	fmt.Fprintf(stdout, "found %d recurring pattern(s):\n", len(patterns))
	for _, p := range patterns {
		fmt.Fprintf(stdout, "  (%d×) %s\n", p.Count, p.Signature)
	}
	ws, err := memory.Open(memory.DefaultAgent)
	if err != nil {
		return err
	}
	gate := &memory.Gate{Mode: cfg.Memory.WriteGate, WS: ws}
	if gate.Mode == "" {
		gate.Mode = "auto"
	}
	// Force review for mined skills regardless of write_gate auto.
	n, err := skill.ProposeSkills(ctx, patterns, gate, 2)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "wrote %d pending skill candidate(s) under %s/pending\n", n, ws.Dir)
	return nil
}
