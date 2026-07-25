package providerconfig

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/matt-riley/waffle/internal/config"
)

// profileTestManager builds a manager over a real config.toml so the tests
// exercise the same locked, staged, journalled commit the product uses.
func profileTestManager(t *testing.T, contents string) *Manager {
	t.Helper()
	home := t.TempDir()
	t.Setenv("WAFFLE_HOME", home)
	configPath := filepath.Join(home, "config.toml")
	if err := os.WriteFile(configPath, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	active := false
	return &Manager{
		ConfigPath:     configPath,
		SecretsPath:    filepath.Join(home, "secrets.age"),
		LockPath:       filepath.Join(home, "provider-config.lock"),
		Identity:       identity,
		Restart:        func(context.Context) error { active = true; return nil },
		Health:         func(context.Context) error { return nil },
		ServiceActive:  func(context.Context) (bool, error) { return active, nil },
		Stop:           func(context.Context) error { active = false; return nil },
		RestoreService: func(_ context.Context, wasActive bool) error { active = wasActive; return nil },
	}
}

const profileBaseConfig = `# Waffle configuration
# This comment must survive every profile edit.

[gateway]
status_listen = "127.0.0.1:8422"  # inline comment

[providers.primary]
type = "openai"
api_key = "secret://provider/primary/api-key"

[models.fast]
provider = "primary"
model = "fast-model"

[agent]
default_model = "fast"

[agent.group.main]
sandbox = "host"

[agent.group.main.tools]
allow = ["bash", "read_file", "write_file"]
deny_prefixes = ["rm -rf"]

[agent.profile.reviewer]
system = "You review changes."
model = "fast"
sandbox = "docker"
max_tokens = 4096

[agent.profile.reviewer.tools]
allow = ["read_file"]
`

func TestPutProfileRoundTripsWithoutTouchingUnrelatedConfig(t *testing.T) {
	manager := profileTestManager(t, profileBaseConfig)
	ctx := context.Background()

	before, err := os.ReadFile(manager.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}

	// Round-trip the profile unchanged: read it back out and write it in.
	loaded, err := config.Load(manager.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	existing := loaded.Agent.Profiles["reviewer"]
	if _, err := manager.PutProfile(ctx, ProfileRequest{
		Name: "reviewer", System: existing.System, Model: existing.Model,
		Sandbox: existing.Sandbox, MaxTokens: existing.MaxTokens,
		Allow: existing.Tools.Allow,
	}, CommitForRestart); err != nil {
		t.Fatalf("PutProfile: %v", err)
	}

	after, err := os.ReadFile(manager.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(after)
	// AC6: unrelated content is preserved verbatim — comments, ordering,
	// providers, models, and the group table.
	for _, preserved := range []string{
		"# Waffle configuration",
		"# This comment must survive every profile edit.",
		`status_listen = "127.0.0.1:8422"  # inline comment`,
		"[providers.primary]",
		`api_key = "secret://provider/primary/api-key"`,
		"[models.fast]",
		"[agent.group.main]",
		`allow = ["bash", "read_file", "write_file"]`,
	} {
		if !strings.Contains(text, preserved) {
			t.Errorf("round-trip dropped %q\n---\n%s", preserved, text)
		}
	}
	if strings.Count(text, "[agent.profile.reviewer]") != 1 {
		t.Errorf("round-trip duplicated the profile table:\n%s", text)
	}

	reloaded, err := config.Load(manager.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	got := reloaded.Agent.Profiles["reviewer"]
	if got.System != existing.System || got.Model != existing.Model ||
		got.Sandbox != existing.Sandbox || got.MaxTokens != existing.MaxTokens ||
		strings.Join(got.Tools.Allow, ",") != strings.Join(existing.Tools.Allow, ",") {
		t.Fatalf("round-trip changed the profile: %+v -> %+v", existing, got)
	}
	if len(reloaded.Providers) != 1 || len(reloaded.Models) != 1 {
		t.Fatalf("round-trip altered providers or models: %+v", reloaded)
	}
	_ = before
}

func TestPutProfileCreatesAndClearsFields(t *testing.T) {
	manager := profileTestManager(t, profileBaseConfig)
	ctx := context.Background()

	if _, err := manager.PutProfile(ctx, ProfileRequest{
		Name: "planner", System: "You plan.", Sandbox: "docker",
		Allow: []string{"read_file"}, Deny: []string{"bash"},
		DenyPrefixes: []string{"git push"}, MaxIterations: 8,
		AllowedChildren: []string{"reviewer"},
	}, CommitForRestart); err != nil {
		t.Fatalf("create: %v", err)
	}
	loaded, err := config.Load(manager.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	created := loaded.Agent.Profiles["planner"]
	if created.System != "You plan." || created.Sandbox != "docker" ||
		created.MaxIterations != 8 ||
		strings.Join(created.Tools.Deny, ",") != "bash" ||
		strings.Join(created.DenyPrefixes, ",") != "git push" ||
		strings.Join(created.AllowedChildren, ",") != "reviewer" {
		t.Fatalf("created profile = %+v", created)
	}

	// A deferred transaction must be finalised before the next one, exactly as
	// serve does after a restart.
	if err := manager.FinalizeDeferred(ctx); err != nil {
		t.Fatalf("FinalizeDeferred: %v", err)
	}

	// Re-saving without the optional fields clears them rather than leaving
	// dead settings behind.
	if _, err := manager.PutProfile(ctx, ProfileRequest{
		Name: "planner", System: "You plan.",
	}, CommitForRestart); err != nil {
		t.Fatalf("clear: %v", err)
	}
	loaded, err = config.Load(manager.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	cleared := loaded.Agent.Profiles["planner"]
	if cleared.Sandbox != "" || cleared.MaxIterations != 0 ||
		len(cleared.Tools.Deny) != 0 || len(cleared.DenyPrefixes) != 0 ||
		len(cleared.AllowedChildren) != 0 {
		t.Fatalf("cleared profile kept dead settings: %+v", cleared)
	}
	if _, stillThere := loaded.Agent.Profiles["reviewer"]; !stillThere {
		t.Fatal("editing one profile removed another")
	}
}

// AC2: each policy field has its own widening refusal, and none of them
// reaches the filesystem.
func TestPutProfileRefusesWideningPerField(t *testing.T) {
	dockerGroup := `
[agent.group.main]
sandbox = "docker"

[agent.group.main.tools]
allow = ["read_file"]
deny = ["bash"]
deny_prefixes = ["rm -rf"]
`
	for _, tc := range []struct {
		name      string
		request   ProfileRequest
		wantField string
	}{
		{
			name:      "sandbox escape",
			request:   ProfileRequest{Name: "escape", Sandbox: "host"},
			wantField: "sandbox",
		},
		{
			name:      "allow beyond the group",
			request:   ProfileRequest{Name: "escape", Allow: []string{"read_file", "write_file"}},
			wantField: "tools.allow",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := profileTestManager(t, dockerGroup)
			original, err := os.ReadFile(manager.ConfigPath)
			if err != nil {
				t.Fatal(err)
			}

			_, err = manager.PutProfile(context.Background(), tc.request, CommitForRestart)
			var widening *config.ProfileWideningError
			if !errors.As(err, &widening) {
				t.Fatalf("PutProfile error = %v, want a widening refusal", err)
			}
			if widening.Field != tc.wantField {
				t.Fatalf("field = %q, want %q", widening.Field, tc.wantField)
			}
			if !errors.Is(err, config.ErrProfileWidens) {
				t.Fatal("widening error does not match ErrProfileWidens")
			}

			// The refusal happens before staging, so the file is untouched.
			after, err := os.ReadFile(manager.ConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(original) {
				t.Fatalf("refused edit still wrote config:\n%s", after)
			}
		})
	}
}

func TestPutProfileNarrowingIsAccepted(t *testing.T) {
	manager := profileTestManager(t, `
[agent.group.main]
sandbox = "host"

[agent.group.main.tools]
allow = ["bash", "read_file"]
`)
	// host -> docker enters the sandbox, and the allowlist shrinks: both narrow.
	if _, err := manager.PutProfile(context.Background(), ProfileRequest{
		Name: "narrow", Sandbox: "docker", Allow: []string{"read_file"},
		Deny: []string{"bash"},
	}, CommitForRestart); err != nil {
		t.Fatalf("narrowing edit was refused: %v", err)
	}
}

func TestPutProfileRejectsMalformedFields(t *testing.T) {
	manager := profileTestManager(t, profileBaseConfig)
	for _, tc := range []struct {
		name    string
		request ProfileRequest
	}{
		{name: "bad name", request: ProfileRequest{Name: "Not A Slug"}},
		{name: "path as name", request: ProfileRequest{Name: "../escape"}},
		{name: "bad sandbox", request: ProfileRequest{Name: "ok", Sandbox: "vm"}},
		{name: "negative tokens", request: ProfileRequest{Name: "ok", MaxTokens: -1}},
		{
			name:    "shell metacharacter in a tool name",
			request: ProfileRequest{Name: "ok", Allow: []string{"bash; rm -rf /"}},
		},
		{
			name:    "path as a tool name",
			request: ProfileRequest{Name: "ok", Deny: []string{"/usr/bin/env"}},
		},
		{
			name:    "invalid allowed child",
			request: ProfileRequest{Name: "ok", AllowedChildren: []string{"../escape"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := manager.PutProfile(context.Background(), tc.request, CommitForRestart); err == nil {
				t.Fatal("malformed request was accepted")
			}
		})
	}
}

// AC4: a referenced profile cannot be deleted, and the references are named.
func TestRemoveProfileRefusesWhileReferenced(t *testing.T) {
	referencing := profileBaseConfig + `
[agent.profile.lead]
allowed_children = ["reviewer"]
`
	manager := profileTestManager(t, referencing)
	_, err := manager.RemoveProfile(context.Background(), "reviewer", nil, CommitForRestart)
	if !errors.Is(err, ErrReferenced) {
		t.Fatalf("RemoveProfile error = %v, want ErrReferenced", err)
	}
	if !strings.Contains(err.Error(), "agent.profile.lead.allowed_children") {
		t.Fatalf("refusal did not name the reference: %v", err)
	}

	// Runtime references supplied by the caller block it just the same.
	_, err = manager.RemoveProfile(context.Background(), "reviewer",
		[]string{"scheduled job Daily review"}, CommitForRestart)
	if !errors.Is(err, ErrReferenced) || !strings.Contains(err.Error(), "Daily review") {
		t.Fatalf("runtime reference was not reported: %v", err)
	}

	loaded, err := config.Load(manager.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, gone := loaded.Agent.Profiles["reviewer"]; !gone {
		t.Fatal("refused delete removed the profile anyway")
	}
}

func TestRemoveProfileDeletesOnlyItsOwnTables(t *testing.T) {
	manager := profileTestManager(t, profileBaseConfig)
	if _, err := manager.RemoveProfile(context.Background(), "reviewer", nil, CommitForRestart); err != nil {
		t.Fatalf("RemoveProfile: %v", err)
	}
	loaded, err := config.Load(manager.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, present := loaded.Agent.Profiles["reviewer"]; present {
		t.Fatal("profile survived deletion")
	}
	if len(loaded.Providers) != 1 || len(loaded.Models) != 1 {
		t.Fatalf("deletion altered providers or models: %+v", loaded)
	}
	text, err := os.ReadFile(manager.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, preserved := range []string{
		"# Waffle configuration", "[providers.primary]", "[agent.group.main]",
	} {
		if !strings.Contains(string(text), preserved) {
			t.Errorf("deletion dropped %q\n---\n%s", preserved, text)
		}
	}
	if strings.Contains(string(text), "[agent.profile.reviewer") {
		t.Errorf("deletion left the profile tables behind:\n%s", text)
	}
}

func TestRemoveProfileRejectsUnknownName(t *testing.T) {
	manager := profileTestManager(t, profileBaseConfig)
	_, err := manager.RemoveProfile(context.Background(), "absent", nil, CommitForRestart)
	if !errors.Is(err, ErrProfileNotFound) {
		t.Fatalf("error = %v, want ErrProfileNotFound", err)
	}
}

func TestProfileReferencesNamesEveryConfigSite(t *testing.T) {
	cfg := config.Config{Agent: config.Agent{
		DefaultProfile: "reviewer",
		Profiles: map[string]config.AgentProfile{
			"reviewer": {},
			"lead":     {AllowedChildren: []string{"reviewer"}},
			"other":    {AllowedChildren: []string{"lead"}},
		},
	}}
	refs := ProfileReferences(cfg, "reviewer")
	want := []string{"agent.default_profile", "agent.profile.lead.allowed_children"}
	if strings.Join(refs, "|") != strings.Join(want, "|") {
		t.Fatalf("references = %v, want %v", refs, want)
	}
	if len(ProfileReferences(cfg, "other")) != 0 {
		t.Fatalf("unreferenced profile reported references")
	}
}
