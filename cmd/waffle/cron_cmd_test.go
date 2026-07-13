package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matt-riley/waffle/internal/schedule"
	"github.com/matt-riley/waffle/internal/store"
)

func TestSplitDeliver(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		wantFields  []string
		wantDeliver string
		wantErr     string
	}{
		{name: "no flag", args: []string{"standup", "summarize"}, wantFields: []string{"standup", "summarize"}},
		{name: "flag in middle", args: []string{"standup", "--deliver", "telegram:900", "summarize"}, wantFields: []string{"standup", "summarize"}, wantDeliver: "telegram:900"},
		{name: "flag at end", args: []string{"standup", "summarize", "--deliver", "telegram:900"}, wantFields: []string{"standup", "summarize"}, wantDeliver: "telegram:900"},
		{name: "trailing flag", args: []string{"standup", "summarize", "--deliver"}, wantErr: "--deliver requires a value (channel:chat_id, e.g. telegram:900)"},
		{name: "invalid target", args: []string{"standup", "--deliver", "banana", "summarize"}, wantErr: "bad delivery target \"banana\" (want channel:chat_id)"},
		{name: "last duplicate wins", args: []string{"standup", "--deliver", "telegram:1", "summarize", "--deliver", "telegram:2"}, wantFields: []string{"standup", "summarize"}, wantDeliver: "telegram:2"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fields, deliver, err := splitDeliver(tt.args)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("splitDeliver(%q) error = %v, want %q", tt.args, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("splitDeliver(%q): %v", tt.args, err)
			}
			if strings.Join(fields, ",") != strings.Join(tt.wantFields, ",") || deliver != tt.wantDeliver {
				t.Errorf("splitDeliver(%q) = (%q, %q), want (%q, %q)", tt.args, fields, deliver, tt.wantFields, tt.wantDeliver)
			}
		})
	}
}

func TestCronAddRejectsMissingDeliverValueWithoutCreatingJob(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WAFFLE_HOME", home)
	ctx := context.Background()
	var stdout, stderr bytes.Buffer
	err := cronCmd(ctx, []string{"add", "standup", "0", "9", "*", "*", "1-5", "summarize", "--deliver"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "--deliver requires a value") {
		t.Fatalf("cron add error = %v, want missing --deliver value", err)
	}

	db, err := store.Open(ctx, filepath.Join(home, "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	jobs, err := schedule.NewStore(db).List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("jobs = %+v, want no persisted jobs", jobs)
	}
}

func TestCronAddPersistsDeliveryOutsidePrompt(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WAFFLE_HOME", home)
	ctx := context.Background()
	var stdout, stderr bytes.Buffer
	err := cronCmd(ctx, []string{"add", "standup", "0", "9", "*", "*", "1-5", "summarize", "the", "day", "--deliver", "telegram:900"}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("cron add: %v", err)
	}
	db, err := store.Open(ctx, filepath.Join(home, "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	jobs, err := schedule.NewStore(db).List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("jobs = %+v, want one", jobs)
	}
	if jobs[0].Deliver != "telegram:900" || jobs[0].Prompt != "summarize the day" || strings.Contains(jobs[0].Prompt, "--deliver") {
		t.Fatalf("stored job deliver=%q prompt=%q", jobs[0].Deliver, jobs[0].Prompt)
	}
}

func TestCronListRendersPersistedAttemptAndNextRetry(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WAFFLE_HOME", home)
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(home, "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	j, err := schedule.NewStore(st).Add(ctx, "retrying", "0 9 * * *", "work", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.DB.Exec(`UPDATE jobs SET attempt=2,max_attempts=4,next_retry='2026-07-13T10:11:12Z',last_status='Stalled' WHERE id=?`, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	var stdout, stderr bytes.Buffer
	if err := cronCmd(ctx, []string{"ls"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"attempt=2/4", "next-retry=2026-07-13 10:11:12", "last=Stalled"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("output missing %q: %s", want, stdout.String())
		}
	}
}
