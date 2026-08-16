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
// the raw upstream value cannot be vouched for.
func Publish(trackerName, rawURL string) string {
	link, _ := PublishReason(trackerName, rawURL)
	return link
}

// Refusal names WHY PublishReason produced no link. A refusal is not one
// thing: the two non-trivial grades have DIFFERENT remedies, and a consumer
// that cannot tell them apart necessarily names the wrong one.
type Refusal int

const (
	// RefusalNone means a link was published; the string is non-empty.
	RefusalNone Refusal = iota
	// RefusalNoURL means the record carries no url at all (omitted, empty, or
	// whitespace-only).
	RefusalNoURL
	// RefusalUnknownTracker means THIS APP's tracker vocabulary does not know
	// the record's tracker (no canonical table entry, or one with no base
	// URL).
	RefusalUnknownTracker
	// RefusalUnvouchableURL means the tracker is known but the url itself
	// cannot be vouched for: smuggling bytes, a foreign host under a trusted
	// label, an unsafe scheme, a hidden-host or protocol-relative form, or a
	// value with no torrent-page shape at all.
	RefusalUnvouchableURL
)

// PublishReason is Publish plus the refusal reason: the full edge, for the
// consumers that DIAGNOSE a refusal rather than just render a link (the audit
// report's row marker, the SeaDex client's aggregate catalogue WARN).
func PublishReason(trackerName, rawURL string) (string, Refusal) {
	f := urlform.Classify(rawURL)
	// Backslashes are rejected outright, even where the canonicalized reading
	// classifies cleanly: browsers treat "\\host" as an authority even though
	// url.Parse does not, and this publisher emits the raw string.
	if f.HasBackslash || f.HasTabOrNewline {
		return "", RefusalUnvouchableURL
	}
	// A userinfo authority is refused ONCE here, for EVERY class, rather than
	// per-arm: it is visual spoofing ("https://trusted@evil/") with no
	// legitimate reading in upstream data, and no publish arm needs one.
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
	// supplies the base URL a relative path needs.
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

// usableSchemelessHost applies Publish's schemeless-host policy.
func usableSchemelessHost(f *urlform.Form, baseURL string) string {
	if _, hostOK := tracker.LookupByHost(f.Host); hostOK {
		if !schemelessPortOK(f.Trimmed) || !hostFormTargeted(f) {
			return ""
		}
		return tracker.CanonicalSourceURL(f)
	}
	return publishRelative(f.Trimmed, baseURL)
}

// schemelessPortOK reports whether the recovered authority of a schemeless
// value carries a publishable port: absent, or numeric and inside the 16-bit
// range usableAbsolute enforces for the absolute form.
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
// same reading the config URL validator already applies.
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
// labeled tracker's base.
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
// segment (e.g.
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
// parameters.
func pathShaped(rooted string) bool {
	pathPart := rooted
	hasTargetParams := false
	if i := strings.IndexAny(rooted, "?#"); i >= 0 {
		// A QUERY is where both trackers put the torrent id, so it stands in
		// for the second path segment - but only when it carries identifying
		// content: a delimiter-only tail ("/view?", "/view#", "/view?#")
		// resolves to the same page as the bare single-segment path the floor
		// already refuses.
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

// pathSegments counts the TARGETING segments of a URL path: the non-empty
// segments that still name something once a client has applied RFC 3986
// remove-dot-segments.
func pathSegments(p string) int {
	n := 0
	for seg := range strings.SplitSeq(p, "/") {
		if seg == "" || isDotSegment(seg) {
			continue
		}
		n++
	}
	return n
}

// isDotSegment reports whether one path segment is a relative dot segment -
// "." or ".." - including the percent-encoded spellings a client decodes before
// resolving ("%2e", "%2E%2e"), which is the same DECODED reading
// internal/indexer's pathHasDotSegments applies to the same untrusted SeaDex
// urls.
func isDotSegment(seg string) bool {
	decoded, err := url.PathUnescape(seg)
	if err != nil {
		return false
	}
	return decoded == "." || decoded == ".."
}

// hostFormTargeted reports whether a host-bearing value names a target beyond
// its authority.
func hostFormTargeted(f *urlform.Form) bool {
	// A schemeless value's authority is only recoverable by net/url once it
	// carries a scheme.
	trimmed := tracker.CanonicalSourceURL(f)
	u, err := url.Parse(trimmed)
	if err != nil {
		return false
	}
	// A query can identify a torrent page on the tracker root
	// ("nyaa.si/?page=view&tid=1"); a FRAGMENT cannot - it is resolved
	// client-side, so it leaves the target the front page.
	if strings.Trim(u.RawQuery, "/?#") != "" {
		return true
	}
	// The tail past the authority is read by pathShaped, the SAME reading the
	// relative arm applies, so the two arms cannot disagree about what names a
	// target - and in particular this arm accepts every value that arm
	// publishes, which is what makes Publish idempotent.
	rooted := u.EscapedPath()
	if u.Fragment != "" {
		rooted += "#" + u.EscapedFragment()
	}
	return pathShaped(rooted)
}

// httpsCanonical rewrites a vouched absolute link's cleartext scheme to https.
func httpsCanonical(trimmed, scheme string) string {
	if !strings.EqualFold(scheme, "http") {
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
// is untrusted upstream data with no legitimate use in a clickable link.
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
	// that cannot connect).
	if strings.EqualFold(f.Scheme, "http") && f.Port != "" && f.Port != "443" {
		return false
	}
	_, ok := tracker.LookupByHost(f.Host)
	return ok
}
