// Package nametoken is the one home for the two LEXICAL rules every
// release-name parser in this app reads: which runes are WORD runes (and so
// where a token ends), and which raw spellings a case-insensitive marker letter
// matches. It owns no policy - not what a marker MEANS, not which delimiters a
// particular marker may sit between - only the alphabet those policies are
// written against.
package nametoken

import (
	"regexp"
	"strings"
)

// wordClass is the release-name word alphabet as a regexp character-class body:
// the ASCII alphanumerics plus U+0130 (LATIN CAPITAL LETTER I WITH DOT ABOVE)
// and U+212A (KELVIN SIGN) - exactly the runes strings.ToLower folds onto an
// ASCII alphanumeric, and so exactly the runes Literal can match for some
// letter.
const wordClass = `A-Za-z0-9\x{0130}\x{212A}`

// NonWordEdge matches exactly one rune that ENDS a token: any rune outside the
// release-name word alphabet. It is the shared replacement for a \b assertion
// (which would treat "_" as part of the word) and for a case-folded [^0-9a-z]
// class (which reads U+017F as a letter and U+0130 as a delimiter, the divergence
// the package doc describes).
const NonWordEdge = `[^` + wordClass + `]`

// Literal renders a marker token as a regexp fragment matching exactly the raw
// spellings whose strings.ToLower image equals the token's lowercase form: each
// ASCII letter becomes an explicit case class - with U+0130 added to the i class
// and U+212A to the k class, the only non-ASCII runes strings.ToLower maps onto
// ASCII - digits match themselves, and anything else is quoted literally.
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
