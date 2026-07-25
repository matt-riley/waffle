package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestResolveProfileSystemReadsInlineAndFileSources(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WAFFLE_HOME", home)
	if err := os.MkdirAll(filepath.Join(home, "prompts"), 0o700); err != nil {
		t.Fatal(err)
	}
	body := "You are the reviewer.\nBe exact.\n"
	if err := os.WriteFile(filepath.Join(home, "prompts", "reviewer.md"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name   string
		system string
		want   SystemPrompt
	}{
		{
			name:   "unset inherits the default prompt",
			system: "  ",
			want:   SystemPrompt{Kind: SystemPromptDefault},
		},
		{
			name:   "inline text",
			system: "  You are terse.  ",
			want:   SystemPrompt{Text: "You are terse.", Kind: SystemPromptInline},
		},
		{
			// Inline text is not a path just because it mentions one.
			name:   "inline text is not treated as a path",
			system: "Prefer the config in etc/waffle for context.",
			want: SystemPrompt{
				Text: "Prefer the config in etc/waffle for context.",
				Kind: SystemPromptInline,
			},
		},
		{
			name:   "at-prefixed relative path",
			system: "@prompts/reviewer.md",
			want: SystemPrompt{
				Text: "You are the reviewer.\nBe exact.",
				Kind: SystemPromptFile,
				Path: "prompts/reviewer.md",
			},
		},
		{
			name:   "bare markdown path",
			system: "prompts/reviewer.md",
			want: SystemPrompt{
				Text: "You are the reviewer.\nBe exact.",
				Kind: SystemPromptFile,
				Path: "prompts/reviewer.md",
			},
		},
		{
			name:   "absolute path inside WAFFLE_HOME",
			system: "@" + filepath.Join(home, "prompts", "reviewer.md"),
			want: SystemPrompt{
				Text: "You are the reviewer.\nBe exact.",
				Kind: SystemPromptFile,
				Path: "prompts/reviewer.md",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveProfileSystem(tc.system)
			if err != nil {
				t.Fatalf("ResolveProfileSystem: %v", err)
			}
			if got != tc.want {
				t.Fatalf("prompt = %+v, want %+v", got, tc.want)
			}
			// The resolved source is always relative, so projecting it can
			// never disclose where WAFFLE_HOME lives (#193 AC4).
			if filepath.IsAbs(got.Path) || strings.Contains(got.Path, home) {
				t.Fatalf("path %q leaked the home location", got.Path)
			}
		})
	}
}

func TestResolveProfileSystemRefusesEscapesAndMissingFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WAFFLE_HOME", home)
	outside := filepath.Join(t.TempDir(), "secret.md")
	if err := os.WriteFile(outside, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name   string
		system string
	}{
		{name: "parent traversal", system: "@../escape.md"},
		{name: "absolute path outside home", system: "@" + outside},
		{name: "missing file", system: "@prompts/absent.md"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveProfileSystem(tc.system)
			if err == nil {
				t.Fatalf("ResolveProfileSystem(%q) = %+v, want an error", tc.system, got)
			}
			if got.Text != "" {
				t.Fatalf("refused resolution still returned text %q", got.Text)
			}
		})
	}
}

func TestApplyProfilePolicyNarrowsAndNeverLiftsDenials(t *testing.T) {
	base := ResolvedAgentPolicy{
		Mode:         "host",
		Allow:        []string{"bash", "read", "write"},
		Deny:         []string{"remember"},
		DenyPrefixes: []string{"rm -rf"},
		Guidance:     "group guidance",
	}

	for _, tc := range []struct {
		name    string
		profile AgentProfile
		want    ResolvedAgentPolicy
	}{
		{
			name:    "empty profile changes nothing",
			profile: AgentProfile{},
			want:    base,
		},
		{
			name:    "sandbox override",
			profile: AgentProfile{Sandbox: "docker"},
			want: ResolvedAgentPolicy{
				Mode: "docker", Allow: base.Allow, Deny: base.Deny,
				DenyPrefixes: base.DenyPrefixes, Guidance: base.Guidance,
			},
		},
		{
			name:    "allow replaces the toolset",
			profile: AgentProfile{Tools: ToolPolicy{Allow: []string{"read"}}},
			want: ResolvedAgentPolicy{
				Mode: "host", Allow: []string{"read"}, Deny: base.Deny,
				DenyPrefixes: base.DenyPrefixes, Guidance: base.Guidance,
			},
		},
		{
			// The group's denial survives: a profile accumulates denials and
			// can never drop one.
			name:    "deny accumulates",
			profile: AgentProfile{Tools: ToolPolicy{Deny: []string{"bash"}}},
			want: ResolvedAgentPolicy{
				Mode: "host", Allow: base.Allow, Deny: []string{"remember", "bash"},
				DenyPrefixes: base.DenyPrefixes, Guidance: base.Guidance,
			},
		},
		{
			name: "deny prefixes merge from both fields without duplicates",
			profile: AgentProfile{
				DenyPrefixes: []string{"git push", "rm -rf"},
				Tools:        ToolPolicy{DenyPrefixes: []string{"curl"}},
			},
			want: ResolvedAgentPolicy{
				Mode: "host", Allow: base.Allow, Deny: base.Deny,
				DenyPrefixes: []string{"rm -rf", "git push", "curl"},
				Guidance:     base.Guidance,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ApplyProfilePolicy(base, tc.profile)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("policy = %+v, want %+v", got, tc.want)
			}
			// Narrowing must never mutate the caller's base policy.
			if !reflect.DeepEqual(base.Deny, []string{"remember"}) ||
				!reflect.DeepEqual(base.DenyPrefixes, []string{"rm -rf"}) ||
				!reflect.DeepEqual(base.Allow, []string{"bash", "read", "write"}) {
				t.Fatalf("base policy was mutated: %+v", base)
			}
		})
	}
}

func TestApplyToolLayerOnlyTightens(t *testing.T) {
	base := ResolvedAgentPolicy{
		Mode:  "host",
		Allow: []string{"bash", "read"},
		Deny:  []string{"remember"},
	}

	// A repo naming a tool the host withheld does not gain it: the allowlist
	// intersects rather than replaces.
	widened := ApplyToolLayer(base, ToolLayer{Allow: []string{"bash", "write"}})
	if !reflect.DeepEqual(widened.Allow, []string{"bash"}) {
		t.Fatalf("allow = %v, want the intersection [bash]", widened.Allow)
	}

	// With no host allowlist, the repo list is itself the intersection.
	open := ApplyToolLayer(ResolvedAgentPolicy{Mode: "host"}, ToolLayer{Allow: []string{"read"}})
	if !reflect.DeepEqual(open.Allow, []string{"read"}) {
		t.Fatalf("allow = %v, want [read]", open.Allow)
	}

	tightened := ApplyToolLayer(base, ToolLayer{
		Deny:         []string{"bash", "remember"},
		DenyPrefixes: []string{"git push"},
		Guidance:     "repo guidance",
	})
	if !reflect.DeepEqual(tightened.Deny, []string{"remember", "bash"}) {
		t.Fatalf("deny = %v", tightened.Deny)
	}
	if !reflect.DeepEqual(tightened.DenyPrefixes, []string{"git push"}) {
		t.Fatalf("deny prefixes = %v", tightened.DenyPrefixes)
	}
	if tightened.Guidance != "repo guidance" {
		t.Fatalf("guidance = %q", tightened.Guidance)
	}
}

func TestLayeredAgentPolicyRecordsEachTier(t *testing.T) {
	cfg := Config{
		Sandbox: Sandbox{Mode: "host", Allow: []string{"bash", "read", "write"}},
		Agent: Agent{
			Groups: map[string]AgentGroup{
				GroupMain: {Tools: ToolPolicy{
					Allow:        []string{"bash", "read", "write"},
					DenyPrefixes: []string{"rm -rf"},
					Guidance:     "group guidance",
				}},
			},
			Profiles: map[string]AgentProfile{
				"reviewer": {
					Sandbox:      "docker",
					Tools:        ToolPolicy{Allow: []string{"read"}, Deny: []string{"bash"}},
					DenyPrefixes: []string{"git push"},
				},
			},
		},
	}
	repo := &ToolLayer{Deny: []string{"read"}, Guidance: "repo tightening"}

	layered := cfg.LayeredAgentPolicy(GroupMain, "reviewer", repo)
	if len(layered.Layers) != 3 {
		t.Fatalf("layers = %d, want group, profile, repo", len(layered.Layers))
	}
	names := []string{layered.Layers[0].Name, layered.Layers[1].Name, layered.Layers[2].Name}
	if !reflect.DeepEqual(names, []string{"group", "profile", "repo"}) {
		t.Fatalf("layer names = %v", names)
	}
	for i, layer := range layered.Layers {
		if !layer.Applied {
			t.Fatalf("layer %d (%s) reported no effect", i, layer.Name)
		}
	}

	// Each tier reports its own contribution, not the running total, so a
	// reader can attribute a restriction to the tier that imposed it (AC2).
	if !reflect.DeepEqual(layered.Layers[1].Allow, []string{"read"}) {
		t.Fatalf("profile layer allow = %v, want its own [read]", layered.Layers[1].Allow)
	}
	if !reflect.DeepEqual(layered.Layers[1].DenyPrefixes, []string{"git push"}) {
		t.Fatalf("profile layer prefixes = %v", layered.Layers[1].DenyPrefixes)
	}
	if layered.Layers[1].Sandbox != "docker" {
		t.Fatalf("profile layer sandbox = %q", layered.Layers[1].Sandbox)
	}

	// Docker mode adds its own memory denials on top of the group's.
	effective := layered.Effective
	if effective.Mode != "docker" {
		t.Fatalf("effective mode = %q, want docker", effective.Mode)
	}
	if !reflect.DeepEqual(effective.Allow, []string{"read"}) {
		t.Fatalf("effective allow = %v", effective.Allow)
	}
	for _, denied := range []string{"bash", "read"} {
		if !containsString(effective.Deny, denied) {
			t.Fatalf("effective deny %v missing %q", effective.Deny, denied)
		}
	}
	if !reflect.DeepEqual(effective.DenyPrefixes, []string{"rm -rf", "git push"}) {
		t.Fatalf("effective prefixes = %v", effective.DenyPrefixes)
	}
}

// Allow/deny lists are unordered policy, so restating a group's allowlist in a
// different order is not a restriction and must not read as "Applied".
func TestLayeredAgentPolicyIgnoresListOrderWhenMarkingApplied(t *testing.T) {
	cfg := Config{
		Sandbox: Sandbox{Mode: "host"},
		Agent: Agent{
			Groups: map[string]AgentGroup{
				GroupMain: {Tools: ToolPolicy{Allow: []string{"bash", "read_file"}}},
			},
			Profiles: map[string]AgentProfile{
				"restater": {Tools: ToolPolicy{Allow: []string{"read_file", "bash"}}},
				"narrower": {Tools: ToolPolicy{Allow: []string{"read_file"}}},
			},
		},
	}

	restated := cfg.LayeredAgentPolicy(GroupMain, "restater", nil)
	if restated.Layers[1].Applied {
		t.Fatalf("reordered allowlist reported as applied: %+v", restated.Layers[1])
	}
	if !samePolicy(restated.Layers[0].Result, restated.Effective) {
		t.Fatal("reordering changed the effective policy")
	}

	// A genuine narrowing still reports as applied.
	narrowed := cfg.LayeredAgentPolicy(GroupMain, "narrower", nil)
	if !narrowed.Layers[1].Applied {
		t.Fatal("a real narrowing was reported as no change")
	}
}

func TestLayeredAgentPolicyMarksInertProfileLayer(t *testing.T) {
	cfg := Config{
		Sandbox: Sandbox{Mode: "host"},
		Agent: Agent{Profiles: map[string]AgentProfile{
			// A profile that only sets a model changes no policy at all.
			"reader": {Model: "primary"},
		}},
	}
	layered := cfg.LayeredAgentPolicy(GroupMain, "reader", nil)
	if len(layered.Layers) != 2 {
		t.Fatalf("layers = %d, want group and profile", len(layered.Layers))
	}
	if layered.Layers[1].Applied {
		t.Fatal("profile layer claimed an effect it did not have")
	}
	if !samePolicy(layered.Layers[0].Result, layered.Effective) {
		t.Fatalf("effective %+v drifted from the group layer %+v",
			layered.Effective, layered.Layers[0].Result)
	}
}
