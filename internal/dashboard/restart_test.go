package dashboard

import (
	"context"
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
