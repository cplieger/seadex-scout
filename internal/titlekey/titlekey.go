// Package titlekey owns the normalized-title key algorithm shared by the
// match index and the AniList payload gate. The key domain is lowercase
// [a-z0-9] only: punctuation, spacing, separators, and non-ASCII characters
// are stripped so two titles differing only in decoration collide as
// intended. It is deliberately conservative (no transliteration or fuzzy
// edits). A dependency-free leaf so both consumers (match, which indexes and
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

// Normalize lowercases a title and strips all non-alphanumeric characters,
// yielding the match key. An empty result means the title cannot key a match
// (punctuation-only, or entirely non-ASCII).
func Normalize(s string) string {
	return reStrip.ReplaceAllString(strings.ToLower(s), "")
}
