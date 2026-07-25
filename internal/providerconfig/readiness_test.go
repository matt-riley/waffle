package providerconfig

import (
	"context"
	"errors"
	"os"
	"testing"
)

// enrolledManager returns a manager whose config already carries a working
// default model, i.e. a host that reported Ready.
func enrolledManager(t *testing.T) *Manager {
	t.Helper()
	manager := newTestManager(t)
	if err := manager.Add(context.Background(), validAddRequest()); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if got := readinessState(t, manager); got != "ready" {
		t.Fatalf("state after enrollment = %q, want ready", got)
	}
	return manager
}

func readinessState(t *testing.T, manager *Manager) string {
	t.Helper()
	status, err := manager.Status(context.Background())
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	return status.State
}

// editConfigOutOfBand appends an unrelated table the way an operator editing the
// live config by hand would, changing the hash without touching provider state.
func editConfigOutOfBand(t *testing.T, manager *Manager) {
	t.Helper()
	existing, err := os.ReadFile(manager.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	updated := append(existing, []byte("\n[dashboard]\nenabled = true\n")...)
	if err := os.WriteFile(manager.ConfigPath, updated, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyReadinessRestoresReadyAfterAnOutOfBandConfigEdit(t *testing.T) {
	manager := enrolledManager(t)
	editConfigOutOfBand(t, manager)

	// This is the regression: an edit that touches nothing about providers still
	// drops the reported state, which consumers read as "shut the service down".
	if got := readinessState(t, manager); got != "installed" {
		t.Fatalf("state after out-of-band edit = %q, want installed", got)
	}

	refreshed, err := manager.VerifyReadiness(context.Background())
	if err != nil {
		t.Fatalf("VerifyReadiness() error = %v", err)
	}
	if !refreshed {
		t.Fatal("VerifyReadiness() refreshed = false, want true")
	}
	if got := readinessState(t, manager); got != "ready" {
		t.Fatalf("state after verification = %q, want ready", got)
	}
}

func TestVerifyReadinessIsIdempotent(t *testing.T) {
	manager := enrolledManager(t)
	editConfigOutOfBand(t, manager)

	first, err := manager.VerifyReadiness(context.Background())
	if err != nil || !first {
		t.Fatalf("first VerifyReadiness() = %v, %v; want true, nil", first, err)
	}
	before, err := os.ReadFile(manager.readyPath())
	if err != nil {
		t.Fatal(err)
	}

	second, err := manager.VerifyReadiness(context.Background())
	if err != nil {
		t.Fatalf("second VerifyReadiness() error = %v", err)
	}
	if second {
		t.Error("second VerifyReadiness() refreshed = true, want false for an already-proven config")
	}
	after, err := os.ReadFile(manager.readyPath())
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("ready generation changed on an idempotent verification")
	}
}

func TestVerifyReadinessLeavesUnprovenStatesAlone(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*testing.T, *Manager)
	}{
		{
			name: "failing health probe",
			prepare: func(_ *testing.T, manager *Manager) {
				manager.Health = func(context.Context) error { return errors.New("unhealthy") }
			},
		},
		{
			name: "absent health probe",
			prepare: func(_ *testing.T, manager *Manager) {
				manager.Health = nil
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := enrolledManager(t)
			editConfigOutOfBand(t, manager)
			if err := os.Remove(manager.readyPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			tt.prepare(t, manager)

			refreshed, err := manager.VerifyReadiness(context.Background())

			if err != nil {
				t.Fatalf("VerifyReadiness() error = %v, want nil for an unproven state", err)
			}
			if refreshed {
				t.Error("VerifyReadiness() refreshed = true, want false")
			}
			if _, statErr := os.Stat(manager.readyPath()); !errors.Is(statErr, os.ErrNotExist) {
				t.Error("ready generation was written without a passing health probe")
			}
			if got := readinessState(t, manager); got != "installed" {
				t.Errorf("state = %q, want installed", got)
			}
		})
	}
}

func TestVerifyReadinessDoesNothingWithoutADefaultModel(t *testing.T) {
	manager := newTestManager(t)

	refreshed, err := manager.VerifyReadiness(context.Background())

	if err != nil {
		t.Fatalf("VerifyReadiness() error = %v", err)
	}
	if refreshed {
		t.Error("VerifyReadiness() refreshed = true, want false with no default model")
	}
	if _, statErr := os.Stat(manager.readyPath()); !errors.Is(statErr, os.ErrNotExist) {
		t.Error("ready generation was written for a host with no default model")
	}
}

func TestVerifyReadinessReportsLockContention(t *testing.T) {
	manager := enrolledManager(t)
	editConfigOutOfBand(t, manager)
	lease, err := manager.acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire() error = %v", err)
	}
	defer func() { _ = lease.Release() }()

	refreshed, err := manager.VerifyReadiness(context.Background())

	if !errors.Is(err, ErrLocked) {
		t.Fatalf("VerifyReadiness() error = %v, want ErrLocked", err)
	}
	if refreshed {
		t.Error("VerifyReadiness() refreshed = true while the config was locked")
	}
}
