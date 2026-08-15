package indexer

import (
	"net/url"
	"strings"

	"github.com/cplieger/seadex-scout/internal/tracker"
	"github.com/cplieger/urlform"
)

// The indexer matches a Prowlarr result back to a SeaDex release by a stable
// per-tracker key: the numeric id in the release's tracker page URL, which both
// SeaDex and Prowlarr's Torznab item carry, so the ids line up regardless of
// title or info-hash availability. The info hash is a secondary key when present.

// trackerScope classifies a tracker name (as SeaDex spells it, "Nyaa" or "AB")
// into the feed scope it maps to: upstreamNyaa, upstreamAB, or "" for any other
// tracker. The tracker vocabulary is owned by the canonical tracker table, so id
// extraction, key building, download links and feed routing all agree.
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

// scopeOfHost returns the feed scope a URL host belongs to ("" for none): the
// canonical host table names the tracker and trackerScope maps that name to a
// scope, so host->scope has one home. Namespace-exact identity is
// isCanonicalTrackerHost's job.
func scopeOfHost(host string) string {
	t, ok := tracker.LookupByHost(host)
	if !ok {
		return ""
	}
	return trackerScope(t.Name)
}

// trackerID extracts the tracker's numeric torrent id from a SeaDex source URL
// for a scope: Nyaa's /view/{id}, AnimeBytes' torrentid/permalink forms. Single
// home of the scope->id-extraction pairing.
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
// /view/{id} route. Parsing first and scanning only the path keeps an id embedded
// in a query value or fragment (?next=/view/123) from being read as the torrent
// id, and anchoring the route at the path START keeps a /view/ buried deeper
// (/redirect/view/123) from minting a key: only the tracker's own torrent-page
// route is identity evidence.
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
func pathHasDotSegments(p string) bool {
	for seg := range strings.SplitSeq(p, "/") {
		if seg == "." || seg == ".." {
			return true
		}
	}
	return false
}

// trackerKey builds the match key for a SeaDex torrent from its tracker name and
// stored URL, or "" when the tracker is unknown, the id is missing, or the URL
// does not belong to the named tracker. The host gate is the fail-closed half of
// the curation trust boundary: the tracker LABEL alone must never authorize an id
// extracted from a foreign URL, or a record with Tracker "Nyaa" and
// https://evil.example/view/123 would mint nyaa:123 and admit the REAL Nyaa
// torrent 123 as curated. A gated-out torrent is simply not curated.
func trackerKey(trackerName, sourceURL string) string {
	scope := trackerScope(trackerName)
	if scope == "" {
		return ""
	}
	// Classify ONCE and extract the id from that same reading, never from the
	// original spelling: see trackerOwnForm. The vouched form is normalized to its
	// canonical absolute spelling first, so a scheme-free spelling keys identically.
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
// canonical host, or, for AnimeBytes only, a rooted relative reference, SeaDex's
// documented AB shape. Anything else fails closed: a foreign host, a subdomain
// (whose torrent-id database is independent), a non-http(s) scheme, a
// userinfo-bearing authority, an unparseable or opaque form.
func trackerOwnForm(scope string, f *urlform.Form) bool {
	if f.HasBackslash || f.HasTabOrNewline {
		// A de-smuggled string is not vouchable: a browser and net/url read it
		// differently, so it must not prove a curation identity.
		return false
	}
	// Host-bearing forms are judged on their host evidence; only the two spellings
	// of an http(s) URL on that host can carry ownership at all (ownableHostForm).
	if f.Host != "" {
		if !ownableHostForm(f) || f.HasUserInfo {
			return false
		}
		// isCanonicalTrackerHost fails closed on an unknown scope, so the emptiness
		// test has one home rather than three.
		return scopeOfHost(f.Host) == scope && isCanonicalTrackerHost(scope, f.Host)
	}
	// No host evidence: only AnimeBytes accepts a rooted relative reference, and a
	// form whose host could not be recovered is a parse failure for evidence.
	return scope == upstreamAB && f.Class == urlform.ClassRelative && !f.HostUnrecoverable
}

// ownableHostForm reports whether a host-bearing form's CLASS and SCHEME can
// carry tracker ownership at all, before its host is judged. Exactly two
// spellings of one thing qualify. An absolute URL must carry an http(s) scheme,
// the same bar the display gate applies, so writer admission and journal identity
// agree. A schemeless-host form is a mislabeled absolute URL with NO scheme to
// bar, and it is scheme-SAFE because the value is normalized to https before
// anything parses it. A protocol-relative or recovered hidden-host form is
// neither spelling: a browser and net/url disagree about them.
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
// of, so a tracker the indexer serves is named once. An unknown scope, or a
// malformed table entry, fails closed.
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
// derived from the canonical tracker table so the host vocabulary stays
// single-homed, or "" for an unknown scope or an unusable table entry.
func canonicalTrackerHost(scope string) string {
	t, ok := scopeTracker(scope)
	if !ok {
		return ""
	}
	return t.Host()
}

// isCanonicalTrackerHost reports whether host is exactly the scope's canonical
// tracker host. Identity keying must be namespace-exact: a tracker torrent id
// only identifies a torrent within one site's database, and a real Nyaa subdomain
// (sukebei.nyaa.si) runs an independent one, so nyaa:123 minted there would
// authorize the UNRELATED nyaa.si torrent 123 and build its download link. The
// shared tracker.LookupByHost accepts subdomains, which is right for tracker
// CLASSIFICATION but too broad for identity. Cross-site matching still works
// through the info hash, which names the bytes themselves.
func isCanonicalTrackerHost(scope, host string) bool {
	// ASCII-only fold, deliberately NOT strings.EqualFold: that is the full-Unicode
	// simple fold urlform.EqualASCIIFold exists to keep out of a host comparison
	// (U+017F folds to 's', so a byte-wise foreign host could match a canonical name).
	c := canonicalTrackerHost(scope)
	if c == "" {
		return false
	}
	return urlform.EqualASCIIFold(strings.TrimSuffix(host, "."), c)
}

// trackerKeyFromURL builds the match key from an arbitrary release URL (a Prowlarr
// item's page URL) by detecting the tracker from the host, so it keys the same way
// trackerKey does for the SeaDex side. Admission is the shared display gate: an
// absolute http(s) form, free of userinfo and of the browser-vs-net/url smuggling
// shapes. Host classification rides the shared host->scope home, so a homograph
// label or an empty-labeled host under a tracker domain yields no key.
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
	// Identity is namespace-exact: a subdomain has its own torrent-id database, so
	// an id there must not key the apex site. Such an item can still match by hash.
	if !isCanonicalTrackerHost(scope, f.Host) {
		return ""
	}
	if id := trackerID(scope, f.Trimmed); id != "" {
		return scope + ":" + id
	}
	return ""
}

// animeBytesID extracts the AnimeBytes torrent id from either URL form: SeaDex
// stores the site form (`/torrents.php?...torrentid={id}`), Prowlarr the permalink
// form (`/torrent/{id}/group`) - the same id in both, and the only key available
// for an AB release, which exposes no info hash in Torznab. The permalink id is
// read only from the path and the site-form id only from the torrentid parameter,
// so an id smuggled inside an unrelated query value never yields a key.
func animeBytesID(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	// Only the two canonical route shapes are identity evidence, each anchored at
	// the path start: the permalink path begins exactly at /torrent/{id}/..., and
	// torrentid is consulted ONLY on the site form's /torrents.php path.
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
	// A duplicated torrentid parameter is ambiguous: another consumer may pick a
	// different value than Go's first-value Get, so an item could be authorized against
	// one torrent while referring to another.
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

// maxTrackerIDDigits bounds a tracker torrent id's decimal width: 20 digits covers
// a full uint64 with margin over both trackers' real id spaces. An extracted id is
// copied into map keys and JSON, and SeaDex permits multi-megabyte pages with no
// per-string cap, so an unbounded digit run is a memory-amplification vector.
const maxTrackerIDDigits = 20

// extractID returns the token in path immediately after needle, up to the next URL
// delimiter (?, #, /, &), and "" unless the token is a valid tracker id
// (validTrackerID). path is a URL PATH, never a raw URL: this is a plain substring
// scan, so a raw URL would let a needle inside a query value or fragment mint the
// torrent id - which is why both callers extract from u.EscapedPath() only.
func extractID(path, needle string) string {
	// A needle the path does not carry yields an empty token, which validTrackerID
	// refuses like any other non-id: no separate not-found arm.
	_, after, _ := strings.Cut(path, needle)
	if cut := strings.IndexAny(after, "?#/&"); cut >= 0 {
		after = after[:cut]
	}
	return validTrackerID(after)
}

// validTrackerID is the single bounded validator every extracted tracker-id
// candidate routes through: it returns id unchanged when it is a non-empty run of
// ASCII digits no longer than maxTrackerIDDigits in the CANONICAL decimal form
// (no leading zero), and "" otherwise.
func validTrackerID(id string) string {
	// isAllDigits carries the non-empty half of the charset rule
	// (it ends in s != ""), so emptiness has ONE home.
	if len(id) > maxTrackerIDDigits || !isAllDigits(id) {
		return ""
	}
	if len(id) > 1 && id[0] == '0' {
		// A non-canonical decimal form ("0123") is the SAME torrent to the tracker but
		// a different identity string here, so it would key the curation set under a
		// second name no Prowlarr item's canonical page URL can match.
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
