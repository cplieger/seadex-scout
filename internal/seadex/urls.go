package seadex

import (
	"net/url"
	"strings"
	"unicode/utf8"

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
	// Resolve the tracker before handling either URL form: the tracker label
	// is untrusted upstream data too, and the canonical table entry supplies
	// both the base URL a relative path needs and the only host an absolute
	// URL is allowed to point at.
	tr, ok := release.LookupTracker(t.Tracker)
	if !ok || tr.BaseURL == "" {
		return ""
	}
	if parsed.IsAbs() {
		// An absolute URL is returned unchanged, but only in http(s) with no
		// userinfo and a host bound to a canonical tracker host; any other
		// scheme (javascript:, data:, file:), a user@host authority (visual
		// spoofing: "https://trusted@evil/"), or a foreign host under a trusted
		// tracker label (phishing: a Nyaa-labeled https://evil.example) is
		// untrusted upstream data with no legitimate use in a clickable link.
		// The host is checked against the whole canonical table rather than
		// only the labeled tracker: the label is itself untrusted, and the
		// URL-aware AB toggle boundary (filter.ABVisible) deliberately keys on
		// the URL host, so a mislabeled AB URL must stay usable when that
		// boundary surfaces it.
		if !usableAbsolute(parsed) {
			return ""
		}
		return u
	}
	return usableRelative(u, parsed, tr.BaseURL)
}

// usableRelative converts a tracker-relative path into a followable link by
// prefixing the tracker's canonical base URL, preserving UsableURL's relative
// rejections in order. A protocol-relative URL ("//host/path") carries no
// scheme, yet a renderer resolves it against the ambient scheme and navigates
// off-site; it has no legitimate use as a tracker link. A relative value whose
// first colon precedes any slash (a query- or fragment-leading colon such as
// "?x:y" or "#a:b") is equally unusable as a relative path; a colon in the
// first path segment (e.g. "1a:b") never reaches here because url.Parse
// already rejects it in UsableURL. A scheme-less relative path parses
// host-free and still passes
// through (tracker-relative AB paths are unaffected), prefixed with one slash
// when absent.
func usableRelative(raw string, parsed *url.URL, baseURL string) string {
	if parsed.Host != "" || strings.HasPrefix(raw, "//") {
		return ""
	}
	if i := strings.Index(raw, ":"); i >= 0 && !strings.Contains(raw[:i], "/") {
		return ""
	}
	if !strings.HasPrefix(raw, "/") {
		raw = "/" + raw
	}
	return baseURL + raw
}

// usableAbsolute reports whether an already-absolute parsed URL is a safe
// clickable link: http(s) scheme, no userinfo authority, and a hostname bound
// to a canonical tracker host from the release tracker table (equal to one or
// a dot-delimited subdomain of one, via release.LookupTrackerByHost).
// Hostname() (the parsed host component) is checked rather than Host (the
// serialized authority), which is non-empty for a port-only authority like
// "https://:443/path" even though no host exists. Non-ASCII hostnames are
// rejected before the table lookup: an IDN lookalike of a tracker host (a
// homograph such as a Cyrillic "nyаa.si") has no legitimate use in SeaDex
// data.
func usableAbsolute(parsed *url.URL) bool {
	if !strings.EqualFold(parsed.Scheme, "http") &&
		!strings.EqualFold(parsed.Scheme, "https") {
		return false
	}
	if parsed.User != nil {
		return false
	}
	host := parsed.Hostname()
	for i := range len(host) {
		if host[i] >= utf8.RuneSelf {
			return false
		}
	}
	_, ok := release.LookupTrackerByHost(host)
	return ok
}
