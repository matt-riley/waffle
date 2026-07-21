package main

import (
	"bytes"
	"context"
	"encoding/json"
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
		{name: "equals form", args: []string{"standup", "summarize", "--deliver=telegram:900"}, wantFields: []string{"standup", "summarize"}, wantDeliver: "telegram:900"},
		{name: "equals form mid args", args: []string{"standup", "--deliver=telegram:900", "summarize"}, wantFields: []string{"standup", "summarize"}, wantDeliver: "telegram:900"},
		{name: "equals empty value", args: []string{"standup", "summarize", "--deliver="}, wantErr: "--deliver requires a value"},
		{name: "equals invalid target", args: []string{"standup", "--deliver=banana", "summarize"}, wantErr: "bad delivery target \"banana\" (want channel:chat_id)"},
		{name: "trailing flag", args: []string{"standup", "summarize", "--deliver"}, wantErr: "--deliver requires a value (channel:chat_id, e.g. telegram:900)"},
		{name: "invalid target", args: []string{"standup", "--deliver", "banana", "summarize"}, wantErr: "bad delivery target \"banana\" (want channel:chat_id)"},
		{name: "last duplicate wins", args: []string{"standup", "--deliver", "telegram:1", "summarize", "--deliver", "telegram:2"}, wantFields: []string{"standup", "summarize"}, wantDeliver: "telegram:2"},
		{name: "equals wins over earlier space form", args: []string{"standup", "--deliver", "telegram:1", "summarize", "--deliver=telegram:2"}, wantFields: []string{"standup", "summarize"}, wantDeliver: "telegram:2"},
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

func TestNormalizeCronAddFields(t *testing.T) {
	tests := []struct {
		name   string
		fields []string
		want   []string
	}{
		{
			name:   "five separate cron tokens unchanged",
			fields: []string{"standup", "0", "9", "*", "*", "1-5", "summarize"},
			want:   []string{"standup", "0", "9", "*", "*", "1-5", "summarize"},
		},
		{
			name:   "single-string cron expanded",
			fields: []string{"standup", "0 9 * * 1-5", "summarize", "the", "day"},
			want:   []string{"standup", "0", "9", "*", "*", "1-5", "summarize", "the", "day"},
		},
		{
			name:   "single-string cron with multi-word prompt",
			fields: []string{"job", "*/15 * * * *", "do", "work"},
			want:   []string{"job", "*/15", "*", "*", "*", "*", "do", "work"},
		},
		{
			name:   "too few fields unchanged",
			fields: []string{"standup", "0 9 * * 1-5"},
			want:   []string{"standup", "0 9 * * 1-5"},
		},
		{
			name:   "non-five-part second field unchanged",
			fields: []string{"standup", "0 9 *", "prompt"},
			want:   []string{"standup", "0 9 *", "prompt"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeCronAddFields(tt.fields)
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("normalizeCronAddFields(%q) = %q, want %q", tt.fields, got, tt.want)
			}
		})
	}
}

func TestSplitCronFlagsProfileEquals(t *testing.T) {
	fields, deliver, profile, err := splitCronFlags([]string{"standup", "0", "9", "*", "*", "*", "prompt", "--profile=researcher"})
	if err != nil {
		t.Fatal(err)
	}
	if profile != "researcher" {
		t.Fatalf("profile = %q, want researcher", profile)
	}
	if deliver != "" {
		t.Fatalf("deliver = %q, want empty", deliver)
	}
	if strings.Join(fields, ",") != "standup,0,9,*,*,*,prompt" {
		t.Fatalf("fields = %q", fields)
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

func TestCronAddAcceptsSingleStringCron(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WAFFLE_HOME", home)
	ctx := context.Background()
	var stdout, stderr bytes.Buffer
	err := cronCmd(ctx, []string{"add", "standup", "0 9 * * 1-5", "summarize", "the", "day"}, &stdout, &stderr)
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
	if jobs[0].Name != "standup" || jobs[0].Cron != "0 9 * * 1-5" || jobs[0].Prompt != "summarize the day" {
		t.Fatalf("stored job name=%q cron=%q prompt=%q", jobs[0].Name, jobs[0].Cron, jobs[0].Prompt)
	}
}

func TestCronAddAcceptsDeliverEqualsSyntax(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WAFFLE_HOME", home)
	ctx := context.Background()
	var stdout, stderr bytes.Buffer
	// Single-string cron + --deliver= must not leak flag into prompt.
	err := cronCmd(ctx, []string{"add", "standup", "0 9 * * 1-5", "summarize", "the", "day", "--deliver=telegram:900"}, &stdout, &stderr)
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
	if jobs[0].Deliver != "telegram:900" {
		t.Fatalf("deliver = %q, want telegram:900", jobs[0].Deliver)
	}
	if jobs[0].Prompt != "summarize the day" {
		t.Fatalf("prompt = %q, want %q", jobs[0].Prompt, "summarize the day")
	}
	if strings.Contains(jobs[0].Prompt, "--deliver") {
		t.Fatalf("prompt leaked --deliver flag: %q", jobs[0].Prompt)
	}
	if jobs[0].Cron != "0 9 * * 1-5" {
		t.Fatalf("cron = %q, want %q", jobs[0].Cron, "0 9 * * 1-5")
	}
}

func TestCronAddFiveTokenCronWithDeliverSpaceForm(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WAFFLE_HOME", home)
	ctx := context.Background()
	var stdout, stderr bytes.Buffer
	err := cronCmd(ctx, []string{"add", "standup", "0", "9", "*", "*", "1-5", "do", "work", "--deliver", "telegram:900"}, &stdout, &stderr)
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
	if jobs[0].Cron != "0 9 * * 1-5" || jobs[0].Deliver != "telegram:900" || jobs[0].Prompt != "do work" {
		t.Fatalf("stored job cron=%q deliver=%q prompt=%q", jobs[0].Cron, jobs[0].Deliver, jobs[0].Prompt)
	}
	if strings.Contains(jobs[0].Prompt, "--deliver") {
		t.Fatalf("prompt leaked --deliver: %q", jobs[0].Prompt)
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

func TestCronListJSONOutput(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WAFFLE_HOME", home)
	ctx := context.Background()
	var stdout, stderr bytes.Buffer
	if err := cronCmd(ctx, []string{"add", "standup", "0 9 * * 1-5", "summarize", "day", "--deliver", "telegram:900"}, &stdout, &stderr); err != nil {
		t.Fatalf("cron add: %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	if err := cronCmd(ctx, []string{"ls", "--json"}, &stdout, &stderr); err != nil {
		t.Fatalf("cron ls --json: %v", err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	if !json.Valid(stdout.Bytes()) {
		t.Fatalf("stdout is not valid JSON: %s", stdout.String())
	}
	var jobs []cronJobJSON
	if err := json.Unmarshal(stdout.Bytes(), &jobs); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("jobs = %+v, want one", jobs)
	}
	j := jobs[0]
	if j.ID == "" || j.Name != "standup" || j.Cron != "0 9 * * 1-5" || j.Prompt != "summarize day" {
		t.Fatalf("job = %+v", j)
	}
	if j.Deliver != "telegram:900" || !j.Enabled {
		t.Fatalf("deliver/enabled = %+v", j)
	}
}

func TestCronListJSONEmpty(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WAFFLE_HOME", home)
	ctx := context.Background()
	var stdout, stderr bytes.Buffer
	if err := cronCmd(ctx, []string{"ls", "--json"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	var jobs []cronJobJSON
	if err := json.Unmarshal(stdout.Bytes(), &jobs); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, stdout.String())
	}
	if jobs == nil {
		t.Fatal("want non-nil empty JSON array")
	}
	if len(jobs) != 0 {
		t.Fatalf("jobs = %+v, want empty", jobs)
	}
}
