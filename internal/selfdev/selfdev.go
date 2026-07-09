// Package selfdev implements waffle's self-development loop (docs/plan.md,
// "Self-development loop"): doctor self-checks a build, upgrade builds a
// new binary from an approved ref and atomically swaps it in after doctor
// passes, and rollback restores the previous binary. Because waffle is a
// single compiled binary and its source is a git repo, code-level
// self-improvement is just repo-workspace work whose repo happens to be
// waffle's own — this package is the deploy end of that pipeline.
package selfdev

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/sandbox"
	"github.com/matt-riley/waffle/internal/secret"
	"github.com/matt-riley/waffle/internal/store"
)

// Check is one doctor probe result.
type Check struct {
	Name string
	OK   bool
	Info string
}

// Doctor runs waffle's self-checks: config parses, the database migrates
// on a throwaway copy, and the secret store round-trips. It never touches
// the live database. Returns the checks and whether all passed.
func Doctor(ctx context.Context) ([]Check, bool, error) {
	var checks []Check
	add := func(name string, err error, info string) {
		c := Check{Name: name, OK: err == nil, Info: info}
		if err != nil {
			c.Info = err.Error()
		}
		checks = append(checks, c)
	}

	cfgPath, err := config.Path()
	if err != nil {
		return nil, false, err
	}
	cfg, err := config.Load(cfgPath)
	add("config parses", err, cfgPath)

	// Migrate a consistent snapshot of the real DB (or a fresh one if there
	// is none yet) so a bad migration is caught without risking live data.
	tmp, err := os.MkdirTemp("", "waffle-doctor-*")
	if err != nil {
		return nil, false, err
	}
	defer os.RemoveAll(tmp) //nolint:errcheck // temp dir
	snapshot := filepath.Join(tmp, "waffle.db")
	dbPath, err := config.DBPath()
	if err != nil {
		return nil, false, err
	}
	hadLive, err := store.Snapshot(ctx, dbPath, snapshot)
	if err != nil {
		add("database snapshot", err, "")
	} else {
		st, err := store.Open(ctx, snapshot)
		info := "on a fresh database"
		if hadLive {
			info = "on a throwaway snapshot"
		}
		add("database migrates", err, info)
		if st != nil {
			_ = st.Close()
		}
	}

	// Secret store: if an identity exists, it must decrypt.
	if id, e := secret.LoadIdentity(); e == nil {
		sp, _ := config.SecretsPath()
		_, e = secret.OpenFile(sp, id).List()
		add("secret store opens", e, "")
	} else {
		add("secret store", nil, "no identity configured (skipped)")
	}

	// Sandbox runner: docker mode bind-mounts a linux waffle binary as the
	// container entrypoint. On a non-linux host that must be an explicitly
	// configured linux build; otherwise the sandbox dies on start with a
	// misleading "runner appears dead" timeout (#42). This gates on
	// cfg.UsesDocker(): the global [sandbox] mode is docker, or any
	// [agent.group.*] opts into docker while the global mode stays host (#33).
	// Repo workspaces always use docker regardless of mode, but they are not
	// gated here — `ws open` resolves the same runner binary at use time via
	// sandbox.ResolveRunnerBinary, so it fails fast with the same error; doctor
	// can't know ahead of time whether the operator will open one.
	if cfg.UsesDocker() {
		info, err := sandboxRunnerCheck(cfg.Sandbox.RunnerBinary)
		add("sandbox runner", err, info)
	} else {
		add("sandbox runner", nil, "host mode (skipped)")
	}
	allOK := true
	for _, c := range checks {
		if !c.OK {
			allOK = false
		}
	}
	return checks, allOK, nil
}

// sandboxRunnerCheck validates the docker-mode runner binary without starting
// a container, by exercising the exact same resolution StartDocker and the
// workspace runtime use — so doctor can't report OK on a setup that would fail
// fast at startup, and the two can't drift apart.
func sandboxRunnerCheck(runnerBinary string) (info string, err error) {
	resolved, err := sandbox.ResolveRunnerBinary(runnerBinary)
	if err != nil {
		return "", err
	}
	if runnerBinary != "" {
		return "runner_binary " + resolved, nil
	}
	return "using the running binary (" + resolved + ")", nil
}

// Upgrade builds waffle from repoDir at ref, runs doctor against the new
// binary, and — if it passes — atomically swaps it into place, keeping the
// previous binary for rollback. It returns the path that was replaced.
func Upgrade(ctx context.Context, repoDir, ref string, stderr io.Writer) (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		return "", err
	}

	if ref != "" {
		if err := validateRef(ref); err != nil {
			return "", err
		}
		// Trailing "--" marks end of pathspecs; with the ref already
		// validated it is belt-and-braces against option injection and
		// does not change checkout semantics for a branch/tag/sha.
		if err := run(ctx, repoDir, stderr, "git", "checkout", ref, "--"); err != nil {
			return "", fmt.Errorf("checkout %s: %w", ref, err)
		}
	}

	built := filepath.Join(repoDir, ".waffle-build")
	if err := run(ctx, repoDir, stderr, "go", "build", "-o", built, "./cmd/waffle"); err != nil {
		return "", fmt.Errorf("build: %w", err)
	}
	defer os.Remove(built) //nolint:errcheck // best-effort cleanup

	// Gate on the *new* binary's own doctor.
	out, err := exec.CommandContext(ctx, built, "doctor").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("new binary failed doctor:\n%s", out)
	}

	backup := self + ".prev"
	if err := copyFile(self, backup); err != nil {
		return "", fmt.Errorf("back up current binary: %w", err)
	}
	// Rename within the same directory is atomic; a crash mid-swap leaves
	// either the old or the new binary, never a truncated one.
	staged := self + ".new"
	if err := copyFile(built, staged); err != nil {
		return "", err
	}
	if err := os.Chmod(staged, 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(staged, self); err != nil {
		return "", fmt.Errorf("swap binary: %w", err)
	}
	return self, nil
}

// Rollback restores the binary saved by the last Upgrade.
func Rollback() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		return "", err
	}
	backup := self + ".prev"
	if _, err := os.Stat(backup); err != nil {
		return "", errors.New("no previous binary to roll back to")
	}
	if err := os.Rename(backup, self); err != nil {
		return "", err
	}
	return self, nil
}

// validateRef rejects refs git would parse as options ("--help", "-c"),
// which would otherwise be injected into the checkout argv and, via the
// build-and-swap that follows, escalate toward code execution.
func validateRef(ref string) error {
	if ref == "" {
		return errors.New("empty git ref")
	}
	if strings.HasPrefix(ref, "-") {
		return fmt.Errorf("invalid git ref %q: refs may not start with '-'", ref)
	}
	return nil
}

func run(ctx context.Context, dir string, stderr io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Stdout = stderr
	cmd.Stderr = stderr
	return cmd.Run()
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close() //nolint:errcheck // read-only
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close() //nolint:errcheck // copy already failed
		return err
	}
	return out.Close()
}

// ReExec replaces the current process with path (execve), preserving the
// environment. A long-running waffle (the gateway) uses this to hot-swap
// into a freshly upgraded binary; on success it does not return. Unix only.
func ReExec(path string, args []string) error {
	return reexec(path, args)
}
