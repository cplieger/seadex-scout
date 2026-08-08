// Package titlekey owns the normalized-title key algorithm shared by the
// match index and the AniList payload gate. The key domain is lowercase
// [a-z0-9] only: each title is Unicode-lowercased first, then every rune
// outside ASCII [a-z0-9] is stripped, so two titles differing only in
// decoration collide as intended. Unicode capitals whose lowercase mapping
// is ASCII therefore contribute to the key rather than being stripped. It is
// deliberately conservative (no transliteration or fuzzy edits). A
// dependency-free leaf so all three consumers share one implementation
// instead of mirroring the character set in lockstep: match indexes and looks
// up by key, anilist pre-rejects payloads whose every title normalizes to an
// empty key, and the indexer's title harvest tests a candidate release name
// for CONTAINMENT of a show's key. The containment consumer is the one
// sensitive to the stripped separators (a key of ANY length occurs inside
// ordinary release metadata - "x" inside "Remux", "gate" inside "Propagate" -
// which is why ContainsKey matches a run of the candidate's own tokens exactly
// rather than testing a normalized substring), so a change to the character set
// must be reviewed against it too.
package titlekey

import (
	"regexp"
	"strings"
)

// reStrip removes every character that is not a lowercase letter or digit.
var reStrip = regexp.MustCompile(`[^a-z0-9]+`)

// Normalize Unicode-lowercases a title and then strips every rune outside
// ASCII [a-z0-9], yielding the match key. An empty result means the title
// cannot key a match: no ASCII letter or digit remains after lowercasing
// and filtering.
func Normalize(s string) string {
	return reStrip.ReplaceAllString(strings.ToLower(s), "")
}

// ContainsKey reports whether candidate carries want - a key produced by
// Normalize - as its own vocabulary: an EXACT match against a run of the
// candidate's own alphanumeric tokens, split on the SAME character class
// Normalize strips, which is why this comparison lives beside it.
//
// Normalize deliberately drops every separator, so a plain normalized
// substring test has no token-boundary evidence at all, at ANY key length: the
// "x" of a one-character title is satisfied by "Remux" or "x265", and a real
// four-to-six-character title is satisfied just as blindly ("gate" inside
// "Propagate", "bleach" inside "Unbleached", "zero" inside "ReZero"). Requiring
// the key to equal a run of whole tokens restores the boundary evidence for
// every length while still ignoring the decoration a release name carries: a
// multi-token key matches across the separators inside it, because Normalize
// stripped exactly the characters FieldsFunc splits on.
func ContainsKey(candidate, want string) bool {
	tokens := strings.FieldsFunc(strings.ToLower(candidate), func(r rune) bool {
		return (r < '0' || r > '9') && (r < 'a' || r > 'z')
	})
	var joined strings.Builder
	for start := range tokens {
		joined.Reset()
		for _, token := range tokens[start:] {
			joined.WriteString(token)
			if joined.Len() >= len(want) {
				if joined.String() == want {
					return true
				}
				break
			}
		}
	}
	return false
}
