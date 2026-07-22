// Package titlekey owns the normalized-title key algorithm shared by the
// match index and the AniList payload gate. The key domain is lowercase
// [a-z0-9] only: each title is Unicode-lowercased first, then every rune
// outside ASCII [a-z0-9] is stripped, so two titles differing only in
// decoration collide as intended. Unicode capitals whose lowercase mapping
// is ASCII therefore contribute to the key rather than being stripped. It is
// deliberately conservative (no transliteration or fuzzy edits). A dependency-free leaf so both consumers (match, which indexes and
// looks up by key, and anilist, which pre-rejects payloads whose every title
// normalizes to an empty key) share one implementation instead of mirroring
// the character set in lockstep.
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
