package seadex

import (
	"net/url"
	"strings"

	"github.com/cplieger/seadex-scout/internal/release"
)

// UsableURL returns a link a human can follow for the torrent. An absolute URL
// is returned unchanged; a relative path (as private trackers return) is
// prefixed with the tracker's base URL from the canonical release tracker
// table, so a finding or report never emits a broken bare path. An unknown
// tracker's relative path drops to "" like every other unusable form (no base
// URL exists to make it followable).
func (t *Torrent) UsableURL() string {
	u := strings.TrimSpace(t.URL)
	if u == "" {
		return ""
	}
	// Parse rather than prefix-match so a malformed absolute value (a bare
	// "https://", an invalid escape, whitespace in the host, a backslash
	// authority) becomes the already-supported empty-URL case - dropping only
	// the unusable link - instead of a published link a human cannot follow.
	// Backslashes are rejected outright: browsers treat "\\host" as an
	// authority even though url.Parse does not.
	parsed, err := url.Parse(u)
	if err != nil || strings.Contains(u, `\`) {
		return ""
	}
	if parsed.IsAbs() {
		// An absolute URL is returned unchanged, but only in http(s) with a real
		// host; any other scheme (javascript:, data:, file:) is untrusted
		// upstream data with no legitimate use in a clickable link.
		if (!strings.EqualFold(parsed.Scheme, "http") &&
			!strings.EqualFold(parsed.Scheme, "https")) || parsed.Host == "" {
			return ""
		}
		return u
	}
	// A protocol-relative URL ("//host/path") carries no scheme, yet a renderer
	// resolves it against the ambient scheme and navigates off-site; it has no
	// legitimate use as a tracker link. A colon-prefixed value that did not
	// parse as a scheme (e.g. "1a:b") is equally unusable as a relative path.
	// A scheme-less relative path parses host-free and still passes through
	// (tracker-relative AB paths are unaffected).
	if parsed.Host != "" || strings.HasPrefix(u, "//") {
		return ""
	}
	if i := strings.Index(u, ":"); i >= 0 && !strings.Contains(u[:i], "/") {
		return ""
	}
	tr, ok := release.LookupTracker(t.Tracker)
	if !ok || tr.BaseURL == "" {
		return ""
	}
	if !strings.HasPrefix(u, "/") {
		u = "/" + u
	}
	return tr.BaseURL + u
}
