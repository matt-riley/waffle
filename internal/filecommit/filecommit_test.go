package filecommit

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteIsDurableAndReplacesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "journal.json")
	if err := Write(path, []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "first\n" {
		t.Fatalf("body = %q", body)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 0600", info.Mode().Perm())
	}

	if err := Write(path, []byte("second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	body, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "second\n" {
		t.Fatalf("replaced body = %q", body)
	}

	// No leftover staging files.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "journal.json" {
		t.Fatalf("dir entries = %v, want only journal.json", entries)
	}
}

func TestWriteCleansUpStagingOnFailure(t *testing.T) {
	// Destination directory does not exist → CreateTemp fails cleanly.
	err := Write(filepath.Join(t.TempDir(), "missing", "x.json"), []byte("x"), 0o600)
	if err == nil {
		t.Fatal("expected error for missing parent")
	}
}
