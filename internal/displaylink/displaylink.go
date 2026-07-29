// Package displaylink is the single home of this app's STRUCTURAL vouch step
// for a browser-destined absolute URL: the publish-or-drop reading of
// urlform.Classify's facts that every gate handing an untrusted URL to a
// renderer (an arr's clickable link, a report row, a Torznab <comments> field)
// applies before it looks at the host at all.
//
// Five gates apply it today, and each keeps its OWN host policy plus any extra
// legs its boundary needs:
//
//   - internal/trackerlink's Publish (absolute arm): the canonical tracker
//     table, plus the publisher's port rule.
//   - internal/indexer's httpDisplayHost: the served scope's own tracker, and a
//     non-empty host (its host evidence feeds a tracker lookup AND mints
//     curation keys).
//   - internal/indexer's snapshotInfoURLAllowed: the fixed releases.moe host,
//     behind urlform.IsASCIIHost.
//   - internal/library's SafeLogURL: a non-empty host, and it SANITIZES a
//     userinfo authority rather than refusing it (VouchSanitizingForm), because
//     the value is the operator's own configured arr base - a reverse-proxy
//     Basic Auth credential there is legitimate, and stripping it keeps the
//     deep-link clickable instead of dropping it.
//   - internal/config's warnPublicURLProblems: the same sanitizing reading, used
//     to PREDICT SafeLogURL's verdict for the boot diagnostic. It reads the
//     shared step rather than re-deriving the refusal legs, so the operator
//     warning "your report deep-links will be broken" cannot drift away from the
//     rule that actually admits the link (l-f208).
//
// Before this package each site re-stated the whole predicate inline, and the
// leg sets had already drifted apart - the port gate existed only in the
// publisher, the explicit ASCII-host gate only in the snapshot reader, the
// empty-host guard only in the indexer's display gate - so the shared rule
// could no longer be read off any single site (h-f13). One home means exactly
// one place learns a new urlform fact or a newly refused smuggling form.
//
// Runner-up home, recorded rather than taken: exporting the step from
// internal/trackerlink, where the first copy lives. Rejected because that
// package's doc deliberately scopes it to TRACKER links ("nothing here knows
// about SeaDex, HTTP, or the app's flows"), and two of the three callers vouch
// links that are not tracker links at all (a releases.moe entry page, a
// Prowlarr display URL).
//
// This package is NOT a urlform wrapper: urlform supplies the structural FACTS
// (which browser-vs-net/url parse form a raw string is), and this is the app's
// POLICY over them. It is a leaf - it imports nothing of the app.
package displaylink

import (
	"strings"

	"github.com/cplieger/urlform"
)

// VouchForm is Vouch over an already-classified form: an absolute http(s) URL,
// free of a userinfo authority and of the smuggling shapes a browser reads
// differently from net/url.
//
// Publish-or-drop is the stance, and every leg is a refusal a renderer would
// otherwise honor: a non-absolute form (malformed, hidden-host,
// protocol-relative, relative) resolves against whatever ambient context the
// renderer supplies rather than against a vouched destination; a non-http(s)
// scheme (javascript:, data:, file:) has no legitimate use in a link built from
// untrusted data; a userinfo authority is visual spoofing
// ("https://trusted@evil/"); and a backslash- or tab/newline-smuggled value is
// not vouchable at all, because the browser's authority reading of it differs
// from net/url's while the caller emits the raw string.
//
// It does NOT look at the host beyond reading it: binding a host to a trusted
// destination is the caller's policy, and this step is what every caller does
// FIRST.
func VouchForm(f *urlform.Form) bool {
	if f.HasUserInfo {
		return false
	}
	return VouchSanitizingForm(f)
}

// VouchSanitizingForm is VouchForm for a caller that SANITIZES a userinfo
// authority instead of refusing it: every structural leg above except the
// userinfo refusal. Only a caller whose value is operator-configured rather
// than upstream-supplied may use it, and only when it actually strips the
// authority before publishing (internal/library's SafeLogURL, and
// internal/config's prediction of that verdict). For an untrusted URL the
// userinfo form is visual spoofing with no legitimate reading, so
// trackerlink/indexer keep VouchForm.
func VouchSanitizingForm(f *urlform.Form) bool {
	if f.Class != urlform.ClassAbsolute {
		return false
	}
	if f.HasBackslash || f.HasTabOrNewline {
		return false
	}
	return isHTTPScheme(f.Scheme)
}

// isHTTPScheme reports whether a classified scheme is http or https, matched
// case-insensitively (a URL scheme is ASCII by grammar, so the fold cannot
// launder a non-ASCII rune into one of these names).
func isHTTPScheme(scheme string) bool {
	return strings.EqualFold(scheme, "http") || strings.EqualFold(scheme, "https")
}
