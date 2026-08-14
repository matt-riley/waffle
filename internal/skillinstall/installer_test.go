package skillinstall

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/policy"
	"github.com/matt-riley/waffle/internal/skill"
	"github.com/matt-riley/waffle/internal/store"
)

const pinnedCommit = "0123456789abcdef0123456789abcdef01234567"

type fixture struct {
	installer *Installer
	imports   string
	skills    string
	stages    string
	source    string
	now       time.Time
}

func newInstallerFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	f := &fixture{
		imports: filepath.Join(root, "imports"),
		skills:  filepath.Join(root, "skills"),
		stages:  filepath.Join(root, "stages"),
		now:     time.Date(2026, time.July, 24, 9, 30, 0, 0, time.UTC),
	}
	if err := os.Mkdir(f.imports, 0o700); err != nil {
		t.Fatal(err)
	}
	f.source = filepath.Join(f.imports, "reviewed-skill")
	if err := os.Mkdir(f.source, 0o700); err != nil {
		t.Fatal(err)
	}
	copyFile(t, filepath.Join("testdata", "valid", "SKILL.md"), filepath.Join(f.source, "SKILL.md"))
	writeFile(t, filepath.Join(f.source, "guide.txt"), "Review the full tree: café 🧇\n", 0o600)

	f.installer = New(f.skills, f.stages, []string{f.imports}, []string{"github.com"})
	f.installer.Now = func() time.Time { return f.now }
	f.installer.Random = bytes.NewReader(bytes.Repeat([]byte{0x11}, 256))
	return f
}

func copyFile(t *testing.T, source, destination string) {
	t.Helper()
	body, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, destination, string(body), 0o600)
}

func writeFile(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func stageLocal(t *testing.T, f *fixture) Manifest {
	t.Helper()
	manifest, err := f.installer.Stage(context.Background(), StageRequest{LocalPath: f.source})
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func assertNoStages(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("stage root contains %v, want empty", entryNames(entries))
	}
}

func entryNames(entries []os.DirEntry) []string {
	out := make([]string, len(entries))
	for index := range entries {
		out[index] = entries[index].Name()
	}
	return out
}

func TestStageBuildsBoundedPrivateReview(t *testing.T) {
	f := newInstallerFixture(t)

	manifest := stageLocal(t, f)

	if manifest.Name != "reviewed-skill" || manifest.Description != "Reviews changes carefully." {
		t.Fatalf("identity = (%q, %q)", manifest.Name, manifest.Description)
	}
	if manifest.SourceRef != "local:reviewed-skill" {
		t.Fatalf("source_ref = %q", manifest.SourceRef)
	}
	if manifest.StageID != strings.Repeat("11", 16) {
		t.Fatalf("stage_id = %q", manifest.StageID)
	}
	if manifest.ContentDigest == "" || !strings.HasPrefix(manifest.ContentDigest, "sha256:") {
		t.Fatalf("content_digest = %q", manifest.ContentDigest)
	}
	if want := f.now.Add(10 * time.Minute); !manifest.ExpiresAt.Equal(want) {
		t.Fatalf("expires_at = %v, want %v", manifest.ExpiresAt, want)
	}
	if !manifest.Audit.Passed || len(manifest.Audit.Flags) != 0 {
		t.Fatalf("audit = %+v", manifest.Audit)
	}
	gotPaths := make([]string, len(manifest.Files))
	for index := range manifest.Files {
		gotPaths[index] = manifest.Files[index].Path
		if manifest.Files[index].Preview == "" {
			t.Fatalf("%s preview is empty", manifest.Files[index].Path)
		}
	}
	if want := []string{"SKILL.md", "guide.txt"}; !slices.Equal(gotPaths, want) {
		t.Fatalf("paths = %v, want %v", gotPaths, want)
	}
	if got := manifest.Files[1].Preview; got != "Review the full tree: café 🧇\n" {
		t.Fatalf("full UTF-8 preview = %q", got)
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(f.imports)) || bytes.Contains(encoded, []byte(f.source)) {
		t.Fatalf("manifest leaked local path: %s", encoded)
	}

	for _, path := range []string{
		f.stages,
		filepath.Join(f.stages, manifest.StageID),
		filepath.Join(f.stages, manifest.StageID, "content"),
	} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode = %v, want private directory", path, info.Mode())
		}
	}
	for _, entry := range manifest.Files {
		info, err := os.Lstat(filepath.Join(f.stages, manifest.StageID, "content", filepath.FromSlash(entry.Path)))
		if err != nil {
			t.Fatal(err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %v, want 0600 regular", entry.Path, info.Mode())
		}
	}
}

func TestStageRejectsInvalidRequestAndLocalEscape(t *testing.T) {
	tests := []struct {
		name    string
		request func(*fixture) StageRequest
	}{
		{name: "no source", request: func(*fixture) StageRequest { return StageRequest{} }},
		{name: "both sources", request: func(f *fixture) StageRequest {
			return StageRequest{LocalPath: f.source, GitURL: "https://github.com/acme/skill.git", Commit: pinnedCommit}
		}},
		{name: "relative path", request: func(*fixture) StageRequest {
			return StageRequest{LocalPath: filepath.Join("..", "reviewed-skill")}
		}},
		{name: "unclean absolute path", request: func(f *fixture) StageRequest {
			return StageRequest{LocalPath: f.imports + string(filepath.Separator) + "child" +
				string(filepath.Separator) + ".." + string(filepath.Separator) + "reviewed-skill"}
		}},
		{name: "missing path", request: func(f *fixture) StageRequest {
			return StageRequest{LocalPath: filepath.Join(f.imports, "missing")}
		}},
		{name: "outside allowlist", request: func(f *fixture) StageRequest {
			outside := filepath.Join(filepath.Dir(f.imports), "outside")
			if err := os.Mkdir(outside, 0o700); err != nil {
				panic(err)
			}
			return StageRequest{LocalPath: outside}
		}},
		{name: "sibling prefix escape", request: func(f *fixture) StageRequest {
			sibling := f.imports + "-escape"
			if err := os.Mkdir(sibling, 0o700); err != nil {
				panic(err)
			}
			return StageRequest{LocalPath: sibling}
		}},
		{name: "unsafe source label", request: func(f *fixture) StageRequest {
			unsafe := filepath.Join(f.imports, "unsafe:label")
			if err := os.Rename(f.source, unsafe); err != nil {
				panic(err)
			}
			return StageRequest{LocalPath: unsafe}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newInstallerFixture(t)
			_, err := f.installer.Stage(context.Background(), test.request(f))
			if err == nil {
				t.Fatal("Stage succeeded, want error")
			}
			assertNoStages(t, f.stages)
		})
	}
}

func TestStageRejectsLocalRootSwapAfterOpening(t *testing.T) {
	f := newInstallerFixture(t)
	original := f.source + "-original"
	outside := filepath.Join(filepath.Dir(f.imports), "outside-swap")
	writeFile(t, filepath.Join(outside, "SKILL.md"), "---\nname: escaped\ndescription: Must not be imported.\n---\n", 0o600)
	f.installer.afterLocalSourceOpen = func() {
		if err := os.Rename(f.source, original); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, f.source); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := f.installer.Stage(context.Background(), StageRequest{LocalPath: f.source}); !errors.Is(err, ErrUnsafeTree) {
		t.Fatalf("error = %v, want ErrUnsafeTree", err)
	}
	assertNoStages(t, f.stages)
}

func TestStageBoundsFileThatGrowsBeforeOpen(t *testing.T) {
	f := newInstallerFixture(t)
	f.installer.beforeLocalEntry = func(relative string) {
		if relative != "guide.txt" {
			return
		}
		if err := os.WriteFile(filepath.Join(f.source, relative), bytes.Repeat([]byte{'x'}, maxReviewBytes), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := f.installer.Stage(context.Background(), StageRequest{LocalPath: f.source}); !errors.Is(err, ErrTreeTooLarge) {
		t.Fatalf("error = %v, want ErrTreeTooLarge", err)
	}
	assertNoStages(t, f.stages)
}

func TestStageRejectsDirectoryComponentSwap(t *testing.T) {
	f := newInstallerFixture(t)
	writeFile(t, filepath.Join(f.source, "docs", "inside.txt"), "inside\n", 0o600)
	outside := filepath.Join(filepath.Dir(f.imports), "outside-component")
	writeFile(t, filepath.Join(outside, "outside.txt"), "outside\n", 0o600)
	original := filepath.Join(f.source, "docs-original")
	f.installer.beforeLocalEntry = func(relative string) {
		if relative != "docs" {
			return
		}
		if err := os.Rename(filepath.Join(f.source, "docs"), original); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(f.source, "docs")); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := f.installer.Stage(context.Background(), StageRequest{LocalPath: f.source}); !errors.Is(err, ErrUnsafeTree) {
		t.Fatalf("error = %v, want ErrUnsafeTree", err)
	}
	assertNoStages(t, f.stages)
}

func TestStageRejectsSymlinksSpecialFilesAndUnsafeContent(t *testing.T) {
	tests := []struct {
		name  string
		build func(*testing.T, *fixture)
	}{
		{name: "symlink file", build: func(t *testing.T, f *fixture) {
			if err := os.Symlink(filepath.Join(f.source, "guide.txt"), filepath.Join(f.source, "link.txt")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "symlink directory", build: func(t *testing.T, f *fixture) {
			target := filepath.Join(filepath.Dir(f.source), "target")
			if err := os.Mkdir(target, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(f.source, "linked-dir")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "fifo", build: func(t *testing.T, f *fixture) {
			if err := syscall.Mkfifo(filepath.Join(f.source, "pipe"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "executable", build: func(t *testing.T, f *fixture) {
			writeFile(t, filepath.Join(f.source, "run.sh"), "#!/bin/sh\nexit 0\n", 0o700)
		}},
		{name: "hidden git state", build: func(t *testing.T, f *fixture) {
			writeFile(t, filepath.Join(f.source, ".git", "config"), "[core]\n", 0o600)
		}},
		{name: "case folded hidden git state", build: func(t *testing.T, f *fixture) {
			writeFile(t, filepath.Join(f.source, ".GIT", "config"), "[core]\n", 0o600)
		}},
		{name: "NUL byte", build: func(t *testing.T, f *fixture) {
			if err := os.WriteFile(filepath.Join(f.source, "nul.txt"), []byte{'a', 0, 'b'}, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "invalid UTF-8", build: func(t *testing.T, f *fixture) {
			if err := os.WriteFile(filepath.Join(f.source, "binary.dat"), []byte{0xff, 0xfe}, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newInstallerFixture(t)
			test.build(t, f)
			if _, err := f.installer.Stage(context.Background(), StageRequest{LocalPath: f.source}); !errors.Is(err, ErrUnsafeTree) {
				t.Fatalf("error = %v, want ErrUnsafeTree", err)
			}
			assertNoStages(t, f.stages)
		})
	}
}

func TestStageEnforcesFileAndByteBounds(t *testing.T) {
	t.Run("file count", func(t *testing.T) {
		f := newInstallerFixture(t)
		for index := 0; index < 63; index++ {
			writeFile(t, filepath.Join(f.source, fmt.Sprintf("file-%02d.txt", index)), "x", 0o600)
		}
		if _, err := f.installer.Stage(context.Background(), StageRequest{LocalPath: f.source}); !errors.Is(err, ErrTreeTooLarge) {
			t.Fatalf("error = %v, want ErrTreeTooLarge", err)
		}
		assertNoStages(t, f.stages)
	})

	t.Run("total bytes", func(t *testing.T) {
		f := newInstallerFixture(t)
		if err := os.WriteFile(filepath.Join(f.source, "large.txt"), bytes.Repeat([]byte{'x'}, 1<<20), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := f.installer.Stage(context.Background(), StageRequest{LocalPath: f.source}); !errors.Is(err, ErrTreeTooLarge) {
			t.Fatalf("error = %v, want ErrTreeTooLarge", err)
		}
		assertNoStages(t, f.stages)
	})
}

func TestStageRequiresStrictRootSkillFrontmatter(t *testing.T) {
	tests := []struct {
		name  string
		build func(*testing.T, *fixture)
	}{
		{name: "missing SKILL", build: func(t *testing.T, f *fixture) {
			if err := os.Remove(filepath.Join(f.source, "SKILL.md")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "nested duplicate SKILL", build: func(t *testing.T, f *fixture) {
			writeFile(t, filepath.Join(f.source, "nested", "SKILL.md"), "---\nname: nested\ndescription: Nested.\n---\n", 0o600)
		}},
		{name: "missing frontmatter", build: func(t *testing.T, f *fixture) {
			writeFile(t, filepath.Join(f.source, "SKILL.md"), "# no metadata\n", 0o600)
		}},
		{name: "invalid slug", build: func(t *testing.T, f *fixture) {
			writeFile(t, filepath.Join(f.source, "SKILL.md"), "---\nname: Reviewed Skill\ndescription: Invalid slug.\n---\n", 0o600)
		}},
		{name: "missing description", build: func(t *testing.T, f *fixture) {
			writeFile(t, filepath.Join(f.source, "SKILL.md"), "---\nname: reviewed-skill\n---\n", 0o600)
		}},
		{name: "multiline description", build: func(t *testing.T, f *fixture) {
			writeFile(t, filepath.Join(f.source, "SKILL.md"), "---\nname: reviewed-skill\ndescription: |\n  two lines\n---\n", 0o600)
		}},
		{name: "duplicate status", build: func(t *testing.T, f *fixture) {
			writeFile(t, filepath.Join(f.source, "SKILL.md"), "---\nname: reviewed-skill\ndescription: Duplicate status.\nstatus: active\nstatus: inactive\n---\n", 0o600)
		}},
		{name: "collection description", build: func(t *testing.T, f *fixture) {
			writeFile(t, filepath.Join(f.source, "SKILL.md"), "---\nname: reviewed-skill\ndescription: [not, a, scalar]\n---\n", 0o600)
		}},
		{name: "malformed quoted description", build: func(t *testing.T, f *fixture) {
			writeFile(t, filepath.Join(f.source, "SKILL.md"), "---\nname: reviewed-skill\ndescription: \"closed\" trailing\"\n---\n", 0o600)
		}},
		{name: "mapping-like plain description", build: func(t *testing.T, f *fixture) {
			writeFile(t, filepath.Join(f.source, "SKILL.md"), "---\nname: reviewed-skill\ndescription: invalid: mapping\n---\n", 0o600)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newInstallerFixture(t)
			test.build(t, f)
			if _, err := f.installer.Stage(context.Background(), StageRequest{LocalPath: f.source}); !errors.Is(err, ErrAuditFailed) {
				t.Fatalf("error = %v, want ErrAuditFailed", err)
			}
			assertNoStages(t, f.stages)
		})
	}
}

func TestStageReportsStableAuditFlags(t *testing.T) {
	f := newInstallerFixture(t)
	writeFile(t, filepath.Join(f.source, "script.sh"), "printf ok\n", 0o600)
	writeFile(t, filepath.Join(f.source, "helper.py"), "print('ok')\n", 0o600)
	writeFile(t, filepath.Join(f.source, "links.txt"), "See https://example.invalid/reference\n", 0o600)

	manifest := stageLocal(t, f)
	want := []string{"code:helper.py", "network-reference:links.txt", "shell:script.sh"}
	if !slices.Equal(manifest.Audit.Flags, want) {
		t.Fatalf("flags = %v, want %v", manifest.Audit.Flags, want)
	}
}

func TestStageRejectsDuplicateInstalledName(t *testing.T) {
	f := newInstallerFixture(t)
	writeFile(t, filepath.Join(f.skills, "reviewed-skill", "SKILL.md"), "existing\n", 0o600)

	if _, err := f.installer.Stage(context.Background(), StageRequest{LocalPath: f.source}); !errors.Is(err, ErrSkillExists) {
		t.Fatalf("error = %v, want ErrSkillExists", err)
	}
	assertNoStages(t, f.stages)
}

func TestStageRejectsSymlinkOwnedRoots(t *testing.T) {
	t.Run("stage root", func(t *testing.T) {
		f := newInstallerFixture(t)
		actual := filepath.Join(filepath.Dir(f.stages), "actual-stages")
		if err := os.Mkdir(actual, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(actual, f.stages); err != nil {
			t.Fatal(err)
		}
		if _, err := f.installer.Stage(context.Background(), StageRequest{LocalPath: f.source}); err == nil {
			t.Fatal("Stage accepted symlink stage root")
		}
		assertNoStages(t, actual)
	})

	t.Run("skills root", func(t *testing.T) {
		f := newInstallerFixture(t)
		actual := filepath.Join(filepath.Dir(f.skills), "actual-skills")
		if err := os.Mkdir(actual, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(actual, f.skills); err != nil {
			t.Fatal(err)
		}
		if _, err := f.installer.Stage(context.Background(), StageRequest{LocalPath: f.source}); err == nil {
			t.Fatal("Stage accepted symlink skills root")
		}
		assertNoStages(t, f.stages)
	})
}

func TestStageRejectsUnsafeGitRequestsBeforeFetch(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		commit  string
		wantErr error
	}{
		{name: "HTTP", url: "http://github.com/acme/skill.git", commit: pinnedCommit, wantErr: ErrSourceNotAllowed},
		{name: "userinfo", url: "https://token@github.com/acme/skill.git", commit: pinnedCommit, wantErr: ErrSourceNotAllowed},
		{name: "query", url: "https://github.com/acme/skill.git?ref=main", commit: pinnedCommit, wantErr: ErrSourceNotAllowed},
		{name: "fragment", url: "https://github.com/acme/skill.git#main", commit: pinnedCommit, wantErr: ErrSourceNotAllowed},
		{name: "GitHub extra path", url: "https://github.com/acme/skill/extra", commit: pinnedCommit, wantErr: ErrSourceNotAllowed},
		{name: "GitHub invalid owner", url: "https://github.com/-acme/skill.git", commit: pinnedCommit, wantErr: ErrSourceNotAllowed},
		{name: "GitHub empty repository", url: "https://github.com/acme/.git", commit: pinnedCommit, wantErr: ErrSourceNotAllowed},
		{name: "disallowed host", url: "https://example.com/acme/skill.git", commit: pinnedCommit, wantErr: ErrGitHostNotAllowed},
		{name: "missing commit", url: "https://github.com/acme/skill.git", wantErr: ErrCommitRequired},
		{name: "short commit", url: "https://github.com/acme/skill.git", commit: "0123456", wantErr: ErrCommitRequired},
		{name: "uppercase commit", url: "https://github.com/acme/skill.git", commit: strings.ToUpper(pinnedCommit), wantErr: ErrCommitRequired},
		{name: "non hex commit", url: "https://github.com/acme/skill.git", commit: strings.Repeat("z", 40), wantErr: ErrCommitRequired},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newInstallerFixture(t)
			fetcher := &fakeFetcher{}
			f.installer.Fetcher = fetcher
			_, err := f.installer.Stage(context.Background(), StageRequest{GitURL: test.url, Commit: test.commit})
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
			if len(fetcher.calls) != 0 {
				t.Fatalf("fetch called for invalid request: %+v", fetcher.calls)
			}
			assertNoStages(t, f.stages)
		})
	}
}

func TestGitEnvironmentDisablesCredentialAndRewriteConfiguration(t *testing.T) {
	t.Setenv("HOME", "/tmp/untrusted-home")
	t.Setenv("XDG_CONFIG_HOME", "/tmp/untrusted-xdg")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "url.ssh://example.invalid/.insteadOf")
	t.Setenv("GIT_CONFIG_VALUE_0", "https://github.com/")
	t.Setenv("GIT_CONFIG_GLOBAL", "/tmp/untrusted-gitconfig")
	t.Setenv("GIT_ASKPASS", "/tmp/untrusted-askpass")

	privateHome := filepath.Join(t.TempDir(), "private-git-home")
	environment := gitEnvironment(privateHome)
	values := make(map[string]string)
	for _, item := range environment {
		key, value, found := strings.Cut(item, "=")
		if found {
			values[key] = value
		}
	}
	for _, forbidden := range []string{"GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0"} {
		if _, present := values[forbidden]; present {
			t.Fatalf("%s survived in fixed Git environment", forbidden)
		}
	}
	if values["GIT_CONFIG_GLOBAL"] != os.DevNull || values["GIT_CONFIG_SYSTEM"] != os.DevNull {
		t.Fatalf("Git config paths = (%q, %q), want disabled", values["GIT_CONFIG_GLOBAL"], values["GIT_CONFIG_SYSTEM"])
	}
	if values["GIT_TERMINAL_PROMPT"] != "0" || values["GIT_ASKPASS"] != os.DevNull ||
		values["SSH_ASKPASS"] != os.DevNull {
		t.Fatalf("Git prompt controls = (%q, %q, %q)",
			values["GIT_TERMINAL_PROMPT"], values["GIT_ASKPASS"], values["SSH_ASKPASS"])
	}
	if values["HOME"] != privateHome || values["XDG_CONFIG_HOME"] != privateHome {
		t.Fatalf("Git homes = (%q, %q), want private %q", values["HOME"], values["XDG_CONFIG_HOME"], privateHome)
	}
}

func TestGitCommandIgnoresEnclosingRepositoryConfiguration(t *testing.T) {
	outer := filepath.Join(t.TempDir(), "outer")
	if err := os.Mkdir(outer, 0o700); err != nil {
		t.Fatal(err)
	}
	run := func(arguments ...string) {
		t.Helper()
		command := exec.Command("git", arguments...)
		command.Dir = outer
		command.Env = gitEnvironment(t.TempDir())
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", arguments, err, output)
		}
	}
	run("init", "--quiet")
	run("config", "url.file:///private/tmp/hostile.git.insteadOf", "https://github.com/acme/reviewed-skill.git")
	t.Chdir(outer)

	privateHome := filepath.Join(outer, "private-git-home")
	if err := os.Mkdir(privateHome, 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("git", "ls-remote", "--get-url", "https://github.com/acme/reviewed-skill.git")
	isolateGitCommand(command, privateHome)
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(output)); got != "https://github.com/acme/reviewed-skill.git" {
		t.Fatalf("Git URL = %q, want approved URL unchanged", got)
	}
}

type fetchCall struct {
	url         string
	commit      string
	destination string
}

type fakeFetcher struct {
	mu    sync.Mutex
	calls []fetchCall
	err   error
	build func(string) error
}

func (f *fakeFetcher) Fetch(_ context.Context, gitURL, commit, destination string) error {
	f.mu.Lock()
	f.calls = append(f.calls, fetchCall{url: gitURL, commit: commit, destination: destination})
	f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	if f.build != nil {
		return f.build(destination)
	}
	if err := os.Mkdir(destination, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(destination, "SKILL.md"), []byte(
		"---\nname: reviewed-skill\ndescription: Fetched safely.\n---\n\nReview fetched changes.\n",
	), 0o600)
}

func TestStageGitUsesPinnedCredentialFreeSource(t *testing.T) {
	f := newInstallerFixture(t)
	fetcher := &fakeFetcher{}
	f.installer.Fetcher = fetcher
	gitURL := "https://github.com/acme/reviewed-skill.git"

	manifest, err := f.installer.Stage(context.Background(), StageRequest{
		GitURL: gitURL,
		Commit: pinnedCommit,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fetcher.calls) != 1 {
		t.Fatalf("fetch calls = %d, want 1", len(fetcher.calls))
	}
	if fetcher.calls[0].url != gitURL || fetcher.calls[0].commit != pinnedCommit {
		t.Fatalf("fetch input = %+v", fetcher.calls[0])
	}
	if manifest.SourceRef != "git:"+gitURL+"@"+pinnedCommit {
		t.Fatalf("source_ref = %q", manifest.SourceRef)
	}
	if strings.Contains(manifest.SourceRef, fetcher.calls[0].destination) {
		t.Fatal("source_ref leaked private stage path")
	}
}

func TestStageGitMismatchAndAuditFailureCleanUp(t *testing.T) {
	t.Run("ref mismatch", func(t *testing.T) {
		f := newInstallerFixture(t)
		f.installer.Fetcher = &fakeFetcher{err: fmt.Errorf("%w: fetched another HEAD", ErrCommitMismatch)}
		if _, err := f.installer.Stage(context.Background(), StageRequest{
			GitURL: "https://github.com/acme/reviewed-skill.git",
			Commit: pinnedCommit,
		}); !errors.Is(err, ErrCommitMismatch) {
			t.Fatalf("error = %v, want ErrCommitMismatch", err)
		}
		assertNoStages(t, f.stages)
	})

	t.Run("audit failure", func(t *testing.T) {
		f := newInstallerFixture(t)
		f.installer.Fetcher = &fakeFetcher{build: func(destination string) error {
			if err := os.Mkdir(destination, 0o700); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(destination, "SKILL.md"), []byte("not frontmatter\n"), 0o600)
		}}
		if _, err := f.installer.Stage(context.Background(), StageRequest{
			GitURL: "https://github.com/acme/reviewed-skill.git",
			Commit: pinnedCommit,
		}); !errors.Is(err, ErrAuditFailed) {
			t.Fatalf("error = %v, want ErrAuditFailed", err)
		}
		assertNoStages(t, f.stages)
	})
}

func TestCommandGitFetcherEnforcesBoundsBeforeMaterialization(t *testing.T) {
	tests := []struct {
		name  string
		build func(*testing.T, string)
	}{
		{
			name: "too many files",
			build: func(t *testing.T, repo string) {
				t.Helper()
				for index := 0; index < maxReviewFiles; index++ {
					writeFile(t, filepath.Join(repo, fmt.Sprintf("extra-%02d.txt", index)), "x\n", 0o600)
				}
			},
		},
		{
			name: "oversize content",
			build: func(t *testing.T, repo string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(repo, "large.txt"), bytes.Repeat([]byte{'x'}, maxReviewBytes), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo, commit := newLocalGitSkill(t, test.build)
			destination := filepath.Join(t.TempDir(), "fetched")

			err := (commandGitFetcher{}).Fetch(context.Background(), "file://"+repo, commit, destination)
			if !errors.Is(err, ErrTreeTooLarge) {
				t.Fatalf("error = %v, want ErrTreeTooLarge", err)
			}
			if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("bounded fetch materialized destination: %v", err)
			}
		})
	}
}

func TestCommandGitFetcherMaterializesExactLocalCommitWithoutNetwork(t *testing.T) {
	repo, commit := newLocalGitSkill(t, nil)
	destination := filepath.Join(t.TempDir(), "fetched")

	if err := (commandGitFetcher{}).Fetch(context.Background(), "file://"+repo, commit, destination); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(destination, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "name: reviewed-skill") {
		t.Fatalf("materialized SKILL.md = %q", body)
	}
	if _, err := os.Lstat(filepath.Join(destination, ".git")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("materialized Git metadata: %v", err)
	}
}

func TestCommandGitFetcherFailsClosedWhenRemoteArchiveCannotServeExactCommit(t *testing.T) {
	repo, commit := newLocalGitSkill(t, nil)
	command := exec.Command("git", "-C", repo, "config", "--unset", "uploadarchive.allowUnreachable")
	command.Env = gitEnvironment(t.TempDir())
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("disable exact remote archive support: %v\n%s", err, output)
	}
	destination := filepath.Join(t.TempDir(), "fetched")

	err := (commandGitFetcher{}).Fetch(context.Background(), "file://"+repo, commit, destination)
	if !errors.Is(err, ErrBoundedGitUnsupported) {
		t.Fatalf("error = %v, want ErrBoundedGitUnsupported", err)
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsupported remote materialized destination: %v", err)
	}
}

type httpDoFunc func(*http.Request) (*http.Response, error)

func (function httpDoFunc) Do(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestCommandGitFetcherUsesBoundedExactGitHubArchive(t *testing.T) {
	const gitURL = "https://github.com/acme/reviewed-skill.git"
	expectedURL := "https://codeload.github.com/acme/reviewed-skill/tar.gz/" + pinnedCommit
	archive := githubArchive(t, "reviewed-skill-"+pinnedCommit, map[string]string{
		"SKILL.md":  "---\nname: reviewed-skill\ndescription: Fetched safely.\n---\n",
		"guide.txt": "Review fetched changes.\n",
	})
	client := httpDoFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.String() != expectedURL {
			t.Fatalf("request = %s %s, want GET %s", request.Method, request.URL, expectedURL)
		}
		for _, header := range []string{"Authorization", "Cookie", "Proxy-Authorization"} {
			if value := request.Header.Get(header); value != "" {
				t.Fatalf("%s = %q, want no credential header", header, value)
			}
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/x-gzip"}},
			Body:       io.NopCloser(bytes.NewReader(archive)),
			Request:    request,
		}, nil
	})
	destination := filepath.Join(t.TempDir(), "fetched")

	err := (commandGitFetcher{client: client}).Fetch(context.Background(), gitURL, pinnedCommit, destination)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(destination, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "name: reviewed-skill") {
		t.Fatalf("materialized SKILL.md = %q", body)
	}
	if _, err := os.Lstat(filepath.Join(destination, "reviewed-skill-"+pinnedCommit)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("GitHub archive root was materialized: %v", err)
	}
}

func TestCommandGitFetcherRejectsUnsafeGitHubResponses(t *testing.T) {
	validArchive := githubArchive(t, "reviewed-skill-"+pinnedCommit, map[string]string{
		"SKILL.md": "---\nname: reviewed-skill\ndescription: Fetched safely.\n---\n",
	})
	oversizedCompressed := gzipBody(t, bytes.Repeat([]byte{'x'}, maxGitArchiveBytes), gzip.NoCompression)
	tests := []struct {
		name        string
		status      int
		contentType string
		body        []byte
		responseURL string
		contentSize int64
		wantErr     error
	}{
		{
			name:        "redirect",
			status:      http.StatusFound,
			contentType: "text/html",
			body:        []byte("redirect"),
			wantErr:     ErrBoundedGitUnsupported,
		},
		{
			name:        "unexpected response host",
			status:      http.StatusOK,
			contentType: "application/x-gzip",
			body:        validArchive,
			responseURL: "https://codeload.github.com.evil.invalid/acme/reviewed-skill/tar.gz/" + pinnedCommit,
			wantErr:     ErrBoundedGitUnsupported,
		},
		{
			name:        "private or auth required",
			status:      http.StatusUnauthorized,
			contentType: "application/json",
			body:        []byte(`{"message":"Requires authentication"}`),
			wantErr:     ErrBoundedGitUnsupported,
		},
		{
			name:        "unexpected content type",
			status:      http.StatusOK,
			contentType: "text/html",
			body:        validArchive,
			wantErr:     ErrBoundedGitUnsupported,
		},
		{
			name:        "declared compressed oversize",
			status:      http.StatusOK,
			contentType: "application/x-gzip",
			body:        validArchive,
			contentSize: maxGitArchiveBytes + 1,
			wantErr:     ErrTreeTooLarge,
		},
		{
			name:        "actual compressed oversize",
			status:      http.StatusOK,
			contentType: "application/x-gzip",
			body:        oversizedCompressed,
			wantErr:     ErrTreeTooLarge,
		},
		{
			name:        "corrupt gzip",
			status:      http.StatusOK,
			contentType: "application/x-gzip",
			body:        []byte("not gzip"),
			wantErr:     ErrUnsafeTree,
		},
		{
			name:        "truncated gzip",
			status:      http.StatusOK,
			contentType: "application/x-gzip",
			body:        validArchive[:len(validArchive)-4],
			wantErr:     ErrUnsafeTree,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			destination := filepath.Join(t.TempDir(), "fetched")
			client := httpDoFunc(func(request *http.Request) (*http.Response, error) {
				responseRequest := request
				if test.responseURL != "" {
					spoofed, err := http.NewRequest(http.MethodGet, test.responseURL, nil)
					if err != nil {
						t.Fatal(err)
					}
					responseRequest = spoofed
				}
				return &http.Response{
					StatusCode:    test.status,
					Header:        http.Header{"Content-Type": []string{test.contentType}},
					Body:          io.NopCloser(bytes.NewReader(test.body)),
					ContentLength: test.contentSize,
					Request:       responseRequest,
				}, nil
			})

			err := (commandGitFetcher{client: client}).Fetch(
				context.Background(),
				"https://github.com/acme/reviewed-skill.git",
				pinnedCommit,
				destination,
			)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
			if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unsafe response materialized destination: %v", err)
			}
		})
	}
}

func TestCommandGitFetcherRejectsUnsafeGitHubArchiveEntries(t *testing.T) {
	root := "reviewed-skill-" + pinnedCommit
	validSkill := "---\nname: reviewed-skill\ndescription: Fetched safely.\n---\n"
	tests := []struct {
		name    string
		entries []githubArchiveEntry
		wantErr error
	}{
		{
			name: "wrong commit root",
			entries: []githubArchiveEntry{
				{name: "reviewed-skill-" + strings.Repeat("f", 40) + "/", typeflag: tar.TypeDir},
				{name: "reviewed-skill-" + strings.Repeat("f", 40) + "/SKILL.md", body: validSkill},
			},
			wantErr: ErrCommitMismatch,
		},
		{
			name: "duplicate stripped path",
			entries: []githubArchiveEntry{
				{name: root + "/", typeflag: tar.TypeDir},
				{name: root + "/SKILL.md", body: validSkill},
				{name: root + "/SKILL.md", body: validSkill},
			},
			wantErr: ErrUnsafeTree,
		},
		{
			name: "duplicate root",
			entries: []githubArchiveEntry{
				{name: root + "/", typeflag: tar.TypeDir},
				{name: root + "/", typeflag: tar.TypeDir},
				{name: root + "/SKILL.md", body: validSkill},
			},
			wantErr: ErrUnsafeTree,
		},
		{
			name: "path traversal",
			entries: []githubArchiveEntry{
				{name: root + "/", typeflag: tar.TypeDir},
				{name: root + "/SKILL.md", body: validSkill},
				{name: root + "/../escape.txt", body: "escape"},
			},
			wantErr: ErrUnsafeTree,
		},
		{
			name: "symlink",
			entries: []githubArchiveEntry{
				{name: root + "/", typeflag: tar.TypeDir},
				{name: root + "/SKILL.md", body: validSkill},
				{name: root + "/link", typeflag: tar.TypeSymlink, linkname: "SKILL.md"},
			},
			wantErr: ErrUnsafeTree,
		},
		{
			name: "hard link",
			entries: []githubArchiveEntry{
				{name: root + "/", typeflag: tar.TypeDir},
				{name: root + "/SKILL.md", body: validSkill},
				{name: root + "/hard", typeflag: tar.TypeLink, linkname: root + "/SKILL.md"},
			},
			wantErr: ErrUnsafeTree,
		},
		{
			name: "device",
			entries: []githubArchiveEntry{
				{name: root + "/", typeflag: tar.TypeDir},
				{name: root + "/SKILL.md", body: validSkill},
				{name: root + "/device", typeflag: tar.TypeChar},
			},
			wantErr: ErrUnsafeTree,
		},
		{
			name: "PAX metadata",
			entries: []githubArchiveEntry{
				{name: root + "/", typeflag: tar.TypeDir},
				{name: root + "/SKILL.md", body: validSkill, pax: map[string]string{"comment": "surprise"}},
			},
			wantErr: ErrUnsafeTree,
		},
		{
			name: "sparse entry",
			entries: []githubArchiveEntry{
				{name: root + "/", typeflag: tar.TypeDir},
				{name: root + "/SKILL.md", body: validSkill},
				{name: root + "/sparse", typeflag: tar.TypeGNUSparse},
			},
			wantErr: ErrUnsafeTree,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			archive := githubArchiveEntries(t, test.entries)
			client := fixedGitHubClient(http.StatusOK, "application/x-gzip", archive)
			destination := filepath.Join(t.TempDir(), "fetched")

			err := (commandGitFetcher{client: client}).Fetch(
				context.Background(),
				"https://github.com/acme/reviewed-skill.git",
				pinnedCommit,
				destination,
			)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
			if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unsafe archive materialized destination: %v", err)
			}
		})
	}
}

func TestCommandGitFetcherBoundsDecompressedGitHubArchive(t *testing.T) {
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(bytes.Repeat([]byte{'x'}, maxGitArchiveBytes+1)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "fetched")

	err := (commandGitFetcher{
		client: fixedGitHubClient(http.StatusOK, "application/x-gzip", compressed.Bytes()),
	}).Fetch(
		context.Background(),
		"https://github.com/acme/reviewed-skill.git",
		pinnedCommit,
		destination,
	)
	if !errors.Is(err, ErrTreeTooLarge) {
		t.Fatalf("error = %v, want ErrTreeTooLarge", err)
	}
}

func TestCommandGitFetcherCancelsGitHubArchiveRequest(t *testing.T) {
	client := httpDoFunc(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	err := (commandGitFetcher{client: client}).Fetch(
		ctx,
		"https://github.com/acme/reviewed-skill.git",
		pinnedCommit,
		filepath.Join(t.TempDir(), "fetched"),
	)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context deadline exceeded", err)
	}
}

func TestGitHubHTTPClientRejectsRedirectsAndHasTimeout(t *testing.T) {
	client := newGitHubHTTPClient()
	if client.Timeout != gitHubRequestTimeout {
		t.Fatalf("timeout = %s, want %s", client.Timeout, gitHubRequestTimeout)
	}
	request, err := http.NewRequest(http.MethodGet, "https://example.invalid/redirect", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.CheckRedirect(request, nil); err == nil {
		t.Fatal("redirect was accepted")
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T, want dedicated HTTP transport", client.Transport)
	}
	if transport.Proxy != nil {
		t.Fatal("GitHub client inherited ambient proxy configuration")
	}
	if !transport.DisableCompression {
		t.Fatal("GitHub client may transparently decompress before compressed-byte accounting")
	}
}

type githubArchiveEntry struct {
	name     string
	body     string
	typeflag byte
	linkname string
	pax      map[string]string
}

func githubArchive(t *testing.T, root string, files map[string]string) []byte {
	t.Helper()
	entries := []githubArchiveEntry{{name: root + "/", typeflag: tar.TypeDir}}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		entries = append(entries, githubArchiveEntry{name: root + "/" + name, body: files[name]})
	}
	return githubArchiveEntries(t, entries)
}

func githubArchiveEntries(t *testing.T, entries []githubArchiveEntry) []byte {
	t.Helper()
	var compressed bytes.Buffer
	gzipWriter := gzip.NewWriter(&compressed)
	tarWriter := tar.NewWriter(gzipWriter)
	for _, entry := range entries {
		typeflag := entry.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}
		body := []byte(entry.body)
		if err := tarWriter.WriteHeader(&tar.Header{
			Name:       entry.name,
			Mode:       0o600,
			Size:       int64(len(body)),
			Typeflag:   typeflag,
			Linkname:   entry.linkname,
			PAXRecords: entry.pax,
		}); err != nil {
			t.Fatal(err)
		}
		if len(body) > 0 {
			if _, err := tarWriter.Write(body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return compressed.Bytes()
}

func gzipBody(t *testing.T, body []byte, level int) []byte {
	t.Helper()
	var compressed bytes.Buffer
	writer, err := gzip.NewWriterLevel(&compressed, level)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return compressed.Bytes()
}

func fixedGitHubClient(status int, contentType string, body []byte) httpDoer {
	return httpDoFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{contentType}},
			Body:       io.NopCloser(bytes.NewReader(body)),
			Request:    request,
		}, nil
	})
}

func newLocalGitSkill(t *testing.T, build func(*testing.T, string)) (string, string) {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	copyFile(t, filepath.Join("testdata", "valid", "SKILL.md"), filepath.Join(repo, "SKILL.md"))
	if build != nil {
		build(t, repo)
	}
	runGit := func(arguments ...string) string {
		t.Helper()
		command := exec.Command("git", append([]string{"-C", repo}, arguments...)...)
		command.Env = gitEnvironment(t.TempDir())
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", arguments, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	runGit("init", "--quiet")
	runGit("config", "uploadarchive.allowUnreachable", "true")
	runGit("-c", "core.hooksPath=/dev/null", "add", "--all")
	runGit(
		"-c", "core.hooksPath=/dev/null",
		"-c", "user.name=Waffle Test",
		"-c", "user.email=waffle@example.invalid",
		"commit", "--quiet", "-m", "fixture",
	)
	return repo, runGit("rev-parse", "HEAD")
}

func TestInstallInjectsInactiveAndConsumesStage(t *testing.T) {
	f := newInstallerFixture(t)
	writeFile(t, filepath.Join(f.source, "SKILL.md"), "---\nname: reviewed-skill\ndescription: Reviews changes carefully.\nstatus: active\nowner: matt\n---\n\n# Reviewed skill\n\nKeep this body exact.\n", 0o600)
	manifest := stageLocal(t, f)

	installed, err := f.installer.Install(context.Background(), manifest.StageID, manifest.ContentDigest)
	if err != nil {
		t.Fatal(err)
	}
	if installed.Name != manifest.Name || installed.Path != filepath.Join(f.skills, manifest.Name, "SKILL.md") {
		t.Fatalf("installed = %+v", installed)
	}
	body, err := os.ReadFile(installed.Path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	const wantSkill = "---\ndescription: Reviews changes carefully.\nname: reviewed-skill\nowner: matt\nmetadata:\n  waffle/status: inactive\n---\n\n# Reviewed skill\n\nKeep this body exact.\n"
	if text != wantSkill {
		t.Fatalf("installed SKILL.md =\n%s\nwant only the status rewrite (metadata form):\n%s", text, wantSkill)
	}
	discovered, err := skill.Discover(f.skills)
	if err != nil {
		t.Fatal(err)
	}
	if len(discovered) != 1 || discovered[0].Name != "reviewed-skill" {
		t.Fatalf("discovered = %+v", discovered)
	}
	active, err := skill.DiscoverActive(f.skills, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 {
		t.Fatalf("installed skill active by default: %+v", active)
	}
	info, err := os.Lstat(filepath.Join(f.skills, manifest.Name))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("installed directory mode = %v", info.Mode())
	}
	for _, entry := range manifest.Files {
		info, err := os.Lstat(filepath.Join(f.skills, manifest.Name, filepath.FromSlash(entry.Path)))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("installed %s mode = %v", entry.Path, info.Mode())
		}
	}
	assertNoStages(t, f.stages)
	if _, err := f.installer.Install(context.Background(), manifest.StageID, manifest.ContentDigest); !errors.Is(err, ErrStageNotFound) {
		t.Fatalf("stage reuse error = %v, want ErrStageNotFound", err)
	}
}

func TestInstallDoesNotRediscoverUnrelatedSkillsAfterCommit(t *testing.T) {
	f := newInstallerFixture(t)
	if err := os.MkdirAll(filepath.Join(f.skills, "unrelated", "SKILL.md"), 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := stageLocal(t, f)

	installed, err := f.installer.Install(context.Background(), manifest.StageID, manifest.ContentDigest)
	if err != nil {
		t.Fatalf("Install returned a post-commit error: %v", err)
	}
	if installed.Name != manifest.Name {
		t.Fatalf("installed = %+v, want %q", installed, manifest.Name)
	}
}

func TestInstallRollsBackParentSyncFailure(t *testing.T) {
	f := newInstallerFixture(t)
	manifest := stageLocal(t, f)
	syncFailure := errors.New("injected parent sync failure")
	f.installer.syncDirectory = func(path string) error {
		if path == f.skills {
			return syncFailure
		}
		return syncDirectory(path)
	}

	result, err := f.installer.InstallReviewed(context.Background(), manifest.StageID, manifest.ContentDigest)
	if !errors.Is(err, syncFailure) {
		t.Fatalf("error = %v, want sync failure", err)
	}
	if result.Committed {
		t.Fatalf("result = %+v, want rolled back", result)
	}
	if _, err := os.Lstat(filepath.Join(f.skills, manifest.Name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target remains after successful rollback: %v", err)
	}
	assertNoStages(t, f.stages)
}

func TestInstallReportsIrreversibleCommitAfterRollbackFailure(t *testing.T) {
	f := newInstallerFixture(t)
	manifest := stageLocal(t, f)
	syncFailure := errors.New("injected parent sync failure")
	rollbackFailure := errors.New("injected rollback failure")
	f.installer.syncDirectory = func(path string) error {
		if path == f.skills {
			return syncFailure
		}
		return syncDirectory(path)
	}
	renames := 0
	f.installer.rename = func(oldPath, newPath string) error {
		renames++
		if renames == 2 {
			return rollbackFailure
		}
		return atomicRenameNoReplace(oldPath, newPath)
	}

	result, err := f.installer.InstallReviewed(context.Background(), manifest.StageID, manifest.ContentDigest)
	if err != nil {
		t.Fatalf("InstallReviewed returned ordinary error after commit: %v", err)
	}
	if !result.Committed || result.Skill.Name != manifest.Name {
		t.Fatalf("result = %+v, want explicit committed skill", result)
	}
	if !errors.Is(errors.Join(result.Warnings...), syncFailure) ||
		!errors.Is(errors.Join(result.Warnings...), rollbackFailure) {
		t.Fatalf("warnings = %v, want sync and rollback failures", result.Warnings)
	}
	if _, err := os.Lstat(filepath.Join(f.skills, manifest.Name, "SKILL.md")); err != nil {
		t.Fatalf("committed target missing: %v", err)
	}
	assertNoStages(t, f.stages)
}

func TestInstallRejectsExpiryDigestMismatchAndTampering(t *testing.T) {
	t.Run("expired", func(t *testing.T) {
		f := newInstallerFixture(t)
		manifest := stageLocal(t, f)
		f.now = manifest.ExpiresAt
		if _, err := f.installer.Install(context.Background(), manifest.StageID, manifest.ContentDigest); !errors.Is(err, ErrStageExpired) {
			t.Fatalf("error = %v, want ErrStageExpired", err)
		}
		assertNoStages(t, f.stages)
	})

	t.Run("digest mismatch", func(t *testing.T) {
		f := newInstallerFixture(t)
		manifest := stageLocal(t, f)
		if _, err := f.installer.Install(context.Background(), manifest.StageID, "sha256:not-reviewed"); !errors.Is(err, ErrDigestMismatch) {
			t.Fatalf("error = %v, want ErrDigestMismatch", err)
		}
		assertNoStages(t, f.stages)
	})

	t.Run("staged content changed", func(t *testing.T) {
		f := newInstallerFixture(t)
		manifest := stageLocal(t, f)
		writeFile(t, filepath.Join(f.stages, manifest.StageID, "content", "guide.txt"), "tampered\n", 0o600)
		if _, err := f.installer.Install(context.Background(), manifest.StageID, manifest.ContentDigest); !errors.Is(err, ErrStageChanged) {
			t.Fatalf("error = %v, want ErrStageChanged", err)
		}
		assertNoStages(t, f.stages)
	})
}

func TestInstallSupportsMaximumBoundedEscapedPreview(t *testing.T) {
	f := newInstallerFixture(t)
	if err := os.Remove(filepath.Join(f.source, "guide.txt")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(f.source, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	remaining := maxReviewBytes - int(info.Size())
	if err := os.WriteFile(filepath.Join(f.source, "newlines.txt"), bytes.Repeat([]byte{'\n'}, remaining), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := stageLocal(t, f)

	if _, err := f.installer.Install(context.Background(), manifest.StageID, manifest.ContentDigest); err != nil {
		t.Fatalf("install maximum bounded preview: %v", err)
	}
}

func TestInstallRejectsMalformedStageIDWithoutEscapingRoot(t *testing.T) {
	f := newInstallerFixture(t)
	outside := filepath.Join(filepath.Dir(f.stages), "outside")
	writeFile(t, filepath.Join(outside, "sentinel"), "keep\n", 0o600)

	if _, err := f.installer.Install(context.Background(), "../outside", "sha256:anything"); !errors.Is(err, ErrStageNotFound) {
		t.Fatalf("error = %v, want ErrStageNotFound", err)
	}
	body, err := os.ReadFile(filepath.Join(outside, "sentinel"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "keep\n" {
		t.Fatalf("outside sentinel changed: %q", body)
	}
}

func TestInstallRejectsSymlinkStageWithoutDeletingTarget(t *testing.T) {
	f := newInstallerFixture(t)
	manifest := stageLocal(t, f)
	stagePath := filepath.Join(f.stages, manifest.StageID)
	if err := os.RemoveAll(stagePath); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(filepath.Dir(f.stages), "outside-stage")
	writeFile(t, filepath.Join(outside, "sentinel"), "keep\n", 0o600)
	if err := os.Symlink(outside, stagePath); err != nil {
		t.Fatal(err)
	}

	if _, err := f.installer.Install(context.Background(), manifest.StageID, manifest.ContentDigest); !errors.Is(err, ErrStageNotFound) {
		t.Fatalf("error = %v, want ErrStageNotFound", err)
	}
	body, err := os.ReadFile(filepath.Join(outside, "sentinel"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "keep\n" {
		t.Fatalf("symlink target changed: %q", body)
	}
}

func TestInstallCollisionAndRenameFailureNeverOverwrite(t *testing.T) {
	t.Run("target appears after review", func(t *testing.T) {
		f := newInstallerFixture(t)
		manifest := stageLocal(t, f)
		existing := filepath.Join(f.skills, manifest.Name, "SKILL.md")
		writeFile(t, existing, "existing library bytes\n", 0o600)

		if _, err := f.installer.Install(context.Background(), manifest.StageID, manifest.ContentDigest); !errors.Is(err, ErrSkillExists) {
			t.Fatalf("error = %v, want ErrSkillExists", err)
		}
		body, err := os.ReadFile(existing)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != "existing library bytes\n" {
			t.Fatalf("existing target overwritten: %q", body)
		}
		assertNoStages(t, f.stages)
	})

	t.Run("atomic rename fails", func(t *testing.T) {
		f := newInstallerFixture(t)
		unrelated := filepath.Join(f.skills, "unrelated", "SKILL.md")
		writeFile(t, unrelated, "unrelated bytes\n", 0o600)
		manifest := stageLocal(t, f)
		injected := errors.New("injected rename failure")
		f.installer.rename = func(string, string) error { return injected }

		if _, err := f.installer.Install(context.Background(), manifest.StageID, manifest.ContentDigest); !errors.Is(err, injected) {
			t.Fatalf("error = %v, want injected failure", err)
		}
		if _, err := os.Lstat(filepath.Join(f.skills, manifest.Name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("target after rename failure = %v", err)
		}
		body, err := os.ReadFile(unrelated)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != "unrelated bytes\n" {
			t.Fatalf("unrelated library changed: %q", body)
		}
		entries, err := os.ReadDir(f.skills)
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"unrelated"}; !slices.Equal(entryNames(entries), want) {
			t.Fatalf("skills root after rename failure = %v, want %v", entryNames(entries), want)
		}
		assertNoStages(t, f.stages)
	})

	t.Run("target symlink appears at atomic commit", func(t *testing.T) {
		f := newInstallerFixture(t)
		manifest := stageLocal(t, f)
		outside := filepath.Join(filepath.Dir(f.skills), "outside-target")
		writeFile(t, filepath.Join(outside, "sentinel"), "keep\n", 0o600)
		target := filepath.Join(f.skills, manifest.Name)
		f.installer.rename = func(oldPath, newPath string) error {
			if err := os.Symlink(outside, target); err != nil {
				return err
			}
			return atomicRenameNoReplace(oldPath, newPath)
		}

		if _, err := f.installer.Install(context.Background(), manifest.StageID, manifest.ContentDigest); !errors.Is(err, ErrSkillExists) {
			t.Fatalf("error = %v, want ErrSkillExists", err)
		}
		info, err := os.Lstat(target)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("target mode = %v, want original symlink", info.Mode())
		}
		body, err := os.ReadFile(filepath.Join(outside, "sentinel"))
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != "keep\n" {
			t.Fatalf("outside sentinel changed: %q", body)
		}
		assertNoStages(t, f.stages)
	})
}

func TestWriteReviewedTreeCleansPartialCopyFailure(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "partial")
	files := []reviewedFile{
		{entry: FileEntry{Path: "collision", Size: 1}, data: []byte("x")},
		{entry: FileEntry{Path: "collision/child.txt", Size: 1}, data: []byte("y")},
	}

	if err := writeReviewedTree(destination, files); err == nil {
		t.Fatal("writeReviewedTree succeeded, want path collision error")
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial destination remains after failure: %v", err)
	}
}

func TestInstallConcurrentConfirmIsSingleUse(t *testing.T) {
	f := newInstallerFixture(t)
	manifest := stageLocal(t, f)

	start := make(chan struct{})
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			_, err := f.installer.Install(context.Background(), manifest.StageID, manifest.ContentDigest)
			errs <- err
		}()
	}
	close(start)
	first, second := <-errs, <-errs
	successes := 0
	notFound := 0
	for _, err := range []error{first, second} {
		if err == nil {
			successes++
		}
		if errors.Is(err, ErrStageNotFound) {
			notFound++
		}
	}
	if successes != 1 || notFound != 1 {
		t.Fatalf("concurrent errors = (%v, %v), want one success and one ErrStageNotFound", first, second)
	}
	discovered, err := skill.Discover(f.skills)
	if err != nil {
		t.Fatal(err)
	}
	if len(discovered) != 1 {
		t.Fatalf("installed skills = %+v, want exactly one", discovered)
	}
}

func TestStageAndInstallWritePolicyAudit(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "skill-audit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	f := newInstallerFixture(t)
	f.installer.AuditDB = st.DB
	manifest := stageLocal(t, f)
	if _, err := f.installer.InstallReviewed(ctx, manifest.StageID, manifest.ContentDigest); err != nil {
		t.Fatal(err)
	}

	var tools []string
	rows, err := st.DB.QueryContext(ctx, `SELECT tool FROM policy_audit ORDER BY at`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var tool string
		if err := rows.Scan(&tool); err != nil {
			t.Fatal(err)
		}
		tools = append(tools, tool)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(tools, []string{"skillinstall.stage", "skillinstall.install"}) {
		t.Fatalf("audit tools = %v", tools)
	}
}

func TestInstallReportsFailedPolicyAuditWrite(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "skill-audit-closed.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	f := newInstallerFixture(t)
	f.installer.AuditDB = st.DB
	f.installer.Log = slog.New(slog.NewTextHandler(&logs, nil))
	manifest := stageLocal(t, f)

	result, err := f.installer.InstallReviewed(ctx, manifest.StageID, manifest.ContentDigest)
	if err != nil {
		t.Fatalf("InstallReviewed: %v", err)
	}
	// The skill is on disk by the time it is audited, so the install stands —
	// but it may not be reported as a clean success (#297).
	if !result.Committed {
		t.Fatal("install should still commit when its audit row is lost")
	}
	if len(result.Warnings) == 0 {
		t.Fatal("committed install with no audit row reported no warning")
	}
	if !errors.Is(errors.Join(result.Warnings...), policy.ErrAuditNotRecorded) {
		t.Fatalf("warnings = %v, want the lost audit write to be identifiable", result.Warnings)
	}
	body := logs.String()
	for _, want := range []string{"msg=\"policy audit write failed\"", "tool=skillinstall.stage", "tool=skillinstall.install", "stage_id="} {
		if !strings.Contains(body, want) {
			t.Fatalf("logs missing %q: %s", want, body)
		}
	}
	// The skill name is caller-supplied audited content, not a safe label.
	if strings.Contains(body, manifest.Name) {
		t.Fatalf("audited skill name leaked into the failure log: %s", body)
	}
}
