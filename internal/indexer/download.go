package indexer

import (
	"net/url"

	"github.com/cplieger/seadex-scout/internal/release"
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
// The tracker-ownership host gate (trackerOwnURL, the same fail-closed check
// journal admission applies via trackerKey) is enforced HERE, before the
// shape-only id extraction (trackerID): a caller handing this a raw SeaDex
// URL cannot mint a download link for an arbitrary tracker torrent id
// smuggled in a foreign host's /view/{id} path. Inputs that already passed
// the gate (every journaled torrent) re-pass it unchanged; anything else
// fails closed with ok=false.
func downloadURL(tracker, sourceURL, abPasskey string) (string, bool) {
	return downloadURLForScope(trackerScope(tracker), sourceURL, abPasskey)
}

// downloadURLForScope is downloadURL keyed on the indexer's own feed scope
// (upstreamNyaa / upstreamAB), for callers that already know it - the
// snapshot reader rebuilds each per-scope feed, so it never needs to detour
// through the SeaDex tracker-name vocabulary. downloadURL remains the bridge
// for callers holding a seadex.Torrent's tracker name.
func downloadURLForScope(scope, sourceURL, abPasskey string) (string, bool) {
	if scope == "" || !trackerOwnURL(scope, sourceURL) {
		return "", false
	}
	id := trackerID(scope, sourceURL)
	if id == "" {
		return "", false
	}
	// The site hosts come from the canonical release tracker table; only the
	// download-endpoint path shapes are indexer knowledge.
	switch scope {
	case upstreamNyaa:
		nyaa, found := release.LookupTracker(release.TrackerNameNyaa)
		if !found || nyaa.BaseURL == "" {
			return "", false
		}
		return nyaa.BaseURL + "/download/" + id + ".torrent", true
	case upstreamAB:
		if abPasskey == "" {
			return "", false
		}
		ab, found := release.LookupTracker(release.TrackerNameAnimeBytes)
		if !found || ab.BaseURL == "" {
			return "", false
		}
		return ab.BaseURL + "/torrent/" + id + "/download/" + url.PathEscape(abPasskey), true
	default:
		return "", false
	}
}
