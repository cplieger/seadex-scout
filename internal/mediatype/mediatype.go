// Package mediatype owns the AniList/Fribb media-type token vocabulary: the
// canonical comparison form, the set of tokens that name a real media type, and
// the movie/special classification over them.
//
// It is a dependency-free leaf so both halves of the type contract share ONE
// implementation: internal/mapping classifies a Fribb/override record's `type`
// (Record.IsMovie/IsSpecial) in this canonical form, and internal/anilist
// ACCEPTS a wire MediaFormat token before that token ever becomes a
// Record.Type. The two halves are coupled by a real invariant, not coincidence:
// the accepted wire token is fed verbatim into Record.Type and then classified
// here, so every token anilist admits must be classified in the same canonical
// space mapping produces. Held as two independent copies (the state before
// l-f87), a token added to only one half - or a change to the canonical form,
// e.g. underscore folding - desynchronized them with no compile error and no
// test spanning both.
//
// This is the same shape, for the same reason, as internal/titlekey: a leaf
// owning one vocabulary that a wire client and a domain package must agree on.
// The runner-up was having anilist import mapping for the canonicalizer, which
// costs that client its no-domain-dependency posture (it imports only appinfo,
// titlekey, and now this leaf).
package mediatype

import "strings"

// The AniList MediaFormat enum as it applies to anime, which is also Fribb's
// `type` vocabulary. AniList's MANGA/NOVEL/ONE_SHOT members cannot appear on a
// SeaDex entry and are deliberately absent.
const (
	Movie   = "MOVIE"
	TV      = "TV"
	TVShort = "TV_SHORT"
	Special = "SPECIAL"
	OVA     = "OVA"
	ONA     = "ONA"
	Music   = "MUSIC"
)

// known is the accepted token set, keyed in canonical form.
var known = map[string]struct{}{
	TV: {}, TVShort: {}, Movie: {}, Special: {},
	OVA: {}, ONA: {}, Music: {},
}

// Normalize canonicalizes a raw type/format token to the upper-cased, trimmed
// form every predicate here compares in - the form a mapping Record.Type is
// stored in, so an exact-key comparison against a stored type is sound.
func Normalize(s string) string { return strings.ToUpper(strings.TrimSpace(s)) }

// Known reports whether s names a real media type. It compares in canonical
// form, so a caller holding a RAW wire token passes it through Normalize first.
func Known(s string) bool {
	_, ok := known[s]
	return ok
}

// IsMovie reports whether a canonical token routes to Radarr (TMDB movie /
// IMDb). Every other token - including an empty or unrecognized one - routes to
// Sonarr (TVDB).
func IsMovie(s string) bool { return s == Movie }

// IsSpecial reports whether a canonical token is an OVA/ONA/special/music video
// rather than a standard TV season or movie, so it can be excluded when the
// operator turns specials off. An empty or unrecognized token is not special.
func IsSpecial(s string) bool {
	switch s {
	case OVA, ONA, Special, Music:
		return true
	default:
		return false
	}
}
