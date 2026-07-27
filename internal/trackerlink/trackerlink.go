// Package trackerlink decides whether an untrusted upstream torrent URL may be
// PUBLISHED as a clickable tracker link, and in what form.
//
// It is one half of a single concern, and it sits beside the other half
// deliberately: internal/filter owns the HIDE decision over the same two
// untrusted inputs (filter.ABVisible / filter.ClassifyAB, both keyed on the
// same (tracker, rawURL) pair), and this package owns the PUBLISH decision.
// The two take deliberately OPPOSITE fail directions - publish-or-drop here,
// extract-evidence-or-hide there - which is correct and documented at both
// sites, but they read the same two fact sources (urlform's structural
// classification and internal/release's canonical tracker table), so they
// belong at one layer.
//
// It used to live as a method on the decoded wire struct inside internal/seadex
// (the releases.moe read client), which split the concern across two layers and
// made every consumer of the SeaDex model reach the tracker registry through the
// HTTP client (l-f86). This package imports release + urlform only, exactly as
// filter does, so it is a leaf: nothing here knows about SeaDex, HTTP, or the
// app's flows.
package trackerlink

import (
	"net/url"
	"strconv"
	"strings"

	"github.com/cplieger/seadex-scout/internal/release"
	"github.com/cplieger/urlform"
)

// Publish returns a link a human can follow for a torrent on tracker, or "" when
// the raw upstream value cannot be vouched for. An absolute URL is returned
// unchanged (apart from edge trimming and only when free of smuggling bytes)
// only when its host is a canonical tracker host from the release tracker table
// (or a dot-delimited subdomain of one), so a compromised upstream response
// cannot surface an attacker-controlled destination under a trusted tracker
// label; a relative path (as private trackers return) is prefixed with the
// tracker's base URL from that table, so a finding or report never emits a
// broken bare path. A schemeless value whose recovered host is itself a
// canonical tracker host is a mislabeled absolute URL, not a path: it is
// published on that recovered host with an https scheme, never base-prefixed
// under the (untrusted) label's host. An unknown tracker's URL drops to "" like
// every other unusable form (no canonical host exists to vouch for it or make a
// relative path followable).
//
// The structural reading of the raw string - which of the browser-vs-net/url
// parse-quirk forms it is - lives in the shared urlform.Classify; this publisher
// applies the publish-or-drop policy over those facts (where the AnimeBytes
// toggle gate, filter.ABVisible, applies extract-evidence-or-hide over the same
// facts). Malformed, hidden-host, and protocol-relative forms have no legitimate
// use as a clickable tracker link and drop; a protocol-relative URL
// ("//host/path") carries no scheme, yet a renderer resolves it against the
// ambient scheme and navigates off-site.
//
// The (tracker, rawURL) argument order mirrors filter.ABVisible and
// filter.ClassifyAB, the hide half of the same concern.
//
// A value that carries only a canonical host ("nyaa.si", "https://nyaa.si/")
// drops like every other unvouchable form: the tracker's front page identifies
// no torrent, so publishing it would emit a plausible-looking 404 AND hide the
// upstream data defect from the caller's unusable-URL accounting. This is the
// host-form half of the shape floor the path-published ladder already applies
// (see pathShaped and hostFormTargeted).
func Publish(tracker, rawURL string) string {
	f := urlform.Classify(rawURL)
	// Backslashes are rejected outright, even where the canonicalized reading
	// classifies cleanly: browsers treat "\\host" as an authority even though
	// url.Parse does not, and this publisher emits the raw string. A
	// tab/newline-smuggled URL (the WHATWG preprocessing removed embedded
	// whitespace to read it) is rejected the same way: Trimmed is emit-safe,
	// but legitimate upstream data has no reason to carry smuggling bytes, and
	// this publisher drops what it cannot vouch for.
	if f.HasBackslash || f.HasTabOrNewline {
		return ""
	}
	// Resolve the tracker before handling any usable form: the tracker label
	// is untrusted upstream data too, and a resolvable canonical table entry
	// supplies the base URL a relative path needs. An absolute URL's host is
	// checked against the WHOLE canonical table in usableAbsolute (a
	// mislabeled cross-tracker URL stays usable), not only this entry's host.
	tr, ok := release.LookupTracker(tracker)
	if !ok || tr.BaseURL == "" {
		return ""
	}
	switch f.Class {
	case urlform.ClassAbsolute:
		if !usableAbsolute(&f) || !hostFormTargeted(f.Trimmed) {
			return ""
		}
		return httpsCanonical(f.Trimmed, f.Scheme)
	case urlform.ClassRelative:
		// In an href context a rooted path resolves tracker-relative, so it
		// is published base-prefixed - subject to the colon rule.
		return publishRelative(f.Trimmed, tr.BaseURL)
	case urlform.ClassSchemelessHost:
		return usableSchemelessHost(&f, tr.BaseURL)
	default:
		// Empty, malformed, hidden-host, and protocol-relative forms drop.
		return ""
	}
}

// usableSchemelessHost applies Publish's schemeless-host policy. A schemeless
// value whose recovered authority IS a canonical tracker host
// ("animebytes.tv/torrents.php?...") is a mislabeled absolute URL, not a
// path: base-prefixing it under the LABELED tracker would publish a
// wrong-tracker link ("https://nyaa.si/animebytes.tv/...") that cannot
// identify the intended torrent, so it is published on its own recovered
// host with an https scheme (every canonical tracker is https). The userinfo
// gate mirrors usableAbsolute: a credential-bearing authority is a spoofing
// vector and never publishes canonicalized; the recovered authority's port
// is range-checked the same way (see schemelessPortOK), so a canonicalized
// publish cannot emit an out-of-range port usableAbsolute would reject on
// the equivalent absolute form. Any other schemeless value keeps
// the href reading - a tracker-relative path under the labeled tracker's
// base (or the inferred owner's, for a tracker-specific relative shape) -
// exactly like Publish's relative form.
func usableSchemelessHost(f *urlform.Form, baseURL string) string {
	if _, hostOK := release.LookupTrackerByHost(f.Host); hostOK && !f.HasUserInfo {
		if !schemelessPortOK(f.Trimmed) || !hostFormTargeted(f.Trimmed) {
			return ""
		}
		return "https://" + f.Trimmed
	}
	return publishRelative(f.Trimmed, baseURL)
}

// schemelessPortOK reports whether the recovered authority of a schemeless
// value carries a publishable port: absent, or numeric and inside the 16-bit
// range usableAbsolute enforces for the absolute form. urlform records the
// recovered Host and HasUserInfo for ClassSchemelessHost but not the
// recovered Port, so the authority is re-parsed here ("//" + value makes
// net/url read it as one) rather than trusting the label-free host fact
// alone. Under urlform v1.1.0 no value actually reaches this gate with a
// port: a colon before the first "/", "?" or "#" makes net/url read a
// scheme ("nyaa.si:65536/x" classifies ClassHiddenHost with no recoverable
// authority, since that scheme is not special) or fail outright ("first
// path segment in URL cannot contain colon", ClassMalformed), and Publish
// drops both before this branch. The check is therefore fail-closed
// defense in depth that keeps this branch at parity with usableAbsolute's
// range gate should the classifier's schemeless recovery ever start
// surfacing a ported authority. An unparsable authority is unpublishable
// too - this publisher drops what it cannot vouch for.
func schemelessPortOK(trimmed string) bool {
	u, err := url.Parse("//" + trimmed)
	if err != nil {
		return false
	}
	return portOK(u.Port())
}

// portOK reports whether a URL port component is publishable: absent, or
// numeric and inside the 16-bit range a real TCP port occupies. It is the
// SINGLE home of the publisher's port rule, so the absolute gate
// (usableAbsolute) and the canonicalized schemeless publish
// (schemelessPortOK) cannot drift apart.
func portOK(port string) bool {
	if port == "" {
		return true
	}
	_, err := strconv.ParseUint(port, 10, 16)
	return err == nil
}

// publishRelative applies the shared inferred-owner-wins policy for
// path-published forms: a tracker-specific relative shape (the AB
// torrent-page form) names its OWN tracker, so the inferred owner's base
// wins over the untrusted label's; anything else publishes under the
// labeled tracker's base. The lookup reads both host-less spellings
// (rooted and slashless) identically, so raw goes in unmodified.
func publishRelative(raw, labelBase string) string {
	if inferred, ok := release.LookupTrackerByRelativeURL(raw); ok {
		return usableRelative(raw, inferred.BaseURL)
	}
	return usableRelative(raw, labelBase)
}

// usableRelative converts a tracker-relative path into a followable link by
// prefixing the tracker's canonical base URL. A relative value whose first
// colon precedes any slash (a query- or fragment-leading colon such as "?x:y"
// or "#a:b") is unusable as a relative path; a colon in the first path
// segment (e.g. "1a:b") never reaches here because such a string classifies
// malformed ("first path segment in URL cannot contain colon") or hidden-host
// (a valid-scheme parse). A scheme-less path is prefixed with one slash when
// absent (tracker-relative AB paths are unaffected).
//
// The value must also LOOK like a torrent-page path: either more than one
// non-empty path segment, or a query/fragment. This is the floor the chain was
// missing (l-f88). It is the last resort of Publish's fallback ladder - an
// unrecognized value is assumed tracker-relative - and it used to accept any
// string without a leading colon, so a structureless token published as a
// plausible-looking 404: the live catalogue carries exactly one such record (AB,
// url "Chihiro" - a release-group name typed into the url field), which became
// "https://animebytes.tv/Chihiro". The shape test keeps every real form (Nyaa's
// "/view/1" has two segments, AB's "/torrents.php?...torrentid=..." carries a
// query) and drops a bare token, so the caller can report an unpublishable URL
// instead of emitting a link that goes nowhere.
func usableRelative(raw, baseURL string) string {
	if i := strings.Index(raw, ":"); i >= 0 && !strings.Contains(raw[:i], "/") {
		return ""
	}
	if !strings.HasPrefix(raw, "/") {
		raw = "/" + raw
	}
	if !pathShaped(raw) {
		return ""
	}
	return baseURL + raw
}

// pathShaped reports whether a rooted relative value has the shape of a torrent
// page rather than a bare label: more than one non-empty path segment, or a
// query/fragment carrying the identifying parameters. It reads the string
// directly (no parse): the value already survived the colon rule, and the two
// real tracker shapes are structural, not semantic.
func pathShaped(rooted string) bool {
	if i := strings.IndexAny(rooted, "?#"); i >= 0 {
		// A query or fragment is where both trackers put the torrent id; the
		// path half may legitimately be a single segment ("/torrents.php").
		return i > 1
	}
	segments := 0
	for seg := range strings.SplitSeq(rooted, "/") {
		if seg != "" {
			segments++
		}
	}
	return segments > 1
}

// hostFormTargeted reports whether a host-bearing value names a target beyond
// its authority. It is the host-form twin of pathShaped: a value that is only
// a canonical host ("nyaa.si", "https://animebytes.tv/") resolves to the
// tracker's front page, which cannot identify the intended torrent - the same
// plausible-404 publish the shape floor (l-f88) closed for the path-published
// ladder, and the same reason to drop rather than publish, so the caller can
// report an unpublishable URL instead. The authority is located by the "://"
// separator (present in every absolute form usableAbsolute admits, since it
// already required an http(s) scheme) and absent from a schemeless value.
//
// The tail past that first delimiter must carry at least one NON-delimiter
// character: a remainder made only of further delimiters ("nyaa.si/?",
// "nyaa.si/#", "nyaa.si//") still resolves to the front page, so it names no
// target either - the same reading pathShaped already applies to the
// equivalent relative spellings. A genuinely targeted root query
// ("nyaa.si/?page=view&tid=1") is kept, which is why this arm is not a
// pathShaped delegation.
func hostFormTargeted(trimmed string) bool {
	rest := trimmed
	if i := strings.Index(rest, "://"); i >= 0 {
		rest = rest[i+len("://"):]
	}
	i := strings.IndexAny(rest, "/?#")
	if i < 0 {
		return false
	}
	return strings.Trim(rest[i:], "/?#") != ""
}

// httpsCanonical rewrites a vouched absolute link's cleartext scheme to https.
//
// usableAbsolute has already bound the host to a canonical tracker from
// release.trackerTable, and EVERY base URL in that table is https - as is the
// sibling schemeless branch's output (usableSchemelessHost prefixes "https://"
// on the stated grounds that "every canonical tracker is https"). The absolute
// branch was the one publish path that emitted the scheme verbatim, so a
// compromised or tampered upstream response could swap https for http on a
// canonical host and Publish would emit a downgradeable link. Neither nyaa.si
// nor animebytes.tv is HSTS-preloaded, so that first hop really is cleartext:
// an on-path attacker answers it with a phishing page under the tracker's own
// URL bar, and AnimeBytes is a login-bearing private tracker. Upgrading rather
// than dropping keeps the link useful - the host is already proven canonical,
// and https is what that host serves.
//
// The scheme is matched case-insensitively against the classifier's own Scheme
// fact and replaced in place, so the rest of the URL (including a mixed-case
// host or path) survives byte-for-byte.
func httpsCanonical(trimmed, scheme string) string {
	if !strings.EqualFold(scheme, "http") {
		return trimmed
	}
	if len(trimmed) < len(scheme) || !strings.EqualFold(trimmed[:len(scheme)], scheme) {
		// Defensive: Trimmed is the preprocessed URL and always leads with its
		// scheme, but a rewrite anchored on an assumption that failed would
		// corrupt the link rather than fail closed.
		return trimmed
	}
	return "https" + trimmed[len(scheme):]
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
// "nyаa.si") has no legitimate use in upstream data, and this gate's fail
// direction (unclassifiable = drop the link) is exactly the predicate's.
// All facts read here are the classifier's semantic fields (Scheme,
// HasUserInfo, Port, Host), never the parser representation, which stays
// private to the release package.
// One further rule: a cleartext URL that names an explicit port other than 443
// is refused, because the https upgrade (httpsCanonical) rewrites the scheme
// only and would publish an https link to a plaintext port.
func usableAbsolute(f *urlform.Form) bool {
	if !strings.EqualFold(f.Scheme, "http") &&
		!strings.EqualFold(f.Scheme, "https") {
		return false
	}
	if f.HasUserInfo {
		return false
	}
	if !portOK(f.Port) {
		return false
	}
	// A cleartext URL carrying an explicit port names the http service's port,
	// and httpsCanonical rewrites only the scheme - it cannot move the port -
	// so publishing it would emit an https link to a plaintext port (a link
	// that cannot connect). 443 is the one port the upgrade leaves coherent;
	// anything else is unvouchable and drops, so the caller reports it as a
	// URL error instead of a plausible-looking dead link.
	if strings.EqualFold(f.Scheme, "http") && f.Port != "" && f.Port != "443" {
		return false
	}
	_, ok := release.LookupTrackerByHost(f.Host)
	return ok
}
