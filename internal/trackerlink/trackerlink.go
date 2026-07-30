// Package trackerlink validates and canonicalizes untrusted tracker URLs for publication.
// Values that cannot be bound to a known tracker are dropped.
package trackerlink

import (
	"net/url"
	"strconv"
	"strings"

	"github.com/cplieger/seadex-scout/internal/displaylink"
	"github.com/cplieger/seadex-scout/internal/tracker"
	"github.com/cplieger/urlform"
)

// Publish returns a link a human can follow for a torrent on tracker, or "" when
// the raw upstream value cannot be vouched for. An absolute URL is returned
// unchanged (apart from edge trimming and only when free of smuggling bytes)
// only when its host is a canonical tracker host from the internal/tracker table
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
//
// A caller that must DIAGNOSE a refusal (name the remedy for a dropped value)
// reads PublishReason instead: the empty string alone cannot distinguish an
// unknown tracker (an app-vocabulary gap) from an unvouchable url (an upstream
// data defect).
func Publish(trackerName, rawURL string) string {
	link, _ := PublishReason(trackerName, rawURL)
	return link
}

// Refusal names WHY PublishReason produced no link. A refusal is not one
// thing: the two non-trivial grades have DIFFERENT remedies, and a consumer
// that cannot tell them apart necessarily names the wrong one (l-f127).
type Refusal int

const (
	// RefusalNone means a link was published; the string is non-empty.
	RefusalNone Refusal = iota
	// RefusalNoURL means the record carries no url at all (omitted, empty, or
	// whitespace-only). Nothing is wrong: SeaDex simply has no link for that
	// release, so there is no remedy and nothing to report.
	RefusalNoURL
	// RefusalUnknownTracker means THIS APP's tracker vocabulary does not know
	// the record's tracker (no canonical table entry, or one with no base
	// URL). Nothing about the upstream record is wrong: the remedy is a
	// seadex-scout change - add the entry to internal/tracker's table and ship
	// a release - so a diagnostic must not blame the SeaDex record for it.
	RefusalUnknownTracker
	// RefusalUnvouchableURL means the tracker is known but the url itself
	// cannot be vouched for: smuggling bytes, a foreign host under a trusted
	// label, an unsafe scheme, a hidden-host or protocol-relative form, or a
	// value with no torrent-page shape at all. The remedy is fixing the SeaDex
	// record, at the source.
	RefusalUnvouchableURL
)

// PublishReason is Publish plus the refusal reason: the full edge, for the
// consumers that DIAGNOSE a refusal rather than just render a link (the audit
// report's row marker, the SeaDex client's aggregate catalogue WARN). Publish
// stays the link-only form for the consumers that genuinely only need the
// link (compare, the indexer feed), so the reason reaches the two diagnostic
// callers without forcing a second return on every call site.
//
// Runner-up shape, recorded rather than taken: make Publish itself return
// (string, Refusal) and let every caller discard the reason. It puts the
// reason unmissably on the one edge, but pays for it at five call sites that
// have no use for it; the split keeps the common read a plain string while
// still leaving exactly one implementation of the policy.
func PublishReason(trackerName, rawURL string) (string, Refusal) {
	f := urlform.Classify(rawURL)
	// Backslashes are rejected outright, even where the canonicalized reading
	// classifies cleanly: browsers treat "\\host" as an authority even though
	// url.Parse does not, and this publisher emits the raw string. A
	// tab/newline-smuggled URL (the WHATWG preprocessing removed embedded
	// whitespace to read it) is rejected the same way: Trimmed is emit-safe,
	// but legitimate upstream data has no reason to carry smuggling bytes, and
	// this publisher drops what it cannot vouch for.
	if f.HasBackslash || f.HasTabOrNewline {
		return "", RefusalUnvouchableURL
	}
	// A userinfo authority is refused ONCE here, for EVERY class, rather than
	// per-arm: it is visual spoofing ("https://trusted@evil/") with no
	// legitimate reading in upstream data, and no publish arm needs one. The
	// per-arm placement it replaces had two homes for the one policy (the
	// absolute arm's displaylink.VouchForm, the canonical schemeless arm's own
	// check) and left the two path-published arms with none, so a
	// credential-shaped authority ("user@animebytes.tv/torrents.php?id=9") was
	// demoted into a path and published under a canonical base as a
	// plausible-looking 404. Gating at the entry makes the output invariant the
	// property test asserts (every published link parses with a nil User)
	// structural rather than emergent.
	if f.HasUserInfo {
		return "", RefusalUnvouchableURL
	}
	// An absent url is graded BEFORE the tracker gate: a record with no link
	// and an unknown tracker is not an app-vocabulary gap worth reporting -
	// there is nothing to publish either way, and grading it
	// RefusalUnknownTracker would annotate every link-less release of an
	// unlisted tracker. The drop itself is unchanged (both arms return "").
	if f.Class == urlform.ClassEmpty {
		return "", RefusalNoURL
	}
	// Resolve the tracker before handling any usable form: the tracker label
	// is untrusted upstream data too, and a resolvable canonical table entry
	// supplies the base URL a relative path needs. An absolute URL's host is
	// checked against the WHOLE canonical table in usableAbsolute (a
	// mislabeled cross-tracker URL stays usable), not only this entry's host.
	tr, ok := tracker.Lookup(trackerName)
	if !ok || tr.BaseURL == "" {
		return "", RefusalUnknownTracker
	}
	switch f.Class {
	case urlform.ClassAbsolute:
		if !usableAbsolute(&f) || !hostFormTargeted(&f) {
			return "", RefusalUnvouchableURL
		}
		return httpsCanonical(f.Trimmed, f.Scheme), RefusalNone
	case urlform.ClassRelative:
		// In an href context a rooted path resolves tracker-relative, so it
		// is published base-prefixed - subject to the colon rule.
		return published(publishRelative(f.Trimmed, tr.BaseURL))
	case urlform.ClassSchemelessHost:
		return published(usableSchemelessHost(&f, tr.BaseURL))
	default:
		// Malformed, hidden-host, and protocol-relative forms drop.
		return "", RefusalUnvouchableURL
	}
}

// published grades one of the path-published arms' string results: the ladder
// itself returns "" for every shape it refuses (the colon rule, the shape
// floor), and every one of those refusals is a property of the URL value, so
// the grade follows from emptiness alone.
func published(link string) (string, Refusal) {
	if link == "" {
		return "", RefusalUnvouchableURL
	}
	return link, RefusalNone
}

// usableSchemelessHost applies Publish's schemeless-host policy. A schemeless
// value whose recovered authority IS a canonical tracker host
// ("animebytes.tv/torrents.php?...") is a mislabeled absolute URL, not a
// path: base-prefixing it under the LABELED tracker would publish a
// wrong-tracker link ("https://nyaa.si/animebytes.tv/...") that cannot
// identify the intended torrent, so it is published on its own recovered
// host with an https scheme (every canonical tracker is https). A
// credential-bearing authority never reaches this arm: PublishReason refuses
// f.HasUserInfo for every class at its entry, so this branch reads only the
// recovered host. The recovered authority's port is range-checked the same
// way (see schemelessPortOK), so a canonicalized
// publish cannot emit an out-of-range port usableAbsolute would reject on
// the equivalent absolute form. Any other schemeless value keeps
// the href reading - a tracker-relative path under the labeled tracker's
// base (or the inferred owner's, for a tracker-specific relative shape) -
// exactly like Publish's relative form.
func usableSchemelessHost(f *urlform.Form, baseURL string) string {
	if _, hostOK := tracker.LookupByHost(f.Host); hostOK {
		if !schemelessPortOK(f.Trimmed) || !hostFormTargeted(f) {
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
// alone. Under urlform v1.2.0 no value actually reaches this gate with a
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
// numeric and inside the 16-bit range a real TCP port occupies. Port 0 is
// refused: it parses as a uint16 but names no destination port (it is the
// kernel's "pick one" sentinel), so a link carrying it cannot connect - the
// same reading the config URL validator already applies. It is the
// SINGLE home of the publisher's port rule, so the absolute gate
// (usableAbsolute) and the canonicalized schemeless publish
// (schemelessPortOK) cannot drift apart.
func portOK(port string) bool {
	if port == "" {
		return true
	}
	n, err := strconv.ParseUint(port, 10, 16)
	return err == nil && n != 0
}

// publishRelative applies the shared inferred-owner-wins policy for
// path-published forms: a tracker-specific relative shape (the AB
// torrent-page form) names its OWN tracker, so the inferred owner's base
// wins over the untrusted label's; anything else publishes under the
// labeled tracker's base. The lookup reads both host-less spellings
// (rooted and slashless) identically, so raw goes in unmodified.
func publishRelative(raw, labelBase string) string {
	if inferred, ok := tracker.LookupByRelativeURL(raw); ok {
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
// non-empty path segment, or a query/fragment. This is the last resort of
// Publish's fallback ladder - an unrecognized value is assumed
// tracker-relative - so without the shape floor a structureless token (the
// live catalogue carries one: tracker AB, url "Chihiro", a release-group name
// typed into the url field) would publish a plausible-looking 404. The test
// keeps every real form (Nyaa's "/view/1" has two segments, AB's
// "/torrents.php?...torrentid=..." carries a query) and drops a bare token,
// so the caller reports an unpublishable URL instead of a link that goes
// nowhere.
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
// single path segment plus a query/fragment carrying the identifying
// parameters. It reads the string directly (no parse): the value already
// survived the colon rule, and the two real tracker shapes are structural, not
// semantic.
func pathShaped(rooted string) bool {
	pathPart := rooted
	hasTargetParams := false
	if i := strings.IndexAny(rooted, "?#"); i >= 0 {
		// A QUERY is where both trackers put the torrent id, so it stands in
		// for the second path segment - but only when it carries identifying
		// content: a delimiter-only tail ("/view?", "/view#", "/view?#")
		// resolves to the same page as the bare single-segment path the floor
		// already refuses. A FRAGMENT never counts: it is resolved client-side,
		// so it leaves the browser wherever the path landed and cannot identify
		// a torrent. That is the same reading hostFormTargeted applies, and
		// counting it here made the two arms disagree - ".#0" published as
		// "https://animebytes.tv/.#0", which this arm's own host-form twin then
		// refused, breaking the idempotence the fuzz property requires.
		pathPart = rooted[:i]
		if rooted[i] == '?' {
			query := rooted[i+1:]
			if j := strings.IndexByte(query, '#'); j >= 0 {
				query = query[:j]
			}
			hasTargetParams = strings.Trim(query, "/?#") != ""
		}
	}
	segments := pathSegments(pathPart)
	// A query/fragment never substitutes for the path segment itself: a value
	// with no segment at all publishes the tracker root ("nyaa.si/?id=1"),
	// which names no torrent page.
	return segments > 1 || (segments == 1 && hasTargetParams)
}

// pathSegments counts the non-empty segments of a URL path. It is the ONE
// reading both shape arms use, so "does this path name anything past the
// authority" cannot mean two things: an empty path, "/" and "//" all count 0.
// It deliberately does NOT resolve dot segments - see hostFormTargeted.
func pathSegments(p string) int {
	n := 0
	for seg := range strings.SplitSeq(p, "/") {
		if seg != "" {
			n++
		}
	}
	return n
}

// hostFormTargeted reports whether a host-bearing value names a target beyond
// its authority. It is the host-form twin of pathShaped: a value that is only
// a canonical host ("nyaa.si", "https://animebytes.tv/") resolves to the
// tracker's front page, which cannot identify the intended torrent - the same
// plausible-404 publish the shape floor (l-f88) closed for the path-published
// ladder, and the same reason to drop rather than publish, so the caller can
// report an unpublishable URL instead. The value is parsed to locate that
// authority, with a scheme supplied first when the classification records
// none (net/url cannot recover an authority without one).
//
// The tail past the authority must name something a browser would resolve to
// more than the front page. The reading is done on the PARSED, normalized URL
// rather than on raw delimiters: a remainder made only of delimiters
// ("nyaa.si/?", "nyaa.si/#", "nyaa.si//") and a remainder made only of
// dot-segments ("nyaa.si/.", "nyaa.si/..", and their percent-encoded
// spellings) both normalize back to the tracker root, so neither names a
// target - the same reading pathShaped already applies to the equivalent
// relative spellings. A genuinely targeted root query
// ("nyaa.si/?page=view&tid=1") is kept, which is why this arm is not a
// pathShaped delegation; a fragment-only tail ("nyaa.si/#1167293") is NOT a
// target, since a fragment is resolved client-side and leaves the browser on
// the front page. An unparsable value names no target either (this
// publisher drops what it cannot vouch for).
func hostFormTargeted(f *urlform.Form) bool {
	trimmed := f.Trimmed
	if f.Scheme == "" {
		// A schemeless value's authority is only recoverable by net/url once
		// it carries a scheme; every canonical tracker is https, which is the
		// scheme the schemeless branch publishes on.
		trimmed = "https://" + trimmed
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return false
	}
	// A query can identify a torrent page on the tracker root
	// ("nyaa.si/?page=view&tid=1"); a FRAGMENT cannot - it is resolved
	// client-side, so it leaves the target the front page. A fragment on a
	// real link is already covered by the path arm below, and the relative
	// twin (pathShaped) refuses a fragment with no path segment for the same
	// reason, so both spellings of the same upstream defect now drop.
	if strings.Trim(u.RawQuery, "/?#") != "" {
		return true
	}
	// The tail past the authority is read by pathShaped, the SAME reading the
	// relative arm applies, so the two arms cannot disagree about what names a
	// target - and in particular this arm accepts every value that arm
	// publishes, which is what makes Publish idempotent. It deliberately does
	// NOT resolve dot segments away: resolving them made this arm refuse
	// "/view/.." while the relative arm published it, so a relative "/view/.."
	// produced an absolute link that would not publish if it came back
	// through. Nothing in the live catalogue carries a dot segment (0 of 9181
	// tracker URLs, checked 2026-07), so resolving them guarded no real value
	// and cost a self-contradiction. A path naming nothing of its own still
	// drops, because dot segments are not segments the floor counts as a
	// target on their own.
	//
	// The one deliberate difference from the relative arm is above: a root
	// QUERY is kept here ("nyaa.si/?page=view&tid=1" is a real Nyaa shape)
	// while the relative arm refuses a query with no path segment at all. That
	// asymmetry is safe in this direction - the relative arm publishing LESS
	// can never produce a link this arm then refuses.
	rooted := u.EscapedPath()
	if u.Fragment != "" {
		rooted += "#" + u.EscapedFragment()
	}
	return pathShaped(rooted)
}

// httpsCanonical rewrites a vouched absolute link's cleartext scheme to https.
//
// usableAbsolute has already bound the host to a canonical tracker from
// tracker.table, and EVERY base URL in that table is https - as is the
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
// hostname bound to a canonical tracker host from the internal/tracker table
// (equal to one or a real dot-delimited subdomain, via
// tracker.LookupByHost). Any other scheme (javascript:, data:, file:)
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
// private to the tracker package.
// One further rule: a cleartext URL that names an explicit port other than 443
// is refused, because the https upgrade (httpsCanonical) rewrites the scheme
// only and would publish an https link to a plaintext port.
//
// The structural legs - absolute, http(s), no userinfo, no smuggling bytes -
// are internal/displaylink's, the app's one home for that vouch step (h-f13),
// shared with the indexer's two display gates; what stays here is this
// publisher's OWN policy: the port rule and the canonical-table host bind.
// (PublishReason already refuses the smuggling forms AND a userinfo authority
// for every class before reaching this arm; asking displaylink again is the
// same answer, from the one place that defines it, and is kept deliberately as
// redundant defense on that shared gate.)
func usableAbsolute(f *urlform.Form) bool {
	if !displaylink.VouchForm(f) {
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
	_, ok := tracker.LookupByHost(f.Host)
	return ok
}
