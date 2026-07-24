package filecommit

import (
	"os"
	"path/filepath"
	"strings"
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
	// CreateTemp fails when the parent is missing (no staging file created).
	err := Write(filepath.Join(t.TempDir(), "missing", "x.json"), []byte("x"), 0o600)
	if err == nil {
		t.Fatal("expected error for missing parent")
	}

	// CreateTemp succeeds but rename fails when the destination is a directory;
	// the !committed cleanup branch must remove the staging temp (#156 review).
	dir := t.TempDir()
	dest := filepath.Join(dir, "journal.json")
	if err := os.Mkdir(dest, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Write(dest, []byte("payload\n"), 0o600); err == nil {
		t.Fatal("expected rename failure when destination is a directory")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".filecommit-") {
			t.Fatalf("staging file left behind after failure: %s", entry.Name())
		}
	}
}
