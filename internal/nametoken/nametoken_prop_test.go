package nametoken

import (
	"regexp"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// preimage maps an ASCII lowercase letter to every raw rune strings.ToLower
// folds onto it - the fold orbit the generator below draws a spelling from. It is
// written out rather than derived so the property is checked against an
// independent statement of the rule, not against Literal's own rendering. 's' is
// listed even though its orbit is the plain ASCII pair: it is the letter whose
// missing member (U+017F, which SimpleFold would add) is the whole point.
var preimage = map[rune][]rune{
	'i': {'i', 'I', '\u0130'},
	'k': {'k', 'K', '\u212a'},
	's': {'s', 'S'},
}

// foldOrbit returns the raw runes that may spell r inside a marker: its ASCII
// case pair, widened by U+0130 for i and U+212A for k, or r alone for a digit or
// a metacharacter.
func foldOrbit(r rune) []rune {
	if r >= 'A' && r <= 'Z' {
		r += 'a' - 'A'
	}
	if orbit, ok := preimage[r]; ok {
		return orbit
	}
	if r >= 'a' && r <= 'z' {
		return []rune{r, r - 'a' + 'A'}
	}
	return []rune{r}
}

// TestLiteralAcceptsEveryFoldSpellingProperty is the multi-rune composition half
// of the exhaustive per-letter proof: for a random ASCII token, ANY spelling
// built by replacing each rune with a member of its strings.ToLower preimage must
// match. Composition is where a rendering bug hides that a single-letter table
// cannot see - a metacharacter token ("h.265", "no-group", a stray "[") goes
// through regexp.QuoteMeta, and a quoting slip there either fails to compile or
// silently turns a literal into a class.
func TestLiteralAcceptsEveryFoldSpellingProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		token := rapid.StringMatching(`[ -~]{0,10}`).Draw(t, "token")
		var raw strings.Builder
		for _, r := range token {
			orbit := foldOrbit(r)
			raw.WriteRune(orbit[rapid.IntRange(0, len(orbit)-1).Draw(t, "spelling")])
		}
		re, err := regexp.Compile(`^` + Literal(token) + `$`)
		if err != nil {
			t.Fatalf("Literal(%+q) does not compile: %v", token, err)
		}
		if spelling := raw.String(); !re.MatchString(spelling) {
			t.Fatalf("Literal(%+q) does not match the fold spelling %+q", token, spelling)
		}
	})
}

// TestLiteralIsExactlyToLowerEqualityProperty pins the other direction over
// arbitrary raw text drawn from the runes that actually decide these markers
// (the ASCII case pairs, the three homographs, the scene delimiters): a rendered
// token matches a candidate spelling exactly when strings.ToLower makes the two
// equal. This is the property a global (?i) fails - it accepts ſ as an s, so it
// matches a spelling ToLower says is a different string.
func TestLiteralIsExactlyToLowerEqualityProperty(t *testing.T) {
	sample := rapid.SampledFrom([]string{
		"a", "A", "i", "I", "\u0130", "k", "K", "\u212a", "s", "S", "\u017f",
		"p", "P", "0", "9", ".", "-", "_", " ", "[",
	})
	rapid.Check(t, func(t *rapid.T) {
		token := rapid.StringMatching(`[ -~]{0,6}`).Draw(t, "token")
		raw := strings.Join(rapid.SliceOfN(sample, 0, 6).Draw(t, "raw_runes"), "")
		re, err := regexp.Compile(`^` + Literal(token) + `$`)
		if err != nil {
			t.Fatalf("Literal(%+q) does not compile: %v", token, err)
		}
		// Lower both sides ONCE, before comparing. Keeping the fold out of the
		// comparison expression is deliberate: strings.EqualFold(raw, token)
		// reads equivalent and is what a linter suggests, but EqualFold is the
		// full-Unicode simple fold this whole package exists to keep out of
		// name parsing (it reads ſ as an s), so an EqualFold oracle would
		// assert the very behaviour Literal refuses.
		lowRaw, lowToken := strings.ToLower(raw), strings.ToLower(token)
		if got := re.MatchString(raw); got != (lowRaw == lowToken) {
			t.Fatalf("Literal(%+q) matching %+q = %v, want %v (strings.ToLower: %+q vs %+q)",
				token, raw, got, lowRaw == lowToken, lowRaw, lowToken)
		}
	})
}
