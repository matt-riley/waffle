package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
)

// Restart outcome codes delivered to Desk clients. Values are stable contracts.
const (
	RestartCodeScheduled             = "restart_scheduled"
	RestartCodeManualRestartRequired = "manual_restart_required"
	RestartCodeScheduleFailed        = "restart_schedule_failed"

	// EventTypeCapabilityRestartOutcome is published after a deferred restart
	// schedule completes so connected Desk clients can leave the wait state.
	EventTypeCapabilityRestartOutcome = "capability.restart_outcome"

	restartMessageScheduled = "Waffle restart was scheduled."
	restartMessageManual    = "Change committed; restart waffle serve to apply."
	restartMessageFailed    = "restart could not be scheduled; restart waffle serve to apply the change"
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

// plannedRestartOutcome is the client-facing outcome known before Schedule runs.
// Standalone is always manual; every other scheduler is treated as scheduled
// until the after-response callback reports otherwise via the event hub.
func plannedRestartOutcome(scheduler RestartScheduler) RestartScheduleOutcome {
	if _, ok := scheduler.(StandaloneRestartScheduler); ok {
		return RestartScheduleOutcome{
			Code:    RestartCodeManualRestartRequired,
			Message: restartMessageManual,
		}
	}
	return RestartScheduleOutcome{
		Scheduled: true,
		Code:      RestartCodeScheduled,
		Message:   restartMessageScheduled,
	}
}

// restartOutcomeFromError maps a Schedule result onto a sanitized public
// outcome. The raw error string never leaves the process.
func restartOutcomeFromError(err error) RestartScheduleOutcome {
	switch {
	case err == nil:
		return RestartScheduleOutcome{
			Scheduled: true,
			Code:      RestartCodeScheduled,
			Message:   restartMessageScheduled,
		}
	case errors.Is(err, ErrManualRestartRequired):
		return RestartScheduleOutcome{
			Code:    RestartCodeManualRestartRequired,
			Message: restartMessageManual,
		}
	default:
		return RestartScheduleOutcome{
			Code:    RestartCodeScheduleFailed,
			Message: restartMessageFailed,
		}
	}
}

// PublishRestartOutcome fans a sanitized restart outcome out through the Desk
// event hub. It never includes transaction IDs, commands, or host detail.
func PublishRestartOutcome(hub *EventHub, outcome RestartScheduleOutcome) {
	if hub == nil {
		return
	}
	// Re-seal fields so a misbehaving observer cannot inject host detail.
	sealed := RestartScheduleOutcome{
		Scheduled: outcome.Scheduled,
		Code:      outcome.Code,
		Message:   outcome.Message,
	}
	switch sealed.Code {
	case RestartCodeScheduled, RestartCodeManualRestartRequired, RestartCodeScheduleFailed:
	default:
		sealed.Code = RestartCodeScheduleFailed
		sealed.Scheduled = false
		sealed.Message = restartMessageFailed
	}
	data, err := json.Marshal(sealed)
	if err != nil {
		return
	}
	hub.Publish(Event{
		Type:     EventTypeCapabilityRestartOutcome,
		Resource: "capability",
		Data:     data,
	})
}

// AfterResponseWriter is supplied by the coordinator-owned mutation wrapper.
// Its callbacks run once only after the response has been copied to the real
// writer and flushed. The wrapper must observe the returned outcome. Cached
// idempotency replays must not register callbacks.
type AfterResponseWriter interface {
	AfterResponse(func() RestartScheduleOutcome)
}
