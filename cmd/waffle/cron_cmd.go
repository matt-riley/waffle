package main

import (
	"context"
	"fmt"
	"io"

	"github.com/matt-riley/waffle/internal/schedule"
)

func cronCmd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		cronUsage(stderr)
		return errUsage
	}

	cfg, st, err := openConfigAndStore(ctx)
	if err != nil {
		return err
	}
	defer st.Close() //nolint:errcheck // process is exiting
	jobs := schedule.NewStore(st)

	switch args[0] {
	case "ls":
		list, err := jobs.List(ctx)
		if err != nil {
			return err
		}
		if len(list) == 0 {
			fmt.Fprintln(stdout, "no jobs — add one with: waffle cron add <name> <cron> <prompt>")
			return nil
		}
		for _, j := range list {
			state := "enabled"
			if !j.Enabled {
				state = "disabled"
			}
			fmt.Fprintf(stdout, "%s  %q  [%s] %s  deliver=%s  last=%s %s\n",
				j.ID, j.Name, j.Cron, state, orNone(j.Deliver), j.LastStatus, j.LastRun.Format("2006-01-02 15:04"))
			fmt.Fprintf(stdout, "    %s\n", j.Prompt)
		}
		return nil

	case "add":
		// waffle cron add <name> <cron(5 fields)> <prompt...> [--deliver channel:chat]
		fields, deliver := splitDeliver(args[1:])
		if len(fields) < 7 {
			return fmt.Errorf("usage: waffle cron add <name> <m> <h> <dom> <mon> <dow> <prompt...> [--deliver channel:chat]")
		}
		name := fields[0]
		cron := joinFields(fields[1:6])
		prompt := joinFields(fields[6:])
		j, err := jobs.Add(ctx, name, cron, prompt, deliver)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "added %s %q [%s]\n", j.ID, j.Name, j.Cron)
		return nil

	case "rm":
		if len(args) != 2 {
			return fmt.Errorf("usage: waffle cron rm <id>")
		}
		if err := jobs.Remove(ctx, args[1]); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "removed %s\n", args[1])
		return nil

	case "run":
		if len(args) != 2 {
			return fmt.Errorf("usage: waffle cron run <id>")
		}
		j, err := jobs.Get(ctx, args[1])
		if err != nil {
			return err
		}
		runner, cleanup, err := buildCronRunner(ctx, cfg, st, stderr)
		if err != nil {
			return err
		}
		defer cleanup()
		reply, err := runner.Run(ctx, *j)
		if err != nil {
			return err
		}
		fmt.Fprintln(stdout, reply)
		return nil

	case "help", "-h", "--help":
		cronUsage(stdout)
		return nil
	default:
		cronUsage(stderr)
		return fmt.Errorf("unknown cron command %q", args[0])
	}
}

func cronUsage(w io.Writer) {
	fmt.Fprint(w, `Scheduled jobs: a cron expression + a prompt + an optional delivery target.
Jobs fire while `+"`waffle serve`"+` is running.

Usage:
  waffle cron add <name> <m> <h> <dom> <mon> <dow> <prompt...> [--deliver channel:chat_id]
  waffle cron ls
  waffle cron run <id>     run a job now
  waffle cron rm <id>

Example:
  waffle cron add standup 0 9 * * 1-5 "Summarize my starred repos" --deliver telegram:900
`)
}

func orNone(s string) string {
	if s == "" {
		return "(log)"
	}
	return s
}

func splitDeliver(args []string) (fields []string, deliver string) {
	for i := 0; i < len(args); i++ {
		if args[i] == "--deliver" && i+1 < len(args) {
			deliver = args[i+1]
			i++
			continue
		}
		fields = append(fields, args[i])
	}
	return fields, deliver
}

func joinFields(fields []string) string {
	out := ""
	for i, f := range fields {
		if i > 0 {
			out += " "
		}
		out += f
	}
	return out
}
