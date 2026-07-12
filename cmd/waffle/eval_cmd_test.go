package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestEvalCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{"eval"}, strings.NewReader(""), &stdout, &stderr)
	if err != nil {
		t.Fatalf("eval: %v\n%s", err, stdout.String())
	}
	if !strings.Contains(stdout.String(), "passed") {
		t.Fatalf("output = %q", stdout.String())
	}
}
