package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/schedule"
)

func cronCmd(ctx context.Context, args []string, stdout, stderr io.Writer) (err error) {
	if len(args) == 0 {
		cronUsage(stderr)
		return errUsage
	}

	cfg, st, err := openConfigAndStore(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := st.Close(); err == nil {
			err = cerr
		}
	}()
	jobs := schedule.NewStore(st)

	switch args[0] {
	case "ls", "list":
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
			next := "-"
			if !j.NextRetry.IsZero() {
				next = j.NextRetry.Format("2006-01-02 15:04:05")
			}
			prof := j.Profile
			if prof == "" {
				prof = "-"
			}
			fmt.Fprintf(stdout, "%s  %q  [%s] %s  deliver=%s  profile=%s  attempt=%d/%d  next-retry=%s  last=%s %s\n",
				j.ID, j.Name, j.Cron, state, orNone(j.Deliver), prof, j.Attempt, j.MaxAttempts, next, j.LastStatus, j.LastRun.Format("2006-01-02 15:04"))
			fmt.Fprintf(stdout, "    %s\n", j.Prompt)
		}
		return nil

	case "add":
		// waffle cron add <name> <cron(5 fields or quoted string)> <prompt...> [--deliver channel:chat] [--profile name]
		fields, deliver, profile, err := splitCronFlags(args[1:])
		if err != nil {
			return err
		}
		fields = normalizeCronAddFields(fields)
		if len(fields) < 7 {
			return fmt.Errorf("usage: waffle cron add <name> <m> <h> <dom> <mon> <dow> <prompt...> [--deliver channel:chat] [--profile name]\n       waffle cron add <name> \"<m> <h> <dom> <mon> <dow>\" <prompt...> [--deliver channel:chat] [--profile name]")
		}
		if profile != "" {
			if !config.ValidProfileName(profile) && profile != "main" {
				return fmt.Errorf("invalid profile name %q", profile)
			}
			if _, ok := cfg.Profile(profile); !ok {
				return fmt.Errorf("unknown agent profile %q", profile)
			}
		}
		name := fields[0]
		cron := joinFields(fields[1:6])
		prompt := joinFields(fields[6:])
		j, err := jobs.AddWithProfile(ctx, name, cron, prompt, deliver, profile)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "added %s %q [%s]", j.ID, j.Name, j.Cron)
		if j.Profile != "" {
			fmt.Fprintf(stdout, " profile=%s", j.Profile)
		}
		fmt.Fprintln(stdout)
		return nil

	case "rm", "remove":
		if len(args) != 2 {
			return fmt.Errorf("usage: waffle cron rm|remove <id>")
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
		runner, cleanup, err := buildCronRunner(ctx, cfg, st)
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
  waffle cron add <name> <m> <h> <dom> <mon> <dow> <prompt...> [--deliver channel:chat_id] [--profile name]
  waffle cron add <name> "<m> <h> <dom> <mon> <dow>" <prompt...> [--deliver=channel:chat_id] [--profile=name]
  waffle cron ls|list
  waffle cron run <id>     run a job now
  waffle cron rm|remove <id>

Example:
  waffle cron add standup 0 9 * * 1-5 "Summarize my starred repos" --deliver telegram:900 --profile researcher
  waffle cron add standup "0 9 * * 1-5" "Summarize my starred repos" --deliver=telegram:900
`)
}

func orNone(s string) string {
	if s == "" {
		return "(log)"
	}
	return s
}

// normalizeCronAddFields expands a single-string 5-field cron expression into
// five separate fields so both of these forms work:
//
//	name 0 9 * * 1-5 prompt...
//	name "0 9 * * 1-5" prompt...
func normalizeCronAddFields(fields []string) []string {
	if len(fields) < 3 {
		return fields
	}
	cronParts := strings.Fields(fields[1])
	if len(cronParts) != 5 {
		return fields
	}
	expanded := make([]string, 0, 1+5+len(fields)-2)
	expanded = append(expanded, fields[0])
	expanded = append(expanded, cronParts...)
	expanded = append(expanded, fields[2:]...)
	return expanded
}

func splitCronFlags(args []string) (fields []string, deliver, profile string, err error) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--deliver":
			if i+1 == len(args) {
				return nil, "", "", fmt.Errorf("--deliver requires a value (channel:chat_id, e.g. telegram:900)")
			}
			deliver = args[i+1]
			if _, _, ok := schedule.ParseTarget(deliver); !ok {
				return nil, "", "", fmt.Errorf("bad delivery target %q (want channel:chat_id)", deliver)
			}
			i++
		case strings.HasPrefix(arg, "--deliver="):
			deliver = strings.TrimPrefix(arg, "--deliver=")
			if deliver == "" {
				return nil, "", "", fmt.Errorf("--deliver requires a value (channel:chat_id, e.g. telegram:900)")
			}
			if _, _, ok := schedule.ParseTarget(deliver); !ok {
				return nil, "", "", fmt.Errorf("bad delivery target %q (want channel:chat_id)", deliver)
			}
		case arg == "--profile":
			if i+1 == len(args) {
				return nil, "", "", fmt.Errorf("--profile requires a name")
			}
			profile = args[i+1]
			i++
		case strings.HasPrefix(arg, "--profile="):
			profile = strings.TrimPrefix(arg, "--profile=")
			if profile == "" {
				return nil, "", "", fmt.Errorf("--profile requires a name")
			}
		default:
			fields = append(fields, arg)
		}
	}
	return fields, deliver, profile, nil
}

// splitDeliver remains for tests that only care about delivery.
func splitDeliver(args []string) (fields []string, deliver string, err error) {
	fields, deliver, _, err = splitCronFlags(args)
	return fields, deliver, err
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
