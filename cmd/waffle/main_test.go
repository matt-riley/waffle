package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	chatpkg "github.com/matt-riley/waffle/internal/chat"
	"github.com/matt-riley/waffle/internal/config"
)

func TestHelpDocumentsChatFlags(t *testing.T) {
	var output bytes.Buffer
	usage(&output)

	for _, flag := range []string{"--continue", "--profile", "--socket", "--plain"} {
		if !strings.Contains(output.String(), flag) {
			t.Errorf("top-level help does not document chat flag %q:\n%s", flag, output.String())
		}
	}
}

func TestChatDocumentationMatchesCommandContract(t *testing.T) {
	path := filepath.Join("..", "..", "docs", "chat.md")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	document := string(body)

	for _, command := range chatpkg.Commands() {
		if !strings.Contains(document, "`"+command.Usage+"`") {
			t.Errorf("docs/chat.md does not document canonical usage %q", command.Usage)
		}
		for _, alias := range command.Aliases {
			if !strings.Contains(document, "`/"+alias+"`") {
				t.Errorf("docs/chat.md does not document alias %q for %q", alias, command.Name)
			}
		}
	}

	for _, term := range []string{
		"/run/waffle/chat.sock",
		"waffle-chat.socket",
		"NO_COLOR",
		"direct mode",
		"does not fall back",
	} {
		if !strings.Contains(document, term) {
			t.Errorf("docs/chat.md does not document %q", term)
		}
	}
}

func TestParseGlobalArgsConfig(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantRest   []string
		wantConfig string
		wantErr    string
	}{
		{
			name:       "long form space",
			args:       []string{"--config", "/tmp/a.toml", "status"},
			wantRest:   []string{"status"},
			wantConfig: "/tmp/a.toml",
		},
		{
			name:       "long form equals",
			args:       []string{"--config=/tmp/b.toml", "chat"},
			wantRest:   []string{"chat"},
			wantConfig: "/tmp/b.toml",
		},
		{
			name:       "short form space",
			args:       []string{"-c", "/tmp/c.toml", "version"},
			wantRest:   []string{"version"},
			wantConfig: "/tmp/c.toml",
		},
		{
			name:       "short form equals",
			args:       []string{"-c=/tmp/d.toml", "help"},
			wantRest:   []string{"help"},
			wantConfig: "/tmp/d.toml",
		},
		{
			name:       "last config wins",
			args:       []string{"-c", "/first.toml", "--config", "/second.toml", "status"},
			wantRest:   []string{"status"},
			wantConfig: "/second.toml",
		},
		{
			name:       "no global flags",
			args:       []string{"status", "--json"},
			wantRest:   []string{"status", "--json"},
			wantConfig: "",
		},
		{
			name:       "unknown leading flag left for command dispatch",
			args:       []string{"--unknown", "status"},
			wantRest:   []string{"--unknown", "status"},
			wantConfig: "",
		},
		{
			name:    "config missing value",
			args:    []string{"--config"},
			wantErr: "--config requires a path argument",
		},
		{
			name:    "short config missing value",
			args:    []string{"-c"},
			wantErr: "-c requires a path argument",
		},
		{
			name:    "empty equals value",
			args:    []string{"--config=", "status"},
			wantErr: "--config requires a path argument",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rest, cfgPath, err := parseGlobalArgs(tt.args)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseGlobalArgs: %v", err)
			}
			if cfgPath != tt.wantConfig {
				t.Errorf("configPath = %q, want %q", cfgPath, tt.wantConfig)
			}
			if len(rest) != len(tt.wantRest) {
				t.Fatalf("rest = %v, want %v", rest, tt.wantRest)
			}
			for i := range rest {
				if rest[i] != tt.wantRest[i] {
					t.Errorf("rest[%d] = %q, want %q", i, rest[i], tt.wantRest[i])
				}
			}
		})
	}
}

func TestRunAppliesConfigFlagOverride(t *testing.T) {
	config.SetConfigPath("")
	t.Cleanup(func() { config.SetConfigPath("") })

	custom := filepath.Join(t.TempDir(), "custom.toml")
	if err := os.WriteFile(custom, []byte("log = { level = \"debug\" }\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Env would point elsewhere; flag must win.
	t.Setenv("WAFFLE_CONFIG", filepath.Join(t.TempDir(), "from-env.toml"))

	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"--config", custom, "version"}, strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatalf("run: %v\nstderr: %s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "waffle") {
		t.Errorf("version output = %q", stdout.String())
	}

	got, err := config.Path()
	if err != nil {
		t.Fatalf("config.Path: %v", err)
	}
	if got != custom {
		t.Errorf("config.Path after --config = %q, want %q", got, custom)
	}
}

func TestRunAppliesWAFFLEConfigEnv(t *testing.T) {
	config.SetConfigPath("")
	t.Cleanup(func() { config.SetConfigPath("") })

	custom := filepath.Join(t.TempDir(), "env-config.toml")
	if err := os.WriteFile(custom, []byte("[gateway]\nstatus_listen = \"127.0.0.1:19999\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WAFFLE_CONFIG", custom)
	t.Setenv("WAFFLE_HOME", t.TempDir()) // default home must not be used for config path

	got, err := config.Path()
	if err != nil {
		t.Fatalf("config.Path: %v", err)
	}
	if got != custom {
		t.Errorf("config.Path with WAFFLE_CONFIG = %q, want %q", got, custom)
	}

	// Loading the custom path should surface the status_listen override.
	cfg, err := config.Load(got)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Gateway.StatusListen != "127.0.0.1:19999" {
		t.Errorf("StatusListen = %q, want 127.0.0.1:19999 from custom config", cfg.Gateway.StatusListen)
	}
}

func TestHelpDocumentsConfigFlag(t *testing.T) {
	var output bytes.Buffer
	usage(&output)
	text := output.String()
	for _, needle := range []string{"--config", "-c", "WAFFLE_CONFIG"} {
		if !strings.Contains(text, needle) {
			t.Errorf("usage does not document %q:\n%s", needle, text)
		}
	}
}
