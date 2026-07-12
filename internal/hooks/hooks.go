// Package hooks runs workspace lifecycle shell hooks inside the sandbox
// (issue #54). Host-side embedded hooks are a separate concern (#41).
package hooks

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Point is one of the four lifecycle points.
type Point string

const (
	AfterCreate  Point = "after_create"
	BeforeRun    Point = "before_run"
	AfterRun     Point = "after_run"
	BeforeRemove Point = "before_remove"
)

// DefaultTimeout is used when a hook does not specify one.
const DefaultTimeout = 5 * time.Minute

// Config holds the four optional commands and a default timeout.
type Config struct {
	AfterCreate  string
	BeforeRun    string
	AfterRun     string
	BeforeRemove string
	Timeout      time.Duration
}

// Command returns the shell command for a point, or empty if unset.
func (c Config) Command(p Point) string {
	switch p {
	case AfterCreate:
		return c.AfterCreate
	case BeforeRun:
		return c.BeforeRun
	case AfterRun:
		return c.AfterRun
	case BeforeRemove:
		return c.BeforeRemove
	default:
		return ""
	}
}

// TimeoutOrDefault returns c.Timeout or DefaultTimeout.
func (c Config) TimeoutOrDefault() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return DefaultTimeout
}

// Fatal reports whether failure at p aborts the lifecycle step.
// after_create and before_run are fatal; after_run and before_remove are not.
func Fatal(p Point) bool {
	return p == AfterCreate || p == BeforeRun
}

// Result is the captured outcome of one hook invocation.
type Result struct {
	Point  Point
	Output string
	Err    error
}

// TimeoutFunc creates a child context that cancels after d. Defaults to
// context.WithTimeout; tests may override for deterministic deadline simulation.
var TimeoutFunc = context.WithTimeout

// Executor runs a shell command inside the workspace container.
type Executor interface {
	// ExecBash runs cmd with a timeout and returns combined output.
	// isError is true when the tool reports a non-zero exit.
	ExecBash(ctx context.Context, cmd string, timeout time.Duration) (output string, isError bool, err error)
}

// Run executes the hook for p if configured. Empty command is a no-op success.
func Run(ctx context.Context, ex Executor, cfg Config, p Point) Result {
	cmd := strings.TrimSpace(cfg.Command(p))
	if cmd == "" {
		return Result{Point: p}
	}
	timeout := cfg.TimeoutOrDefault()
	runCtx, cancel := TimeoutFunc(ctx, timeout)
	defer cancel()
	out, isError, err := ex.ExecBash(runCtx, cmd, timeout)
	res := Result{Point: p, Output: out}
	if err != nil {
		res.Err = fmt.Errorf("hook %s: %w", p, err)
		return res
	}
	if isError {
		res.Err = fmt.Errorf("hook %s failed: %s", p, strings.TrimSpace(out))
		return res
	}
	if runCtx.Err() != nil && ctx.Err() == nil {
		res.Err = fmt.Errorf("hook %s timed out after %s", p, timeout)
	}
	return res
}

// ClientExecutor adapts a sandbox-like client with Exec(name, inputJSON).
type ClientExecutor struct {
	// Exec matches sandbox.Client.Exec: (ctx, name, input) -> (out, isError, err)
	Exec func(ctx context.Context, name string, input json.RawMessage) (string, bool, error)
}

// ExecBash implements Executor.
func (c ClientExecutor) ExecBash(ctx context.Context, cmd string, timeout time.Duration) (string, bool, error) {
	if c.Exec == nil {
		return "", false, fmt.Errorf("hooks: no executor")
	}
	secs := int(timeout.Seconds())
	if secs < 1 {
		secs = 1
	}
	input, err := json.Marshal(map[string]any{"command": cmd, "timeout_seconds": secs})
	if err != nil {
		return "", false, err
	}
	return c.Exec(ctx, "bash", input)
}

// Merge prefers non-empty repo hook commands over host config, then falls
// back. Timeout uses the shorter of the two positive values.
func Merge(host, repo Config) Config {
	out := host
	if repo.AfterCreate != "" {
		out.AfterCreate = repo.AfterCreate
	}
	if repo.BeforeRun != "" {
		out.BeforeRun = repo.BeforeRun
	}
	if repo.AfterRun != "" {
		out.AfterRun = repo.AfterRun
	}
	if repo.BeforeRemove != "" {
		out.BeforeRemove = repo.BeforeRemove
	}
	switch {
	case host.Timeout > 0 && repo.Timeout > 0:
		if repo.Timeout < host.Timeout {
			out.Timeout = repo.Timeout
		} else {
			out.Timeout = host.Timeout
		}
	case repo.Timeout > 0:
		out.Timeout = repo.Timeout
	}
	return out
}
