package indexer

import "strings"

// The indexer matches a Prowlarr result back to a SeaDex release by a stable
// per-tracker key: the numeric id in the release's tracker page URL. SeaDex
// stores that URL (Nyaa /view/{id}, AnimeBytes ...torrentid={id}); Prowlarr's
// Torznab item carries the same page URL (in <comments>/<guid>), so the ids
// line up regardless of title or info-hash availability. The info hash is used
// as a secondary key when present.

// trackerKey builds the match key for a SeaDex torrent from its tracker name
// and stored URL, or "" when the tracker is unknown or the id is missing.
func trackerKey(tracker, sourceURL string) string {
	switch strings.ToLower(strings.TrimSpace(tracker)) {
	case "nyaa":
		if id := extractID(sourceURL, "/view/"); id != "" {
			return "nyaa:" + id
		}
	case "ab", "animebytes":
		if id := animeBytesID(sourceURL); id != "" {
			return "ab:" + id
		}
	}
	return ""
}

// trackerKeyFromURL builds the match key from an arbitrary release URL (a
// Prowlarr item's page URL) by detecting the tracker from the host, so it keys
// the same way trackerKey does for the SeaDex side.
func trackerKeyFromURL(raw string) string {
	lower := strings.ToLower(raw)
	switch {
	case strings.Contains(lower, "nyaa"):
		if id := extractID(raw, "/view/"); id != "" {
			return "nyaa:" + id
		}
	case strings.Contains(lower, "animebytes"):
		if id := animeBytesID(raw); id != "" {
			return "ab:" + id
		}
	}
	return ""
}

// animeBytesID extracts the AnimeBytes torrent id from either URL form: SeaDex
// stores the site form (`/torrents.php?...torrentid={id}`), while Prowlarr's
// Torznab items use the permalink form (`/torrent/{id}/group`) - the same id in
// both. AnimeBytes exposes no info hash in Torznab, so this id is the only key
// available for matching an AB release.
func animeBytesID(url string) string {
	if id := extractID(url, "/torrent/"); id != "" {
		return id
	}
	return extractID(url, "torrentid=")
}

// extractID returns the token in url immediately after needle, up to the next
// URL delimiter (?, #, /, &). It returns "" unless the token is a non-empty run
// of ASCII digits, so a malformed or unexpected URL never yields a bogus key
// (adopted from seadexerr's id extraction).
func extractID(url, needle string) string {
	_, after, found := strings.Cut(url, needle)
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
