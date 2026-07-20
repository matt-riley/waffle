package modelcatalog

import (
	"reflect"
	"strings"
	"testing"

	"github.com/matt-riley/waffle/internal/config"
)

func TestNormalizeModels(t *testing.T) {
	t.Run("validates provider fields", func(t *testing.T) {
		long := strings.Repeat("a", MaxFieldBytes+1)
		tests := []struct {
			name   string
			models []Model
		}{
			{name: "empty ID", models: []Model{{}}},
			{name: "negative context window", models: []Model{{ID: "model", ContextWindow: -1}}},
			{name: "long ID", models: []Model{{ID: long}}},
			{name: "long display name", models: []Model{{ID: "model", DisplayName: long}}},
			{name: "long owner", models: []Model{{ID: "model", Owner: long}}},
			{name: "long capability", models: []Model{{ID: "model", Capabilities: []string{long}}}},
			{name: "newline in ID", models: []Model{{ID: "bad\nid"}}},
			{name: "escape in display name", models: []Model{{ID: "model", DisplayName: "bad\x1bname"}}},
			{name: "control in owner", models: []Model{{ID: "model", Owner: "bad\u0085owner"}}},
			{name: "control in capability", models: []Model{{ID: "model", Capabilities: []string{"bad\u009fcapability"}}}},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				if _, err := Normalize(tc.models); err == nil {
					t.Fatalf("Normalize(%q) succeeded", tc.name)
				}
			})
		}
	})

	t.Run("rejects too many models", func(t *testing.T) {
		models := make([]Model, MaxModels+1)
		for i := range models {
			models[i].ID = "model"
		}
		if _, err := Normalize(models); err == nil {
			t.Fatalf("Normalize accepted %d models", len(models))
		}
	})

	t.Run("deduplicates and sorts deterministically", func(t *testing.T) {
		models := []Model{
			{ID: "zeta", Capabilities: []string{"vision", "text-output", "audio"}},
			{ID: "alpha", DisplayName: "Alpha"},
			{ID: "zeta", Capabilities: []string{"vision", "text-output", "audio"}},
		}
		got, err := Normalize(models)
		if err != nil {
			t.Fatalf("Normalize() error = %v", err)
		}
		want := []Model{
			{ID: "alpha", DisplayName: "Alpha"},
			{ID: "zeta", Capabilities: []string{"audio", "text-output", "vision"}},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("Normalize() = %#v; want %#v", got, want)
		}

		models[0].ID = "changed"
		models[0].Capabilities[0] = "changed"
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("Normalize() result changed with input: %#v; want %#v", got, want)
		}
	})
}

func TestSearchModels(t *testing.T) {
	models := []Model{
		{ID: "anthropic/claude-sonnet-4.6", DisplayName: "Claude Sonnet 4.6"},
		{ID: "google/gemini-2.5-flash", DisplayName: "Gemini Flash"},
		{ID: "openai/gpt-5.4", DisplayName: "GPT 5.4"},
	}
	tests := []struct {
		name  string
		query string
		want  []Model
	}{
		{name: "ID substring is case insensitive", query: "GEMINI-2.5", want: models[1:2]},
		{name: "display name substring is case insensitive", query: "sonnet", want: models[0:1]},
		{name: "exact ID", query: "openai/gpt-5.4", want: models[2:3]},
		{name: "no match", query: "llama", want: []Model{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Search(models, tc.query); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Search(%q) = %#v; want %#v", tc.query, got, tc.want)
			}
		})
	}
}

func TestAliasFor(t *testing.T) {
	tests := []struct {
		id   string
		want string
		err  bool
	}{
		{id: "anthropic/claude-sonnet-4.6", want: "anthropic-claude-sonnet-4-6"},
		{id: " GPT-5.4 ", want: "gpt-5-4"},
		{id: "///", err: true},
		{id: strings.Repeat("a", 80), want: strings.Repeat("a", config.ProviderConnectionNameMax)},
	}
	for _, tc := range tests {
		got, err := AliasFor(tc.id)
		if tc.err {
			if err == nil {
				t.Fatalf("AliasFor(%q) succeeded", tc.id)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Fatalf("AliasFor(%q) = %q, %v; want %q", tc.id, got, err, tc.want)
		}
	}
}

func TestSafeTextEscapesControls(t *testing.T) {
	if got, want := SafeText("line one\nline two\x1b[31m"), `line one\nline two\u001b[31m`; got != want {
		t.Fatalf("SafeText() = %q; want %q", got, want)
	}
}
