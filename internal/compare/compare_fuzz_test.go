package compare

import (
	"strings"
	"testing"
)

// splitEscapedPartsBytes is a byte-based test-local inverse of
// escapeJoinParts (arbitrary fuzz bytes need not be valid UTF-8, so the
// rune-based splitEscapedParts oracle from the rapid property does not
// apply): split on unescaped commas, unescaping '\'-escaped bytes. A genuine
// round-trip oracle, not a copy of the encoder.
func splitEscapedPartsBytes(s string) []string {
	var parts []string
	var cur strings.Builder
	escaped := false
	for i := range len(s) {
		c := s[i]
		if escaped {
			cur.WriteByte(c)
			escaped = false
			continue
		}
		switch c {
		case '\\':
			escaped = true
		case ',':
			parts = append(parts, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	parts = append(parts, cur.String())
	return parts
}

// FuzzDedupeKeyEncodingInjective is the weekly-fuzz complement of the rapid
// properties in compare_prop_test.go: coverage-guided exploration over
// arbitrary bytes (including invalid UTF-8, which the rapid generators'
// 5-rune alphabet never reaches) of the dedupe-key encoding invariants the
// escaping and bounding exist to defend - a collision here suppresses a
// distinct finding as already alerted. Invariants: element boundaries
// round-trip through the escaped join, a delimiter-bearing merge cannot
// alias the split form, length-prefixed hashing preserves boundaries, the
// raw and hashed output domains stay disjoint, and oversized component sets
// reduce to the fixed-size hashed identity (CWE-400 bounding).
func FuzzDedupeKeyEncodingInjective(f *testing.F) {
	f.Add("a", "b")
	f.Add("a,b", "")
	f.Add(`x\`, "|y")
	f.Add("sha256:", "0000")
	f.Add(strings.Repeat("x", 5000), strings.Repeat("y", 4000))
	f.Add("\xff\xfe,|", `a\`)
	f.Fuzz(func(t *testing.T, a, b string) {
		parts := []string{a, b}
		encoded := escapeJoinParts(parts)
		back := splitEscapedPartsBytes(encoded)
		if len(back) != 2 || back[0] != a || back[1] != b {
			t.Errorf("round-trip lost element boundaries: [%q,%q] -> %q -> %q", a, b, encoded, back)
		}
		if escapeJoinParts([]string{a + "," + b}) == encoded {
			t.Errorf("[%q] and [%q,%q] share encoding %q", a+","+b, a, b, encoded)
		}
		if hashKeyParts(parts) == hashKeyParts([]string{a + b}) {
			t.Errorf("hashKeyParts collapsed element boundaries for [%q,%q]", a, b)
		}
		forged := hashKeyParts([]string{a})
		if boundedPart(forged) == forged {
			t.Errorf("boundedPart(%q) returned the raw hashed-identity spelling", forged)
		}
		if len(a)+len(b) > maxKeyComponentBytes {
			if got := boundedJoinParts(parts); len(got) > len(hashedKeyPrefix)+64 {
				t.Errorf("oversized input not reduced to hashed identity: %d bytes", len(got))
			}
		}
	})
}
