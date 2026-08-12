package llm

import "testing"

// TestCostModelBilledInput pins the cost arithmetic per provider: cache
// writes carry the provider's cache-write surcharge and cache reads the
// cache-read discount, instead of both billing at the input rate (#247).
func TestCostModelBilledInput(t *testing.T) {
	cases := []struct {
		name  string
		model CostModel
		usage Usage
		want  float64
	}{
		{
			name:  "anthropic",
			model: AnthropicCost,
			usage: Usage{InputTokens: 100, CacheCreationInputTokens: 20, CacheReadInputTokens: 30},
			want:  100 + 20*1.25 + 30*0.1,
		},
		{
			name:  "openai",
			model: OpenAICost,
			usage: Usage{InputTokens: 100, CacheCreationInputTokens: 20, CacheReadInputTokens: 30},
			want:  100 + 20*1.0 + 30*0.5,
		},
		{
			name:  "no caching",
			model: AnthropicCost,
			usage: Usage{InputTokens: 100, OutputTokens: 50},
			want:  100,
		},
		{
			name:  "cache reads cheaper than uncached input",
			model: AnthropicCost,
			usage: Usage{InputTokens: 0, CacheReadInputTokens: 1000},
			want:  100,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.model.BilledInput(tc.usage); got != tc.want {
				t.Fatalf("BilledInput(%+v) = %v, want %v", tc.usage, got, tc.want)
			}
		})
	}
}

// TestCostModelForType pins provider-type selection: OpenAI-compatible
// endpoints price cache reads at 0.5x with no write surcharge, and every
// other type — including the empty legacy attribution — prices at the
// Anthropic model (#247 review).
func TestCostModelForType(t *testing.T) {
	cases := []struct {
		typ  string
		want CostModel
	}{
		{typ: "openai", want: OpenAICost},
		{typ: "anthropic", want: AnthropicCost},
		{typ: "", want: AnthropicCost}, // legacy rows / unattributed usage
		{typ: "unknown-future-provider", want: AnthropicCost},
	}
	for _, tc := range cases {
		if got := CostModelForType(tc.typ); got != tc.want {
			t.Errorf("CostModelForType(%q) = %+v, want %+v", tc.typ, got, tc.want)
		}
	}
}
