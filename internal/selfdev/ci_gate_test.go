package selfdev

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// fakeCIGate records its inputs and returns canned evidence or an error.
type fakeCIGate struct {
	evidence CIEvidence
	err      error
	gotRepo  string
	gotSHA   string
	gotReq   []string
}

func (f *fakeCIGate) Verify(ctx context.Context, repo, sha string, required []string) (CIEvidence, error) {
	f.gotRepo, f.gotSHA, f.gotReq = repo, sha, required
	if f.err != nil {
		return CIEvidence{}, f.err
	}
	return f.evidence, nil
}

func check(name, status, conclusion, headSHA, url string) CheckResult {
	return CheckResult{Name: name, Status: status, Conclusion: conclusion, HeadSHA: headSHA, URL: url}
}

func TestCIEvidencePassesFailClosed(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef01234567"
	green := check("ci", "completed", "success", sha, "https://ci/1")
	tests := []struct {
		name     string
		required []string
		checks   []CheckResult
		wantOK   bool
	}{
		{"green", []string{"ci"}, []CheckResult{green}, true},
		{"green ignores unrelated runs", []string{"ci"}, []CheckResult{green, check("other", "completed", "failure", sha, "")}, true},
		{"red", []string{"ci"}, []CheckResult{check("ci", "completed", "failure", sha, "https://ci/fail")}, false},
		{"pending", []string{"ci"}, []CheckResult{check("ci", "in_progress", "", sha, "")}, false},
		{"queued", []string{"ci"}, []CheckResult{check("ci", "queued", "", sha, "")}, false},
		{"missing", []string{"ci"}, nil, false},
		{"wrong SHA", []string{"ci"}, []CheckResult{check("ci", "completed", "success", "ffffffffffffffffffffffffffffffffffffffff", "")}, false},
		{"skipped", []string{"ci"}, []CheckResult{check("ci", "completed", "skipped", sha, "")}, false},
		{"neutral", []string{"ci"}, []CheckResult{check("ci", "completed", "neutral", sha, "")}, false},
		{"cancelled", []string{"ci"}, []CheckResult{check("ci", "completed", "cancelled", sha, "")}, false},
		{"timed out", []string{"ci"}, []CheckResult{check("ci", "completed", "timed_out", sha, "")}, false},
		{"action required", []string{"ci"}, []CheckResult{check("ci", "completed", "action_required", sha, "")}, false},
		{"one of two red", []string{"ci", "lint"}, []CheckResult{green, check("lint", "completed", "failure", sha, "https://lint/1")}, false},
		{"stale duplicate plus fresh green", []string{"ci"}, []CheckResult{
			check("ci", "completed", "success", "ffffffffffffffffffffffffffffffffffffffff", ""),
			green,
		}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := CIEvidence{SHA: sha, Checks: tt.checks, VerifiedAt: time.Now()}
			ok, detail := ev.Passes(tt.required)
			if ok != tt.wantOK {
				t.Fatalf("Passes = %v (%s), want %v", ok, detail, tt.wantOK)
			}
			if !ok && detail == "" {
				t.Fatal("denial must include check name/URL detail")
			}
			if ok && detail != "" {
				t.Fatalf("pass must have empty detail, got %q", detail)
			}
		})
	}
}

// ciFixtureRepo builds a git repo with an origin remote and returns its main
// sha plus a helper to add commits.
func ciFixtureRepo(t *testing.T) (dir, sha string) {
	t.Helper()
	dir, sha = writeImmutableFixture(t)
	git := exec.Command("git", "-C", dir, "remote", "add", "origin", "https://github.com/owner/repo.git")
	if out, err := git.CombinedOutput(); err != nil {
		t.Fatalf("add origin: %v: %s", err, out)
	}
	return dir, sha
}

// TestUpgradeCIDenialFailsBeforeReview proves a red or missing required check
// denies the upgrade with the exact SHA and check detail, before any review
// provider call (no config is present in this test, so reaching review would
// fail differently).
func TestUpgradeCIDenialFailsBeforeReview(t *testing.T) {
	t.Setenv("WAFFLE_HOME", t.TempDir())
	ctx := context.Background()
	dir, sha := ciFixtureRepo(t)
	gate := &fakeCIGate{evidence: CIEvidence{SHA: sha, Checks: []CheckResult{
		check("ci", "completed", "failure", sha, "https://ci/fail"),
	}}}
	_, err := UpgradeWithOptions(ctx, dir, "", ioDiscard(), false, "ci", nil, WithCIGate(gate, nil))
	if err == nil || !strings.Contains(err.Error(), "ci approval denied") {
		t.Fatalf("err = %v, want ci denial", err)
	}
	if !strings.Contains(err.Error(), sha) || !strings.Contains(err.Error(), "https://ci/fail") {
		t.Fatalf("denial lacks sha/url: %v", err)
	}
	if gate.gotSHA != sha || strings.Join(gate.gotReq, ",") != "ci" {
		t.Fatalf("gate saw sha=%q required=%v, want %q [ci]", gate.gotSHA, gate.gotReq, sha)
	}
	if gate.gotRepo != "owner/repo" {
		t.Fatalf("gate repo = %q, want owner/repo from origin remote", gate.gotRepo)
	}
}

// TestUpgradeCIAPIFailureFailsClosed proves a network/API error is a distinct
// closed failure, not a green result (#415).
func TestUpgradeCIAPIFailureFailsClosed(t *testing.T) {
	t.Setenv("WAFFLE_HOME", t.TempDir())
	ctx := context.Background()
	dir, _ := ciFixtureRepo(t)
	ciBoom := errors.New("check-runs API unreachable")
	gate := &fakeCIGate{err: ciBoom}
	_, err := UpgradeWithOptions(ctx, dir, "", ioDiscard(), false, "ci", nil, WithCIGate(gate, nil))
	if err == nil || !strings.Contains(err.Error(), "ci approval failed closed") || !strings.Contains(err.Error(), "check-runs API unreachable") {
		t.Fatalf("err = %v, want closed API failure", err)
	}
}

// TestUpgradeCIWithoutGateFailsClosed proves approval=ci with no configured
// GitHub App denies before any review or build (#415).
func TestUpgradeCIWithoutGateFailsClosed(t *testing.T) {
	t.Setenv("WAFFLE_HOME", t.TempDir())
	ctx := context.Background()
	dir, _ := ciFixtureRepo(t)
	_, err := UpgradeWithOptions(ctx, dir, "", ioDiscard(), false, "ci", nil)
	if err == nil || !strings.Contains(err.Error(), "requires a configured [github.app]") {
		t.Fatalf("err = %v, want app-required denial", err)
	}
}

// TestUpgradeCIGreenProceedsPastGate proves green evidence satisfies the gate:
// the failure that follows is the review step (no reviewer configured), not a
// CI denial, and the gate saw the exact immutable SHA and required checks.
func TestUpgradeCIGreenProceedsPastGate(t *testing.T) {
	t.Setenv("WAFFLE_HOME", t.TempDir())
	ctx := context.Background()
	dir, sha := ciFixtureRepo(t)
	gate := &fakeCIGate{evidence: CIEvidence{SHA: sha, Checks: []CheckResult{
		check("ci", "completed", "success", sha, "https://ci/ok"),
	}}}
	_, err := UpgradeWithOptions(ctx, dir, "", ioDiscard(), false, "ci", []string{"internal/eval"}, WithCIGate(gate, []string{"ci"}))
	if err == nil {
		t.Fatal("expected a later-stage failure (review has no provider), not a CI pass end-to-end")
	}
	if strings.Contains(err.Error(), "ci approval") {
		t.Fatalf("green CI gate still denied: %v", err)
	}
	if gate.gotSHA != sha || strings.Join(gate.gotReq, ",") != "ci" {
		t.Fatalf("gate saw sha=%q required=%v", gate.gotSHA, gate.gotReq)
	}
	// CI evidence must be bound to the exact SHA.
	if gate.evidence.SHA != sha {
		t.Fatalf("evidence bound to %q, want %q", gate.evidence.SHA, sha)
	}
}

// TestRemoteRepoParsesScpAndHttpsURLs covers the origin-remote normalization.
func TestRemoteRepoParsesScpAndHttpsURLs(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	git := exec.Command("git", "-C", dir, "init", "-q")
	_ = git.Run()
	for _, url := range []string{
		"https://github.com/owner/repo.git",
		"git@github.com:owner/repo.git",
		"owner/repo",
	} {
		_ = exec.Command("git", "-C", dir, "remote", "remove", "origin").Run()
		if out, err := exec.Command("git", "-C", dir, "remote", "add", "origin", url).CombinedOutput(); err != nil {
			t.Fatalf("add origin %q: %v %s", url, err, out)
		}
		got, err := remoteRepo(ctx, dir)
		if err != nil {
			t.Fatalf("remoteRepo(%q) = %v", url, err)
		}
		if got != "owner/repo" {
			t.Fatalf("remoteRepo(%q) = %q, want owner/repo", url, got)
		}
	}
}

// TestRequiredChecksDefaultsAndDedupes verifies the safe default covers the
// merge gate and configured lists are deduped with empties dropped.
func TestRequiredChecksDefaultsAndDedupes(t *testing.T) {
	got := requiredChecks(nil)
	if len(got) != 1 || got[0] != "ci" {
		t.Fatalf("default = %v, want [ci]", got)
	}
	got = requiredChecks([]string{"ci", "", "ci", "go-lint"})
	if strings.Join(got, ",") != "ci,go-lint" {
		t.Fatalf("deduped = %v", got)
	}
}
