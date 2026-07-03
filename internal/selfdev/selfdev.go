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

	"github.com/matt-riley/waffle/internal/config"
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

	// Migrate on a copy of the real DB (or a fresh one) so a bad migration
	// is caught without risking live data.
	tmp, err := os.MkdirTemp("", "waffle-doctor-*")
	if err != nil {
		return nil, false, err
	}
	defer os.RemoveAll(tmp) //nolint:errcheck // temp dir
	if dbPath, e := config.DBPath(); e == nil {
		if src, e := os.ReadFile(dbPath); e == nil {
			_ = os.WriteFile(filepath.Join(tmp, "waffle.db"), src, 0o600)
		}
	}
	st, err := store.Open(ctx, filepath.Join(tmp, "waffle.db"))
	add("database migrates", err, "on a throwaway copy")
	if st != nil {
		_ = st.Close()
	}

	// Secret store: if an identity exists, it must decrypt.
	if id, e := secret.LoadIdentity(); e == nil {
		sp, _ := config.SecretsPath()
		_, e = secret.OpenFile(sp, id).List()
		add("secret store opens", e, "")
	} else {
		add("secret store", nil, "no identity configured (skipped)")
	}

	_ = cfg
	allOK := true
	for _, c := range checks {
		if !c.OK {
			allOK = false
		}
	}
	return checks, allOK, nil
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
		if err := run(ctx, repoDir, stderr, "git", "checkout", ref); err != nil {
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
