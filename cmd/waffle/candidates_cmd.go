package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/matt-riley/waffle/internal/memory"
	"github.com/matt-riley/waffle/internal/secret"
)

// candidatesCmd is the operator workflow for pending memory and skill
// candidates (#416): list, inspect, approve, or deny the queue written by
// write_gate=review (or untrusted-derived writes). Decisions are atomic and
// serialized by the shared Gate; approval applies exactly the reviewed
// payload, denial never mutates live state.
const candidatesUsage = `Usage: waffle candidates <command> [args]

Manage pending memory and skill candidates (write_gate=review queue).

  waffle candidates list [--status pending|applied|denied] [--json]
  waffle candidates show <id> [--json]
  waffle candidates approve <id>
  waffle candidates deny <id> --reason "..."
`

func candidatesCmd(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: waffle candidates <list|show|approve|deny>")
	}
	ws, err := memory.Open(memory.DefaultAgent)
	if err != nil {
		return err
	}
	svc := &memory.CandidateService{Gate: &memory.Gate{WS: ws}}
	switch args[0] {
	case "list":
		return candidatesList(ctx, args[1:], svc, stdout)
	case "show":
		return candidatesShow(ctx, args[1:], svc, stdout)
	case "approve":
		return candidatesApprove(ctx, args[1:], svc, stdout)
	case "deny":
		return candidatesDeny(ctx, args[1:], svc, stdout)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, candidatesUsage)
		return nil
	default:
		return fmt.Errorf("unknown candidates subcommand %q (want list, show, approve, or deny)", args[0])
	}
}

func candidatesList(ctx context.Context, args []string, svc *memory.CandidateService, stdout io.Writer) error {
	status := ""
	asJSON := false
	for _, arg := range args {
		switch {
		case arg == "--json":
			asJSON = true
		case strings.HasPrefix(arg, "--status="):
			status = strings.TrimPrefix(arg, "--status=")
		case arg == "--status":
			return errors.New("usage: waffle candidates list [--status pending|applied|denied] [--json]")
		default:
			return fmt.Errorf("unknown list flag %q", arg)
		}
	}
	candidates, corrupt, err := svc.List(ctx, status)
	if err != nil {
		return err
	}
	if asJSON {
		return json.NewEncoder(stdout).Encode(candidates)
	}
	if len(candidates) == 0 {
		fmt.Fprintln(stdout, "no candidates")
	}
	for _, c := range candidates {
		fmt.Fprintf(stdout, "%-20s %-8s %-7s %-24s %s  %s\n", c.ID, c.Kind, c.Status, c.Name, c.CreatedAt.UTC().Format(time.RFC3339), c.Preview)
	}
	for _, c := range corrupt {
		fmt.Fprintf(stdout, "CORRUPT %s\n", c)
	}
	return nil
}

func candidatesShow(ctx context.Context, args []string, svc *memory.CandidateService, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: waffle candidates show <id> [--json]")
	}
	asJSON := false
	var id string
	for _, arg := range args {
		switch arg {
		case "--json":
			asJSON = true
		case "help", "-h", "--help":
			fmt.Fprint(stdout, candidatesUsage)
			return nil
		default:
			if id != "" {
				return errors.New("usage: waffle candidates show <id> [--json]")
			}
			id = arg
		}
	}
	insp, err := svc.Get(ctx, id)
	if err != nil {
		return err
	}
	if asJSON {
		return json.NewEncoder(stdout).Encode(insp)
	}
	c := insp.Candidate
	fmt.Fprintf(stdout, "id:          %s\n", c.ID)
	fmt.Fprintf(stdout, "kind:        %s\n", c.Kind)
	if c.Name != "" {
		fmt.Fprintf(stdout, "name:        %s\n", c.Name)
	}
	fmt.Fprintf(stdout, "status:      %s\n", c.Status)
	fmt.Fprintf(stdout, "created:     %s\n", c.CreatedAt.UTC().Format(time.RFC3339))
	p := c.Provenance
	fmt.Fprintf(stdout, "provenance:  source=%s source_id=%s trust=%s session=%s channel=%s untrusted=%t\n",
		p.SourceKind, p.SourceID, p.TrustClass, p.SessionID, p.Channel, p.UntrustedContext)
	if len(p.EvidenceIDs) > 0 {
		fmt.Fprintf(stdout, "evidence:    %s\n", strings.Join(p.EvidenceIDs, ", "))
	}
	if c.TargetID != "" {
		fmt.Fprintf(stdout, "target:      %s (%s)\n", c.TargetID, c.Action)
	}
	if c.Diff != "" {
		fmt.Fprintf(stdout, "diff:\n%s\n", c.Diff)
	}
	if c.Current != "" {
		fmt.Fprintf(stdout, "current:\n%s\n", c.Current)
	}
	if c.Body != "" {
		fmt.Fprintf(stdout, "body:\n%s\n", c.Body)
	}
	if c.ApprovedBy != "" {
		fmt.Fprintf(stdout, "approved:    by %s at %s\n", c.ApprovedBy, c.ApprovedAt.UTC().Format(time.RFC3339))
	}
	if c.DeniedBy != "" {
		fmt.Fprintf(stdout, "denied:      by %s at %s reason=%q\n", c.DeniedBy, c.DeniedAt.UTC().Format(time.RFC3339), c.DenyReason)
	}
	fmt.Fprintf(stdout, "approve:     waffle candidates approve %s\n", c.ID)
	fmt.Fprintf(stdout, "deny:        waffle candidates deny %s --reason \"...\"\n", c.ID)
	return nil
}

func candidatesApprove(ctx context.Context, args []string, svc *memory.CandidateService, stdout io.Writer) error {
	if len(args) != 1 {
		return errors.New("usage: waffle candidates approve <id>")
	}
	approver, err := candidateApprover()
	if err != nil {
		return err
	}
	// Inspect first so the decision is bound to the exact payload reviewed;
	// Approve re-checks the file digest under the gate lock.
	insp, err := svc.Get(ctx, args[0])
	if err != nil {
		return err
	}
	c, err := svc.Approve(ctx, insp.Candidate.ID, approver, insp.FileDigest)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "approved %s %s by %s\n", c.Kind, c.ID, approver)
	if c.Diff != "" {
		fmt.Fprintf(stdout, "%s\n", c.Diff)
	}
	if c.Kind == "skill" {
		fmt.Fprintln(stdout, "skill written inactive; activate it explicitly with `waffle skills activate "+c.Name+"`")
	}
	return nil
}

func candidatesDeny(ctx context.Context, args []string, svc *memory.CandidateService, stdout io.Writer) error {
	var id, reason string
	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "--reason="):
			reason = strings.TrimPrefix(arg, "--reason=")
		case arg == "--reason":
			return errors.New("usage: waffle candidates deny <id> --reason \"...\"")
		default:
			if id != "" {
				return errors.New("usage: waffle candidates deny <id> --reason \"...\"")
			}
			id = arg
		}
	}
	if id == "" || strings.TrimSpace(reason) == "" {
		return errors.New("usage: waffle candidates deny <id> --reason \"...\"")
	}
	approver, err := candidateApprover()
	if err != nil {
		return err
	}
	insp, err := svc.Get(ctx, id)
	if err != nil {
		return err
	}
	c, err := svc.Deny(ctx, insp.Candidate.ID, approver, reason, insp.FileDigest)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "denied %s %s by %s: %s\n", c.Kind, c.ID, approver, reason)
	return nil
}

// candidateApprover identifies the operator for the decision audit trail:
// the waffle identity when one exists, else "cli".
func candidateApprover() (string, error) {
	if id, err := secret.LoadIdentity(); err == nil {
		return id.String(), nil
	}
	return "cli", nil
}
