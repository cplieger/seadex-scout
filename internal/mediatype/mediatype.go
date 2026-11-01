// Package mediatype owns the AniList media-format token vocabulary: the
// canonical comparison form, the set of tokens that name a real format, and
// which of them is a film. Only the unmapped fallback and the feed's memo
// typing read a format; a mapped entry routes by its links.
package mediatype

import "strings"

// The AniList MediaFormat enum as it applies to anime. AniList's
// MANGA/NOVEL/ONE_SHOT members cannot appear on a SeaDex entry and are
// deliberately absent.
const (
	movie   = "MOVIE"
	tv      = "TV"
	tvShort = "TV_SHORT"
	special = "SPECIAL"
	ova     = "OVA"
	ona     = "ONA"
	music   = "MUSIC"
)

// known is the accepted token set, keyed in canonical form.
var known = map[string]struct{}{
	tv: {}, tvShort: {}, movie: {}, special: {},
	ova: {}, ona: {}, music: {},
}

// Normalize canonicalizes a raw format token to the upper-cased, trimmed form
// every predicate here compares in.
func Normalize(s string) string { return strings.ToUpper(strings.TrimSpace(s)) }

// Known reports whether s names a real media type. It compares in canonical
// form, so a caller holding a RAW wire token passes it through Normalize first.
func Known(s string) bool {
	_, ok := known[s]
	return ok
}

// IsMovie reports whether a canonical AniList format token is a film.
func IsMovie(s string) bool { return s == movie }
