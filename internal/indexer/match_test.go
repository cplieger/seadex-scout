package indexer

import (
	"strings"
	"testing"

	"github.com/cplieger/urlform"
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
// (tracker.LookupByHost): a non-ASCII homograph label under a tracker
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

// TestAnimeBytesIDRejectsMalformedQuery pins that a query Go cannot parse
// cleanly never mints a key. url.URL.Query silently drops malformed pairs and
// their error, so `torrentid=123&x=1;torrentid=456` would leave exactly one
// surviving value while a semicolon-splitting consumer downstream resolves
// torrent 456 - the same parameter-pollution ambiguity, one parser layer down.
func TestAnimeBytesIDRejectsMalformedQuery(t *testing.T) {
	if got := animeBytesID("/torrents.php?torrentid=123&x=1;torrentid=456"); got != "" {
		t.Errorf("semicolon-smuggled torrentid = %q, want empty (query does not parse cleanly)", got)
	}
	if got := animeBytesID("/torrents.php?torrentid=123&%zz=1"); got != "" {
		t.Errorf("invalid percent-escape in query = %q, want empty (query does not parse cleanly)", got)
	}
	if got := animeBytesID("/torrents.php?id=1&torrentid=123"); got != "123" {
		t.Errorf("well-formed single torrentid = %q, want 123", got)
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
		{"nyaa view path with a dot segment", nyaaID("https://nyaa.si/view/123/../456")},
		{"ab permalink path with a dot segment", animeBytesID("https://animebytes.tv/torrent/123/../456/group")},
		{"nyaa view path with a single-dot segment", nyaaID("https://nyaa.si/view/123/./456")},
		{"ab permalink path with a single-dot segment", animeBytesID("https://animebytes.tv/torrent/123/./456/group")},
		{"nyaa view path with a percent-encoded single-dot segment", nyaaID("https://nyaa.si/view/123/%2e/456")},
		{"nyaa view path with a percent-encoded dot segment", nyaaID("https://nyaa.si/view/123/%2e%2e/456")},
		{"ab permalink path with a percent-encoded dot segment", animeBytesID("https://animebytes.tv/torrent/123/%2E%2E/456/group")},
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

// TestTrackerIDExtractionRejectsNonCanonicalDecimalForms pins the identity half
// of validTrackerID's contract: a zero-padded id is the SAME torrent to a
// tracker that routes on an integer, so admitting it would key one torrent
// under two identity strings - the curation match would miss the canonical
// page URL a Prowlarr item carries, and the journal could list the release
// twice under two GUIDs. It fails closed like a non-numeric id, while a lone
// "0" (canonical) and an ordinary id still key.
func TestTrackerIDExtractionRejectsNonCanonicalDecimalForms(t *testing.T) {
	if got := nyaaID("https://nyaa.si/view/0123"); got != "" {
		t.Errorf("nyaaID(zero-padded id) = %q, want empty (non-canonical identity form)", got)
	}
	if got := animeBytesID("/torrents.php?torrentid=00"); got != "" {
		t.Errorf("animeBytesID(zero-padded torrentid) = %q, want empty (non-canonical identity form)", got)
	}
	if got := nyaaID("https://nyaa.si/view/0"); got != "0" {
		t.Errorf("nyaaID(\"0\") = %q, want 0 (canonical)", got)
	}
}

// TestTrackerKeyRejectsForeignHostURLs pins the SeaDex-side host gate
// (trackerOwnForm): the record's tracker LABEL alone must never authorize an
// id extracted from a foreign URL - a malformed or compromised SeaDex record
// (Tracker "Nyaa", https://evil.example/view/123) would otherwise mint
// nyaa:123 as curation authorization for the REAL Nyaa torrent 123. An
// absolute URL keys only on the tracker's own host; the relative site form is
// accepted for AnimeBytes alone (SeaDex's documented AB shape, resolved
// against animebytes.tv by the publisher); opaque non-hierarchical forms fail
// closed.
func TestTrackerKeyRejectsForeignHostURLs(t *testing.T) {
	tests := []struct {
		name    string
		tracker string
		url     string
		want    string
	}{
		{"nyaa on its own host keys", "Nyaa", "https://nyaa.si/view/123", "nyaa:123"},
		// The absolute-root FQDN spelling denotes the same host, and the
		// identity gate normalizes it (isCanonicalTrackerHost trims one
		// trailing dot before folding): without these cases a change dropping
		// that trim silently stops curating and journaling every release whose
		// SeaDex URL carries the trailing dot, and no test fails.
		{"nyaa trailing-dot FQDN keys the apex site", "Nyaa", "https://nyaa.si./view/123", "nyaa:123"},
		{"ab trailing-dot FQDN keys the apex site", "AB", "https://animebytes.tv./torrents.php?id=1&torrentid=456", "ab:456"},
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
// trackerOwnForm answers false for a scope outside the nyaa/ab vocabulary,
// so any future caller reaching these helpers directly still fails closed
// on the curation trust boundary.
func TestTrackerIDHelpersFailClosedOnUnparseableInput(t *testing.T) {
	if got := nyaaID("http://[::1"); got != "" {
		t.Errorf("nyaaID(unparseable) = %q, want empty", got)
	}
	if got := animeBytesID("http://[::1"); got != "" {
		t.Errorf("animeBytesID(unparseable) = %q, want empty", got)
	}
	if ownURL("other", "https://nyaa.si/view/1") {
		t.Error("trackerOwnForm(unknown scope) = true, want false (fail closed)")
	}
}

// ownURL is the raw-string spelling of trackerOwnForm for the ownership tables:
// production callers classify once and keep the form (so they can extract the id
// from the same reading, h-f8), while a table case reads better as the URL text
// it pins.
func ownURL(scope, raw string) bool {
	f := urlform.Classify(raw)
	return trackerOwnForm(scope, &f)
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
// TestTrackerIDHelpersFailClosedOnUnparseableInput pins for trackerOwnForm.
func TestTrackerIDUnknownScopeFailsClosed(t *testing.T) {
	if got := trackerID("other", "https://nyaa.si/view/123"); got != "" {
		t.Errorf(`trackerID("other", ...) = %q, want empty (fail closed on an unknown scope)`, got)
	}
	if got := trackerID("", "/torrents.php?torrentid=123"); got != "" {
		t.Errorf(`trackerID("", ...) = %q, want empty (fail closed on an empty scope)`, got)
	}
}

// TestTrackerOwnURLReadsOneStructuralVocabulary pins the urlform adoption at
// the writer-side admission gate (l-f162). It used to hand-roll the raw-URL
// vocabulary with net/url, and the two readings had already diverged on a live
// shape: for a schemeless-host SeaDex URL, urlform reports host evidence (so
// trackerlink.Publish and internal/filter's AB gate treat it as AnimeBytes and
// publish the link) while the triple-empty net/url test
// (Scheme=="" && Host=="" && Opaque=="") called the same string a "true relative
// reference" and admitted it here - after which the id extraction found nothing
// and the release was silently dropped as unresolvable. One string, two
// structural readings, one app.
//
// The gate now reads urlform throughout: a host-bearing form is judged on its
// host evidence and must be a userinfo-free http(s) URL on the exact canonical
// host, in either of that URL's two spellings (absolute, or the scheme-free
// form a browser reads the same way - admitted since l-f19), and only a ROOTED
// relative reference takes the AB relative arm. The relative arm stays ClassRelative rather than the narrower
// tracker.LookupByRelativeURL (which also demands the
// "/torrents.php?...torrentid=" shape), so a relative Prowlarr permalink keeps
// working.
func TestTrackerOwnURLReadsOneStructuralVocabulary(t *testing.T) {
	tests := map[string]struct {
		scope string
		raw   string
		want  bool
	}{
		"absolute canonical nyaa":       {upstreamNyaa, "https://nyaa.si/view/1234567", true},
		"absolute canonical ab":         {upstreamAB, "https://animebytes.tv/torrents.php?id=1&torrentid=456", true},
		"ab permalink form":             {upstreamAB, "https://animebytes.tv/torrent/1167293/group", true},
		"rooted relative is ab's own":   {upstreamAB, "/torrents.php?id=1&torrentid=456", true},
		"rooted relative permalink":     {upstreamAB, "/torrent/1167293/group", true},
		"rooted relative is not nyaa's": {upstreamNyaa, "/view/1234567", false},
		"nyaa subdomain refused":        {upstreamNyaa, "https://sukebei.nyaa.si/view/123", false},
		"foreign host refused":          {upstreamNyaa, "https://evil.example/view/123", false},
		"userinfo authority refused":    {upstreamAB, "https://evil@animebytes.tv/torrents.php?torrentid=1", false},
		"non-http scheme refused":       {upstreamAB, "javascript:/torrents.php?torrentid=456", false},
		// The divergent shape: host evidence to urlform, so it takes the HOST
		// arm and is judged on its host like any other absolute-ish form - and
		// admitted there (l-f19), which is what stops the daemon alerting on a
		// release the feed omits. The host policy still decides everything.
		"schemeless canonical ab host admitted":     {upstreamAB, "animebytes.tv/torrents.php?id=1&torrentid=456", true},
		"schemeless canonical nyaa host admitted":   {upstreamNyaa, "nyaa.si/view/1234567", true},
		"schemeless foreign host refused":           {upstreamAB, "evil.example/torrents.php?torrentid=456", false},
		"schemeless subdomain refused":              {upstreamNyaa, "sukebei.nyaa.si/view/123", false},
		"schemeless canonical host wrong scope":     {upstreamNyaa, "animebytes.tv/torrents.php?torrentid=456", false},
		"schemeless userinfo authority refused":     {upstreamAB, "evil@animebytes.tv/torrents.php?torrentid=1", false},
		"schemeless backslash refused":              {upstreamAB, `animebytes.tv\torrents.php?torrentid=1`, false},
		"schemeless tab-smuggled host refused":      {upstreamAB, "animeby\ttes.tv/torrents.php?torrentid=1", false},
		"protocol-relative canonical host refused":  {upstreamAB, "//animebytes.tv/torrents.php?id=1&torrentid=456", false},
		"hidden-host canonical host refused":        {upstreamAB, "https:animebytes.tv/torrents.php?torrentid=456", false},
		"schemeless nyaa host under ab scope stays": {upstreamAB, "nyaa.si/view/123", false},
		// Smuggling forms: a browser and net/url read these differently, so
		// they must never prove a curation identity.
		"backslash authority refused": {upstreamAB, "\\torrents.php?torrentid=456", false},
		"tab-smuggled host refused":   {upstreamAB, "https://animeby\ttes.tv/torrents.php?torrentid=1", false},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := ownURL(tc.scope, tc.raw); got != tc.want {
				t.Errorf("trackerOwnForm(%q, %q) = %v, want %v", tc.scope, tc.raw, got, tc.want)
			}
		})
	}
}

// TestTrackerKeyRejectsNonHTTPTrackerURLs pins the HTTP(S)-only half of the
// curation gate through the exported-to-the-package entry point: a host-bearing
// URL on the canonical tracker host but on another scheme must not authorize a
// tracker id. The table above cannot protect that branch - its javascript: case
// is an opaque form with no recovered host, so it is refused before the scheme
// check is reached - and without this case an ftp:// URL on nyaa.si /
// animebytes.tv would mint a curation key for RSS and search matching.
func TestTrackerKeyRejectsNonHTTPTrackerURLs(t *testing.T) {
	tests := []struct {
		name, tracker, raw string
	}{
		{"Nyaa FTP URL", "Nyaa", "ftp://nyaa.si/view/123"},
		{"AnimeBytes FTP URL", "AB", "ftp://animebytes.tv/torrents.php?id=1&torrentid=456"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := trackerKey(tc.tracker, tc.raw); got != "" {
				t.Errorf("trackerKey(%q, %q) = %q, want empty for a non-HTTP(S) URL", tc.tracker, tc.raw, got)
			}
		})
	}
}

// TestTrackerKeysReadTheVouchedForm pins the classify-once contract on both key
// builders (h-f8). Ownership is decided on urlform's reading of the raw string -
// which preprocesses edge padding the way a browser does - so the id must be
// extracted from that same reading (Form.Trimmed). Before the fix the original
// spelling reached nyaaID/animeBytesID, whose net/url parse kept the padding, so
// an edge-padded SeaDex or Prowlarr URL passed ownership and then minted no key:
// the release was silently absent from the curation set and the RSS journal.
//
// The strictness of nyaaID/animeBytesID is unchanged; they simply receive a
// cleaned string. Everything the ownership gate refuses is still refused before
// an id is read, so edge padding is the ONLY family that newly keys - the
// refusal rows below are the regression guard for that.
func TestTrackerKeysReadTheVouchedForm(t *testing.T) {
	tests := map[string]struct {
		tracker, sourceURL string
		// wantKey is the expected key from trackerKey; "" means refused.
		wantKey string
		// wantFromURL is the expected key from trackerKeyFromURL (which admits
		// absolute display URLs only); "" means refused.
		wantFromURL string
	}{
		"nyaa leading tab":      {"Nyaa", "\thttps://nyaa.si/view/123", "nyaa:123", "nyaa:123"},
		"nyaa leading nul":      {"Nyaa", "\x00https://nyaa.si/view/123", "nyaa:123", "nyaa:123"},
		"nyaa leading space":    {"Nyaa", " https://nyaa.si/view/123", "nyaa:123", "nyaa:123"},
		"nyaa trailing cr":      {"Nyaa", "https://nyaa.si/view/123\r", "nyaa:123", "nyaa:123"},
		"nyaa trailing nul":     {"Nyaa", "https://nyaa.si/view/123\x00", "nyaa:123", "nyaa:123"},
		"nyaa trailing space":   {"Nyaa", "https://nyaa.si/view/123 ", "nyaa:123", "nyaa:123"},
		"ab absolute pad":       {"AB", "\x00https://animebytes.tv/torrents.php?id=1&torrentid=456", "ab:456", "ab:456"},
		"ab permalink pad":      {"AB", " https://animebytes.tv/torrent/456/group\r", "ab:456", "ab:456"},
		"ab relative pad":       {"AB", " /torrents.php?id=1&torrentid=456", "ab:456", ""},
		"ab relative pad nul":   {"AB", "/torrents.php?id=1&torrentid=456\x00", "ab:456", ""},
		"ab relative permalink": {"AB", " /torrent/456/group\n", "ab:456", ""},
		// Regression guards: every shape the ownership gate refuses stays
		// refused, so passing the cleaned form downstream cannot widen identity.
		"embedded tab refused":     {"Nyaa", "https://nyaa\t.si/view/123", "", ""},
		"embedded lf refused":      {"Nyaa", "https://nyaa.si/view\n/123", "", ""},
		"backslash refused":        {"Nyaa", "https:/\\nyaa.si/view/123", "", ""},
		"hidden host refused":      {"Nyaa", "https:nyaa.si/view/123", "", ""},
		"unparseable refused":      {"Nyaa", "http://[::1", "", ""},
		"foreign host refused":     {"Nyaa", "https://evil.example/view/123", "", ""},
		"subdomain refused":        {"Nyaa", "https://sukebei.nyaa.si/view/123", "", ""},
		"userinfo refused":         {"Nyaa", "https://evil@nyaa.si/view/123", "", ""},
		"non-http scheme refused":  {"Nyaa", "ftp://nyaa.si/view/123", "", ""},
		"inner space mints no id":  {"Nyaa", "https://nyaa.si/view/ 123", "", ""},
		"non-numeric mints no id":  {"Nyaa", "https://nyaa.si/view/abc ", "", ""},
		"padded label still gated": {"Nyaa", " https://animebytes.tv/torrent/456/group", "", "ab:456"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := trackerKey(tc.tracker, tc.sourceURL); got != tc.wantKey {
				t.Errorf("trackerKey(%q, %q) = %q, want %q", tc.tracker, tc.sourceURL, got, tc.wantKey)
			}
			if got := trackerKeyFromURL(tc.sourceURL); got != tc.wantFromURL {
				t.Errorf("trackerKeyFromURL(%q) = %q, want %q", tc.sourceURL, got, tc.wantFromURL)
			}
		})
	}
}

// TestSchemelessHostKeysAsItsAbsoluteSpelling pins l-f19: a SeaDex record may
// spell a tracker page without its scheme ("animebytes.tv/torrents.php?...").
// The publisher and the AnimeBytes evidence gate have always read that as host
// evidence and published a working https link, while this package refused it -
// so the daemon emitted "better release available" for a release the Torznab
// feed silently omitted as unresolvable, breaking the invariant that one cycle
// means an alert and the feed cannot diverge.
//
// The two spellings must produce the SAME key, not merely a non-empty one: they
// name one torrent, so a record that changes spelling (or a catalogue carrying
// both) must not enter the curation set and the seen ledger twice. That
// equality is intended same-tracker deduplication, and it is also what makes
// the journal GUID (the canonical absolute URL) round-trip to the same key.
func TestSchemelessHostKeysAsItsAbsoluteSpelling(t *testing.T) {
	pairs := map[string]struct {
		tracker, schemeless, absolute string
	}{
		"ab site form":  {"AB", "animebytes.tv/torrents.php?id=1&torrentid=456", "https://animebytes.tv/torrents.php?id=1&torrentid=456"},
		"ab permalink":  {"AB", "animebytes.tv/torrent/1167293/group", "https://animebytes.tv/torrent/1167293/group"},
		"nyaa view":     {"Nyaa", "nyaa.si/view/1234567", "https://nyaa.si/view/1234567"},
		"nyaa fqdn dot": {"Nyaa", "nyaa.si./view/1234567", "https://nyaa.si./view/1234567"},
	}
	for name, tc := range pairs {
		t.Run(name, func(t *testing.T) {
			want := trackerKey(tc.tracker, tc.absolute)
			if want == "" {
				t.Fatalf("trackerKey(%q, %q) = %q, want the absolute spelling to key (test premise)", tc.tracker, tc.absolute, want)
			}
			if got := trackerKey(tc.tracker, tc.schemeless); got != want {
				t.Errorf("trackerKey(%q, %q) = %q, want the absolute spelling's key %q", tc.tracker, tc.schemeless, got, want)
			}
			// The download builder is the second SeaDex-source consumer of the
			// same normalization; if it drifted, a keyed release would journal
			// with no grabbable link.
			wantURL, wantOK := downloadURL(tc.tracker, tc.absolute, "pk")
			if !wantOK {
				t.Fatalf("downloadURL(%q, %q) ok=false, want a link (test premise)", tc.tracker, tc.absolute)
			}
			gotURL, gotOK := downloadURL(tc.tracker, tc.schemeless, "pk")
			if !gotOK || gotURL != wantURL {
				t.Errorf("downloadURL(%q, %q) = %q, ok=%v, want the absolute spelling's %q", tc.tracker, tc.schemeless, gotURL, gotOK, wantURL)
			}
		})
	}
}

// TestSchemelessHostAdmissionKeepsEveryRefusal is the regression guard for the
// admission above: it widens exactly one spelling of a canonical-host URL and
// nothing else. Every refusal a schemeless form could plausibly smuggle through
// is asserted at the key builders, past the ownership table, because these are
// the functions whose output authorizes curation and a download link.
//
// The strict id extractors are deliberately unchanged, so a schemeless form
// whose route is not the tracker's own still mints nothing - the normalization
// hands them a properly-schemed string, it does not relax them.
func TestSchemelessHostAdmissionKeepsEveryRefusal(t *testing.T) {
	tests := map[string]struct{ tracker, sourceURL string }{
		"foreign host":                 {"AB", "evil.example/torrents.php?id=1&torrentid=456"},
		"foreign host with nyaa route": {"Nyaa", "evil.example/view/123"},
		"subdomain of canonical host":  {"Nyaa", "sukebei.nyaa.si/view/123"},
		"canonical host wrong scope":   {"Nyaa", "animebytes.tv/torrents.php?id=1&torrentid=456"},
		"userinfo authority":           {"AB", "evil@animebytes.tv/torrents.php?torrentid=456"},
		"backslash authority":          {"AB", `animebytes.tv\torrents.php?torrentid=456`},
		"embedded tab":                 {"Nyaa", "nyaa\t.si/view/123"},
		"embedded newline":             {"Nyaa", "nyaa.si/view\n/123"},
		"protocol-relative":            {"AB", "//animebytes.tv/torrents.php?id=1&torrentid=456"},
		"hidden host":                  {"AB", "https:animebytes.tv/torrents.php?torrentid=456"},
		"homograph host":               {"Nyaa", "nyaa.sı/view/123"},
		"route not the tracker's own":  {"AB", "animebytes.tv/not-a-torrent?torrentid=456"},
		"nyaa route not anchored":      {"Nyaa", "nyaa.si/redirect/view/123"},
		"dot segments in the route":    {"Nyaa", "nyaa.si/view/123/../456"},
		"non-numeric id":               {"AB", "animebytes.tv/torrents.php?torrentid=abc"},
		"duplicate torrentid":          {"AB", "animebytes.tv/torrents.php?torrentid=1&torrentid=2"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := trackerKey(tc.tracker, tc.sourceURL); got != "" {
				t.Errorf("trackerKey(%q, %q) = %q, want empty", tc.tracker, tc.sourceURL, got)
			}
			if got, ok := downloadURL(tc.tracker, tc.sourceURL, "pk"); ok {
				t.Errorf("downloadURL(%q, %q) = %q, ok=true, want refused", tc.tracker, tc.sourceURL, got)
			}
		})
	}
}

// TestTrackerKeyFromURLStaysAbsoluteOnly pins the deliberate asymmetry between
// the two key builders. trackerKeyFromURL reads a PROWLARR item's display URL,
// which is a real absolute link the arr will render and follow, so it keeps the
// shared display gate (httpDisplayForm) and never accepts the scheme-free
// spelling - only SeaDex records carry that shape, and only trackerKey and
// downloadTarget normalize it.
func TestTrackerKeyFromURLStaysAbsoluteOnly(t *testing.T) {
	for _, raw := range []string{
		"animebytes.tv/torrents.php?id=1&torrentid=456",
		"animebytes.tv/torrent/1167293/group",
		"nyaa.si/view/1234567",
	} {
		if got := trackerKeyFromURL(raw); got != "" {
			t.Errorf("trackerKeyFromURL(%q) = %q, want empty (display URLs must be absolute)", raw, got)
		}
	}
}
