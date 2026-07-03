package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLoadMissingFileReturnsDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "config.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(cfg, Default()) {
		t.Errorf("got %+v, want defaults %+v", cfg, Default())
	}
}

func TestDefaultSandboxImageIncludesWorkspaceTools(t *testing.T) {
	if got := Default().Sandbox.Image; got != "buildpack-deps:bookworm-scm" {
		t.Fatalf("Sandbox.Image = %q, want default image containing Git", got)
	}
}

func TestLoadOverridesDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, "[gateway]\nlisten = \"127.0.0.1:9999\"\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Gateway.Listen != "127.0.0.1:9999" {
		t.Errorf("Listen = %q, want 127.0.0.1:9999", cfg.Gateway.Listen)
	}
	if cfg.Log.Level != "info" {
		t.Errorf("Log.Level = %q, want default info", cfg.Log.Level)
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, "[gateway]\nlisten = \"x\"\nlistne_typo = true\n")

	if _, err := Load(path); err == nil {
		t.Fatal("Load accepted unknown key, want error")
	}
}

func TestHomeRespectsEnv(t *testing.T) {
	t.Setenv("WAFFLE_HOME", "/tmp/waffle-test-home")
	h, err := Home()
	if err != nil {
		t.Fatalf("Home: %v", err)
	}
	if h != "/tmp/waffle-test-home" {
		t.Errorf("Home = %q, want WAFFLE_HOME value", h)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
