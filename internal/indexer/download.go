package indexer

// downloadURL resolves a grabbable .torrent download URL for a SeaDex torrent
// from its tracker and SeaDex source URL. It reports ok=false when the release
// cannot be turned into a download the arr can fetch: an unknown tracker, a
// source URL missing the expected id, or an AnimeBytes release with no passkey.
//
// Only the two trackers that carry ~all of SeaDex are resolved: public Nyaa
// (no credential) and private AnimeBytes (needs the operator's passkey). The
// AB download URL embeds the passkey, so it is a secret and callers must not
// log it.
func downloadURL(tracker, sourceURL, abPasskey string) (string, bool) {
	switch trackerScope(tracker) {
	case upstreamNyaa:
		id := extractID(sourceURL, "/view/")
		if id == "" {
			return "", false
		}
		return "https://nyaa.si/download/" + id + ".torrent", true
	case upstreamAB:
		if abPasskey == "" {
			return "", false
		}
		id := animeBytesID(sourceURL)
		if id == "" {
			return "", false
		}
		return "https://animebytes.tv/torrent/" + id + "/download/" + abPasskey, true
	default:
		return "", false
	}
}
