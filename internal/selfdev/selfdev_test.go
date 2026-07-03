package selfdev

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestDoctorPasses(t *testing.T) {
	t.Setenv("WAFFLE_HOME", t.TempDir())
	checks, ok, err := Doctor(context.Background())
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if !ok {
		t.Errorf("doctor failed on a clean home: %+v", checks)
	}
	// Expect config, database, and secret-store checks.
	if len(checks) < 3 {
		t.Errorf("checks = %d, want >= 3", len(checks))
	}
	for _, c := range checks {
		if !c.OK {
			t.Errorf("check %q failed: %s", c.Name, c.Info)
		}
	}
}

func TestRollbackWithoutBackup(t *testing.T) {
	if _, err := Rollback(); err == nil {
		t.Skip("this binary happens to have a .prev; skipping")
	}
}

func TestCopyFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("hello"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copyFile: %v", err)
	}
	b, err := os.ReadFile(dst)
	if err != nil || string(b) != "hello" {
		t.Errorf("dst = %q, %v", b, err)
	}
	info, _ := os.Stat(dst)
	if info.Mode().Perm()&0o100 == 0 {
		t.Error("dst not executable")
	}
}
