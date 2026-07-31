// Package nametoken is the one home for the two LEXICAL rules every
// release-name parser in this app reads: which runes are WORD runes (and so
// where a token ends), and which raw spellings a case-insensitive marker letter
// matches. It owns no policy - not what a marker MEANS, not which delimiters a
// particular marker may sit between - only the alphabet those policies are
// written against.
//
// Three packages parse release names, each owning a different policy, and all
// three used to carry their own copy of these two rules:
//
//   - internal/release classifies a name and the entry notes into a release
//     fingerprint (resolution, codec, remux-vs-encode). It held the rules as
//     evidenceWordClass/nonWordEdge plus a lowerLiteralPattern renderer.
//   - internal/indexer tokenizes the season/episode markers the synthesized RSS
//     title is built from. It held them as a global (?i) plus its own
//     [^0-9a-z] terminator class.
//   - internal/payload decides which files may identify a release. It held the
//     case classes hand-spelled ([Ss][Aa][Mm]...) beside ASCII-alnum edges.
//
// The first two had already DIVERGED, on exactly the runes the rule exists to
// pin. Go's regexp (?i) folds by unicode.SimpleFold, which disagrees with
// strings.ToLower in both directions: (?i)s also matches U+017F (ſ LATIN SMALL
// LETTER LONG S), which ToLower never folds onto s, while (?i)i misses U+0130
// (İ LATIN CAPITAL LETTER I WITH DOT ABOVE), which ToLower does fold onto i. So
// one file name read two ways inside one binary: "ſ01E01" was a
// season-1-episode-1 token to the indexer and no marker at all to the
// classifier, and U+0130 ENDED a token for the indexer while CONTINUING one for
// the classifier.
//
// The strings.ToLower reading is the one that won, for three reasons. It is
// what the rest of the app already means by case-insensitive - group names,
// hosts, extensions and resolutions are all compared through strings.ToLower,
// and internal/tracker refuses a host on exactly this basis (a ToLower-foldable
// homograph must not launder into an ASCII tracker host). It is the reading that
// was chosen and documented, whereas (?i) was the default nobody picked. And it
// is the only one that is INTERNALLY coherent: the word alphabet below is
// exactly the strings.ToLower preimage of the ASCII alphanumerics (pinned
// exhaustively over the whole rune space by nametoken_test.go), so "is this rune
// part of a word" and "does this rune fold onto that letter" cannot contradict
// each other - which a global (?i) beside a hand-written class could never
// guarantee.
//
// Why one home rather than exporting from internal/release, the consumer whose
// rule won: internal/payload sits BELOW the classifier (internal/classify
// imports both, and release imports payload no more than payload imports
// release), so a payload -> release edge would invert that layering for a
// character class; and internal/indexer reaches release only transitively,
// through internal/classify's adapter, which is the seam that keeps the feed's
// title synthesis off the classification engine's surface. A pure leaf all three
// may import is the only home that keeps both properties - the same shape and
// the same reasoning as internal/credname and internal/secretref, which own
// neighbouring single-rule questions for the same reason.
//
// What is deliberately NOT here: the SEPARATOR sets. Which delimiters may sit
// INSIDE a marker is per-marker policy, not vocabulary - internal/release
// accepts the full scene set [\s._-] between the halves of its own markers,
// while internal/indexer's absolute-episode form accepts only [\s_] around its
// dash, and both are deliberate. Boundaries say where a token ENDS; separators
// say what a policy is willing to read as one token. Only the former is shared.
package nametoken

import (
	"regexp"
	"strings"
)

// wordClass is the release-name word alphabet as a regexp character-class body:
// the ASCII alphanumerics plus U+0130 (LATIN CAPITAL LETTER I WITH DOT ABOVE)
// and U+212A (KELVIN SIGN) - exactly the runes strings.ToLower folds onto an
// ASCII alphanumeric, and so exactly the runes Literal can match for some
// letter. Underscore is deliberately OUT: Go regexp counts it as a word
// character (\b would), but scene naming uses "_" everywhere a space would sit,
// so every parser here reads it as a delimiter.
//
// It stays unexported: a consumer needs the EDGE, and a raw class body invites
// a fourth hand-built class beside it.
const wordClass = `A-Za-z0-9\x{0130}\x{212A}`

// NonWordEdge matches exactly one rune that ENDS a token: any rune outside the
// release-name word alphabet. It is the shared replacement for a \b assertion
// (which would treat "_" as part of the word) and for a case-folded [^0-9a-z]
// class (which reads U+017F as a letter and U+0130 as a delimiter, the divergence
// the package doc describes). A caller pairs it with ^ or $ for the
// start/end-of-string cases, which it cannot express itself:
//
//	`(?:^|` + nametoken.NonWordEdge + `)` + nametoken.Literal("crf") + `\d+`
//
// It consumes the edge rune, so a caller that must return the token's own text
// reads a capture group rather than the whole match.
const NonWordEdge = `[^` + wordClass + `]`

// Literal renders a marker token as a regexp fragment matching exactly the raw
// spellings whose strings.ToLower image equals the token's lowercase form: each
// ASCII letter becomes an explicit case class - with U+0130 added to the i class
// and U+212A to the k class, the only non-ASCII runes strings.ToLower maps onto
// ASCII - digits match themselves, and anything else is quoted literally. An
// ASCII uppercase letter in token is folded to lowercase before rendering, so a
// token spelled "CRF" renders the same class "crf" does instead of a
// case-SENSITIVE literal.
//
// token is expected to be ASCII, which the marker vocabulary of every consumer
// is. A non-ASCII rune is quoted literally and matches only itself, so passing
// one silently narrows the fragment rather than folding it.
func Literal(token string) string {
	var b strings.Builder
	for _, r := range token {
		if r >= 'A' && r <= 'Z' {
			r += 'a' - 'A'
		}
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteByte('[')
			b.WriteRune(r)
			b.WriteRune(r - 'a' + 'A')
			switch r {
			case 'i':
				b.WriteString(`\x{0130}`)
			case 'k':
				b.WriteString(`\x{212A}`)
			}
			b.WriteByte(']')
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	return b.String()
}

// Alternation joins the Literal renderings of tokens into one regexp
// alternation. It carries no grouping, so a caller that needs the alternation
// bounded wraps it in its own (?:...) - the same way it would a Literal.
func Alternation(tokens []string) string {
	parts := make([]string, len(tokens))
	for i, t := range tokens {
		parts[i] = Literal(t)
	}
	return strings.Join(parts, "|")
}
