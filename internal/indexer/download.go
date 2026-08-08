package indexer

import (
	"net/url"

	"github.com/cplieger/seadex-scout/internal/tracker"
	"github.com/cplieger/urlform"
)

// downloadURLForScope resolves a grabbable .torrent download URL for one curated
// release, keyed on the indexer's own feed scope (upstreamNyaa / upstreamAB). A
// caller holding a seadex.Torrent's tracker name converts it with trackerScope
// first; both journal admission and the snapshot reader already hold the scope.
//
// It reports ok=false when the release cannot be turned into a download the arr
// can fetch: an unknown scope, a source URL that is not the tracker's own or is
// missing the expected id, an AnimeBytes release with no passkey, or a tracker
// table entry without a usable site base.
//
// Only the two trackers that carry ~all of SeaDex are resolved: public Nyaa
// (no credential) and private AnimeBytes (needs the operator's passkey). The
// AB download URL embeds the passkey, so it is a secret and callers must not
// log it.
//
// The tracker-ownership host gate (trackerOwnForm, the same fail-closed check
// journal admission applies via trackerKey) is enforced in downloadTarget
// BEFORE the shape-only id extraction (trackerID): a caller handing this a raw
// SeaDex URL cannot mint a download link for an arbitrary tracker torrent id
// smuggled in a foreign host's /view/{id} path. Inputs that already passed the
// gate (every journaled torrent) re-pass it unchanged; anything else fails
// closed with ok=false.
func downloadURLForScope(scope, sourceURL, abPasskey string) (string, bool) {
	base, id, ok := downloadTarget(scope, sourceURL)
	if !ok {
		return "", false
	}
	// The site hosts come from the canonical tracker table (resolved by
	// downloadTarget); only the download-endpoint path shapes are indexer
	// knowledge, so each arm appends its own suffix.
	switch scope {
	case upstreamNyaa:
		return base + "/download/" + id + ".torrent", true
	case upstreamAB:
		// unusableABPasskey is this package's ONE home for "can this passkey
		// build a grabbable AnimeBytes link" (server.go, over
		// internal/secretref), so every site that decides on the passkey reads
		// it rather than spelling out its own emptiness test: minting a
		// non-credential into the link would report the release as fully
		// resolved while every arr grab failed at the tracker. config's
		// validateABPasskey is the format gate that keeps a malformed value from
		// reaching any of them on the daemon path; this is the same fail-closed
		// answer for any other construction, and it keeps the writer's
		// ab_releases_skipped count and the reader's AB-feed clear
		// (rebuildABDownloadURLs) in agreement.
		if unusableABPasskey(abPasskey) {
			return "", false
		}
		return base + "/torrent/" + id + "/download/" + url.PathEscape(abPasskey), true
	default:
		return "", false
	}
}

// downloadTarget applies every PASSKEY-INDEPENDENT gate a download link must
// pass - the tracker-ownership host gate, the shape-only id extraction, and
// the canonical tracker table lookup (scopeTracker) with its fail-closed
// found/BaseURL validation - and returns the site base plus the extracted id.
// It is the shared front half of downloadURLForScope and resolvableForScope, so
// the two can never disagree about which records are structurally sound.
func downloadTarget(scope, sourceURL string) (base, id string, ok bool) {
	if scope == "" {
		return "", "", false
	}
	// Classify once and extract the id from that same reading, so a URL the
	// ownership gate vouches can never fail id extraction because the extractor
	// re-parsed the original spelling (h-f8, see trackerOwnForm). The vouched
	// form is normalized to its canonical absolute spelling
	// (tracker.CanonicalSourceURL) - the SAME normalization trackerKey applies,
	// which is why that rule lives in the tracker package instead of twice here.
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

// resolvableForScope reports whether sourceURL is a structurally sound
// download source for scope APART from the AnimeBytes passkey: it applies
// every gate downloadURLForScope applies except the passkey itself.
//
// It exists for the one case where a release is sound but its link is not yet
// derivable - an AnimeBytes torrent while no usable indexer.ab_passkey is
// configured (absent, or a value that is not a credential at all) - which
// journal admission refuses WITHOUT recording a publication, so it journals as
// new once the passkey arrives (see journal.go's
// journalLink). It deliberately does NOT report a release that is unresolvable
// for an upstream DATA reason (a foreign host, an id-less URL): such a record
// must stay refused so a corrected one still journals as new.
func resolvableForScope(scope, sourceURL string) bool {
	_, _, ok := downloadTarget(scope, sourceURL)
	return ok
}
