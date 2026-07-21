package seadex

import (
	"strconv"
	"strings"

	"github.com/cplieger/seadex-scout/internal/release"
	"github.com/cplieger/urlform"
)

// DefaultBaseURL is the canonical releases.moe site base - the SINGLE home of
// the SeaDex site-base fact, beside the package's other releases.moe contract
// knowledge (EntryURL, ValidInfoHash). The indexer's fallback and the
// composition root (build.go) both reference it; config deliberately carries
// no equal literal (it is a dependency leaf and a second copy could silently
// drift).
const DefaultBaseURL = "https://releases.moe"

// EntryURL returns the SeaDex entry page for an AniList id under baseURL
// (the releases.moe site base), or "" when the id is unknown. The entry-page
// rule lives here, beside the package's other releases.moe contract knowledge
// (ValidInfoHash/RedactedInfoHash), so every consumer builds the same link
// from the same base.
func EntryURL(baseURL string, aniListID int) string {
	if aniListID <= 0 {
		return ""
	}
	return strings.TrimRight(baseURL, "/") + "/" + strconv.Itoa(aniListID)
}

// UsableURL returns a link a human can follow for the torrent. An absolute URL
// is returned unchanged only when its host is a canonical tracker host from
// the release tracker table (or a dot-delimited subdomain of one), so a
// compromised SeaDex response cannot surface an attacker-controlled
// destination under a trusted tracker label; a relative path (as private
// trackers return) is prefixed with the tracker's base URL from that table,
// so a finding or report never emits a broken bare path. A schemeless value
// whose recovered host is itself a canonical tracker host is a mislabeled
// absolute URL, not a path: it is published on that recovered host with an
// https scheme, never base-prefixed under the (untrusted) label's host. An
// unknown tracker's URL drops to "" like every other unusable form (no
// canonical host exists to vouch for it or make a relative path followable).
//
// The structural reading of the raw string - which of the browser-vs-net/url
// parse-quirk forms it is - lives in the shared urlform.Classify; this
// publisher applies the publish-or-drop policy over those facts (where the
// AnimeBytes toggle gate, filter.ABVisible, applies extract-evidence-or-hide
// over the same facts). Malformed, hidden-host, and protocol-relative forms
// have no legitimate use as a clickable tracker link and drop; a
// protocol-relative URL ("//host/path") carries no scheme, yet a renderer
// resolves it against the ambient scheme and navigates off-site.
func (t *Torrent) UsableURL() string {
	f := urlform.Classify(t.URL)
	// Backslashes are rejected outright, even where the canonicalized reading
	// classifies cleanly: browsers treat "\\host" as an authority even though
	// url.Parse does not, and this publisher emits the raw string.
	if f.HasBackslash {
		return ""
	}
	// Resolve the tracker before handling any usable form: the tracker label
	// is untrusted upstream data too, and a resolvable canonical table entry
	// supplies the base URL a relative path needs. An absolute URL's host is
	// checked against the WHOLE canonical table in usableAbsolute (a
	// mislabeled cross-tracker URL stays usable), not only this entry's host.
	tr, ok := release.LookupTracker(t.Tracker)
	if !ok || tr.BaseURL == "" {
		return ""
	}
	switch f.Class {
	case urlform.ClassAbsolute:
		if !usableAbsolute(&f) {
			return ""
		}
		return f.Trimmed
	case urlform.ClassRelative:
		// In an href context a rooted path resolves tracker-relative, so it
		// is published base-prefixed - subject to the colon rule.
		return usableRelative(f.Trimmed, tr.BaseURL)
	case urlform.ClassSchemelessHost:
		// A schemeless value whose recovered authority IS a canonical
		// tracker host ("animebytes.tv/torrents.php?...") is a mislabeled
		// absolute URL, not a path: base-prefixing it under the LABELED
		// tracker would publish a wrong-tracker link
		// ("https://nyaa.si/animebytes.tv/...") that cannot identify the
		// intended torrent, so it is published on its own recovered host
		// with an https scheme (every canonical tracker is https). The
		// userinfo gate mirrors usableAbsolute: a credential-bearing
		// authority is a spoofing vector and never publishes canonicalized.
		// Any other schemeless value keeps the href reading - a
		// tracker-relative path under the labeled tracker's base - exactly
		// like the relative form above.
		if _, hostOK := release.LookupTrackerByHost(f.Host); hostOK && !f.HasUserInfo {
			return "https://" + f.Trimmed
		}
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
// All facts read here are the classifier's semantic fields (Scheme,
// HasUserInfo, Port, Host), never the parser representation, which stays
// private to the release package.
func usableAbsolute(f *urlform.Form) bool {
	if !strings.EqualFold(f.Scheme, "http") &&
		!strings.EqualFold(f.Scheme, "https") {
		return false
	}
	if f.HasUserInfo {
		return false
	}
	if f.Port != "" {
		if _, err := strconv.ParseUint(f.Port, 10, 16); err != nil {
			return false
		}
	}
	_, ok := release.LookupTrackerByHost(f.Host)
	return ok
}
