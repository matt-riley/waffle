package main

import (
	"context"
	"strings"
	"testing"
)

func TestFixtureManyWorkspaceListAndGetRemainConsistent(t *testing.T) {
	workspaces := newFixtureWorkspaces()
	previousMode := fixtureWorkspaceMode.Load()
	fixtureWorkspaceMode.Store(fixtureWorkspaceModeMany)
	t.Cleanup(func() { fixtureWorkspaceMode.Store(previousMode) })

	listed, err := workspaces.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}

	manyCount := 0
	for _, listedWorkspace := range listed {
		if !strings.HasPrefix(listedWorkspace.ID, "workspace-many-") {
			continue
		}
		manyCount++
		listedWorkspace := listedWorkspace
		t.Run(listedWorkspace.ID, func(t *testing.T) {
			got, err := workspaces.Get(context.Background(), listedWorkspace.ID)
			if err != nil {
				t.Fatalf("Get(%q) error = %v", listedWorkspace.ID, err)
			}
			if got.ID != listedWorkspace.ID {
				t.Errorf("Get(%q).ID = %q, want %q", listedWorkspace.ID, got.ID, listedWorkspace.ID)
			}
			if got.Repo != listedWorkspace.Repo {
				t.Errorf("Get(%q).Repo = %q, want %q", listedWorkspace.ID, got.Repo, listedWorkspace.Repo)
			}
			if got.SessionID != listedWorkspace.SessionID {
				t.Errorf("Get(%q).SessionID = %q, want %q", listedWorkspace.ID, got.SessionID, listedWorkspace.SessionID)
			}
		})
	}
	if manyCount != 4 {
		t.Fatalf("List() returned %d many workspaces, want 4", manyCount)
	}
}

func TestEmptyStateThemeDocumentStartUsesApprovedLiterals(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		requested string
		shell     bool
		want      string
	}{
		{
			name:      "light shell",
			requested: "light",
			shell:     true,
			want:      `<!doctype html><html lang="en" data-theme="light" data-theme-preference="light">`,
		},
		{
			name:      "dark shell",
			requested: "dark",
			shell:     true,
			want:      `<!doctype html><html lang="en" data-theme="dark" data-theme-preference="dark">`,
		},
		{
			name:      "light standalone",
			requested: "light",
			want:      `<!doctype html><html lang="en" data-theme="light">`,
		},
		{
			name:      "dark standalone",
			requested: "dark",
			want:      `<!doctype html><html lang="en" data-theme="dark">`,
		},
		{
			name:      "malicious shell query",
			requested: `"><script>alert("theme")</script>`,
			shell:     true,
			want:      `<!doctype html><html lang="en" data-theme="light" data-theme-preference="light">`,
		},
		{
			name:      "malicious standalone query",
			requested: `"><img src=x onerror=alert("theme")>`,
			want:      `<!doctype html><html lang="en" data-theme="light">`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := emptyStateThemeDocumentStart(test.requested, test.shell); got != test.want {
				t.Fatalf("emptyStateThemeDocumentStart(%q, %t) = %q, want approved literal %q", test.requested, test.shell, got, test.want)
			}
		})
	}
}
