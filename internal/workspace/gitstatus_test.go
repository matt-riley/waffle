package workspace

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestParseGitStatusReadsBranchDirtinessAndTracking(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
		want GitStatus
	}{
		{
			name: "clean tracked branch",
			out:  "branch\tmain\ndirty\t0\ntracking\t1\ncounts\t0\t0\nsha\t1a2b3c4\nsubject\tfeat: add git status",
			want: GitStatus{Branch: "main", Tracking: true, CommitSHA: "1a2b3c4", Subject: "feat: add git status"},
		},
		{
			name: "dirty and diverged",
			out:  "branch\ttopic\ndirty\t4\ntracking\t1\ncounts\t5\t2\nsha\tabc1234\nsubject\twip",
			want: GitStatus{
				Branch: "topic", DirtyFiles: 4, Tracking: true,
				Behind: 5, Ahead: 2, CommitSHA: "abc1234", Subject: "wip",
			},
		},
		{
			name: "no upstream",
			out:  "branch\tsolo\ndirty\t1\ntracking\t0\nsha\tdeadbee\nsubject\tinitial commit",
			want: GitStatus{Branch: "solo", DirtyFiles: 1, CommitSHA: "deadbee", Subject: "initial commit"},
		},
		{
			name: "detached head",
			out:  "branch\tHEAD\ndirty\t0\ntracking\t0\nsha\tfeedfac\nsubject\tdetached work",
			want: GitStatus{Detached: true, CommitSHA: "feedfac", Subject: "detached work"},
		},
		{
			name: "empty repository",
			out:  "branch\tHEAD\ndirty\t0\ntracking\t0\nsha\t\nsubject\t",
			want: GitStatus{Detached: true},
		},
		{
			name: "subject containing a tab keeps the whole subject",
			out:  "branch\tmain\ndirty\t0\ntracking\t0\nsha\t1a2b3c4\nsubject\tfix: tabs\there",
			want: GitStatus{Branch: "main", CommitSHA: "1a2b3c4", Subject: "fix: tabs\there"},
		},
		{
			name: "unparsable output stays zero rather than guessing",
			out:  "not a status line at all",
			want: GitStatus{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseGitStatus(tc.out)
			if got == nil || *got != tc.want {
				t.Fatalf("parseGitStatus = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestInspectGitReadsRunningWorkspaceWithoutChangingState(t *testing.T) {
	ctx := context.Background()
	tools := &scriptedBash{outputs: map[string]string{
		"--left-right": "branch\tmain\ndirty\t2\ntracking\t1\ncounts\t1\t3\nsha\t1a2b3c4\nsubject\tfeat: land it",
	}}
	mgr, rt := newTestManager(t, tools)

	ws, client, err := mgr.Open(ctx, "matt-riley/waffle")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			t.Errorf("close client: %v", err)
		}
	}()
	before := len(rt.events)

	status, err := mgr.InspectGit(ctx, ws.ID)
	if err != nil {
		t.Fatalf("InspectGit: %v", err)
	}
	want := GitStatus{
		Branch: "main", DirtyFiles: 2, Tracking: true, Behind: 1, Ahead: 3,
		CommitSHA: "1a2b3c4", Subject: "feat: land it",
	}
	if *status != want {
		t.Fatalf("status = %+v, want %+v", *status, want)
	}
	if got := rt.events[before:]; len(got) != 0 {
		t.Fatalf("git status touched the container lifecycle: %v", got)
	}

	after, err := mgr.Get(ctx, ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != StatusOpen {
		t.Fatalf("status = %q, want it unchanged", after.Status)
	}

	// The projection is a query: no fetch, push, checkout, or reset.
	tools.mu.Lock()
	commands := strings.Join(tools.commands, "\n")
	tools.mu.Unlock()
	for _, forbidden := range []string{"git fetch", "git pull", "git push", "git checkout", "git reset", "git clean"} {
		if strings.Contains(commands, forbidden) {
			t.Fatalf("git status ran %q: %s", forbidden, commands)
		}
	}
}

// AC4: an idle workspace reports unavailable instead of being started.
func TestInspectGitRefusesWorkspacesThatAreNotRunning(t *testing.T) {
	ctx := context.Background()
	tools := &scriptedBash{outputs: map[string]string{}}
	mgr, rt := newTestManager(t, tools)

	ws, client, err := mgr.Open(ctx, "matt-riley/waffle")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("close client: %v", err)
	}
	if err := mgr.Idle(ctx, ws.ID); err != nil {
		t.Fatalf("Idle: %v", err)
	}
	before := len(rt.events)

	if _, err := mgr.InspectGit(ctx, ws.ID); !errors.Is(err, ErrWorkspaceNotRunning) {
		t.Fatalf("InspectGit on idle = %v, want ErrWorkspaceNotRunning", err)
	}
	if got := rt.events[before:]; len(got) != 0 {
		t.Fatalf("idle inspection touched the runtime: %v", got)
	}
	after, err := mgr.Get(ctx, ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != StatusIdle {
		t.Fatalf("status = %q, want idle", after.Status)
	}

	if _, err := mgr.InspectGit(ctx, "ws-missing"); !errors.Is(err, ErrWorkspaceNotFound) {
		t.Fatalf("InspectGit on unknown = %v, want ErrWorkspaceNotFound", err)
	}
}
