package seadex

import (
	"strconv"
	"strings"

	"github.com/cplieger/seadex-scout/internal/release"
)

// UsableURL returns a link a human can follow for the torrent. An absolute URL
// is returned unchanged only when its host is a canonical tracker host from
// the release tracker table (or a dot-delimited subdomain of one), so a
// compromised SeaDex response cannot surface an attacker-controlled
// destination under a trusted tracker label; a relative path (as private
// trackers return) is prefixed with the tracker's base URL from that table,
// so a finding or report never emits a broken bare path. An unknown tracker's
// URL drops to "" like every other unusable form (no canonical host exists to
// vouch for it or make a relative path followable).
//
// The structural reading of the raw string - which of the browser-vs-net/url
// parse-quirk forms it is - lives in the shared release.ClassifyRawURL; this
// publisher applies the publish-or-drop policy over those facts (where the
// AnimeBytes toggle gate, filter.ABVisible, applies extract-evidence-or-hide
// over the same facts). Malformed, hidden-host, and protocol-relative forms
// have no legitimate use as a clickable tracker link and drop; a
// protocol-relative URL ("//host/path") carries no scheme, yet a renderer
// resolves it against the ambient scheme and navigates off-site.
func (t *Torrent) UsableURL() string {
	f := release.ClassifyRawURL(t.URL)
	// Backslashes are rejected outright, even where the canonicalized reading
	// classifies cleanly: browsers treat "\\host" as an authority even though
	// url.Parse does not, and this publisher emits the raw string.
	if f.HasBackslash {
		return ""
	}
	// Resolve the tracker before handling any usable form: the tracker label
	// is untrusted upstream data too, and the canonical table entry supplies
	// both the base URL a relative path needs and the only host an absolute
	// URL is allowed to point at.
	tr, ok := release.LookupTracker(t.Tracker)
	if !ok || tr.BaseURL == "" {
		return ""
	}
	switch f.Class {
	case release.URLFormAbsolute:
		if !usableAbsolute(&f) {
			return ""
		}
		return f.Trimmed
	case release.URLFormRelative, release.URLFormSchemelessHost:
		// In an href context both forms resolve as tracker-relative paths
		// (the schemeless-host reading applies to address bars, not links),
		// so they are published base-prefixed - subject to the colon rule.
		return usableRelative(f.Trimmed, tr.BaseURL)
	default:
		// Empty, malformed, hidden-host, and protocol-relative forms drop.
		return ""
	}
}

// usableRelative converts a tracker-relative path into a followable link by
// prefixing the tracker's canonical base URL. A relative value whose first
// colon precedes any slash (a query- or fragment-leading colon such as "?x:y"
// or "#a:b") is unusable as a relative path; a colon in the first path
// segment (e.g. "1a:b") never reaches here because such a string classifies
// malformed ("first path segment in URL cannot contain colon") or hidden-host
// (a valid-scheme parse). A scheme-less path is prefixed with one slash when
// absent (tracker-relative AB paths are unaffected).
func usableRelative(raw, baseURL string) string {
	if i := strings.Index(raw, ":"); i >= 0 && !strings.Contains(raw[:i], "/") {
		return ""
	}
	if !strings.HasPrefix(raw, "/") {
		raw = "/" + raw
	}
	return baseURL + raw
}

// usableAbsolute reports whether an absolute-classified URL is a safe
// clickable link: http(s) scheme, no userinfo authority (visual spoofing:
// "https://trusted@evil/"), a numeric 16-bit port when one is present, and a
// hostname bound to a canonical tracker host from the release tracker table
// (equal to one or a real dot-delimited subdomain, via
// release.LookupTrackerByHost). Any other scheme (javascript:, data:, file:)
// is untrusted upstream data with no legitimate use in a clickable link. The
// host is checked against the whole canonical table rather than only the
// labeled tracker: the label is itself untrusted, and the URL-aware AB toggle
// boundary (filter.ABVisible) deliberately keys on the URL host, so a
// mislabeled AB URL must stay usable when that boundary surfaces it.
// Non-ASCII and empty-labeled hostnames are rejected by the shared predicate
// itself: an IDN lookalike of a tracker host (a homograph such as a Cyrillic
// "nyаa.si") has no legitimate use in SeaDex data, and this gate's fail
// direction (unclassifiable = drop the link) is exactly the predicate's.
func usableAbsolute(f *release.URLForm) bool {
	if !strings.EqualFold(f.Parsed.Scheme, "http") &&
		!strings.EqualFold(f.Parsed.Scheme, "https") {
		return false
	}
	if f.Parsed.User != nil {
		return false
	}
	if port := f.Parsed.Port(); port != "" {
		if _, err := strconv.ParseUint(port, 10, 16); err != nil {
			return false
		}
	}
	_, ok := release.LookupTrackerByHost(f.Host)
	return ok
}
