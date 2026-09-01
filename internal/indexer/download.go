package indexer

import (
	"net/url"

	"github.com/cplieger/seadex-scout/internal/tracker"
	"github.com/cplieger/urlform"
)

// downloadURLForScope resolves a grabbable .torrent download URL for one curated
// release, keyed on the indexer's own feed scope (upstreamNyaa / upstreamAB). A
// caller holding a seadex.Torrent's tracker name converts it with trackerScope
// first. ok is false when the release cannot become a download the arr can
// fetch: an unknown scope, a source URL not owned by the tracker or missing its
// id, an AnimeBytes release with no passkey, or a tracker table entry with no
// usable site base. The AB URL embeds the passkey, so callers must not log it.
func downloadURLForScope(scope, sourceURL, abPasskey string) (string, bool) {
	base, id, ok := downloadTarget(scope, sourceURL)
	if !ok {
		return "", false
	}
	// The site hosts come from the canonical tracker table; only the
	// download-endpoint path shapes are indexer knowledge.
	switch scope {
	case upstreamNyaa:
		return base + "/download/" + id + ".torrent", true
	case upstreamAB:
		// unusableABPasskey is this package's one home for "can this passkey build a
		// grabbable AnimeBytes link" - minting a non-credential would report the
		// release as resolved while every arr grab failed at the tracker.
		if unusableABPasskey(abPasskey) {
			return "", false
		}
		return base + "/torrent/" + id + "/download/" + url.PathEscape(abPasskey), true
	default:
		return "", false
	}
}

// downloadTarget applies every PASSKEY-INDEPENDENT gate a download link must
// pass - the tracker-ownership host gate, the shape-only id extraction, and the
// canonical tracker table lookup with its fail-closed validation - and returns
// the site base plus the extracted id. Shared front half of downloadURLForScope
// and resolvableForScope, so the two cannot disagree about soundness.
func downloadTarget(scope, sourceURL string) (base, id string, ok bool) {
	if scope == "" {
		return "", "", false
	}
	// Classify once and extract the id from that same reading, so a URL the
	// ownership gate vouches can never fail id extraction because the extractor
	// re-parsed the original spelling. The vouched form is normalized to its
	// canonical absolute spelling - the SAME normalization trackerKey applies.
	f := urlform.Classify(sourceURL)
	if !trackerOwnForm(scope, &f) {
		return "", "", false
	}
	if id = trackerID(scope, tracker.CanonicalSourceURL(&f)); id == "" {
		return "", "", false
	}
	// The scope->tracker step is match.go's scopeTracker (the one home of that
	// correspondence); only the base-URL requirement is this caller's own.
	tr, found := scopeTracker(scope)
	if !found || tr.BaseURL == "" {
		return "", "", false
	}
	return tr.BaseURL, id, true
}

// resolvableForScope reports whether sourceURL is a structurally sound download
// source for scope APART from the AnimeBytes passkey. It exists for the one case
// where a release is sound but its link is not yet derivable - an AnimeBytes
// torrent while no usable passkey is configured - which journal admission refuses
// WITHOUT recording a publication, so it journals as new once the passkey
// arrives. It deliberately does NOT report a release unresolvable for an upstream
// DATA reason: such a record must stay refused so a corrected one journals as new.
func resolvableForScope(scope, sourceURL string) bool {
	_, _, ok := downloadTarget(scope, sourceURL)
	return ok
}
