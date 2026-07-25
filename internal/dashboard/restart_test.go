package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestManagedRestartSchedulerUsesFixedCommandAndIgnoresTransactionID(t *testing.T) {
	var name string
	var args []string
	scheduler := ManagedRestartScheduler{
		Run: func(_ context.Context, gotName string, gotArgs ...string) error {
			name = gotName
			args = append([]string(nil), gotArgs...)
			return nil
		},
	}

	if err := scheduler.Schedule(t.Context(), "provider; touch /tmp/unsafe"); err != nil {
		t.Fatal(err)
	}
	if name != "systemctl" || !reflect.DeepEqual(args, []string{"--no-block", "restart", "waffle.service"}) {
		t.Fatalf("command = %q %v", name, args)
	}
}

func TestStandaloneRestartSchedulerReturnsOnlySanitizedInstruction(t *testing.T) {
	const transactionID = "secret-looking-transaction"
	err := (StandaloneRestartScheduler{}).Schedule(t.Context(), transactionID)
	if !errors.Is(err, ErrManualRestartRequired) {
		t.Fatalf("error = %v, want ErrManualRestartRequired", err)
	}
	if !strings.Contains(err.Error(), "waffle serve") || strings.Contains(err.Error(), transactionID) {
		t.Fatalf("unsafe standalone instruction: %v", err)
	}
}

func TestPlannedRestartOutcomeMatchesSchedulerMode(t *testing.T) {
	manual := plannedRestartOutcome(StandaloneRestartScheduler{})
	if manual.Code != RestartCodeManualRestartRequired || manual.Scheduled || !strings.Contains(manual.Message, "waffle serve") {
		t.Fatalf("standalone planned = %#v", manual)
	}
	scheduled := plannedRestartOutcome(ManagedRestartScheduler{})
	if scheduled.Code != RestartCodeScheduled || !scheduled.Scheduled {
		t.Fatalf("managed planned = %#v", scheduled)
	}
	// Wrappers that implement PlannedRestartReporter must not require a concrete
	// StandaloneRestartScheduler type assertion.
	manualWrapper := plannedRestartOutcome(manualRestartPlanner{})
	if manualWrapper.Code != RestartCodeManualRestartRequired || manualWrapper.Scheduled {
		t.Fatalf("reporter wrapper planned = %#v", manualWrapper)
	}
}

// manualRestartPlanner is a non-Standalone scheduler that still plans manual
// restart via PlannedRestartReporter.
type manualRestartPlanner struct{}

func (manualRestartPlanner) Schedule(context.Context, string) error {
	return ErrManualRestartRequired
}

func (manualRestartPlanner) PlannedRestart() RestartScheduleOutcome {
	return RestartScheduleOutcome{
		Code:    RestartCodeManualRestartRequired,
		Message: restartMessageManual,
	}
}

func TestPublishRestartOutcomeSealsAndOmitsHostDetail(t *testing.T) {
	hub := NewEventHub(2)
	PublishRestartOutcome(hub, RestartScheduleOutcome{
		Scheduled: false,
		Code:      RestartCodeManualRestartRequired,
		Message:   restartMessageManual,
	})
	PublishRestartOutcome(nil, RestartScheduleOutcome{Code: RestartCodeScheduled}) // no panic

	events, resync := hub.Subscribe(0)
	if resync {
		t.Fatal("unexpected resync")
	}
	defer hub.Unsubscribe(events)
	event := <-events
	if event.Type != EventTypeCapabilityRestartOutcome {
		t.Fatalf("type = %q", event.Type)
	}
	if strings.Contains(string(event.Data), "systemctl") || strings.Contains(string(event.Data), "INVOCATION_ID") {
		t.Fatalf("host detail in event: %s", event.Data)
	}
	var outcome RestartScheduleOutcome
	if err := json.Unmarshal(event.Data, &outcome); err != nil {
		t.Fatal(err)
	}
	if outcome.Code != RestartCodeManualRestartRequired || outcome.Message != restartMessageManual {
		t.Fatalf("outcome = %#v", outcome)
	}
}

func TestRestartOutcomeFromErrorIsSanitized(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want RestartScheduleOutcome
	}{
		{name: "nil", err: nil, want: RestartScheduleOutcome{Scheduled: true, Code: RestartCodeScheduled, Message: restartMessageScheduled}},
		{name: "manual", err: ErrManualRestartRequired, want: RestartScheduleOutcome{Code: RestartCodeManualRestartRequired, Message: restartMessageManual}},
		{name: "failed", err: errors.New("systemctl restart waffle.service: permission denied"), want: RestartScheduleOutcome{Code: RestartCodeScheduleFailed, Message: restartMessageFailed}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := restartOutcomeFromError(tt.err)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %#v want %#v", got, tt.want)
			}
			if strings.Contains(got.Message, "systemctl") || strings.Contains(got.Message, "permission") {
				t.Fatalf("message leaked host detail: %q", got.Message)
			}
		})
	}
}
