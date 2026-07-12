package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestResolveVersionPrefersStamped(t *testing.T) {
	if got := resolveVersion("v1.2.3"); got != "v1.2.3" {
		t.Errorf("resolveVersion(stamped) = %q, want stamped value", got)
	}
	if got := resolveVersion("abc1234.dirty"); got != "abc1234.dirty" {
		t.Errorf("resolveVersion(custom) = %q, want unchanged", got)
	}
}

func TestResolveVersionDevFallback(t *testing.T) {
	// Without controlling build info we only assert the function returns a
	// non-empty string: either VCS/module info or the "dev" placeholder.
	got := resolveVersion("dev")
	if got == "" {
		t.Error("resolveVersion(\"dev\") returned empty string")
	}
}

func TestVersionCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"version"}, strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatalf("run version: %v", err)
	}
	got := strings.TrimSpace(stdout.String())
	if !strings.HasPrefix(got, "waffle ") {
		t.Errorf("version output = %q, want prefix %q", got, "waffle ")
	}
	if version == "" {
		t.Error("package version is empty")
	}
	if !strings.Contains(got, version) {
		t.Errorf("version output = %q, want it to contain %q", got, version)
	}
}
