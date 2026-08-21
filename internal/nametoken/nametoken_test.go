package nametoken

import (
	"regexp"
	"strings"
	"testing"
)

// maxRune is the last valid Unicode code point; the exhaustive tests below walk
// the whole space (skipping the surrogate range, which string(r) replaces with
// U+FFFD and so cannot be tested through a rune round-trip).
const maxRune = 0x10FFFF

// forEachRune calls fn for every valid rune. It exists so both exhaustive
// invariants scan the same domain and skip the same surrogates.
func forEachRune(fn func(r rune)) {
	for r := rune(0); r <= maxRune; r++ {
		if r >= 0xD800 && r <= 0xDFFF {
			continue
		}
		fn(r)
	}
}

// asciiLower reports whether s is exactly one ASCII lowercase letter.
func asciiLower(s string) bool {
	return len(s) == 1 && s[0] >= 'a' && s[0] <= 'z'
}

// TestNonWordEdgeIsExactlyTheNonToLowerAlnumRunes is the load-bearing invariant
// of this package: a rune is a WORD rune exactly when strings.ToLower maps it
// onto an ASCII alphanumeric. That equivalence is what makes the boundary rule
// and the folding rule impossible to contradict - the alphabet a token is spelled
// in is the alphabet its edges are defined against - and it is why the
// ToLower reading won over a global (?i), which cannot state such a relation.
// Exhaustive over the whole rune space, so a hand-edited class body that adds a
// rune ToLower does not fold (or drops one it does - U+0130, U+212A) fails here
// rather than in one consumer's classification.
func TestNonWordEdgeIsExactlyTheNonToLowerAlnumRunes(t *testing.T) {
	edge := regexp.MustCompile(`^` + NonWordEdge + `$`)
	forEachRune(func(r rune) {
		s := string(r)
		low := strings.ToLower(s)
		foldsToAlnum := len(low) == 1 && (low[0] >= '0' && low[0] <= '9' || asciiLower(low))
		if isEdge := edge.MatchString(s); isEdge == foldsToAlnum {
			t.Fatalf("U+%04X (%q): NonWordEdge match = %v, strings.ToLower folds onto an ASCII alphanumeric = %v (the two must be opposites)",
				r, s, isEdge, foldsToAlnum)
		}
	})
}

// TestLiteralMatchesExactlyTheToLowerPreimage pins the folding half of the same
// rule, exhaustively and in both directions: Literal(letter) matches a rune
// exactly when strings.ToLower folds that rune onto the letter. The
// over-matching direction is the one that bites - it is how (?i)s came to accept
// U+017F (ſ) as an "s", inventing a marker in a name that carries none - so the
// any-letter alternation is asserted for every rune ToLower does NOT fold onto a
// letter, not just for the ASCII ones.
func TestLiteralMatchesExactlyTheToLowerPreimage(t *testing.T) {
	letters := make([]string, 0, 26)
	perLetter := make(map[string]*regexp.Regexp, 26)
	for c := byte('a'); c <= 'z'; c++ {
		letter := string(c)
		letters = append(letters, letter)
		perLetter[letter] = regexp.MustCompile(`^` + Literal(letter) + `$`)
	}
	anyLetter := regexp.MustCompile(`^(?:` + Alternation(letters) + `)$`)

	forEachRune(func(r rune) {
		s := string(r)
		low := strings.ToLower(s)
		if !asciiLower(low) {
			if anyLetter.MatchString(s) {
				t.Fatalf("U+%04X (%q): matched a letter class, but strings.ToLower gives %q - no letter may accept it",
					r, s, low)
			}
			return
		}
		if !perLetter[low].MatchString(s) {
			t.Fatalf("U+%04X (%q): Literal(%q) does not match it, but strings.ToLower folds it onto %q",
				r, s, low, low)
		}
		for _, other := range letters {
			if other != low && perLetter[other].MatchString(s) {
				t.Fatalf("U+%04X (%q): Literal(%q) matched it, but strings.ToLower folds it onto %q",
					r, s, other, low)
			}
		}
	})
}

// TestLiteral pins the rendering itself - the shape a consumer reads in a
// pattern - including the two homograph classes, the uppercase-token fold (a
// token spelled "CRF" must not render a case-SENSITIVE literal) and the
// metacharacter quoting the dotted codec spellings depend on.
func TestLiteral(t *testing.T) {
	for _, tc := range []struct{ token, want string }{
		{"", ""},
		{"s", `[sS]`},
		{"i", `[iI\x{0130}]`},
		{"k", `[kK\x{212A}]`},
		// Both ends of the ASCII uppercase range, spelled out: the fold that
		// gives an uppercase token its case class is written as a range, and a
		// letter outside it renders as a case-SENSITIVE literal instead.
		{"A", `[aA]`},
		{"Z", `[zZ]`},
		{"CRF", `[cC][rR][fF]`},
		{"crf", `[cC][rR][fF]`},
		{"1080p", `1080[pP]`},
		{"h.265", `[hH]\.265`},
		{"no-group", `[nN][oO]-[gG][rR][oO][uU][pP]`},
	} {
		if got := Literal(tc.token); got != tc.want {
			t.Errorf("Literal(%q) = %q, want %q", tc.token, got, tc.want)
		}
	}
}

// TestAlternation pins that the joined form carries no grouping of its own (the
// caller wraps it), so a consumer splicing it into a larger pattern gets the
// precedence it wrote rather than one this package chose.
func TestAlternation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tokens []string
		want   string
	}{
		{"empty", nil, ""},
		{"single token is a bare literal", []string{"kbps"}, `[kK\x{212A}][bB][pP][sS]`},
		{"two tokens are pipe-joined, ungrouped", []string{"kbps", "mbps"}, `[kK\x{212A}][bB][pP][sS]|[mM][bB][pP][sS]`},
	} {
		if got := Alternation(tc.tokens); got != tc.want {
			t.Errorf("%s: Alternation(%q) = %q, want %q", tc.name, tc.tokens, got, tc.want)
		}
	}
}

// TestLiteralHomographs is the readable table behind the exhaustive invariants:
// the four runes that make the ToLower reading differ from regexp's (?i), spelled
// out per letter. U+0130 and U+212A must be accepted (ToLower folds them onto i
// and k) and U+017F must be refused (ToLower leaves ſ alone, however much
// SimpleFold wants to read it as an s).
func TestLiteralHomographs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		letter string
		raw    string
		want   bool
	}{
		{"dotted capital I folds onto i", "i", "\u0130", true},
		{"ASCII I folds onto i", "i", "I", true},
		{"long s does not fold onto i", "i", "\u017f", false},
		{"kelvin sign folds onto k", "k", "\u212a", true},
		{"long s does not fold onto s", "s", "\u017f", false},
		{"ASCII S folds onto s", "s", "S", true},
		{"dotted capital I does not fold onto s", "s", "\u0130", false},
	} {
		re := regexp.MustCompile(`^` + Literal(tc.letter) + `$`)
		if got := re.MatchString(tc.raw); got != tc.want {
			t.Errorf("%s: Literal(%q) matching %+q = %v, want %v (strings.ToLower gives %+q)",
				tc.name, tc.letter, tc.raw, got, tc.want, strings.ToLower(tc.raw))
		}
	}
}

// TestLiteralFoldsUppercaseTokens pins Literal's documented uppercase-token
// rule: a token spelled in uppercase must render the same ToLower-faithful case
// class its lowercase spelling does, so a marker token added to a vocabulary
// list in uppercase keeps matching every real-world spelling. Every token in
// internal/release's current lists is already lowercase, so the ASCII-uppercase
// fold at the top of the loop is exercised by nothing: dropping it renders a
// case-SENSITIVE literal, and that whole marker family silently stops
// classifying.
func TestLiteralFoldsUppercaseTokens(t *testing.T) {
	for _, token := range []string{"CRF", "BDRip", "X265", "H.264", "KBPS"} {
		t.Run(token, func(t *testing.T) {
			re := regexp.MustCompile(Literal(token))
			for _, spelling := range []string{
				strings.ToLower(token), strings.ToUpper(token), token,
			} {
				if !re.MatchString(spelling) {
					t.Errorf("Literal(%q) does not match %q; an uppercase token must render the same case class its lowercase spelling does", token, spelling)
				}
			}
		})
	}
	// The Unicode class members ride the fold too: an uppercase token's i and
	// k classes must still admit the two runes strings.ToLower maps onto ASCII.
	re := regexp.MustCompile(Literal("KBPS"))
	if !re.MatchString("\u212abps") {
		t.Error("uppercase token KBPS does not admit U+212A KELVIN SIGN; the k class was lost with the fold")
	}
	re = regexp.MustCompile(Literal("BDRIP"))
	if !re.MatchString("bdr\u0130p") {
		t.Error("uppercase token BDRIP does not admit U+0130; the i class was lost with the fold")
	}
}

// TestNonWordEdgeHomographs is the boundary half of the same table: the two runes
// on which the release classifier and the indexer's tokenizer used to disagree
// about where a token ends. U+0130 and U+212A CONTINUE a word (they are letters
// under strings.ToLower) while U+017F ENDS one, which is the exact inverse of
// what a case-folded [^0-9a-z] class says about U+0130 and U+017F.
func TestNonWordEdgeHomographs(t *testing.T) {
	edge := regexp.MustCompile(`^` + NonWordEdge + `$`)
	for _, tc := range []struct {
		name string
		raw  string
		want bool
	}{
		{"dotted capital I continues a word", "\u0130", false},
		{"kelvin sign continues a word", "\u212a", false},
		{"long s ends a token", "\u017f", true},
		{"underscore ends a token", "_", true},
		{"dot ends a token", ".", true},
		{"hyphen ends a token", "-", true},
		{"space ends a token", " ", true},
		{"a digit continues a word", "7", false},
		{"an ASCII letter continues a word", "P", false},
	} {
		if got := edge.MatchString(tc.raw); got != tc.want {
			t.Errorf("%s: NonWordEdge matching %+q = %v, want %v", tc.name, tc.raw, got, tc.want)
		}
	}
}
