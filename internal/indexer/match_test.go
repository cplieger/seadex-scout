package indexer

import (
	"strings"
	"testing"
)

// TestTrackerScope pins the two documented contracts of trackerScope that the
// other indexer tests only exercise indirectly: the defensive "animebytes"
// alias for the "AB" spelling, and the tail-drop default (any unknown tracker
// maps to "") that makes the journal/downloadURL exclude those releases from
// the synthesized feed. Nyaa/AB spellings are normalized case- and
// whitespace-insensitively.
func TestTrackerScope(t *testing.T) {
	tests := []struct{ tracker, want string }{
		{"Nyaa", upstreamNyaa},
		{"nyaa", upstreamNyaa},
		{"  Nyaa  ", upstreamNyaa},
		{"AB", upstreamAB},
		{"ab", upstreamAB},
		{"animebytes", upstreamAB},
		{"AnimeBytes", upstreamAB},
		{"AnimeTosho", ""},
		{"RuTracker", ""},
		{"", ""},
	}
	for _, tc := range tests {
		if got := trackerScope(tc.tracker); got != tc.want {
			t.Errorf("trackerScope(%q) = %q, want %q", tc.tracker, got, tc.want)
		}
	}
}

// TestTrackerKeyRejectsUnknownAndUnparseable pins the empty-key fallbacks the
// happy-path tests skip: an unknown tracker and a tracker URL without its id
// both yield no key (the release simply cannot be matched), and an unparseable
// URL yields no key from the Prowlarr-side extractor rather than an error or a
// bogus match.
func TestTrackerKeyRejectsUnknownAndUnparseable(t *testing.T) {
	if got := trackerKey("AnimeTosho", "https://animetosho.org/view/123"); got != "" {
		t.Errorf("unknown tracker key = %q, want empty", got)
	}
	if got := trackerKey("Nyaa", "https://nyaa.si/about"); got != "" {
		t.Errorf("nyaa URL without an id key = %q, want empty", got)
	}
	if got := trackerKey("AB", "/torrents.php?id=1"); got != "" {
		t.Errorf("ab URL without a torrentid key = %q, want empty", got)
	}
	if got := trackerKeyFromURL("http://nyaa.si/view/1%zz"); got != "" {
		t.Errorf("unparseable URL key = %q, want empty", got)
	}
	if got := trackerKey("Nyaa", "http://nyaa.si/view/1%zz"); got != "" {
		t.Errorf("nyaa unparseable URL key = %q, want empty", got)
	}
	if got := trackerKey("AB", "http://animebytes.tv/torrent/1%zz"); got != "" {
		t.Errorf("ab unparseable URL key = %q, want empty", got)
	}
}

// TestTrackerKeyFromURLRejectsForgedTrackerHosts pins the protection
// trackerKeyFromURL inherits from the shared tracker predicate
// (release.LookupTrackerByHost): a non-ASCII homograph label under a tracker
// domain and an empty-labeled host must never yield a curation key, so a
// tracker-controlled URL cannot smuggle an item into the curation match on a
// host no real tracker page can live on.
func TestTrackerKeyFromURLRejectsForgedTrackerHosts(t *testing.T) {
	tests := []struct{ name, url string }{
		{"homograph label under nyaa.si", "https://x\u00e9.nyaa.si/view/1234567"},
		{"homograph label under animebytes.tv", "https://x\u00e9.animebytes.tv/torrent/1167293/group"},
		{"fullwidth-dot nyaa host", "https://nyaa\uff0esi/view/1234567"},
		{"empty-label nyaa host", "https://.nyaa.si/view/1234567"},
		{"inner-empty-label nyaa host", "https://a..nyaa.si/view/1234567"},
		{"empty-label animebytes host", "https://.animebytes.tv/torrent/1167293/group"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := trackerKeyFromURL(tc.url); got != "" {
				t.Errorf("trackerKeyFromURL(%q) = %q, want empty (forged tracker host must not key)", tc.url, got)
			}
		})
	}
}

// TestAnimeBytesIDRejectsDuplicateTorrentIDParams pins the fail-closed rule
// on a duplicated torrentid query parameter (HTTP parameter pollution): Go's
// url.Values.Get would silently pick the first value while another consumer
// may pick a different one, so an ambiguous query-form URL must yield no key
// in either ordering, while the unambiguous single-value form still matches.
func TestAnimeBytesIDRejectsDuplicateTorrentIDParams(t *testing.T) {
	if got := animeBytesID("/torrents.php?id=1&torrentid=1167293&torrentid=999"); got != "" {
		t.Errorf("duplicate torrentid (curated first) = %q, want empty (ambiguous)", got)
	}
	if got := animeBytesID("/torrents.php?id=1&torrentid=999&torrentid=1167293"); got != "" {
		t.Errorf("duplicate torrentid (curated last) = %q, want empty (ambiguous)", got)
	}
	if got := animeBytesID("/torrents.php?id=1&torrentid=1167293"); got != "1167293" {
		t.Errorf("single torrentid = %q, want 1167293", got)
	}
}

// TestTrackerIDExtractionRejectsNonCanonicalRoutes pins the route-anchoring
// half of the identity gate: only the tracker's own canonical torrent-page
// route shapes are identity evidence, so a /view/ or /torrent/ buried deeper
// in the path, or a torrentid parameter on a non-/torrents.php path, must
// never mint a key even on the tracker's own host - a compromised SeaDex
// response could otherwise authorize and build a canonical download link for
// an unrelated torrent.
func TestTrackerIDExtractionRejectsNonCanonicalRoutes(t *testing.T) {
	tests := []struct {
		name string
		got  string
	}{
		{"nyaa /view/ not at path start", nyaaID("https://nyaa.si/redirect/view/123")},
		{"ab torrentid on a non-torrents.php path", animeBytesID("/not-a-torrent?torrentid=123")},
		{"ab permalink route not at path start", animeBytesID("https://animebytes.tv/x/torrent/123/group")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != "" {
				t.Errorf("extracted id = %q, want empty (non-canonical route is not identity evidence)", tc.got)
			}
		})
	}
	if got := animeBytesID("https://animebytes.tv/torrent/1167293/group"); got != "1167293" {
		t.Errorf("canonical permalink = %q, want 1167293", got)
	}
	if got := nyaaID("https://nyaa.si/view/123"); got != "123" {
		t.Errorf("canonical nyaa route = %q, want 123", got)
	}
}

// TestTrackerIDExtractionRejectsOverlongDigitRuns pins the width bound
// (maxTrackerIDDigits) on every extraction route: an arbitrarily long digit
// run from a multi-megabyte SeaDex page must fail closed like a non-numeric
// id instead of being copied into byKey/byPair/Seen keys and JSON encoding
// (memory amplification), while a full-width real id still keys.
func TestTrackerIDExtractionRejectsOverlongDigitRuns(t *testing.T) {
	over := strings.Repeat("9", maxTrackerIDDigits+1)
	atMax := strings.Repeat("9", maxTrackerIDDigits)
	if got := nyaaID("https://nyaa.si/view/" + over); got != "" {
		t.Errorf("nyaaID(overlong id) = %q, want empty", got)
	}
	if got := nyaaID("https://nyaa.si/view/" + atMax); got != atMax {
		t.Errorf("nyaaID(max-width id) = %q, want %q", got, atMax)
	}
	if got := animeBytesID("/torrents.php?torrentid=" + over); got != "" {
		t.Errorf("animeBytesID(overlong torrentid) = %q, want empty", got)
	}
	if got := animeBytesID("https://animebytes.tv/torrent/" + over + "/group"); got != "" {
		t.Errorf("animeBytesID(overlong permalink id) = %q, want empty", got)
	}
}

// TestTrackerKeyRejectsForeignHostURLs pins the SeaDex-side host gate
// (trackerOwnURL): the record's tracker LABEL alone must never authorize an
// id extracted from a foreign URL - a malformed or compromised SeaDex record
// (Tracker "Nyaa", https://evil.example/view/123) would otherwise mint
// nyaa:123 as curation authorization for the REAL Nyaa torrent 123. An
// absolute URL keys only on the tracker's own host; the relative site form is
// accepted for AnimeBytes alone (SeaDex's documented AB shape, resolved
// against animebytes.tv by UsableURL); opaque non-hierarchical forms fail
// closed.
func TestTrackerKeyRejectsForeignHostURLs(t *testing.T) {
	tests := []struct {
		name    string
		tracker string
		url     string
		want    string
	}{
		{"nyaa on its own host keys", "Nyaa", "https://nyaa.si/view/123", "nyaa:123"},
		{"nyaa label with a foreign host fails closed", "Nyaa", "https://evil.example/view/123", ""},
		{"nyaa label with a homograph-adjacent host fails closed", "Nyaa", "https://notnyaa.example/view/123", ""},
		{"nyaa relative form fails closed (SeaDex ships nyaa absolute)", "Nyaa", "/view/123", ""},
		{"ab on its own host keys", "AB", "https://animebytes.tv/torrents.php?id=1&torrentid=456", "ab:456"},
		{"ab relative site form keys", "AB", "/torrents.php?id=1&torrentid=456", "ab:456"},
		{"ab label with a foreign host fails closed", "AB", "https://evil.example/torrents.php?id=1&torrentid=456", ""},
		{"ab opaque scheme fails closed", "AB", "javascript:/torrents.php?torrentid=456", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := trackerKey(tc.tracker, tc.url); got != tc.want {
				t.Errorf("trackerKey(%q, %q) = %q, want %q", tc.tracker, tc.url, got, tc.want)
			}
		})
	}
}

// TestTrackerIDHelpersFailClosedOnUnparseableInput pins the defensive
// fail-closed arms the current calling paths cannot reach on their own:
// nyaaID and animeBytesID return "" for a URL url.Parse rejects, and
// trackerOwnURL answers false for a scope outside the nyaa/ab vocabulary,
// so any future caller reaching these helpers directly still fails closed
// on the curation trust boundary.
func TestTrackerIDHelpersFailClosedOnUnparseableInput(t *testing.T) {
	if got := nyaaID("http://[::1"); got != "" {
		t.Errorf("nyaaID(unparseable) = %q, want empty", got)
	}
	if got := animeBytesID("http://[::1"); got != "" {
		t.Errorf("animeBytesID(unparseable) = %q, want empty", got)
	}
	if trackerOwnURL("other", "https://nyaa.si/view/1") {
		t.Error("trackerOwnURL(unknown scope) = true, want false (fail closed)")
	}
}

// TestCanonicalTrackerHost pins the canonical-host vocabulary the identity
// keying (isCanonicalTrackerHost) relies on: each scope derives exactly the
// apex hostname from the release tracker table, and an unknown scope fails
// closed with "" - the defensive arm no calling path reaches today, kept
// honest for any future direct caller on the curation trust boundary.
func TestCanonicalTrackerHost(t *testing.T) {
	tests := []struct{ scope, want string }{
		{upstreamNyaa, "nyaa.si"},
		{upstreamAB, "animebytes.tv"},
		{"other", ""},
		{"", ""},
	}
	for _, tc := range tests {
		if got := canonicalTrackerHost(tc.scope); got != tc.want {
			t.Errorf("canonicalTrackerHost(%q) = %q, want %q", tc.scope, got, tc.want)
		}
	}
}

// TestTrackerIDUnknownScopeFailsClosed pins trackerID's fail-closed default:
// a scope outside the nyaa/ab vocabulary extracts no id even from a URL that
// carries one, so any future caller reaching the dispatcher with an
// unclassified scope cannot mint identity evidence - the same defensive arm
// TestTrackerIDHelpersFailClosedOnUnparseableInput pins for trackerOwnURL.
func TestTrackerIDUnknownScopeFailsClosed(t *testing.T) {
	if got := trackerID("other", "https://nyaa.si/view/123"); got != "" {
		t.Errorf(`trackerID("other", ...) = %q, want empty (fail closed on an unknown scope)`, got)
	}
	if got := trackerID("", "/torrents.php?torrentid=123"); got != "" {
		t.Errorf(`trackerID("", ...) = %q, want empty (fail closed on an empty scope)`, got)
	}
}
