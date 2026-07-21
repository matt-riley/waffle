package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCommandAliasDispatch verifies ls|list and rm|remove (and ws close)
// short/long forms hit the same command path rather than "unknown command"
// (issue #135).
func TestCommandAliasDispatch(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WAFFLE_HOME", home)

	fake := installFakeProviderManager(t)
	fake.list = []byte(`{"state":"installed","providers":{},"models":{}}` + "\n")
	installFakeProviderCatalogue(t, &fakeProviderCatalogue{})

	ctx := context.Background()

	t.Run("provider list aliases", func(t *testing.T) {
		for _, verb := range []string{"ls", "list"} {
			var stdout, stderr bytes.Buffer
			err := providerCmd(ctx, []string{verb}, strings.NewReader(""), &stdout, &stderr)
			if err != nil {
				t.Fatalf("provider %s: %v (stderr=%q)", verb, err, stderr.String())
			}
			if !strings.Contains(stdout.String(), "State:") {
				t.Fatalf("provider %s stdout = %q, want listing", verb, stdout.String())
			}
		}
	})

	t.Run("provider remove aliases", func(t *testing.T) {
		for _, verb := range []string{"rm", "remove"} {
			fake.removeName = ""
			var stdout bytes.Buffer
			err := providerCmd(ctx, []string{verb, "primary"}, strings.NewReader(""), &stdout, io.Discard)
			if err != nil {
				t.Fatalf("provider %s: %v", verb, err)
			}
			if fake.removeName != "primary" {
				t.Fatalf("provider %s removeName = %q, want primary", verb, fake.removeName)
			}
			if !strings.Contains(stdout.String(), "removed provider primary") {
				t.Fatalf("provider %s stdout = %q", verb, stdout.String())
			}
		}
	})

	t.Run("provider remove missing arg is usage not unknown", func(t *testing.T) {
		for _, verb := range []string{"rm", "remove"} {
			err := providerCmd(ctx, []string{verb}, strings.NewReader(""), io.Discard, io.Discard)
			if err == nil || !strings.Contains(err.Error(), "usage:") {
				t.Fatalf("provider %s missing arg error = %v, want usage", verb, err)
			}
			if strings.Contains(err.Error(), "unknown provider command") {
				t.Fatalf("provider %s treated as unknown: %v", verb, err)
			}
		}
	})

	t.Run("cron list aliases", func(t *testing.T) {
		for _, verb := range []string{"ls", "list"} {
			var stdout, stderr bytes.Buffer
			err := cronCmd(ctx, []string{verb}, &stdout, &stderr)
			if err != nil {
				t.Fatalf("cron %s: %v (stderr=%q)", verb, err, stderr.String())
			}
			if !strings.Contains(stdout.String(), "no jobs") {
				t.Fatalf("cron %s stdout = %q, want empty listing", verb, stdout.String())
			}
		}
	})

	t.Run("cron remove aliases missing arg is usage not unknown", func(t *testing.T) {
		for _, verb := range []string{"rm", "remove"} {
			err := cronCmd(ctx, []string{verb}, io.Discard, io.Discard)
			if err == nil || !strings.Contains(err.Error(), "usage:") {
				t.Fatalf("cron %s missing arg error = %v, want usage", verb, err)
			}
			if strings.Contains(err.Error(), "unknown cron command") {
				t.Fatalf("cron %s treated as unknown: %v", verb, err)
			}
		}
	})

	t.Run("cron remove aliases", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if err := cronCmd(ctx, []string{"add", "standup", "0", "9", "*", "*", "1-5", "hello"}, &stdout, &stderr); err != nil {
			t.Fatalf("cron add: %v", err)
		}
		// Extract job id from "added <id> ..."
		fields := strings.Fields(stdout.String())
		if len(fields) < 2 {
			t.Fatalf("cron add stdout = %q", stdout.String())
		}
		id := fields[1]
		stdout.Reset()
		stderr.Reset()
		if err := cronCmd(ctx, []string{"remove", id}, &stdout, &stderr); err != nil {
			t.Fatalf("cron remove: %v", err)
		}
		if !strings.Contains(stdout.String(), "removed "+id) {
			t.Fatalf("cron remove stdout = %q", stdout.String())
		}
		// Re-add and delete with short form.
		stdout.Reset()
		if err := cronCmd(ctx, []string{"add", "standup2", "0", "10", "*", "*", "1-5", "hello"}, &stdout, &stderr); err != nil {
			t.Fatalf("cron add: %v", err)
		}
		fields = strings.Fields(stdout.String())
		id = fields[1]
		stdout.Reset()
		if err := cronCmd(ctx, []string{"rm", id}, &stdout, &stderr); err != nil {
			t.Fatalf("cron rm: %v", err)
		}
		if !strings.Contains(stdout.String(), "removed "+id) {
			t.Fatalf("cron rm stdout = %q", stdout.String())
		}
	})

	t.Run("session list aliases", func(t *testing.T) {
		for _, verb := range []string{"ls", "list"} {
			var stdout, stderr bytes.Buffer
			err := sessionCmd(ctx, []string{verb}, strings.NewReader(""), &stdout, &stderr)
			if err != nil {
				t.Fatalf("session %s: %v", verb, err)
			}
			if !strings.Contains(stdout.String(), "no sessions") {
				t.Fatalf("session %s stdout = %q, want empty listing", verb, stdout.String())
			}
		}
	})

	t.Run("session remove aliases missing arg is usage not unknown", func(t *testing.T) {
		for _, verb := range []string{"rm", "remove"} {
			err := sessionCmd(ctx, []string{verb}, strings.NewReader(""), io.Discard, io.Discard)
			if err == nil || !strings.Contains(err.Error(), "usage:") {
				t.Fatalf("session %s missing arg error = %v, want usage", verb, err)
			}
			if strings.Contains(err.Error(), "unknown session command") {
				t.Fatalf("session %s treated as unknown: %v", verb, err)
			}
		}
	})

	t.Run("skills list aliases", func(t *testing.T) {
		for _, verb := range []string{"ls", "list"} {
			var stdout, stderr bytes.Buffer
			err := skillsCmd(ctx, []string{verb}, &stdout, &stderr)
			if err != nil {
				t.Fatalf("skills %s: %v", verb, err)
			}
			if !strings.Contains(stdout.String(), "no skills") {
				t.Fatalf("skills %s stdout = %q, want empty listing", verb, stdout.String())
			}
		}
	})

	t.Run("ws list aliases", func(t *testing.T) {
		for _, verb := range []string{"ls", "list"} {
			var stdout, stderr bytes.Buffer
			err := wsCmd(ctx, []string{verb}, &stdout, &stderr)
			if err != nil {
				t.Fatalf("ws %s: %v", verb, err)
			}
			if !strings.Contains(stdout.String(), "no workspaces") {
				t.Fatalf("ws %s stdout = %q, want empty listing", verb, stdout.String())
			}
		}
	})

	t.Run("ws close/rm/remove parse rejects before store", func(t *testing.T) {
		// Use a home that must not be created: parse failure for delete verbs
		// must not open the store (same boundary as close).
		blocked := filepath.Join(t.TempDir(), "must-not-be-created")
		t.Setenv("WAFFLE_HOME", blocked)
		t.Cleanup(func() { t.Setenv("WAFFLE_HOME", home) })

		for _, verb := range []string{"close", "rm", "remove"} {
			var stdout, stderr bytes.Buffer
			err := run(context.Background(), []string{"ws", verb, "a", "b"}, strings.NewReader(""), &stdout, &stderr)
			if err == nil {
				t.Fatalf("ws %s accepted ambiguous ids", verb)
			}
			if !strings.Contains(err.Error(), `got "a" and "b"`) {
				t.Fatalf("ws %s error = %q, want both conflicting tokens", verb, err)
			}
			if _, statErr := os.Stat(blocked); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("ws %s parse rejection mutated home: %v", verb, statErr)
			}
		}
	})

	t.Run("ws delete missing id is usage not unknown", func(t *testing.T) {
		for _, verb := range []string{"close", "rm", "remove"} {
			err := wsCmd(ctx, []string{verb}, io.Discard, io.Discard)
			if err == nil || !strings.Contains(err.Error(), "usage:") {
				t.Fatalf("ws %s missing arg error = %v, want usage", verb, err)
			}
			if strings.Contains(err.Error(), "unknown ws command") {
				t.Fatalf("ws %s treated as unknown: %v", verb, err)
			}
		}
	})
}

func TestCommandAliasHelpDocumentsVerbs(t *testing.T) {
	t.Run("provider", func(t *testing.T) {
		var b bytes.Buffer
		providerUsage(&b)
		out := b.String()
		for _, want := range []string{"ls|list", "rm|remove"} {
			if !strings.Contains(out, want) {
				t.Errorf("provider usage missing %q:\n%s", want, out)
			}
		}
	})
	t.Run("cron", func(t *testing.T) {
		var b bytes.Buffer
		cronUsage(&b)
		out := b.String()
		for _, want := range []string{"ls|list", "rm|remove"} {
			if !strings.Contains(out, want) {
				t.Errorf("cron usage missing %q:\n%s", want, out)
			}
		}
	})
	t.Run("ws", func(t *testing.T) {
		var b bytes.Buffer
		wsUsage(&b)
		out := b.String()
		for _, want := range []string{"ls|list", "close|rm|remove"} {
			if !strings.Contains(out, want) {
				t.Errorf("ws usage missing %q:\n%s", want, out)
			}
		}
	})
	t.Run("session", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if err := sessionCmd(context.Background(), []string{"help"}, strings.NewReader(""), &stdout, &stderr); err != nil {
			t.Fatal(err)
		}
		out := stdout.String()
		for _, want := range []string{"ls|list", "rm|remove"} {
			if !strings.Contains(out, want) {
				t.Errorf("session help missing %q:\n%s", want, out)
			}
		}
	})
	t.Run("skills", func(t *testing.T) {
		err := skillsCmd(context.Background(), nil, io.Discard, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "ls|list") {
			t.Fatalf("skills empty usage = %v, want ls|list", err)
		}
	})
}

func TestCommandAliasUnknownStillRejected(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WAFFLE_HOME", home)
	ctx := context.Background()

	// provider unknown does not need manager; cron/ws/session open store first.
	tests := []struct {
		name string
		run  func() error
		want string
	}{
		{
			name: "provider",
			run: func() error {
				return providerCmd(ctx, []string{"nope"}, strings.NewReader(""), io.Discard, io.Discard)
			},
			want: `unknown provider command "nope"`,
		},
		{
			name: "cron",
			run: func() error {
				return cronCmd(ctx, []string{"nope"}, io.Discard, io.Discard)
			},
			want: `unknown cron command "nope"`,
		},
		{
			name: "ws",
			run: func() error {
				return wsCmd(ctx, []string{"nope"}, io.Discard, io.Discard)
			},
			want: `unknown ws command "nope"`,
		},
		{
			name: "session",
			run: func() error {
				return sessionCmd(ctx, []string{"nope"}, strings.NewReader(""), io.Discard, io.Discard)
			},
			want: `unknown session command "nope"`,
		},
		{
			name: "skills",
			run: func() error {
				return skillsCmd(ctx, []string{"nope"}, io.Discard, io.Discard)
			},
			want: "usage: waffle skills",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.run()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want containing %q", err, tt.want)
			}
		})
	}
}
