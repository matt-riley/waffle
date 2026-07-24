package dashboard

import (
	"context"
	"errors"
	"os/exec"
)

// RestartScheduler requests a process restart after a successful deferred
// mutation. transactionID is diagnostic only and must never reach command
// arguments.
type RestartScheduler interface {
	Schedule(context.Context, string) error
}

// CommandRunner is the narrow command seam used by managed scheduling.
type CommandRunner func(context.Context, string, ...string) error

// ManagedRestartScheduler asks systemd to restart the fixed Waffle service.
type ManagedRestartScheduler struct {
	Run CommandRunner
}

func (s ManagedRestartScheduler) Schedule(ctx context.Context, _ string) error {
	run := s.Run
	if run == nil {
		run = func(ctx context.Context, name string, args ...string) error {
			return exec.CommandContext(ctx, name, args...).Run()
		}
	}
	return run(ctx, "systemctl", "--no-block", "restart", "waffle.service")
}

// ErrManualRestartRequired is returned in standalone mode without terminating
// the serving process.
var ErrManualRestartRequired = errors.New("restart waffle serve to apply the change")

// StandaloneRestartScheduler leaves the committed transaction recoverable and
// provides a credential-free operator instruction.
type StandaloneRestartScheduler struct{}

func (StandaloneRestartScheduler) Schedule(context.Context, string) error {
	return ErrManualRestartRequired
}

// RestartScheduleOutcome is the sanitized, operator-actionable result returned
// to the coordinator after the response is already visible to the client.
type RestartScheduleOutcome struct {
	Scheduled bool   `json:"scheduled"`
	Code      string `json:"code"`
	Message   string `json:"message"`
}

// AfterResponseWriter is supplied by the coordinator-owned mutation wrapper.
// Its callbacks run once only after the response has been copied to the real
// writer and flushed. The wrapper must observe the returned outcome. Cached
// idempotency replays must not register callbacks.
type AfterResponseWriter interface {
	AfterResponse(func() RestartScheduleOutcome)
}
