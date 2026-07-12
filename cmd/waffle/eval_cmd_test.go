package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
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
