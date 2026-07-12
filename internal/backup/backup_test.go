package backup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRestoreValidationFailureLeavesLiveStateUntouched(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WAFFLE_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("# live\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "backup")
	if err := os.MkdirAll(src, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "manifest.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "waffle.db"), []byte("not sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Restore(context.Background(), src); err == nil {
		t.Fatal("Restore succeeded for invalid database")
	}
	got, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "# live") {
		t.Fatalf("live config was changed: %q", got)
	}
}
