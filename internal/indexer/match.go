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
// trackerKey, trackerKeyFromURL, and downloadURL.
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
// and stored URL, or "" when the tracker is unknown, the id is missing, or
// the URL does not belong to the named tracker. The host gate is the
// fail-closed half of the curation trust boundary: the tracker LABEL alone
// must never authorize an id extracted from a foreign URL (a malformed or
// compromised SeaDex record with Tracker "Nyaa" and
// https://evil.example/view/123 would otherwise mint nyaa:123 and admit the
// REAL Nyaa torrent 123 as curated), so the id counts only when the URL is
// the tracker's own (see trackerOwnURL). A gated-out torrent is simply not
// curated/journaled - the safe direction, surfaced by the journal's
// unresolvable counter.
func trackerKey(tracker, sourceURL string) string {
	scope := trackerScope(tracker)
	if scope == "" || !trackerOwnURL(scope, sourceURL) {
		return ""
	}
	if id := trackerID(scope, sourceURL); id != "" {
		return scope + ":" + id
	}
	return ""
}

// trackerOwnURL reports whether a SeaDex source URL belongs to the scope's
// own tracker: an absolute URL on the tracker's host (the shared
// release.Is*Host predicates, so homograph labels never pass), or - for
// AnimeBytes only - a true relative reference, SeaDex's documented AB shape
// (UsableURL resolves it against animebytes.tv, so a relative URL is an AB
// URL by construction). Anything else - a foreign host, an unparseable URL,
// an opaque non-hierarchical form - fails closed.
func trackerOwnURL(scope, sourceURL string) bool {
	u, err := url.Parse(sourceURL)
	if err != nil {
		return false
	}
	switch scope {
	case upstreamNyaa:
		return release.IsNyaaHost(u.Hostname())
	case upstreamAB:
		if release.IsAnimeBytesHost(u.Hostname()) {
			return true
		}
		return u.Scheme == "" && u.Host == "" && u.Opaque == ""
	}
	return false
}

// trackerKeyFromURL builds the match key from an arbitrary release URL (a
// Prowlarr item's page URL) by detecting the tracker from the host, so it keys
// the same way trackerKey does for the SeaDex side. Host classification rides
// the shared tracker predicate (release.LookupTrackerByHost via the Is*Host
// twins), so a non-ASCII homograph label or an empty-labeled host under a
// tracker domain never yields a curation key.
func trackerKeyFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	var scope string
	switch host := u.Hostname(); {
	case release.IsNyaaHost(host):
		scope = upstreamNyaa
	case release.IsAnimeBytesHost(host):
		scope = upstreamAB
	default:
		return ""
	}
	if id := trackerID(scope, raw); id != "" {
		return scope + ":" + id
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
	// A duplicated torrentid parameter is ambiguous: another consumer (a
	// PHP-style tracker, a proxy) may pick a different value than Go's
	// first-value Get, so an item could be authorized against one torrent
	// while referring to another (HTTP parameter pollution). Fail closed.
	values, ok := u.Query()["torrentid"]
	if !ok || len(values) != 1 {
		return ""
	}
	id := strings.TrimSpace(values[0])
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
