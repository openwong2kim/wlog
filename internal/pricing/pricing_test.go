package pricing

import (
	"math"
	"testing"
)

func approx(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

// Lookup classifies by case-insensitive substring and returns the family price;
// unknown models report ok=false.
func TestLookup(t *testing.T) {
	cases := []struct {
		name      string
		wantOK    bool
		wantInput float64
	}{
		{"claude-opus-4-20250514", true, 15},
		{"claude-3-5-sonnet-20241022", true, 3},
		{"claude-haiku-4", true, 1},
		// case-insensitive substring matching.
		{"CLAUDE-OPUS-LATEST", true, 15},
		{"some-Sonnet-thing", true, 3},
		// unknown / empty.
		{"gpt-4o", false, 0},
		{"", false, 0},
	}
	for _, c := range cases {
		p, ok := Lookup(c.name)
		if ok != c.wantOK {
			t.Fatalf("Lookup(%q) ok = %v want %v", c.name, ok, c.wantOK)
		}
		if ok && !approx(p.Input, c.wantInput) {
			t.Fatalf("Lookup(%q) Input = %v want %v", c.name, p.Input, c.wantInput)
		}
	}
}

// CacheSavedUSD computes (input-cache_read)/1e6 * tokens for known models.
func TestCacheSavedUSD_Known(t *testing.T) {
	// haiku: (1 - 0.10)/1e6 * 1_000_000 = 0.90.
	got, ok := CacheSavedUSD("claude-haiku-4", 1_000_000)
	if !ok {
		t.Fatalf("haiku ok = false want true")
	}
	if !approx(got, 0.90) {
		t.Fatalf("haiku saved = %v want 0.90", got)
	}

	// opus: (15 - 1.5)/1e6 * 2_000_000 = 27.0.
	got, ok = CacheSavedUSD("claude-opus-4", 2_000_000)
	if !ok {
		t.Fatalf("opus ok = false want true")
	}
	if !approx(got, 27.0) {
		t.Fatalf("opus saved = %v want 27.0", got)
	}

	// sonnet: (3 - 0.30)/1e6 * 1_000_000 = 2.70.
	got, ok = CacheSavedUSD("claude-sonnet-4", 1_000_000)
	if !ok || !approx(got, 2.70) {
		t.Fatalf("sonnet saved = %v ok=%v want 2.70/true", got, ok)
	}
}

// CacheSavedUSD reports ok=false for unknown models and clamps non-positive
// token counts to 0 (still ok for a known model).
func TestCacheSavedUSD_EdgeCases(t *testing.T) {
	if _, ok := CacheSavedUSD("mystery-model", 1000); ok {
		t.Fatalf("unknown model ok = true want false")
	}
	if got, ok := CacheSavedUSD("claude-haiku-4", 0); !ok || got != 0 {
		t.Fatalf("zero cache read: got=%v ok=%v want 0/true", got, ok)
	}
	if got, ok := CacheSavedUSD("claude-haiku-4", -5); !ok || got != 0 {
		t.Fatalf("negative cache read: got=%v ok=%v want 0/true", got, ok)
	}
}
