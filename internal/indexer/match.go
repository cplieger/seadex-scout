package indexer

import (
	"net/url"
	"strings"

	"github.com/cplieger/seadex-scout/internal/tracker"
	"github.com/cplieger/urlform"
)

// The indexer matches a Prowlarr result back to a SeaDex release by a stable
// per-tracker key: the numeric id in the release's tracker page URL. SeaDex
// stores that URL (Nyaa /view/{id}, AnimeBytes ...torrentid={id}); Prowlarr's
// Torznab item carries the same page URL (in <comments>/<guid>), so the ids
// line up regardless of title or info-hash availability. The info hash is used
// as a secondary key when present.

// --- Tracker scope and canonical hosts ---

// trackerScope classifies a tracker name (as SeaDex spells it, "Nyaa" or "AB")
// into the feed scope it maps to: upstreamNyaa, upstreamAB, or "" for any other
// tracker (a negligible SeaDex tail). The tracker vocabulary (which aliases
// denote which tracker) is owned by the canonical tracker table
// (tracker.Lookup), so id extraction, key building, download-link
// building, feed routing, and the alert/report path all agree on what counts
// as AnimeBytes.
func trackerScope(trackerName string) string {
	t, ok := tracker.Lookup(trackerName)
	if !ok {
		return ""
	}
	switch t.Name {
	case tracker.NameNyaa:
		return upstreamNyaa
	case tracker.NameAnimeBytes:
		return upstreamAB
	}
	return ""
}

// scopeOfHost returns the feed scope a URL host belongs to ("" for none):
// the canonical host table names the tracker and trackerScope maps that
// name to a scope, so host->scope has one home. Subdomains classify like the
// Is*Host twins; namespace-exact identity is isCanonicalTrackerHost's job.
func scopeOfHost(host string) string {
	t, ok := tracker.LookupByHost(host)
	if !ok {
		return ""
	}
	return trackerScope(t.Name)
}

// trackerID extracts the tracker's numeric torrent id from a SeaDex source
// URL for a scope: Nyaa's /view/{id}, AnimeBytes' torrentid=/permalink forms.
// It is the single home of the scope->id-extraction pairing, shared by
// trackerKey, trackerKeyFromURL, and downloadTarget.
func trackerID(scope, sourceURL string) string {
	switch scope {
	case upstreamNyaa:
		return nyaaID(sourceURL)
	case upstreamAB:
		return animeBytesID(sourceURL)
	}
	return ""
}

// nyaaID extracts the Nyaa torrent id from a URL whose path is the canonical
// /view/{id} route. Parsing first and scanning only the path keeps an id
// embedded in a query value or fragment (e.g. ?next=/view/123) from being
// read as the torrent id, and anchoring the route at the path START keeps a
// /view/ buried deeper in the path (e.g. /redirect/view/123) from minting a
// key: only the tracker's own torrent-page route is identity evidence, so a
// curation key is only ever derived from the URL component that actually
// identifies the torrent.
func nyaaID(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	path := u.EscapedPath()
	if !strings.HasPrefix(path, "/view/") || pathHasDotSegments(u.Path) {
		return ""
	}
	return extractID(path, "/view/")
}

// pathHasDotSegments reports whether a URL path carries a "." or ".." segment.
// A dot segment makes the raw path text disagree with the page a client
// actually resolves (RFC 3986 remove-dot-segments, which every browser and
// every HTTP client applies): "/view/123/../456" names torrent 456 to the
// tracker while a raw scan of the path reads 123, so the minted key, the
// download link built from that id, and the page URL served to the arr would
// name different torrents. Identity evidence must survive normalization, so
// such a path mints no key - the same fail-closed direction as a route that is
// not anchored at the path start.
//
// The DECODED path (url.URL.Path) is deliberately the input, so a
// percent-encoded dot segment ("/view/123/%2e%2e/456", which clients also
// normalize) is covered by the same check.
//
// This deliberately does NOT adopt urlform.Form.NormalizedPath, and the reason
// is a semantic delta rather than inertia (l-f68, declined 2026-08). That
// function resolves dot segments against the DECODED path, so it treats %2F as a
// separator where a browser keeps it inside one segment: "/a%2f../view/456"
// normalizes to "/view/456" for the library while the real destination is not a
// /view page at all, which would mint a tracker identity for a torrent the URL
// does not name. That is worse than the omission adopting it would fix, because
// the never-pruned publication log KEYS on this identity - a wrong key marks the
// wrong torrent as already served, permanently.
//
// And the omission it would fix does not occur: across the whole live catalogue
// (2821 entries / 9208 torrents, measured 2026-08) exactly ZERO URLs carry a dot
// segment and zero carry an escaped slash. Revisit if urlform grows a path
// reading whose escaped-separator semantics match a browser's, which would make
// the adoption strictly better rather than a trade.
func pathHasDotSegments(p string) bool {
	for seg := range strings.SplitSeq(p, "/") {
		if seg == "." || seg == ".." {
			return true
		}
	}
	return false
}

// trackerKey builds the match key for a SeaDex torrent from its tracker name
// and stored URL, or "" when the tracker is unknown, the id is missing, or
// the URL does not belong to the named tracker. The host gate is the
// fail-closed half of the curation trust boundary: the tracker LABEL alone
// must never authorize an id extracted from a foreign URL (a malformed or
// compromised SeaDex record with Tracker "Nyaa" and
// https://evil.example/view/123 would otherwise mint nyaa:123 and admit the
// REAL Nyaa torrent 123 as curated), so the id counts only when the URL is
// the tracker's own (see trackerOwnForm). A gated-out torrent is simply not
// curated/journaled - the safe direction, surfaced by the journal's
// unresolvable counter.
func trackerKey(trackerName, sourceURL string) string {
	scope := trackerScope(trackerName)
	if scope == "" {
		return ""
	}
	// Classify ONCE and extract the id from that same reading, never from the
	// original spelling: see trackerOwnForm. The vouched form is normalized to
	// its canonical absolute spelling first (tracker.CanonicalSourceURL), so the
	// scheme-free spelling of a tracker page keys identically to the absolute
	// one instead of losing its route to net/url.
	f := urlform.Classify(sourceURL)
	if !trackerOwnForm(scope, &f) {
		return ""
	}
	if id := trackerID(scope, tracker.CanonicalSourceURL(&f)); id != "" {
		return scope + ":" + id
	}
	return ""
}

// trackerOwnForm reports whether a classified SeaDex source URL belongs to the
// scope's own tracker: a userinfo-free absolute http(s) URL - or the same URL
// spelled without its scheme (see ownableHostForm) - on the tracker's EXACT
// canonical host (the shared tracker.Is*Host predicates reject homograph
// labels; the additional canonical-host check rejects subdomains, whose
// torrent-id databases are independent of the apex site's - see
// isCanonicalTrackerHost; the scheme/userinfo bar matches trackerKeyFromURL,
// so writer admission and journal identity agree and an odd-scheme SeaDex
// record is never journaled in the first place), or - for AnimeBytes only -
// a rooted relative reference, SeaDex's documented AB shape (the publisher
// resolves it against animebytes.tv, so a relative URL is an AB URL by
// construction). Anything else - a foreign host, a subdomain, a non-http(s)
// scheme, a userinfo-bearing authority, an unparseable URL, an opaque
// non-hierarchical form - fails closed.
//
// Structural facts come from urlform, so this reads a raw SeaDex URL the same
// way its three sibling consumers do (trackerlink.Publish,
// internal/filter's AB evidence gate, tracker.LookupByRelativeURL). It
// used to hand-roll the vocabulary with net/url, and the two readings had
// already diverged in one live shape (l-f162): for a schemeless-host URL
// ("animebytes.tv/torrents.php?id=1&torrentid=456") urlform reports
// ClassSchemelessHost with Host=animebytes.tv, so the publisher and the evidence
// gate treated it as AnimeBytes host evidence and published the link - while the
// triple-empty net/url test (Scheme=="" && Host=="" && Opaque=="") called it a
// "true relative reference" and admitted it here, after which the id extraction
// found nothing and the indexer silently DROPPED the release. Same string, two
// structural readings, one app. Under urlform a schemeless-host form is host
// evidence on both sides: it takes the host arm and is judged against the
// canonical-host policy like any other absolute-ish form, and it is ADMITTED
// there (l-f19) so the divergence closes in the direction that ends the split -
// the daemon used to alert on such a release with a clickable link while the
// feed silently omitted it as unresolvable. Admission happens only after every
// other gate below, and the id extractors stay exactly as strict; what makes it
// resolvable is tracker.CanonicalSourceURL, which hands them the https spelling.
//
// The AB relative arm is deliberately ClassRelative (a rooted "/x" path) rather
// than the narrower tracker.LookupByRelativeURL, which additionally
// demands the "/torrents.php?...torrentid=" shape: keying a relative Prowlarr
// permalink ("/torrent/123/group") works today and must keep working.
//
// It takes the CLASSIFIED form rather than the raw string because its callers
// must parse the same vouched reading for their tracker components. That is what
// closes the classify-then-reparse split (h-f8): ownership vouched the BROWSER's
// reading of a padded value ("\thttps://nyaa.si/view/123", which urlform
// preprocesses the way a browser does) while the id extraction re-parsed the
// ORIGINAL spelling with net/url, found no id, and silently dropped the release.
// Every caller now classifies once and extracts from f.Trimmed, so both halves
// read one string.
//
// This is deliberately NOT a loosening of nyaaID/animeBytesID: they stay strict
// and simply receive an already-preprocessed value. Embedded tab/newline,
// backslash, and hidden-host forms are still refused HERE, before any id is
// extracted, so the only family that newly resolves is edge padding.
func trackerOwnForm(scope string, f *urlform.Form) bool {
	if f.HasBackslash || f.HasTabOrNewline {
		// A de-smuggled string is not vouchable: a browser and net/url read it
		// differently, so it must not prove a curation identity.
		return false
	}
	// Host-bearing forms (absolute, protocol-relative, schemeless-host, or a
	// hidden-host form whose authority urlform recovered) are judged on their
	// host evidence; only the two spellings of an http(s) URL on that host can
	// carry ownership at all (ownableHostForm).
	if f.Host != "" {
		if !ownableHostForm(f) || f.HasUserInfo {
			return false
		}
		return scope != "" && scopeOfHost(f.Host) == scope &&
			isCanonicalTrackerHost(scope, f.Host)
	}
	// No host evidence: only AnimeBytes accepts a rooted relative reference, and
	// a form whose host could not be recovered (HostUnrecoverable) is a parse
	// failure for evidence purposes and must not slip through as "relative".
	return scope == upstreamAB && f.Class == urlform.ClassRelative && !f.HostUnrecoverable
}

// ownableHostForm reports whether a host-bearing form's CLASS and SCHEME can
// carry tracker ownership at all, before its host is judged. Exactly two
// spellings of one thing - an http(s) URL on the tracker's own site - qualify;
// everything else fails closed.
//
// An absolute URL must carry an http(s) scheme, the same bar the display gate
// applies (httpDisplayForm, used by trackerKeyFromURL), so writer admission and
// journal identity agree and an odd-scheme SeaDex record is never journaled.
//
// A schemeless-host form ("animebytes.tv/torrents.php?id=1&torrentid=456") is a
// mislabeled absolute URL, and it has NO scheme to bar: urlform only reaches
// that class when the parse found none, so the test is that the scheme is
// absent - stated explicitly rather than left to hold by accident. It is
// scheme-SAFE for the same reason the link publisher can publish it: the value
// is normalized to https before anything parses it
// (tracker.CanonicalSourceURL), and every canonical tracker in the table is an
// https site, so the form cannot smuggle a non-http scheme past this gate.
//
// A protocol-relative form ("//animebytes.tv/...") and a recovered hidden-host
// form ("https:animebytes.tv/...") also carry a host but are neither spelling: a
// browser and net/url disagree about them, so they must not prove an identity.
func ownableHostForm(f *urlform.Form) bool {
	switch f.Class {
	case urlform.ClassAbsolute:
		return isHTTPScheme(f.Scheme)
	case urlform.ClassSchemelessHost:
		return f.Scheme == ""
	default:
		return false
	}
}

// scopeTracker resolves a feed scope to its canonical tracker table entry - the
// scope->tracker half of the vocabulary bridge trackerScope owns the other half
// of. It is the ONE place a scope is turned into a table entry, so a tracker the
// indexer serves is named once rather than in every consumer that needs a field
// of its entry (the host for identity keying, the base URL for download links).
// An unknown scope, or a malformed table entry, fails closed.
func scopeTracker(scope string) (tracker.Tracker, bool) {
	var name string
	switch scope {
	case upstreamNyaa:
		name = tracker.NameNyaa
	case upstreamAB:
		name = tracker.NameAnimeBytes
	default:
		return tracker.Tracker{}, false
	}
	return tracker.Lookup(name)
}

// canonicalTrackerHost returns the exact hostname of a scope's tracker site,
// derived from the canonical tracker table (scopeTracker) so the host vocabulary
// stays single-homed, or "" for an unknown scope or a table entry whose BaseURL
// yields no hostname.
func canonicalTrackerHost(scope string) string {
	t, ok := scopeTracker(scope)
	if !ok {
		return ""
	}
	return t.Host()
}

// isCanonicalTrackerHost reports whether host is exactly the scope's
// canonical tracker host. Identity keying must be namespace-exact: a
// tracker torrent id only identifies a torrent within one site's database,
// and a real Nyaa subdomain (sukebei.nyaa.si) runs an id database
// independent of nyaa.si's, so an id read from a subdomain URL must not
// mint the apex site's key - nyaa:123 minted from sukebei.nyaa.si/view/123
// would authorize the UNRELATED nyaa.si torrent 123 as curated and build
// its download link for the wrong bytes. The shared tracker.Is*Host
// predicates accept subdomains, which is right for tracker CLASSIFICATION
// (obtainability, display) but too broad for identity; callers apply this
// check after them, so the ASCII/homograph gates have already run - and the
// fold here is ASCII-only regardless (urlform.EqualASCIIFold), so the predicate
// does not depend on that call order. Cross-site matching still works for
// mirrors through the info hash, which names the bytes themselves.
func isCanonicalTrackerHost(scope, host string) bool {
	// ASCII-only fold, deliberately NOT strings.EqualFold: that is the
	// full-Unicode simple fold urlform.EqualASCIIFold exists to keep out of a
	// host comparison (U+017F folds to 's', so a byte-wise foreign host could
	// match a canonical name) - see snapshotInfoURLAllowed for the same rule.
	// Both callers already hand us urlform's ASCII-lowercased host behind
	// tracker.Is*Host's IsASCIIHost gate, so this is byte equality on every
	// reachable input; it simply no longer DEPENDS on that call order.
	c := canonicalTrackerHost(scope)
	if c == "" {
		return false
	}
	return urlform.EqualASCIIFold(strings.TrimSuffix(host, "."), c)
}

// --- Match-key building ---

// trackerKeyFromURL builds the match key from an arbitrary release URL (a
// Prowlarr item's page URL) by detecting the tracker from the host, so it keys
// the same way trackerKey does for the SeaDex side. Admission is the shared
// display gate (httpDisplayHost): an absolute http(s) form, free of userinfo and
// of the browser-vs-net/url smuggling shapes, so an odd-scheme, userinfo-bearing
// or de-smuggled URL never proves an identity the rest of the boundary would
// refuse to display. Host classification rides the shared host->scope home
// (scopeOfHost, over tracker.LookupByHost), so a non-ASCII
// homograph label or an empty-labeled host under a tracker domain never
// yields a curation key.
func trackerKeyFromURL(raw string) string {
	// Classify once: the id is extracted from the VOUCHED reading (f.Trimmed),
	// not the original spelling (h-f8, see trackerOwnForm).
	f, ok := httpDisplayForm(raw)
	if !ok {
		return ""
	}
	scope := scopeOfHost(f.Host)
	if scope == "" {
		return ""
	}
	// Identity is namespace-exact: a subdomain (sukebei.nyaa.si) has its own
	// torrent-id database, so an id there must not key the apex site (see
	// isCanonicalTrackerHost). Such an item can still match by info hash.
	if !isCanonicalTrackerHost(scope, f.Host) {
		return ""
	}
	if id := trackerID(scope, f.Trimmed); id != "" {
		return scope + ":" + id
	}
	return ""
}

// --- Id extraction and validation ---

// animeBytesID extracts the AnimeBytes torrent id from either URL form: SeaDex
// stores the site form (`/torrents.php?...torrentid={id}`), while Prowlarr's
// Torznab items use the permalink form (`/torrent/{id}/group`) - the same id in
// both. AnimeBytes exposes no info hash in Torznab, so this id is the only key
// available for matching an AB release. The permalink id is read only from the
// URL path and the site-form id only from the torrentid query parameter, so an
// id smuggled inside an unrelated query value (e.g. ?next=/torrent/123/group)
// never yields a key.
func animeBytesID(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	// Only the two canonical route shapes are identity evidence, each
	// anchored at the path start: the permalink form's path begins exactly
	// at /torrent/{id}/..., and the torrentid parameter is consulted ONLY on
	// the site form's /torrents.php path - a torrentid on any other path
	// (e.g. /not-a-torrent?torrentid=123) is not evidence for the tracker
	// record and never mints a key.
	path := u.EscapedPath()
	if pathHasDotSegments(u.Path) {
		return ""
	}
	if strings.HasPrefix(path, "/torrent/") {
		return extractID(path, "/torrent/")
	}
	if path != "/torrents.php" {
		return ""
	}
	// A duplicated torrentid parameter is ambiguous: another consumer (a
	// PHP-style tracker, a proxy) may pick a different value than Go's
	// first-value Get, so an item could be authorized against one torrent
	// while referring to another (HTTP parameter pollution). Fail closed.
	//
	// url.URL.Query discards malformed pairs and their error, so a query like
	// `torrentid=123&x=1;torrentid=456` would surface exactly one value here
	// while a semicolon-splitting parser downstream resolves a different
	// torrent. Parse the raw query explicitly and reject any parse error so an
	// ambiguous query never mints a key.
	values, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return ""
	}
	torrentIDs, ok := values["torrentid"]
	if !ok || len(torrentIDs) != 1 {
		return ""
	}
	return validTrackerID(strings.TrimSpace(torrentIDs[0]))
}

// maxTrackerIDDigits bounds a tracker torrent id's decimal width: 20 digits
// covers a full uint64 with margin over both trackers' real id spaces. SeaDex
// permits multi-megabyte response pages with no per-string cap, and an
// extracted id is copied into byKey/byPair/Seen keys and JSON encoding, so an
// unbounded digit run would be a memory-amplification vector; anything longer
// fails closed exactly like a non-numeric id.
const maxTrackerIDDigits = 20

// extractID returns the token in path immediately after needle, up to the
// next URL delimiter (?, #, /, &). It returns "" unless the token is a valid
// tracker id (validTrackerID: a non-empty, width-bounded run of ASCII digits
// in canonical decimal form - no leading zero except the single value "0"),
// so a malformed or unexpected URL never yields a bogus key (adopted
// from seadexerr's id extraction).
//
// path is a URL PATH, never a raw URL: this is a plain substring scan, so a
// raw URL would let a needle inside a query value or fragment
// ("?next=/view/123") mint the torrent id - the smuggling both callers anchor
// against by extracting from u.EscapedPath() only.
func extractID(path, needle string) string {
	// A needle the path does not carry yields an empty token, which
	// validTrackerID refuses like any other non-id: no separate
	// not-found arm.
	_, after, _ := strings.Cut(path, needle)
	if cut := strings.IndexAny(after, "?#/&"); cut >= 0 {
		after = after[:cut]
	}
	return validTrackerID(after)
}

// validTrackerID is the single bounded validator every extracted tracker-id
// candidate routes through: it returns id unchanged when it is a non-empty
// run of ASCII digits no longer than maxTrackerIDDigits in the CANONICAL
// decimal form (no leading zero), and "" otherwise.
func validTrackerID(id string) string {
	// isAllDigits carries the non-empty half of the charset rule
	// (it ends in s != ""), so emptiness has ONE home.
	if len(id) > maxTrackerIDDigits || !isAllDigits(id) {
		return ""
	}
	if len(id) > 1 && id[0] == '0' {
		// A non-canonical decimal form ("0123") is the SAME torrent to the
		// tracker (both trackers route on an integer) but a different identity
		// string here, so it would key the curation set and the journal under a
		// second name that a Prowlarr item's canonical page URL can never
		// match. Fail closed like a non-numeric id.
		return ""
	}
	return id
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
