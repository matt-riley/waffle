package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	waffleeval "github.com/matt-riley/waffle/internal/eval"
)

func TestEvalCommand(t *testing.T) {
	t.Setenv("WAFFLE_HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{"eval"}, strings.NewReader(""), &stdout, &stderr)
	if err != nil {
		t.Fatalf("eval: %v\n%s", err, stdout.String())
	}
	if !strings.Contains(stdout.String(), "passed") {
		t.Fatalf("output = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if err := run(context.Background(), []string{"eval", "--history"}, strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatalf("eval --history: %v", err)
	}
	if !strings.Contains(stdout.String(), "ok") && !strings.Contains(stdout.String(), "passed") {
		t.Fatalf("history = %q", stdout.String())
	}
}

func TestEvalCommandReturnsFailureAndAssertionOutput(t *testing.T) {
	t.Setenv("WAFFLE_HOME", t.TempDir())
	old := evalRegistry
	evalRegistry = func() []waffleeval.Case {
		return []waffleeval.Case{{Name: "injected-failure", Run: func(context.Context) error {
			return fmt.Errorf("assertion: expected current source verification")
		}}}
	}
	t.Cleanup(func() { evalRegistry = old })

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{"eval"}, strings.NewReader(""), &stdout, &stderr)
	if err == nil {
		t.Fatal("eval command returned nil for failing case")
	}
	if !strings.Contains(stdout.String(), "FAIL injected-failure") ||
		!strings.Contains(stdout.String(), "assertion: expected current source verification") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}
