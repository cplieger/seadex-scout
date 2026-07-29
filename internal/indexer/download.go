package indexer

import (
	"net/url"

	"github.com/cplieger/seadex-scout/internal/tracker"
	"github.com/cplieger/urlform"
)

// downloadURL resolves a grabbable .torrent download URL for a SeaDex torrent
// from its tracker and SeaDex source URL. It reports ok=false when the release
// cannot be turned into a download the arr can fetch: an unknown tracker, a
// source URL missing the expected id, an AnimeBytes release with no passkey,
// or a tracker table entry without a usable site base.
//
// Only the two trackers that carry ~all of SeaDex are resolved: public Nyaa
// (no credential) and private AnimeBytes (needs the operator's passkey). The
// AB download URL embeds the passkey, so it is a secret and callers must not
// log it.
//
// The tracker-ownership host gate (trackerOwnForm, the same fail-closed check
// journal admission applies via trackerKey) is enforced HERE, before the
// shape-only id extraction (trackerID): a caller handing this a raw SeaDex
// URL cannot mint a download link for an arbitrary tracker torrent id
// smuggled in a foreign host's /view/{id} path. Inputs that already passed
// the gate (every journaled torrent) re-pass it unchanged; anything else
// fails closed with ok=false.
func downloadURL(trackerName, sourceURL, abPasskey string) (string, bool) {
	return downloadURLForScope(trackerScope(trackerName), sourceURL, abPasskey)
}

// downloadURLForScope is downloadURL keyed on the indexer's own feed scope
// (upstreamNyaa / upstreamAB), for callers that already know it - the
// snapshot reader rebuilds each per-scope feed, so it never needs to detour
// through the SeaDex tracker-name vocabulary. downloadURL remains the bridge
// for callers holding a seadex.Torrent's tracker name.
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
		if abPasskey == "" {
			return "", false
		}
		return base + "/torrent/" + id + "/download/" + url.PathEscape(abPasskey), true
	default:
		return "", false
	}
}

// downloadTarget applies every PASSKEY-INDEPENDENT gate a download link must
// pass - the tracker-ownership host gate, the shape-only id extraction, and
// the canonical tracker table lookup with its fail-closed found/BaseURL
// validation - and returns the site base plus the extracted id. It is the
// shared front half of downloadURLForScope and resolvableForScope, so the two
// can never disagree about which records are structurally sound.
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
	var trackerName string
	switch scope {
	case upstreamNyaa:
		trackerName = tracker.NameNyaa
	case upstreamAB:
		trackerName = tracker.NameAnimeBytes
	default:
		return "", "", false
	}
	tr, found := tracker.Lookup(trackerName)
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
// derivable - an AnimeBytes torrent while indexer.ab_passkey is unset - which
// the journal admits GUID-only rather than refusing (see journal.go's
// journalLink). It deliberately does NOT report a release that is unresolvable
// for an upstream DATA reason (a foreign host, an id-less URL): such a record
// must stay refused so a corrected one still journals as new.
func resolvableForScope(scope, sourceURL string) bool {
	_, _, ok := downloadTarget(scope, sourceURL)
	return ok
}
