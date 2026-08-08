package backup

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/matt-riley/waffle/internal/secret"
	"github.com/matt-riley/waffle/internal/store"
)

func TestCreateIncludesStateAndRequiresIdentityOptIn(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WAFFLE_HOME", home)
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(home, "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO sessions(id,title,created_at,updated_at) VALUES ('session-recovery','backup','','')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"config.toml":              "[provider]\nname='openai'\n",
		"secrets.age":              "encrypted-placeholder",
		"workspace/MEMORY.md":      "- durable memory\n",
		"skills/recovery/SKILL.md": "# Recovery\n",
	} {
		path := filepath.Join(home, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	withoutIdentity := filepath.Join(t.TempDir(), "without-identity")
	if err := Create(ctx, withoutIdentity, false, ""); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"waffle.db", "config.toml", "secrets.age", "workspace/MEMORY.md", "skills/recovery/SKILL.md"} {
		if _, err := os.Stat(filepath.Join(withoutIdentity, name)); err != nil {
			t.Errorf("backup missing %s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(withoutIdentity, "identity")); !os.IsNotExist(err) {
		t.Fatalf("default backup contains identity: %v", err)
	}
	var gotManifest manifest
	b, err := os.ReadFile(filepath.Join(withoutIdentity, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &gotManifest); err != nil {
		t.Fatal(err)
	}
	if gotManifest.Identity {
		t.Fatal("default manifest claims identity is included")
	}

	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	withIdentity := filepath.Join(t.TempDir(), "with-identity")
	if err := Create(ctx, withIdentity, true, id.String()); err != nil {
		t.Fatal(err)
	}
	identity, err := os.ReadFile(filepath.Join(withIdentity, "identity"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(identity)) != id.String() {
		t.Fatal("opt-in backup identity differs")
	}
	info, err := os.Stat(filepath.Join(withIdentity, "identity"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("identity mode = %o", info.Mode().Perm())
	}
}

func TestDisasterRecoveryRestoresSessionsMemoryJobsAndSecrets(t *testing.T) {
	ctx := context.Background()
	sourceHome := t.TempDir()
	t.Setenv("WAFFLE_HOME", sourceHome)
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(ctx, filepath.Join(sourceHome, "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO sessions(id,title,summary,created_at,updated_at) VALUES ('session-dr','Recovered','summary','','')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO jobs(id,name,cron,prompt,created_at) VALUES ('job-dr','Recovered job','0 8 * * *','run recovery','')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(sourceHome, "workspace"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceHome, "workspace", "MEMORY.md"), []byte("- recovery memory\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := secret.OpenFile(filepath.Join(sourceHome, "secrets.age"), id).Set("recovery/token", "restored-secret"); err != nil {
		t.Fatal(err)
	}
	backupDir := filepath.Join(t.TempDir(), "backup")
	if err := Create(ctx, backupDir, false, ""); err != nil {
		t.Fatal(err)
	}

	freshHome := filepath.Join(t.TempDir(), "fresh-home")
	t.Setenv("WAFFLE_HOME", freshHome)
	t.Setenv(secret.EnvIdentity, id.String())
	if err := Restore(ctx, backupDir); err != nil {
		t.Fatal(err)
	}
	restored, err := store.Open(ctx, filepath.Join(freshHome, "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restored.Close() }()
	for table, want := range map[string]string{"sessions": "session-dr", "jobs": "job-dr"} {
		var got string
		if err := restored.DB.QueryRowContext(ctx, "SELECT id FROM "+table+" WHERE id = ?", want).Scan(&got); err != nil {
			t.Errorf("restored %s: %v", table, err)
		}
	}
	memory, err := os.ReadFile(filepath.Join(freshHome, "workspace", "MEMORY.md"))
	if err != nil || !strings.Contains(string(memory), "recovery memory") {
		t.Errorf("restored memory = %q, %v", memory, err)
	}
	gotSecret, err := secret.OpenFile(filepath.Join(freshHome, "secrets.age"), id).Get("recovery/token")
	if err != nil || gotSecret != "restored-secret" {
		t.Errorf("restored secret = %q, %v", gotSecret, err)
	}
}

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

// TestCreateLeavesNoStagingFilesInBackup covers #263: the manifest and identity
// are committed through the crash-safe helper, which stages a temp file next to
// its destination. None may survive into the backup directory, where a stray
// file would be restored alongside real state.
func TestCreateLeavesNoStagingFilesInBackup(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WAFFLE_HOME", home)
	ctx := context.Background()
	for name, body := range map[string]string{
		"config.toml":         "[provider]\nname='openai'\n",
		"secrets.age":         "encrypted-placeholder",
		"workspace/MEMORY.md": "- durable memory\n",
	} {
		path := filepath.Join(home, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	destination := filepath.Join(t.TempDir(), "backup")
	if err := Create(ctx, destination, true, "AGE-SECRET-KEY-TEST"); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(destination)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".filecommit-") {
			t.Errorf("staging file survived into the backup: %s", entry.Name())
		}
	}
	// The manifest is this backup's completion marker, so it must be present
	// and parseable after every other file is written.
	body, err := os.ReadFile(filepath.Join(destination, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got manifest
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("manifest is not valid JSON: %v", err)
	}
	if !got.Identity {
		t.Errorf("manifest = %+v, want the identity opt-in recorded", got)
	}
	if _, err := os.Stat(filepath.Join(destination, "identity")); err != nil {
		t.Errorf("identity missing from an opted-in backup: %v", err)
	}
}

// TestCreateSyncsEveryDirectoryItCreates covers the review follow-up on #263:
// fsyncing the files inside a backup is pointless if the directory entry that
// holds them never reaches stable storage, so every level Create makes must be
// made durable — not only the innermost one.
func TestCreateSyncsEveryDirectoryItCreates(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WAFFLE_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[provider]\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	// Two levels below an existing directory, so MkdirAll creates both.
	destination := filepath.Join(root, "nested", "backup")
	if err := Create(context.Background(), destination, false, ""); err != nil {
		t.Fatal(err)
	}

	created := missingDirs(filepath.Join(root, "fresh", "deeper", "leaf"))
	want := []string{
		filepath.Join(root, "fresh", "deeper", "leaf"),
		filepath.Join(root, "fresh", "deeper"),
		filepath.Join(root, "fresh"),
	}
	if len(created) != len(want) {
		t.Fatalf("missingDirs = %v, want the whole missing chain %v", created, want)
	}
	for i, dir := range want {
		if created[i] != dir {
			t.Errorf("missingDirs[%d] = %s, want %s (deepest first)", i, created[i], dir)
		}
	}
	// An existing directory contributes nothing to sync.
	if got := missingDirs(root); len(got) != 0 {
		t.Errorf("missingDirs on an existing path = %v, want none", got)
	}
	if err := syncCreatedDirs(created[:0]); err != nil {
		t.Errorf("syncCreatedDirs on an empty chain: %v", err)
	}

	if _, err := os.Stat(filepath.Join(destination, "manifest.json")); err != nil {
		t.Errorf("backup incomplete: %v", err)
	}
}

// TestCreateSyncsNestedDirectoriesItCopies is the second review follow-up on
// #263: copyFile syncs the directory each file lands in, but a directory that
// holds only subdirectories is never a file's parent, so nothing made its own
// entry durable and the subtree could vanish under a backup that reported
// success.
func TestCreateSyncsNestedDirectoriesItCopies(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WAFFLE_HOME", home)
	// skills/ holds only a directory; the file lives one level down.
	nested := filepath.Join(home, "skills", "recovery", "reference")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "NOTES.md"), []byte("# Notes\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	destination := filepath.Join(t.TempDir(), "backup")
	if err := Create(context.Background(), destination, false, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(destination, "skills", "recovery", "reference", "NOTES.md")); err != nil {
		t.Fatalf("nested file missing from backup: %v", err)
	}

	// The copy reports every directory it created, deepest first, so the
	// caller can sync entries no file write covers.
	source := filepath.Join(home, "skills")
	target := filepath.Join(t.TempDir(), "copy", "skills")
	created, err := copyDirIfExists(source, target)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		filepath.Join(target, "recovery", "reference"),
		filepath.Join(target, "recovery"),
		target,
	}
	if len(created) < len(want) {
		t.Fatalf("created = %v, want at least the copied tree %v", created, want)
	}
	for i, dir := range want {
		if created[i] != dir {
			t.Errorf("created[%d] = %s, want %s (deepest first)", i, created[i], dir)
		}
	}
	if missing, err := copyDirIfExists(filepath.Join(home, "absent"), target); err != nil || missing != nil {
		t.Errorf("copying an absent tree = %v, %v; want no directories and no error", missing, err)
	}
}

// TestCreateRemovesDestinationWhenItFails is the third review follow-up on
// #263. Cleanup is now keyed on the outcome rather than only on the presence
// of manifest.json, so a failure can never leave a destination that Restore
// accepts as complete and that a retry cannot overwrite. The reordering above
// also makes a post-marker failure unreachable — the marker is the last write
// — and this covers the general failure path and the retry that follows.
func TestCreateRemovesDestinationWhenItFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WAFFLE_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[provider]\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	destination := filepath.Join(t.TempDir(), "backup")
	// --with-identity with no identity fails after the state files are copied,
	// standing in for any post-copy failure.
	if err := Create(context.Background(), destination, true, ""); err == nil {
		t.Fatal("Create reported success without an identity")
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("failed backup left its destination behind: %v", err)
	}
	// And the path is reusable, so the owner can simply retry.
	if err := Create(context.Background(), destination, false, ""); err != nil {
		t.Fatalf("retry after a failed backup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destination, "manifest.json")); err != nil {
		t.Errorf("retry did not complete: %v", err)
	}
}

// TestRemoveFailedBackupRetractsTheMarkerAndReportsFailure is the fourth review
// follow-up on #263: filecommit.Write renames manifest.json into place before
// syncing its directory, so a failure in that sync leaves a marker on disk. The
// teardown has to retract it durably, and must not fail silently.
func TestRemoveFailedBackupRetractsTheMarkerAndReportsFailure(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "backup")
	if err := os.MkdirAll(filepath.Join(destination, "workspace"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"manifest.json", "secrets.age"} {
		if err := os.WriteFile(filepath.Join(destination, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := removeFailedBackup(destination); err != nil {
		t.Fatalf("removeFailedBackup: %v", err)
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("failed backup survived teardown: %v", err)
	}

	// A destination with no marker (the common case) is still torn down.
	bare := filepath.Join(t.TempDir(), "bare")
	if err := os.MkdirAll(bare, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := removeFailedBackup(bare); err != nil {
		t.Fatalf("removeFailedBackup without a marker: %v", err)
	}

	// A teardown that cannot complete is reported, not swallowed: the owner
	// has to know a destination was left behind.
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory write permissions")
	}
	parent := t.TempDir()
	stuck := filepath.Join(parent, "stuck")
	if err := os.MkdirAll(stuck, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stuck, "manifest.json"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	err := removeFailedBackup(stuck)
	if chmodErr := os.Chmod(parent, 0o700); chmodErr != nil {
		t.Fatal(chmodErr)
	}
	if err == nil {
		t.Fatal("removeFailedBackup reported success when it could not remove the destination")
	}
	// The marker is retracted even when the teardown that follows cannot
	// complete: no step gives up on the ones after it.
	if _, statErr := os.Stat(filepath.Join(stuck, "manifest.json")); !os.IsNotExist(statErr) {
		t.Errorf("a destination that could not be removed kept looking complete: %v", statErr)
	}
}
