package indexer

import (
	"net/url"

	"github.com/cplieger/seadex-scout/internal/release"
)

// downloadURL resolves a grabbable .torrent download URL for a SeaDex torrent
// from its tracker and SeaDex source URL. It reports ok=false when the release
// cannot be turned into a download the arr can fetch: an unknown tracker, a
// source URL missing the expected id, or an AnimeBytes release with no passkey.
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
	scope := trackerScope(tracker)
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
		nyaa, _ := release.LookupTracker(release.TrackerNameNyaa)
		return nyaa.BaseURL + "/download/" + id + ".torrent", true
	case upstreamAB:
		if abPasskey == "" {
			return "", false
		}
		ab, _ := release.LookupTracker(release.TrackerNameAnimeBytes)
		return ab.BaseURL + "/torrent/" + id + "/download/" + url.PathEscape(abPasskey), true
	default:
		return "", false
	}
}
