package hooks

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeExec struct {
	// delay simulates a slow hook; cancelled by ctx.
	delay   time.Duration
	fail    bool
	lastCmd string
	calls   []string
}

func (f *fakeExec) ExecBash(ctx context.Context, cmd string, timeout time.Duration) (string, bool, error) {
	f.lastCmd = cmd
	f.calls = append(f.calls, cmd)
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return "", false, ctx.Err()
		}
	}
	if f.fail {
		return "boom", true, nil
	}
	return "ok: " + cmd, false, nil
}

func TestRunNoop(t *testing.T) {
	res := Run(context.Background(), &fakeExec{}, Config{}, AfterCreate)
	if res.Err != nil || res.Output != "" {
		t.Fatalf("%+v", res)
	}
}

func TestFatalSemantics(t *testing.T) {
	if !Fatal(AfterCreate) || !Fatal(BeforeRun) {
		t.Fatal("after_create and before_run must be fatal")
	}
	if Fatal(AfterRun) || Fatal(BeforeRemove) {
		t.Fatal("after_run and before_remove must not be fatal")
	}
}

func TestRunCapturesOutputAndFailure(t *testing.T) {
	ex := &fakeExec{}
	res := Run(context.Background(), ex, Config{AfterCreate: "go mod download", Timeout: time.Minute}, AfterCreate)
	if res.Err != nil || !strings.Contains(res.Output, "go mod download") {
		t.Fatalf("%+v", res)
	}
	ex.fail = true
	res = Run(context.Background(), ex, Config{BeforeRun: "false"}, BeforeRun)
	if res.Err == nil || !strings.Contains(res.Err.Error(), "before_run") {
		t.Fatalf("expected failure: %+v", res)
	}
}

func TestRunTimeout(t *testing.T) {
	// Stubbed clock: cancel immediately so a long fakeExec delay never waits
	// on a real timer (#54).
	old := TimeoutFunc
	t.Cleanup(func() { TimeoutFunc = old })
	TimeoutFunc = func(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
		c, cancel := context.WithCancel(parent)
		if d > 0 {
			cancel() // deadline already exceeded
		}
		return c, cancel
	}

	ex := &fakeExec{delay: time.Hour} // would hang if real clock were used
	res := Run(context.Background(), ex, Config{BeforeRun: "sleep 10", Timeout: time.Millisecond}, BeforeRun)
	if res.Err == nil {
		t.Fatal("expected timeout error")
	}
	if !errors.Is(res.Err, context.DeadlineExceeded) && !errors.Is(res.Err, context.Canceled) &&
		!strings.Contains(res.Err.Error(), "before_run") {
		t.Fatalf("err = %v", res.Err)
	}
}

func TestMerge(t *testing.T) {
	host := Config{AfterCreate: "host-setup", Timeout: time.Minute}
	repo := Config{BeforeRun: "repo-test", Timeout: 10 * time.Second}
	m := Merge(host, repo)
	if m.AfterCreate != "host-setup" || m.BeforeRun != "repo-test" || m.Timeout != 10*time.Second {
		t.Fatalf("%+v", m)
	}
}
