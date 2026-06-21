// Package pricing maps Claude model names to approximate per-token USD prices so
// the cost API can report dollar figures (cumulative spend, burn rate, cache
// savings) instead of raw token counts.
//
// Prices are approximate published Anthropic rates in USD per MTok (million
// tokens). They drift over time — UPDATE HERE when Anthropic changes pricing.
// All lookups are estimates by definition; callers should flag derived figures
// as estimated.
package pricing

import "strings"

// Price holds the per-MTok USD rates for one model family. Multiply a token
// count by rate/1e6 to get USD.
type Price struct {
	Input      float64 // USD per MTok of input (prompt) tokens
	Output     float64 // USD per MTok of output (completion) tokens
	CacheRead  float64 // USD per MTok read from the prompt cache
	CacheWrite float64 // USD per MTok written to the prompt cache (cache creation)
}

// Family prices. USD per MTok. Approximate Claude 4.x rates — UPDATE HERE.
var (
	priceOpus = Price{
		Input: 15, Output: 75, CacheRead: 1.5, CacheWrite: 18.75,
	}
	priceSonnet = Price{
		Input: 3, Output: 15, CacheRead: 0.30, CacheWrite: 3.75,
	}
	priceHaiku = Price{
		Input: 1, Output: 5, CacheRead: 0.10, CacheWrite: 1.25,
	}
)

// Lookup classifies a model name into a Claude family by case-insensitive
// substring ("opus" / "sonnet" / "haiku") and returns its Price. ok is false for
// names that match no known family (an empty string included).
func Lookup(model string) (Price, bool) {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "opus"):
		return priceOpus, true
	case strings.Contains(m, "sonnet"):
		return priceSonnet, true
	case strings.Contains(m, "haiku"):
		return priceHaiku, true
	default:
		return Price{}, false
	}
}

// CacheSavedUSD estimates the dollars saved by reading cacheReadTokens from the
// prompt cache instead of paying the full input rate for the same tokens. It is
// the per-token rate difference (input - cache_read) times the token count:
//
//	(Input - CacheRead) / 1e6 * cacheReadTokens
//
// Returns ok=false for unknown models (no price to compute against). Negative
// token counts and non-positive savings are clamped to 0 so a known model never
// reports a "negative saving".
func CacheSavedUSD(model string, cacheReadTokens int64) (float64, bool) {
	p, ok := Lookup(model)
	if !ok {
		return 0, false
	}
	if cacheReadTokens <= 0 {
		return 0, true
	}
	saved := (p.Input - p.CacheRead) / 1e6 * float64(cacheReadTokens)
	if saved < 0 {
		saved = 0
	}
	return saved, true
}
