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
