package tool

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewFileRootsRejectsBadRoots(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		paths []string
	}{
		{name: "empty entry", paths: []string{""}},
		{name: "missing directory", paths: []string{filepath.Join(dir, "nope")}},
		{name: "not a directory", paths: []string{file}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewFileRoots(tc.paths); err == nil {
				t.Fatalf("NewFileRoots(%q) accepted", tc.paths)
			}
		})
	}
}

func TestFileRootsResolve(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("s3cret"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A symlink planted inside the root must not become a way out.
	escape := filepath.Join(root, "escape")
	if err := os.Symlink(outside, escape); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	roots, err := NewFileRoots([]string{root})
	if err != nil {
		t.Fatalf("NewFileRoots: %v", err)
	}

	allowed := []string{
		filepath.Join(root, "a.txt"),
		filepath.Join(root, "sub", "b.txt"),
		filepath.Join(root, "sub", "new", "deep", "c.txt"), // not created yet
		root,
	}
	for _, p := range allowed {
		if _, err := roots.Resolve(p); err != nil {
			t.Errorf("Resolve(%q) = %v, want allowed", p, err)
		}
	}

	refused := []string{
		secret,
		filepath.Join(root, "..", "elsewhere.txt"),
		filepath.Join(root, "sub", "..", "..", "elsewhere.txt"),
		filepath.Join(escape, "secret.txt"), // symlink escape
		root + "-sibling/f.txt",             // prefix collision, not a child
	}
	for _, p := range refused {
		if _, err := roots.Resolve(p); !errors.Is(err, ErrOutsideRoots) {
			t.Errorf("Resolve(%q) = %v, want ErrOutsideRoots", p, err)
		}
	}
}

func TestFileRootsUnrestrictedAllowsAnything(t *testing.T) {
	var roots FileRoots
	if !roots.Unrestricted() {
		t.Fatal("zero FileRoots must be unrestricted")
	}
	got, err := roots.Resolve("/etc/hosts")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "/etc/hosts" {
		t.Errorf("Resolve = %q", got)
	}
	if _, err := roots.Resolve("  "); err == nil {
		t.Error("blank path accepted")
	}
}

func TestFileRootsConfines(t *testing.T) {
	base := t.TempDir()
	inner := filepath.Join(base, "inner")
	other := t.TempDir()
	if err := os.MkdirAll(inner, 0o700); err != nil {
		t.Fatal(err)
	}
	wide, err := NewFileRoots([]string{base})
	if err != nil {
		t.Fatal(err)
	}
	narrow, err := NewFileRoots([]string{inner})
	if err != nil {
		t.Fatal(err)
	}
	elsewhere, err := NewFileRoots([]string{other})
	if err != nil {
		t.Fatal(err)
	}
	var none FileRoots

	if !wide.Confines(narrow) {
		t.Error("a subdirectory must be within the wider root")
	}
	if wide.Confines(elsewhere) {
		t.Error("an unrelated root must not be within")
	}
	if wide.Confines(none) {
		t.Error("dropping the boundary must not count as within")
	}
	if !none.Confines(elsewhere) {
		t.Error("an unrestricted base cannot be widened past")
	}
}

// TestFileToolsRefuseOutsideRoots is the #269 regression: with roots
// configured, the builtins must not touch anything else — this is the
// /etc/shadow and ~/.ssh case from the issue.
func TestFileToolsRefuseOutsideRoots(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("s3cret"), 0o600); err != nil {
		t.Fatal(err)
	}
	roots, err := NewFileRoots([]string{root})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		tool  Tool
		input func(path string) string
	}{
		{
			name:  "read_file",
			tool:  ReadFile{Roots: roots},
			input: func(p string) string { return fmt.Sprintf(`{"path":%q}`, p) },
		},
		{
			name:  "write_file",
			tool:  WriteFile{Roots: roots},
			input: func(p string) string { return fmt.Sprintf(`{"path":%q,"content":"pwned"}`, p) },
		},
		{
			name:  "edit_file",
			tool:  EditFile{Roots: roots},
			input: func(p string) string { return fmt.Sprintf(`{"path":%q,"old_string":"s3cret","new_string":"pwned"}`, p) },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := run(t, tc.tool, tc.input(secret))
			if !errors.Is(err, ErrOutsideRoots) {
				t.Fatalf("err = %v, want ErrOutsideRoots", err)
			}
			if got, _ := os.ReadFile(secret); string(got) != "s3cret" {
				t.Fatalf("file outside roots was modified: %q", got)
			}
			// The same call inside the roots must still work.
			inside := filepath.Join(root, tc.name+".txt")
			if err := os.WriteFile(inside, []byte("s3cret"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := run(t, tc.tool, tc.input(inside)); err != nil {
				t.Fatalf("inside roots: %v", err)
			}
		})
	}

	// search returns file contents, so it sits behind the same boundary.
	if _, err := run(t, Search{Roots: roots}, fmt.Sprintf(`{"pattern":"s3cret","path":%q}`, outside)); !errors.Is(err, ErrOutsideRoots) {
		t.Errorf("search outside roots: %v, want ErrOutsideRoots", err)
	}
	out, err := run(t, Search{Roots: roots}, fmt.Sprintf(`{"pattern":"s3cret","path":%q}`, root))
	if err != nil {
		t.Fatalf("search inside roots: %v", err)
	}
	if !strings.Contains(out, "s3cret") {
		t.Errorf("search output = %q", out)
	}
}

// TestFileToolsUnboundedByDefault keeps the owner's default deployment working:
// no configured roots means no boundary (docker mode is where isolation comes
// from), so this must not silently start refusing paths.
func TestFileToolsUnboundedByDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if _, err := run(t, WriteFile{}, fmt.Sprintf(`{"path":%q,"content":"x"}`, path)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := run(t, ReadFile{}, fmt.Sprintf(`{"path":%q}`, path)); err != nil {
		t.Fatalf("read: %v", err)
	}
}

// TestFileToolsReportResolvedPath: Resolve may make the path absolute or walk
// it through a symlink, so the success message must name where the bytes
// actually landed, not the string the model happened to pass.
func TestFileToolsReportResolvedPath(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	roots, err := NewFileRoots([]string{dir})
	if err != nil {
		t.Fatal(err)
	}

	via := filepath.Join(link, "f.txt")
	landed := filepath.Join(target, "f.txt")
	out, err := run(t, WriteFile{Roots: roots}, fmt.Sprintf(`{"path":%q,"content":"old"}`, via))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if !strings.Contains(out, landed) {
		t.Errorf("write_file reported %q, want the resolved path %q", out, landed)
	}
	out, err = run(t, EditFile{Roots: roots}, fmt.Sprintf(`{"path":%q,"old_string":"old","new_string":"new"}`, via))
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if !strings.Contains(out, landed) {
		t.Errorf("edit_file reported %q, want the resolved path %q", out, landed)
	}
}
