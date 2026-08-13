package plugin

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveWithin(t *testing.T) {
	root := t.TempDir()
	resolvedRoot := resolved(t, root)

	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "server")
	if err := os.WriteFile(outsideFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustWrite := func(rel string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, rel), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mustMkdir := func(rel string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(root, rel), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustSymlink := func(target, link string) {
		t.Helper()
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	mustMkdir("bin")
	mustWrite("bin/server")
	mustMkdir("data")
	mustSymlink(filepath.Join(root, "data"), "link-absolute-inside")
	mustSymlink("data", "link-relative-inside")
	mustSymlink(outsideFile, "link-outside")
	mustSymlink("link-outside", "link-chain-outside")

	cases := []struct {
		name    string
		rel     string
		want    string
		wantErr error
	}{
		{name: "dot-relative file stays inside", rel: "./bin/server", want: filepath.Join(resolvedRoot, "bin", "server")},
		{name: "dot-relative directory stays inside", rel: "./data", want: filepath.Join(resolvedRoot, "data")},
		{name: "absolute symlink inside root allowed", rel: "./link-absolute-inside", want: filepath.Join(resolvedRoot, "data")},
		{name: "relative symlink inside root allowed", rel: "./link-relative-inside", want: filepath.Join(resolvedRoot, "data")},
		{name: "parent escape rejected", rel: "../bin/server", wantErr: ErrPathOutsideRoot},
		{name: "hidden parent escape rejected", rel: "./../bin/server", wantErr: ErrPathOutsideRoot},
		{name: "deep escape rejected", rel: "./data/../../x", wantErr: ErrPathOutsideRoot},
		{name: "bare name without dot prefix rejected", rel: "data", wantErr: ErrPathOutsideRoot},
		{name: "absolute path rejected", rel: "/etc/passwd", wantErr: ErrPathOutsideRoot},
		{name: "dot-dot only rejected", rel: "./..", wantErr: ErrPathOutsideRoot},
		{name: "symlink resolving outside rejected", rel: "./link-outside", wantErr: ErrPathOutsideRoot},
		{name: "symlink chain resolving outside rejected", rel: "./link-chain-outside", wantErr: ErrPathOutsideRoot},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveWithin(root, tc.rel)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("ResolveWithin(%q) error = %v, want %v", tc.rel, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveWithin(%q): %v", tc.rel, err)
			}
			if got != tc.want {
				t.Errorf("ResolveWithin(%q) = %q, want %q", tc.rel, got, tc.want)
			}
		})
	}

	// A missing target cannot be proven contained and is rejected.
	if _, err := ResolveWithin(root, "./nope"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing target error = %v, want fs.ErrNotExist", err)
	}
	// NUL bytes never reach the filesystem.
	if _, err := ResolveWithin(root, "./a\x00b"); !errors.Is(err, ErrPathOutsideRoot) {
		t.Errorf("NUL path error = %v, want ErrPathOutsideRoot", err)
	}
}

func TestResolveWithinSymlinkedRoot(t *testing.T) {
	real := t.TempDir()
	if err := os.WriteFile(filepath.Join(real, "file"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "root-link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveWithin(link, "./file")
	if err != nil {
		t.Fatalf("ResolveWithin through symlinked root: %v", err)
	}
	if want := filepath.Join(resolved(t, real), "file"); got != want {
		t.Errorf("ResolveWithin = %q, want %q", got, want)
	}
}

func TestInstallRoot(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".waffle")

	got, err := InstallRoot(home, "my-plugin")
	if err != nil {
		t.Fatalf("InstallRoot: %v", err)
	}
	if want := filepath.Join(home, "plugins", "my-plugin"); got != want {
		t.Errorf("InstallRoot = %q, want %q", got, want)
	}
	if PluginsDir(home) != filepath.Join(home, "plugins") {
		t.Errorf("PluginsDir = %q", PluginsDir(home))
	}

	if _, err := InstallRoot(home, "My-Plugin"); !errors.Is(err, ErrInvalidName) {
		t.Errorf("invalid name error = %v, want ErrInvalidName", err)
	}
	if _, err := InstallRoot("", "my-plugin"); err == nil {
		t.Error("empty home accepted")
	}
}
