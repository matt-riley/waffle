package main

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

func TestForgetCmdHelpPrintsUsageWithoutOpeningStore(t *testing.T) {
	// Point WAFFLE_HOME at a path that cannot be a valid store. If forgetCmd
	// tried to open config/store for help, the call would fail.
	t.Setenv("WAFFLE_HOME", filepath.Join(t.TempDir(), "must-not-be-opened"))

	for _, args := range [][]string{
		{"--help"},
		{"-h"},
		{"help"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout bytes.Buffer
			err := forgetCmd(context.Background(), args, strings.NewReader("unused"), &stdout, io.Discard)
			if err != nil {
				t.Fatalf("forgetCmd %v: %v", args, err)
			}
			out := stdout.String()
			if !strings.Contains(out, "usage: waffle forget") {
				t.Fatalf("forgetCmd %v stdout = %q, want usage", args, out)
			}
			if strings.Contains(out, "[y/N]") {
				t.Fatalf("forgetCmd %v prompted for deletion: %q", args, out)
			}
		})
	}
}
