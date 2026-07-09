package seadex

import "strings"

// trackerBaseURLs maps a lowercase tracker name to its site base URL, used to
// turn the relative torrent paths private trackers return (for example
// AnimeBytes "/torrents.php?id=..") into a usable link.
var trackerBaseURLs = map[string]string{ //nolint:gosec // G101 false positive: tracker site names/URLs, not credentials
	"nyaa":           "https://nyaa.si",
	"animetosho":     "https://animetosho.org",
	"ab":             "https://animebytes.tv",
	"animebytes":     "https://animebytes.tv",
	"beyondhd":       "https://beyond-hd.me",
	"bhd":            "https://beyond-hd.me",
	"hdbits":         "https://hdbits.org",
	"passthepopcorn": "https://passthepopcorn.me",
	"ptp":            "https://passthepopcorn.me",
	"broadcasthenet": "https://broadcasthe.net",
	"btn":            "https://broadcasthe.net",
	"blutopia":       "https://blutopia.cc",
	"aither":         "https://aither.cc",
}

// UsableURL returns a link a human can follow for the torrent. An absolute URL
// is returned unchanged; a relative path (as private trackers return) is
// prefixed with the tracker's base URL when known, so a finding or report never
// emits a broken bare path. An unknown tracker's relative path is returned
// as-is.
func (t *Torrent) UsableURL() string {
	u := strings.TrimSpace(t.URL)
	if u == "" {
		return ""
	}
	if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
		return u
	}
	base, ok := trackerBaseURLs[strings.ToLower(strings.TrimSpace(t.Tracker))]
	if !ok {
		return u
	}
	if !strings.HasPrefix(u, "/") {
		u = "/" + u
	}
	return base + u
}
