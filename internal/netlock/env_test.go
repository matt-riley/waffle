package netlock

import (
	"errors"
	"strings"
	"testing"
)

func TestApplyFromEnvNoopWhenUnset(t *testing.T) {
	called := false
	err := ApplyFromEnv(func(string) string { return "" }, func(string) error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if called {
		t.Fatal("lockdown should not run when env unset")
	}
}

func TestApplyFromEnvFailClosed(t *testing.T) {
	err := ApplyFromEnv(func(k string) string {
		if k == EnvLockdown {
			return "1"
		}
		return ""
	}, func(host string) error {
		if host != "waffle-host" {
			t.Fatalf("host = %q", host)
		}
		return errors.New("permission denied")
	})
	if err == nil {
		t.Fatal("want error when lockdown fails")
	}
	if !strings.Contains(err.Error(), "net lockdown required") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("err = %v, want wrapped cause", err)
	}
}

func TestApplyFromEnvSuccess(t *testing.T) {
	var gotHost string
	err := ApplyFromEnv(func(k string) string {
		switch k {
		case EnvLockdown:
			return "true"
		case EnvLockdownHost:
			return "my-host"
		default:
			return ""
		}
	}, func(host string) error {
		gotHost = host
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotHost != "my-host" {
		t.Fatalf("host = %q", gotHost)
	}
}
