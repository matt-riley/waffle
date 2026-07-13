package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matt-riley/waffle/internal/entity"
	"github.com/matt-riley/waffle/internal/session"
)

func TestSessionProfileCommandBindsAndRejectsInvalidProfiles(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("WAFFLE_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(`
[agent.profile.reviewer]
system = "review"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, st, err := openConfigAndStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	entities := entity.New(st, session.New(st))
	if _, err := entities.GroupFor(ctx, "telegram", "42", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if err := sessionCmd(ctx, []string{"profile", "telegram:42", "reviewer"}, strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), `bound profile "reviewer"`) {
		t.Fatalf("stdout = %q", stdout.String())
	}
	_, reopened, err := openConfigAndStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	group, err := entity.New(reopened, session.New(reopened)).GroupFor(ctx, "telegram", "42", "")
	if err != nil || group.Profile != "reviewer" {
		t.Fatalf("bound group = %+v, %v", group, err)
	}

	for _, name := range []string{"Bad/Profile", "missing"} {
		err := sessionCmd(ctx, []string{"profile", "telegram:42", name}, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "profile") || !strings.Contains(err.Error(), name) {
			t.Fatalf("profile %q error = %v", name, err)
		}
	}
}
