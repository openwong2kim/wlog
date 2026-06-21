package model

import "testing"

// TestDedupKeyDeterministic verifies DedupKey is a pure deterministic function:
// the same parts always yield the same 16-hex-char key.
func TestDedupKeyDeterministic(t *testing.T) {
	a := DedupKey("session-1", "api_request", "1700000000000", "claude-opus-4")
	b := DedupKey("session-1", "api_request", "1700000000000", "claude-opus-4")
	if a != b {
		t.Fatalf("DedupKey not deterministic: %q != %q", a, b)
	}
	if len(a) != 16 {
		t.Fatalf("DedupKey length = %d, want 16", len(a))
	}
	for _, r := range a {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			t.Fatalf("DedupKey %q contains non-hex char %q", a, r)
		}
	}
}

// TestDedupKeyInputOrderSensitive verifies DedupKey is sensitive to the ORDER of
// its parts (it hashes parts in input order, no sorting). Reordering must change
// the key so distinct events do not collide.
func TestDedupKeyInputOrderSensitive(t *testing.T) {
	a := DedupKey("x", "y")
	b := DedupKey("y", "x")
	if a == b {
		t.Fatalf("DedupKey should be order-sensitive, but %q == %q", a, b)
	}
}

// TestDedupKeySeparatorPreventsAmbiguity verifies the NUL separator distinguishes
// part boundaries: ("ab","c") must differ from ("a","bc") despite identical
// concatenation.
func TestDedupKeySeparatorPreventsAmbiguity(t *testing.T) {
	a := DedupKey("ab", "c")
	b := DedupKey("a", "bc")
	if a == b {
		t.Fatalf("DedupKey adjacent-part ambiguity: %q == %q", a, b)
	}
}

// TestDedupKeyDistinctInputs verifies different inputs produce different keys
// (no trivial collisions across a representative set).
func TestDedupKeyDistinctInputs(t *testing.T) {
	seen := map[string]string{}
	inputs := [][]string{
		{"s1", "api_request", "t1"},
		{"s1", "api_request", "t2"},
		{"s1", "api_error", "t1"},
		{"s2", "api_request", "t1"},
		{"s1", "tool_decision", "t1", "Bash", "accept"},
		{"s1", "tool_decision", "t1", "Bash", "reject"},
	}
	for _, in := range inputs {
		k := DedupKey(in...)
		if prev, ok := seen[k]; ok {
			t.Fatalf("DedupKey collision: %v and %v both -> %q", prev, in, k)
		}
		seen[k] = joinForMsg(in)
	}
}

// TestSeriesKeyOrderInsensitive verifies SeriesKey is independent of dims map
// iteration order: the same metric name + same dims always yields the same key
// regardless of how the map was built. (Maps have no guaranteed iteration order,
// so this exercises the internal sort.)
func TestSeriesKeyOrderInsensitive(t *testing.T) {
	name := "claude_code.token.usage"
	d1 := map[string]string{"type": "input", "model": "claude-opus-4", "session.id": "s1"}
	d2 := map[string]string{"session.id": "s1", "model": "claude-opus-4", "type": "input"}
	k1 := SeriesKey(name, d1)
	k2 := SeriesKey(name, d2)
	if k1 != k2 {
		t.Fatalf("SeriesKey order-sensitive: %q != %q", k1, k2)
	}
	// Run many times to defeat any chance map order accidentally aligned.
	for i := 0; i < 50; i++ {
		if got := SeriesKey(name, map[string]string{
			"model": "claude-opus-4", "session.id": "s1", "type": "input",
		}); got != k1 {
			t.Fatalf("SeriesKey unstable across rebuilds: %q != %q", got, k1)
		}
	}
}

// TestSeriesKeyDimsMatter verifies distinct dims produce distinct keys: a
// different value for any dimension forks the series.
func TestSeriesKeyDimsMatter(t *testing.T) {
	name := "claude_code.token.usage"
	base := SeriesKey(name, map[string]string{"type": "input", "model": "opus"})
	cases := []map[string]string{
		{"type": "output", "model": "opus"},      // type changed
		{"type": "input", "model": "sonnet"},     // model changed
		{"type": "input"},                        // model dropped
		{"type": "input", "model": "opus", "x": "y"}, // extra dim
	}
	for _, c := range cases {
		if got := SeriesKey(name, c); got == base {
			t.Fatalf("SeriesKey should differ for dims %v but matched base", c)
		}
	}
}

// TestSeriesKeyMetricNameMatters verifies the metric name participates in the
// key: same dims under different metric names must not collide.
func TestSeriesKeyMetricNameMatters(t *testing.T) {
	dims := map[string]string{"model": "opus"}
	a := SeriesKey("claude_code.token.usage", dims)
	b := SeriesKey("claude_code.cost.usage", dims)
	if a == b {
		t.Fatalf("SeriesKey ignores metric name: %q == %q", a, b)
	}
}

// TestSeriesKeyEmptyDims verifies an empty/nil dims map is handled (the key is
// just the metric name hash) and is stable.
func TestSeriesKeyEmptyDims(t *testing.T) {
	a := SeriesKey("m", nil)
	b := SeriesKey("m", map[string]string{})
	if a != b {
		t.Fatalf("SeriesKey nil vs empty dims differ: %q != %q", a, b)
	}
	if a == "" {
		t.Fatal("SeriesKey empty for empty dims")
	}
	// SHA-256 hex digest length is 64.
	if len(a) != 64 {
		t.Fatalf("SeriesKey length = %d, want 64", len(a))
	}
}

// TestSeriesKeyWhitelistEquivalence documents the contract the otel package
// relies on: SeriesKey hashes exactly the dims it is GIVEN. Callers (otel)
// pre-filter to the whitelist {type,model,language,session.id}; two dim maps that
// agree on the whitelisted keys but differ only in a key the caller would have
// excluded must, after that exclusion, produce the same key. We model the
// caller's exclusion here to assert the equivalence holds.
func TestSeriesKeyWhitelistEquivalence(t *testing.T) {
	whitelist := map[string]struct{}{
		"type": {}, "model": {}, "language": {}, "session.id": {},
	}
	filter := func(in map[string]string) map[string]string {
		out := map[string]string{}
		for k, v := range in {
			if _, ok := whitelist[k]; ok {
				out[k] = v
			}
		}
		return out
	}

	name := "claude_code.token.usage"
	// Same whitelisted dims, but volatile attrs differ (version, email, terminal).
	full1 := map[string]string{
		"type": "input", "model": "opus", "session.id": "s1",
		"app.version": "1.0.0", "user.email": "a@x", "terminal.type": "xterm",
	}
	full2 := map[string]string{
		"type": "input", "model": "opus", "session.id": "s1",
		"app.version": "2.0.0", "user.email": "b@y", "terminal.type": "tmux",
	}
	k1 := SeriesKey(name, filter(full1))
	k2 := SeriesKey(name, filter(full2))
	if k1 != k2 {
		t.Fatalf("whitelisted SeriesKey forked on volatile attrs: %q != %q", k1, k2)
	}
}

// joinForMsg renders parts for a collision message.
func joinForMsg(parts []string) string {
	out := "["
	for i, p := range parts {
		if i > 0 {
			out += ","
		}
		out += p
	}
	return out + "]"
}
