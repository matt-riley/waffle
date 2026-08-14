// CI approval gate (#415): approval="ci" requires successful, current CI
// evidence for the exact immutable candidate SHA before an upgrade may proceed.
// The gate uses the scoped GitHub App boundary (a checks:read installation
// token) — never ambient or broad credentials from the runtime — and fails
// closed on missing, failed, cancelled, timed-out, action-required, stale,
// pending, or skipped required checks, as well as on network/API errors.
package selfdev

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/matt-riley/waffle/internal/gitcred"
)

// CIGate verifies required checks for one immutable commit SHA.
type CIGate interface {
	Verify(ctx context.Context, repo string, sha string, required []string) (CIEvidence, error)
}

// CheckResult is one check run observed for a commit.
type CheckResult struct {
	Name       string
	Status     string // completed | in_progress | queued | pending
	Conclusion string // success | failure | cancelled | timed_out | action_required | skipped | neutral
	URL        string
	HeadSHA    string // the commit the run actually targeted
}

// CIEvidence is the commit-bound check evidence collected at approval time.
type CIEvidence struct {
	SHA        string
	Checks     []CheckResult
	VerifiedAt time.Time
}

// Passes fails closed: every required check must exist, target exactly e.SHA,
// be completed with conclusion "success". Anything else — missing, pending,
// skipped, neutral, failed, cancelled, timed-out, action-required, or a run
// for another SHA — is a denial with the check name and URL (#415).
func (e CIEvidence) Passes(required []string) (bool, string) {
	byName := map[string][]CheckResult{}
	for _, c := range e.Checks {
		byName[c.Name] = append(byName[c.Name], c)
	}
	for _, name := range required {
		runs := byName[name]
		if len(runs) == 0 {
			return false, fmt.Sprintf("required check %q is missing for %s", name, e.SHA)
		}
		// Any matching run must be green on this exact SHA; a stale run for a
		// different head can never satisfy the gate.
		ok := false
		var detail string
		for _, r := range runs {
			if r.HeadSHA != "" && r.HeadSHA != e.SHA {
				detail = fmt.Sprintf("required check %q targets %s, not %s", name, r.HeadSHA, e.SHA)
				continue
			}
			if r.Status != "completed" {
				detail = fmt.Sprintf("required check %q is %s for %s", name, r.Status, e.SHA)
				continue
			}
			if r.Conclusion != "success" {
				detail = fmt.Sprintf("required check %q conclusion %q for %s", name, r.Conclusion, e.SHA)
				if r.URL != "" {
					detail += " (" + r.URL + ")"
				}
				continue
			}
			ok = true
			break
		}
		if !ok {
			if detail == "" {
				detail = fmt.Sprintf("required check %q has no valid run for %s", name, e.SHA)
			}
			return false, detail
		}
	}
	return true, ""
}

// defaultRequiredChecks covers the repository's merge gate when
// selfdev.required_checks is unset: the primary CI workflow. Operators with
// exact job-name gates set required_checks explicitly.
var defaultRequiredChecks = []string{"ci"}

// NewGitHubCIGate returns the scoped GitHub-App-backed CI gate used by
// approval=ci (#415). It mints only checks:read installation tokens.
func NewGitHubCIGate(app *gitcred.App) CIGate {
	return &githubCIGate{app: app}
}

// githubCIGate verifies checks through the scoped GitHub App installation.
type githubCIGate struct {
	app *gitcred.App
}

// Verify collects check runs for the exact SHA and returns the evidence.
// Network/API errors fail closed (returned as errors), distinguishable from a
// genuine red check (#415).
func (g *githubCIGate) Verify(ctx context.Context, repo, sha string, required []string) (CIEvidence, error) {
	if g == nil || g.app == nil {
		return CIEvidence{}, fmt.Errorf("ci approval requires a configured [github.app]")
	}
	runs, err := g.app.CheckRunsForCommit(ctx, repo, sha)
	if err != nil {
		return CIEvidence{}, fmt.Errorf("verify CI checks for %s: %w", sha, err)
	}
	checks := make([]CheckResult, 0, len(runs))
	for _, r := range runs {
		checks = append(checks, CheckResult{
			Name: r.Name, Status: r.Status, Conclusion: r.Conclusion,
			URL: r.DetailsURL, HeadSHA: r.HeadSHA,
		})
	}
	return CIEvidence{SHA: sha, Checks: checks, VerifiedAt: time.Now().UTC()}, nil
}

// requiredChecks returns the effective required-check list: the configured
// list, or the safe default. Empty and duplicate entries are dropped.
func requiredChecks(configured []string) []string {
	if len(configured) == 0 {
		return append([]string(nil), defaultRequiredChecks...)
	}
	var out []string
	seen := map[string]bool{}
	for _, c := range configured {
		c = strings.TrimSpace(c)
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	return out
}
