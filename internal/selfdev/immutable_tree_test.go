package selfdev

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/matt-riley/waffle/internal/config"
)

// writeImmutableFixture creates a git repo whose cmd/waffle/main.go carries a
// LEGIT marker and whose binary passes doctor/eval so a real build+swap works.
// Returns the repo dir and the sha of the initial commit.
func writeImmutableFixture(t *testing.T) (dir, sha string) {
	t.Helper()
	dir = t.TempDir()
	files := map[string]string{
		"go.mod": "module immutablefixture\n\ngo 1.25\n",
		"cmd/waffle/main.go": `package main

import "os"

const legitMarker = "LEGIT-MARKER-31337"

func main() {
	switch {
	case len(os.Args) > 1 && os.Args[1] == "eval":
		_, _ = os.Stdout.WriteString("PASS fixture-eval\n")
	case len(os.Args) > 1 && os.Args[1] == "doctor":
		_, _ = os.Stdout.WriteString("PASS fixture-doctor\n")
	default:
		_, _ = os.Stderr.WriteString(legitMarker + "\n")
		os.Exit(1)
	}
}
`,
	}
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	git("add", ".")
	git("commit", "-qm", "fixture base")
	return dir, git("rev-parse", "HEAD")
}

func buildFromWorktree(t *testing.T, ctx context.Context, repoDir, sha string) (binary string, wt string) {
	t.Helper()
	wt, err := addDetachedTempWorktree(ctx, repoDir, sha)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = removeWorktree(ctx, repoDir, wt) })
	if err := assertCleanTree(ctx, wt); err != nil {
		t.Fatalf("fresh worktree not clean: %v", err)
	}
	buildDir := t.TempDir()
	built, _, err := verifyAndBuild(ctx, wt, buildDir, ioDiscard(), false)
	if err != nil {
		t.Fatalf("build from worktree: %v", err)
	}
	return built, wt
}

func ioDiscard() *bytes.Buffer { return &bytes.Buffer{} }

// TestUpgradeWorktreeExcludesDirtyCheckout is the #413 regression: a
// non-overlapping dirty Go file in the configured source checkout must be
// absent from the built artifact, because verification and build happen in an
// isolated worktree created from the reviewed SHA.
func TestUpgradeWorktreeExcludesDirtyCheckout(t *testing.T) {
	ctx := context.Background()
	repo, sha := writeImmutableFixture(t)

	// Dirty, uncommitted Go file in the configured checkout (would be compiled
	// by a checkout-in-place build).
	dir := filepath.Join(repo, "internal", "agent")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	dirty := `package agent

const sneakyMarker = "SNEAKY-MARKER-999"
`
	if err := os.WriteFile(filepath.Join(dir, "sneaky.go"), []byte(dirty), 0o600); err != nil {
		t.Fatal(err)
	}

	built, wt := buildFromWorktree(t, ctx, repo, sha)
	if strings.Contains(strings.ToLower(wt), "sneaky") {
		t.Fatal("worktree leaked the dirty file")
	}
	raw, err := os.ReadFile(built)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte("LEGIT-MARKER-31337")) {
		t.Fatal("legit marker missing from artifact")
	}
	if bytes.Contains(raw, []byte("SNEAKY-MARKER-999")) {
		t.Fatal("dirty checkout bytes entered the artifact")
	}
	// The configured checkout still carries the dirty file untouched.
	if _, err := os.Stat(filepath.Join(repo, "internal", "agent", "sneaky.go")); err != nil {
		t.Fatalf("configured checkout mutated: %v", err)
	}
}

// TestNoRefUpgradeIsCommitBound verifies the no-ref/HEAD path resolves an
// immutable SHA, is reviewed against its parent, and never deploys uncommitted
// work (#413).
func TestNoRefUpgradeIsCommitBound(t *testing.T) {
	ctx := context.Background()
	repo, sha := writeImmutableFixture(t)

	resolved, err := resolveCommit(ctx, repo, "")
	if err != nil || resolved != sha {
		t.Fatalf("resolveCommit(HEAD) = %q, %v; want %q", resolved, err, sha)
	}
	base, err := reviewBaseSHA(ctx, repo, "", sha)
	if err != nil {
		t.Fatal(err)
	}
	if base == sha || base == "" {
		t.Fatalf("no-ref review base = %q, want HEAD's parent", base)
	}

	// Uncommitted work in the checkout must not reach the worktree.
	if err := os.WriteFile(filepath.Join(repo, "uncommitted.go"), []byte("package fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	wt, err := addDetachedTempWorktree(ctx, repo, resolved)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = removeWorktree(ctx, repo, wt) }()
	if _, err := os.Stat(filepath.Join(wt, "uncommitted.go")); !os.IsNotExist(err) {
		t.Fatalf("uncommitted file deployed into worktree: %v", err)
	}
}

// TestWorktreeCleanupAfterBuildFailure proves temporary worktree cleanup is
// reliable when the build fails, leaving no temp dir behind (#413).
func TestWorktreeCleanupAfterBuildFailure(t *testing.T) {
	ctx := context.Background()
	repo, _ := writeImmutableFixture(t)
	// Break the committed tree so the build fails.
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module immutablefixture\n\nbroken"), 0o600); err != nil {
		t.Fatal(err)
	}
	git := exec.Command("git", "-C", repo, "add", "go.mod")
	_ = git.Run()
	git = exec.Command("git", "-C", repo, "-c", "user.name=t", "-c", "user.email=t@e", "commit", "-qm", "break build")
	_ = git.Run()
	brokenSHA, err := resolveCommit(ctx, repo, "HEAD")
	if err != nil {
		t.Fatal(err)
	}

	wt, err := addDetachedTempWorktree(ctx, repo, brokenSHA)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := verifyAndBuild(ctx, wt, t.TempDir(), ioDiscard(), false); err == nil {
		_ = removeWorktree(ctx, repo, wt)
		t.Fatal("build unexpectedly succeeded")
	}
	if err := removeWorktree(ctx, repo, wt); err != nil {
		t.Fatalf("worktree removal: %v", err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("worktree dir %q leaked after failed build", wt)
	}
}

// TestSwapBinaryKeepsPreviousBinaryForRollback proves the install path keeps
// the previous binary as target.prev with its contents intact (#413).
func TestSwapBinaryKeepsPreviousBinaryForRollback(t *testing.T) {
	ctx := context.Background()
	repo, sha := writeImmutableFixture(t)
	built, _ := buildFromWorktree(t, ctx, repo, sha)

	target := filepath.Join(t.TempDir(), "waffle")
	if err := os.WriteFile(target, []byte("original-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	path, err := swapBinary(ctx, built, target)
	if err != nil {
		t.Fatal(err)
	}
	if path != target {
		t.Fatalf("swap returned %q", path)
	}
	prev, err := os.ReadFile(target + ".prev")
	if err != nil || string(prev) != "original-binary" {
		t.Fatalf("previous binary = %q, %v", prev, err)
	}
	newBin, err := os.ReadFile(target)
	if err != nil || !bytes.Contains(newBin, []byte("LEGIT-MARKER-31337")) {
		t.Fatalf("swapped binary = %d bytes, %v", len(newBin), err)
	}
}

// TestUpgradeRecordPersistsFullProvenance verifies the audit record carries
// base SHA, candidate SHA, tree hash, artifact SHA-256, approval policy, and
// verification result, appended durably and readable back (#413).
func TestUpgradeRecordPersistsFullProvenance(t *testing.T) {
	t.Setenv("WAFFLE_HOME", t.TempDir())
	ctx := context.Background()
	repo, sha := writeImmutableFixture(t)
	built, wt := buildFromWorktree(t, ctx, repo, sha)
	tree, err := treeHash(ctx, wt)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := fileSHA256(built)
	if err != nil {
		t.Fatal(err)
	}
	rec := UpgradeRecord{
		BaseSHA: sha, CandidateSHA: sha, TreeHash: tree,
		ArtifactSHA256: digest, Approval: "auto-patch", Verify: true,
		Verification: "ok", Version: "v1",
	}
	if err := persistUpgradeRecord(rec); err != nil {
		t.Fatal(err)
	}
	home, err := config.Home()
	if err != nil {
		t.Fatal(err)
	}
	raw, readErr := os.ReadFile(filepath.Join(home, "selfdev-upgrades.jsonl"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	var got UpgradeRecord
	if err := json.Unmarshal(bytes.TrimSpace(raw), &got); err != nil {
		t.Fatal(err)
	}
	if got.BaseSHA != sha || got.CandidateSHA != sha || got.TreeHash != tree ||
		got.ArtifactSHA256 != digest || got.Approval != "auto-patch" || got.Verification != "ok" {
		t.Fatalf("audit record = %+v", got)
	}
}

// TestResolveCommitPinsBranchToImmutableSha verifies a branch ref is resolved
// once to a commit SHA, so later force-pushes cannot change the reviewed tree.
func TestResolveCommitPinsBranchToImmutableSha(t *testing.T) {
	ctx := context.Background()
	repo, sha := writeImmutableFixture(t)
	got, err := resolveCommit(ctx, repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if got != sha {
		t.Fatalf("resolved = %q, want %q", got, sha)
	}
	// A dirty working tree does not change the resolved commit.
	if err := os.WriteFile(filepath.Join(repo, "dirty.go"), []byte("package fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got2, err := resolveCommit(ctx, repo, "")
	if err != nil || got2 != sha {
		t.Fatalf("resolve HEAD with dirty tree = %q, %v; want %q", got2, err, sha)
	}
}

// TestPersistUpgradeRecordConcurrentAppends proves JSONL appends never
// interleave or lose lines under concurrent writers (#425 review).
func TestPersistUpgradeRecordConcurrentAppends(t *testing.T) {
	t.Setenv("WAFFLE_HOME", t.TempDir())
	const writers = 4
	const each = 10
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if err := persistUpgradeRecord(UpgradeRecord{
					BaseSHA:      fmt.Sprintf("base-%d-%d", w, i),
					CandidateSHA: fmt.Sprintf("cand-%d-%d", w, i),
					Approval:     "manual",
					Verification: "ok",
				}); err != nil {
					t.Errorf("persist: %v", err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	home, _ := config.Home()
	raw, err := os.ReadFile(filepath.Join(home, "selfdev-upgrades.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(raw), []byte("\n"))
	if len(lines) != writers*each {
		t.Fatalf("lines = %d, want %d", len(lines), writers*each)
	}
	seen := map[string]bool{}
	for _, line := range lines {
		var r UpgradeRecord
		if err := json.Unmarshal(line, &r); err != nil {
			t.Fatalf("interleaved/corrupt audit line: %v: %q", err, line)
		}
		if seen[r.CandidateSHA] {
			t.Fatalf("duplicate audit line for %s", r.CandidateSHA)
		}
		seen[r.CandidateSHA] = true
	}
}

// TestUpgradeFailurePersistsAuditRecord proves an early failure (auto-patch
// protected-path refusal, before any worktree) still writes a durable failure
// record (#425 review).
func TestUpgradeFailurePersistsAuditRecord(t *testing.T) {
	t.Setenv("WAFFLE_HOME", t.TempDir())
	ctx := context.Background()
	dir := t.TempDir()
	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "-q", "-b", "base")
	if err := os.MkdirAll(filepath.Join(dir, "internal", "eval"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "internal", "eval", "eval.go"), []byte("package eval\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-qm", "touches protected path")
	_, err := UpgradeWithOptions(ctx, dir, "", ioDiscard(), false, "auto-patch", nil)
	if err == nil || !strings.Contains(err.Error(), "auto-patch refused") {
		t.Fatalf("UpgradeWithOptions = %v, want protected-path refusal", err)
	}
	home, _ := config.Home()
	raw, readErr := os.ReadFile(filepath.Join(home, "selfdev-upgrades.jsonl"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	var got UpgradeRecord
	if err := json.Unmarshal(bytes.TrimSpace(raw), &got); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Error, "auto-patch refused") || got.Verification != "failed" || got.CandidateSHA == "" {
		t.Fatalf("failure audit record = %+v", got)
	}
}
