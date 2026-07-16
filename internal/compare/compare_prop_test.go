package compare

import (
	"slices"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// splitEscapedParts is the test-local inverse of escapeJoinParts: split on
// unescaped commas, unescaping '\'-escaped runes. A genuine round-trip oracle,
// not a copy of the encoder.
func splitEscapedParts(s string) []string {
	var parts []string
	var cur strings.Builder
	escaped := false
	for _, r := range s {
		if escaped {
			cur.WriteRune(r)
			escaped = false
			continue
		}
		switch r {
		case '\\':
			escaped = true
		case ',':
			parts = append(parts, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	parts = append(parts, cur.String())
	return parts
}

// TestEscapeJoinPartsRoundTripProperty pins the dedupe-key component encoding
// with a round-trip: decoding the escaped join recovers the exact original
// element boundaries for any non-empty parts drawn from a delimiter-heavy
// alphabet (production groups are non-empty by the release.NoGroup invariant).
// A boundary-collapsing encoding (the naive strings.Join this replaced, under
// which ["a,b"] and ["a","b"] collide and suppress a distinct finding as
// already alerted) cannot survive the round-trip.
func TestEscapeJoinPartsRoundTripProperty(t *testing.T) {
	part := rapid.StringOfN(rapid.RuneFrom([]rune{'a', 'b', ',', '|', '\\'}), 1, 4, -1)
	gen := rapid.SliceOfN(part, 1, 4)
	rapid.Check(t, func(t *rapid.T) {
		parts := gen.Draw(t, "parts")
		encoded := escapeJoinParts(parts)
		back := splitEscapedParts(encoded)
		if !slices.Equal(back, parts) {
			t.Errorf("round-trip lost element boundaries: %q -> %q -> %q", parts, encoded, back)
		}
	})
}
