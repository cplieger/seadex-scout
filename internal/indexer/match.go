package indexer

import (
	"net/url"
	"strings"

	"github.com/cplieger/seadex-scout/internal/release"
)

// The indexer matches a Prowlarr result back to a SeaDex release by a stable
// per-tracker key: the numeric id in the release's tracker page URL. SeaDex
// stores that URL (Nyaa /view/{id}, AnimeBytes ...torrentid={id}); Prowlarr's
// Torznab item carries the same page URL (in <comments>/<guid>), so the ids
// line up regardless of title or info-hash availability. The info hash is used
// as a secondary key when present.

// trackerScope classifies a tracker name (as SeaDex spells it, "Nyaa" or "AB")
// into the feed scope it maps to: upstreamNyaa, upstreamAB, or "" for any other
// tracker (a negligible SeaDex tail). The tracker vocabulary (which aliases
// denote which tracker) is owned by the canonical release tracker table
// (release.LookupTracker), so id extraction, key building, download-link
// building, feed routing, and the alert/report path all agree on what counts
// as AnimeBytes.
func trackerScope(tracker string) string {
	t, ok := release.LookupTracker(tracker)
	if !ok {
		return ""
	}
	switch t.Name {
	case release.TrackerNameNyaa:
		return upstreamNyaa
	case release.TrackerNameAnimeBytes:
		return upstreamAB
	}
	return ""
}

// trackerID extracts the tracker's numeric torrent id from a SeaDex source
// URL for a scope: Nyaa's /view/{id}, AnimeBytes' torrentid=/permalink forms.
// It is the single home of the scope->id-extraction pairing, shared by
// trackerKey and downloadURL.
func trackerID(scope, sourceURL string) string {
	switch scope {
	case upstreamNyaa:
		return nyaaID(sourceURL)
	case upstreamAB:
		return animeBytesID(sourceURL)
	}
	return ""
}

// nyaaID extracts the Nyaa torrent id from a URL's /view/{id} path component.
// Parsing first and scanning only the path keeps an id embedded in a query
// value or fragment (e.g. ?next=/view/123) from being read as the torrent id,
// so a curation key is only ever derived from the URL component that actually
// identifies the torrent.
func nyaaID(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return extractID(u.EscapedPath(), "/view/")
}

// trackerKey builds the match key for a SeaDex torrent from its tracker name
// and stored URL, or "" when the tracker is unknown or the id is missing.
func trackerKey(tracker, sourceURL string) string {
	scope := trackerScope(tracker)
	if id := trackerID(scope, sourceURL); id != "" {
		return scope + ":" + id
	}
	return ""
}

// trackerKeyFromURL builds the match key from an arbitrary release URL (a
// Prowlarr item's page URL) by detecting the tracker from the host, so it keys
// the same way trackerKey does for the SeaDex side.
func trackerKeyFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case release.IsNyaaHost(host):
		if id := nyaaID(raw); id != "" {
			return upstreamNyaa + ":" + id
		}
	case release.IsAnimeBytesHost(host):
		if id := animeBytesID(raw); id != "" {
			return upstreamAB + ":" + id
		}
	}
	return ""
}

// animeBytesID extracts the AnimeBytes torrent id from either URL form: SeaDex
// stores the site form (`/torrents.php?...torrentid={id}`), while Prowlarr's
// Torznab items use the permalink form (`/torrent/{id}/group`) - the same id in
// both. AnimeBytes exposes no info hash in Torznab, so this id is the only key
// available for matching an AB release. The permalink id is read only from the
// URL path and the site-form id only from the torrentid query parameter, so an
// id smuggled inside an unrelated query value (e.g. ?next=/torrent/123/group)
// never yields a key.
func animeBytesID(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	if id := extractID(u.EscapedPath(), "/torrent/"); id != "" {
		return id
	}
	id := strings.TrimSpace(u.Query().Get("torrentid"))
	if !isAllDigits(id) {
		return ""
	}
	return id
}

// extractID returns the token in rawURL immediately after needle, up to the
// next URL delimiter (?, #, /, &). It returns "" unless the token is a
// non-empty run of ASCII digits, so a malformed or unexpected URL never yields
// a bogus key (adopted from seadexerr's id extraction).
func extractID(rawURL, needle string) string {
	_, after, found := strings.Cut(rawURL, needle)
	if !found {
		return ""
	}
	if cut := strings.IndexAny(after, "?#/&"); cut >= 0 {
		after = after[:cut]
	}
	if after == "" || !isAllDigits(after) {
		return ""
	}
	return after
}

// isAllDigits reports whether s is a non-empty run of ASCII digits.
func isAllDigits(s string) bool {
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}
