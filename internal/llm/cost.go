package llm

// CostModel prices billed token classes relative to one uncached input
// token. Waffle has no per-endpoint price table; the named models carry the
// provider-published prompt-cache multipliers so budget binding and
// reporting can weight cache writes and cache reads instead of billing both
// at the input rate.
type CostModel struct {
	// CacheWrite is the multiplier applied to cache-creation tokens (the
	// provider's cache-write surcharge).
	CacheWrite float64
	// CacheRead is the multiplier applied to cache-read tokens (the
	// provider's cache-read discount).
	CacheRead float64
}

// AnthropicCost models Anthropic prompt caching: cache writes bill at 1.25x
// the input rate, cache reads at 0.1x.
var AnthropicCost = CostModel{CacheWrite: 1.25, CacheRead: 0.1}

// OpenAICost models OpenAI-compatible automatic caching: cached input bills
// at 0.5x the input rate on OpenAI proper, and there is no cache-write
// surcharge (writes are ordinary input).
var OpenAICost = CostModel{CacheWrite: 1.0, CacheRead: 0.5}

// BilledInput returns u's billed-input-equivalent token count under m:
// cache-creation tokens carry the cache-write surcharge and cache-read
// tokens the cache-read discount, instead of both billing at the input rate.
func (m CostModel) BilledInput(u Usage) float64 {
	return float64(u.InputTokens) +
		m.CacheWrite*float64(u.CacheCreationInputTokens) +
		m.CacheRead*float64(u.CacheReadInputTokens)
}

// CostModelForType returns the cost model for a provider type: "openai"
// covers any OpenAI-compatible endpoint (automatic caching; cache reads at
// 0.5x, no write surcharge). Any other type — including the empty string
// carried by usage rows written before provider attribution existed, or by
// callers that never learned the provider — prices at the Anthropic model,
// the legacy default and the only provider with an explicit cache API.
func CostModelForType(typ string) CostModel {
	if typ == "openai" {
		return OpenAICost
	}
	return AnthropicCost
}
