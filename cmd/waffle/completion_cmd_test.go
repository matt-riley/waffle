package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestCompletionCmdShellScripts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		shell   string
		markers []string
	}{
		{
			shell: "bash",
			markers: []string{
				"complete",
				"chat",
				"serve",
				"completion",
			},
		},
		{
			shell: "zsh",
			markers: []string{
				"#compdef",
				"chat",
				"serve",
				"completion",
			},
		},
		{
			shell: "fish",
			markers: []string{
				"complete -c waffle",
				"chat",
				"serve",
				"completion",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.shell, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			err := completionCmd([]string{tt.shell}, &stdout, &stderr)
			if err != nil {
				t.Fatalf("completionCmd(%q): %v (stderr=%q)", tt.shell, err, stderr.String())
			}
			out := stdout.String()
			if out == "" {
				t.Fatalf("completionCmd(%q): empty stdout", tt.shell)
			}
			for _, marker := range tt.markers {
				if !strings.Contains(out, marker) {
					t.Errorf("completionCmd(%q) stdout missing %q:\n%s", tt.shell, marker, out)
				}
			}
		})
	}
}

func TestCompletionCmdHelp(t *testing.T) {
	t.Parallel()

	for _, args := range [][]string{
		{},
		{"--help"},
		{"-h"},
		{"help"},
	} {
		name := "no-args"
		if len(args) > 0 {
			name = strings.Join(args, " ")
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var stdout bytes.Buffer
			err := completionCmd(args, &stdout, io.Discard)
			if err != nil {
				t.Fatalf("completionCmd %v: %v", args, err)
			}
			out := stdout.String()
			if !strings.Contains(out, "bash") {
				t.Fatalf("completionCmd %v stdout missing bash install: %q", args, out)
			}
			if !strings.Contains(out, "source") {
				t.Fatalf("completionCmd %v stdout missing source install: %q", args, out)
			}
			if !strings.Contains(out, "zsh") || !strings.Contains(out, "fish") {
				t.Fatalf("completionCmd %v stdout missing shell install lines: %q", args, out)
			}
		})
	}
}

func TestCompletionCmdUnknownShell(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	err := completionCmd([]string{"powershell"}, &stdout, &stderr)
	if err == nil {
		t.Fatal("completionCmd(powershell): want error")
	}
	if !strings.Contains(stderr.String(), "unknown shell") {
		t.Fatalf("stderr = %q, want unknown shell", stderr.String())
	}
}
