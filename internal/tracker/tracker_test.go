package tracker

import (
	"net/url"
	"testing"

	"github.com/cplieger/urlform"
)

// TestLookupByHostFailClosed pins the fail-closed guards of the
// URL-host tracker resolver consumed by the link-safety gate
// (trackerlink.usableAbsolute) and the host twins
// (IsAnimeBytesHost/IsNyaaHost): an
// empty host, a bare DNS-root dot, whitespace-only input, an empty-labeled
// host (a leading dot or an inner ".." - no resolvable DNS name has an empty
// label), and a non-ASCII homograph label never match, and neither a
// suffix-confusion host nor a parent-domain spoof survives the dot-delimited
// comparison. Positive cases pin the documented tolerance: exact host,
// real dot-delimited subdomain, case folding, and one DNS-root trailing dot.
func TestLookupByHostFailClosed(t *testing.T) {
	tests := []struct {
		host     string
		wantName string
		wantOK   bool
	}{
		// Fail-closed degenerate inputs.
		{host: "", wantOK: false},
		{host: ".", wantOK: false},
		{host: "   ", wantOK: false},
		// Exact / subdomain / trailing-dot / case-insensitive matches.
		{host: "nyaa.si", wantName: NameNyaa, wantOK: true},
		{host: "sub.nyaa.si", wantName: NameNyaa, wantOK: true},
		{host: "sukebei.nyaa.si", wantName: NameNyaa, wantOK: true},
		{host: "NYAA.SI", wantName: NameNyaa, wantOK: true},
		{host: "nyaa.si.", wantName: NameNyaa, wantOK: true},
		{host: "animebytes.tv", wantName: NameAnimeBytes, wantOK: true},
		// Fail-closed lookalikes: suffix confusion and parent-domain spoof.
		{host: "evil-nyaa.si", wantOK: false},
		{host: "evilnyaa.si", wantOK: false},
		{host: "nyaa.si.evil.com", wantOK: false},
		// Fail-closed empty labels: plain suffix matching would accept these,
		// but no resolvable DNS name carries an empty label.
		{host: ".nyaa.si", wantOK: false},
		{host: "a..nyaa.si", wantOK: false},
		{host: ".animebytes.tv", wantOK: false},
		// Fail-closed non-ASCII: a homograph label under a tracker domain and
		// a fullwidth-dot spelling of the domain itself never classify.
		{host: "x\u00e9.nyaa.si", wantOK: false},
		{host: "animebytes\uff0etv", wantOK: false},
		// Fail-closed fold-laundering homographs: the ASCII gate must run on
		// the RAW host, because strings.ToLower folds U+0130 (LATIN CAPITAL
		// LETTER I WITH DOT ABOVE) to ASCII 'i' and U+212A (KELVIN SIGN) to
		// ASCII 'k' - a pre-gate fold would launder these into the canonical
		// hosts and classify them.
		{host: "an\u0130mebytes.tv", wantOK: false},
		{host: "rutrac\u212Aer.org", wantOK: false},
		// Fail-closed trim-laundering whitespace: Unicode WHITESPACE is
		// non-ASCII host bytes too and must not be trimmed into a match
		// before the gate - strings.TrimSpace trims unicode.IsSpace (U+00A0
		// NBSP, U+3000 ideographic space), so a pre-gate trim would launder
		// a whitespace-decorated host into the canonical hosts.
		{host: "nyaa.si\u00a0", wantOK: false},
		{host: "\u00a0nyaa.si", wantOK: false},
		{host: "nyaa.si\u3000", wantOK: false},
	}
	for _, tc := range tests {
		t.Run(tc.host, func(t *testing.T) {
			got, ok := LookupByHost(tc.host)
			if ok != tc.wantOK {
				t.Errorf("LookupByHost(%q) ok = %v, want %v", tc.host, ok, tc.wantOK)
				return
			}
			if ok && got.Name != tc.wantName {
				t.Errorf("LookupByHost(%q) = %q, want %q", tc.host, got.Name, tc.wantName)
			}
		})
	}
}

// TestLookupByHostPinsHostSet pins the host allowlist the URL-host
// resolver derives from the tracker table (one https site host per canonical
// tracker, order-insensitive by construction), so a table edit that drops or
// respells a tracker's site cannot silently shrink the allowlist the
// link-safety gate keys on; an unknown host never matches.
func TestLookupByHostPinsHostSet(t *testing.T) {
	wantHosts := map[string]string{
		"animebytes.tv":  NameAnimeBytes,
		"animetosho.org": NameAnimeTosho,
		"nyaa.si":        NameNyaa,
		"rutracker.org":  NameRuTracker,
	}
	for host, wantName := range wantHosts {
		got, ok := LookupByHost(host)
		if !ok {
			t.Errorf("LookupByHost(%q) not found, want %q", host, wantName)
			continue
		}
		if got.Name != wantName {
			t.Errorf("LookupByHost(%q) = %q, want %q", host, got.Name, wantName)
		}
	}
	if len(byHost) != len(wantHosts) {
		t.Errorf("byHost has %d entries, want %d: a tracker site was added or dropped without updating this pin", len(byHost), len(wantHosts))
	}
	if _, ok := LookupByHost("example.com"); ok {
		t.Error("LookupByHost(example.com) found, want not found")
	}
}

// TestTableBaseURLsAreHTTPS pins the shape of every canonical table
// entry's BaseURL: it must parse, carry the https scheme, and yield a
// non-empty hostname. The BaseURLs seed both the host allowlist
// (byHost) and the link/download-URL builders, so a table edit that
// downgrades a tracker to http or breaks its URL would silently weaken every
// consumer; the host-set pin above does not guard the scheme.
func TestTableBaseURLsAreHTTPS(t *testing.T) {
	for _, tr := range table {
		u, err := url.Parse(tr.BaseURL)
		if err != nil {
			t.Errorf("tracker %s BaseURL %q does not parse: %v", tr.Name, tr.BaseURL, err)
			continue
		}
		if u.Scheme != "https" {
			t.Errorf("tracker %s BaseURL %q scheme = %q, want https", tr.Name, tr.BaseURL, u.Scheme)
		}
		if u.Hostname() == "" {
			t.Errorf("tracker %s BaseURL %q has an empty hostname", tr.Name, tr.BaseURL)
		}
	}
}

// TestLookupByHostRejectsClassifiedHomographs pins the cross-library
// behavior this app actually relies on, instead of unit-testing the urlform
// dependency (whose own suite already pins homograph preservation and
// IsASCIIHost's byte boundary): a fold-laundering homograph host classified
// by urlform.Classify must be preserved as non-ASCII evidence AND rejected by
// LookupByHost's ASCII gate. Removing the gate would let both planted
// subdomains pass HostMatchesDomain (strings.ToLower folds U+0130 to ASCII
// 'i' and U+212A to ASCII 'k'), so this test fails if either side launders
// or accepts a homograph.
func TestLookupByHostRejectsClassifiedHomographs(t *testing.T) {
	tests := []string{
		"https://an\u0130mebytes.tv/torrents.php?id=1",
		"https://rutrac\u212Aer.org/forum/viewtopic.php?t=1",
	}
	for _, raw := range tests {
		t.Run(raw, func(t *testing.T) {
			host := urlform.Classify(raw).Host
			if host == "" {
				t.Fatalf("urlform.Classify(%q).Host is empty, want preserved homograph evidence", raw)
			}
			if got, ok := LookupByHost(host); ok {
				t.Errorf("LookupByHost(%q) = %q, want no match for classified non-ASCII host", host, got.Name)
			}
		})
	}
}

// TestLookupByRelativeURL pins the structural relative-URL tracker
// resolver consumed by filter's AB evidence gate and the link publisher:
// only SeaDex's documented AnimeBytes relative page shape - a "/torrents.php"
// path carrying a "torrentid" query parameter - resolves (to the canonical
// AnimeBytes table entry), case-insensitively on the path. A host-less
// slashless value is read as that same path rooted (the href reading the link
// publisher resolves), so "torrents.php?...torrentid=..." resolves while
// "animebytes.tv/torrents.php?..." does not - its rooted reading is
// "/animebytes.tv/torrents.php", not the AB page path. Everything else fails
// closed: an absolute URL (tracker identity must then come from the host gate,
// never this shape), a protocol-relative form, a different relative path, a
// torrentid-less torrents.php query, and the empty string.
func TestLookupByRelativeURL(t *testing.T) {
	tests := []struct {
		name   string
		raw    string
		wantOK bool
	}{
		{name: "documented AB relative shape", raw: "/torrents.php?id=12345&torrentid=1167293", wantOK: true},
		{name: "torrentid alone", raw: "/torrents.php?torrentid=1", wantOK: true},
		{name: "path case-insensitive", raw: "/TORRENTS.PHP?torrentid=1", wantOK: true},
		{name: "slashless href reading of the AB page shape", raw: "torrents.php?id=1&torrentid=2", wantOK: true},
		{name: "backslash-rooted AB torrent page resolves", raw: `\torrents.php?id=1&torrentid=2`, wantOK: true},
		{name: "Unicode long-s is not ASCII path case", raw: "/torrent\u017f.php?torrentid=1", wantOK: false},
		{name: "surrounding whitespace tolerated", raw: "  /torrents.php?torrentid=1  ", wantOK: true},
		{name: "missing torrentid", raw: "/torrents.php?id=12345", wantOK: false},
		{name: "no query at all", raw: "/torrents.php", wantOK: false},
		{name: "different relative path", raw: "/view/1918784", wantOK: false},
		{name: "subpath is not the AB page", raw: "/torrents.php/extra?torrentid=1", wantOK: false},
		{name: "absolute AB URL is not a relative shape", raw: "https://animebytes.tv/torrents.php?torrentid=1", wantOK: false},
		{name: "protocol-relative form is not relative", raw: "//animebytes.tv/torrents.php?torrentid=1", wantOK: false},
		{name: "schemeless host form is not the AB page path", raw: "animebytes.tv/torrents.php?torrentid=1", wantOK: false},
		{name: "empty string", raw: "", wantOK: false},
		{name: "whitespace only", raw: "   ", wantOK: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := LookupByRelativeURL(tc.raw)
			if ok != tc.wantOK {
				t.Errorf("LookupByRelativeURL(%q) ok = %v, want %v", tc.raw, ok, tc.wantOK)
				return
			}
			if ok && got.Name != NameAnimeBytes {
				t.Errorf("LookupByRelativeURL(%q) = %q, want %q", tc.raw, got.Name, NameAnimeBytes)
			}
		})
	}
}

// TestTypeOf pins the obtainability class every Release fingerprint reads
// through this package: a canonical name and an accepted alias resolve to the
// table's class, and an unrecognized or empty name is Unknown rather than
// silently obtainable.
func TestTypeOf(t *testing.T) {
	tests := []struct {
		name string
		want Type
	}{
		{name: "Nyaa", want: Public},
		{name: "nyaa", want: Public},
		{name: "AB", want: Private},
		{name: "animebytes", want: Private},
		{name: "AnimeTosho", want: Public},
		{name: "beyondhd", want: Unknown},
		{name: "", want: Unknown},
	}
	for _, tc := range tests {
		if got := TypeOf(tc.name); got != tc.want {
			t.Errorf("TypeOf(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestTrackerHost pins Tracker.Host's documented fail-closed contract: the
// canonical lowercased site hostname when BaseURL parses to one (case folded,
// port and path excluded), and "" when it does not - an unparseable URL or one
// carrying no host at all. Both byHost (the host allowlist the link-safety
// gate and the host twins key on) and the indexer's canonical-host
// check consume this method, so a malformed table entry must yield no host
// rather than a partial one; the table-shape and host-set pins only ever see
// well-formed entries, leaving the fail-closed arm unexercised.
func TestTrackerHost(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		want    string
	}{
		{name: "canonical https base", baseURL: "https://nyaa.si", want: "nyaa.si"},
		{name: "uppercase host folds to lowercase", baseURL: "https://ANIMEBYTES.TV", want: "animebytes.tv"},
		{name: "port and path are not part of the host", baseURL: "https://nyaa.si:8080/view/1", want: "nyaa.si"},
		{name: "unparseable url fails closed", baseURL: "https://[::1", want: ""},
		{name: "control character in url fails closed", baseURL: "ht\x7ftps://nyaa.si", want: ""},
		{name: "url with no host fails closed", baseURL: "notaurl", want: ""},
		{name: "empty base url fails closed", baseURL: "", want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tr := Tracker{Name: "Test", Type: Public, BaseURL: tc.baseURL}
			if got := tr.Host(); got != tc.want {
				t.Errorf("Tracker{BaseURL: %q}.Host() = %q, want %q", tc.baseURL, got, tc.want)
			}
		})
	}
}

// TestEqualASCIIFold pins the ASCII-only case fold the AB relative-page shape
// check rests on: the fold is symmetric across both operands (either side may
// carry the uppercase spelling), a length mismatch never compares equal, and a
// non-ASCII lookalike never equals an ASCII protocol token - U+0130 (which
// strings.ToLower folds onto ASCII i) and U+017F (which regexp's SimpleFold
// folds onto ASCII s) must both stay unequal, since this comparison is the
// byte-wise gate that keeps a Unicode-laundered path or query name from
// classifying as the AnimeBytes torrent page.
func TestEqualASCIIFold(t *testing.T) {
	tests := []struct {
		a    string
		b    string
		want bool
	}{
		{a: "/torrents.php", b: "/torrents.php", want: true},
		{a: "/TORRENTS.PHP", b: "/torrents.php", want: true},
		{a: "/torrents.php", b: "/TORRENTS.PHP", want: true},
		{a: "TorrentID", b: "torrentid", want: true},
		{a: "torrentid", b: "TORRENTID", want: true},
		{a: "", b: "", want: true},
		{a: "torrentid", b: "torrentids", want: false},
		{a: "torrentid", b: "", want: false},
		{a: "torrentid", b: "torrentix", want: false},
		{a: "torrent\u0130", b: "torrentid", want: false},
		{a: "torrentid", b: "torrent\u0130", want: false},
		{a: "torrent\u017f", b: "torrents", want: false},
	}
	for _, tc := range tests {
		if got := equalASCIIFold(tc.a, tc.b); got != tc.want {
			t.Errorf("equalASCIIFold(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestLookupByHostMostSpecificWins pins the most-specific-match rule
// LookupByHost documents as its defense against Go's randomized map
// iteration order: when two canonical table hosts both match a host (one a
// subdomain of the other - the sukebei.nyaa.si-beside-nyaa.si shape the
// comment names), the longer canonical wins, deterministically. No table
// entry is a subdomain of another today, so bestLen is always 0 when a match
// lands and the length comparison is never exercised by any other test; this
// test swaps in a nested two-entry index so the comparison decides the
// answer. The repeat loop makes the map-order dependence a certain failure
// rather than a coin flip when the comparison is dropped.
func TestLookupByHostMostSpecificWins(t *testing.T) {
	parent := Tracker{Name: "ParentSite", Type: Public, BaseURL: "https://example.test"}
	child := Tracker{Name: "ChildSite", Type: Private, BaseURL: "https://sub.example.test"}
	saved := byHost
	byHost = map[string]Tracker{"example.test": parent, "sub.example.test": child}
	t.Cleanup(func() { byHost = saved })

	tests := []struct {
		host string
		want string
	}{
		{host: "sub.example.test", want: "ChildSite"},
		{host: "deep.sub.example.test", want: "ChildSite"},
		{host: "example.test", want: "ParentSite"},
		{host: "other.example.test", want: "ParentSite"},
	}
	for _, tc := range tests {
		t.Run(tc.host, func(t *testing.T) {
			for range 32 {
				got, ok := LookupByHost(tc.host)
				if !ok {
					t.Fatalf("LookupByHost(%q) not found, want %q", tc.host, tc.want)
				}
				if got.Name != tc.want {
					t.Fatalf("LookupByHost(%q) = %q, want the most specific match %q", tc.host, got.Name, tc.want)
				}
			}
		})
	}
}

// TestIsAnimeBytes pins the tracker-LABEL AnimeBytes predicate every consumer
// of the operator's opt-in toggle reads: the canonical name and the "AB" alias
// match (case- and whitespace-insensitively, through the table's alias set),
// any other tracker and the empty string do not.
func TestIsAnimeBytes(t *testing.T) {
	tests := []struct {
		tracker string
		want    bool
	}{
		{tracker: "AB", want: true},
		{tracker: " animebytes ", want: true},
		{tracker: "Nyaa", want: false},
		{tracker: "", want: false},
	}
	for _, tc := range tests {
		if got := IsAnimeBytes(tc.tracker); got != tc.want {
			t.Errorf("IsAnimeBytes(%q) = %v, want %v", tc.tracker, got, tc.want)
		}
	}
}

// TestIsAnimeBytesHost pins the AB host gate consumed by the AnimeBytes link
// hider (filter.ABVisible) and the indexer's tracker-key routing
// (trackerKeyFromURL): the exact site host, its real dot-delimited
// subdomains, and the DNS-root trailing-dot form match; a suffix-confusion
// host, a parent-domain spoof, an empty-labeled host, a non-ASCII homograph
// label, and any other tracker do not. The gate resolves through the
// canonical tracker table (LookupByHost), which folds case, so a
// mixed-case host matches too.
func TestIsAnimeBytesHost(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{host: "animebytes.tv", want: true},
		{host: "www.animebytes.tv", want: true},
		{host: "tracker.animebytes.tv", want: true},
		{host: "animebytes.tv.", want: true},
		{host: "www.animebytes.tv.", want: true},
		{host: "maliciousanimebytes.tv", want: false},
		{host: "evil-animebytes.tv", want: false},
		{host: "animebytes.tv.evil.com", want: false},
		{host: "animebytes.tv..", want: false},
		{host: ".animebytes.tv", want: false},
		{host: "a..animebytes.tv", want: false},
		{host: "x\u00e9.animebytes.tv", want: false},
		{host: "nyaa.si", want: false},
		{host: "AnimeBytes.tv", want: true},
		{host: "", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.host, func(t *testing.T) {
			if got := IsAnimeBytesHost(tc.host); got != tc.want {
				t.Errorf("IsAnimeBytesHost(%q) = %v, want %v", tc.host, got, tc.want)
			}
		})
	}
}

// TestLookup pins the canonical tracker table contract every consumer
// (classification, link building, indexer feed routing) resolves
// through: canonical names and aliases resolve case- and whitespace-
// insensitively to one entry with a non-empty base URL, and empty, unknown,
// or deliberately-stripped tracker names are not found.
func TestLookup(t *testing.T) {
	found := []struct {
		in       string
		wantName string
		wantType Type
	}{
		{in: "Nyaa", wantName: NameNyaa, wantType: Public},
		{in: "nyaa", wantName: NameNyaa, wantType: Public},
		{in: " AB ", wantName: NameAnimeBytes, wantType: Private},
		{in: "animebytes", wantName: NameAnimeBytes, wantType: Private},
		{in: "AnimeTosho", wantName: NameAnimeTosho, wantType: Public},
		{in: "RuTracker", wantName: NameRuTracker, wantType: Public},
	}
	for _, tc := range found {
		got, ok := Lookup(tc.in)
		if !ok {
			t.Errorf("Lookup(%q) not found, want %q", tc.in, tc.wantName)
			continue
		}
		if got.Name != tc.wantName || got.Type != tc.wantType {
			t.Errorf("Lookup(%q) = %q/%q, want %q/%q", tc.in, got.Name, got.Type, tc.wantName, tc.wantType)
		}
		if got.BaseURL == "" {
			t.Errorf("Lookup(%q) has an empty BaseURL; every table entry carries one", tc.in)
		}
	}
	for _, in := range []string{"", "   ", "beyondhd", "bhd", "passthepopcorn", "ptp", "broadcasthenet", "btn", "hdbits", "blutopia", "aither", "SomeRandomTracker"} {
		if _, ok := Lookup(in); ok {
			t.Errorf("Lookup(%q) found, want not found", in)
		}
	}
}

// TestCanonicalName pins the label ladder both human-facing renderers share
// (the daemon's alert attributes and the season report's links cell): host
// evidence outranks the untrusted SeaDex label, the label answers a host-less
// or unrecognized-host link, an unclaimed host names itself, and only a link
// with nothing nameable at all yields "".
func TestCanonicalName(t *testing.T) {
	cases := map[string]struct {
		label, rawURL, want string
	}{
		"host evidence wins over a blank label":  {"", "https://nyaa.si/view/1", NameNyaa},
		"host evidence wins over a wrong label":  {"AB", "https://nyaa.si/view/1", NameNyaa},
		"host evidence wins over an alias label": {"nyaa.si mirror", "https://animebytes.tv/t/1", NameAnimeBytes},
		"alias label answers a relative URL":     {"ab", "/torrents.php?id=1&torrentid=2", NameAnimeBytes},
		"odd-cased label answers a bare path":    {"  nYaA ", "/view/1", NameNyaa},
		"unclaimed host names itself":            {"  ", "https://example.org/t", "example.org"},
		"unknown label with no host is nameless": {"SomeTracker", "", ""},
		"no evidence at all is nameless":         {"", "http://", ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := CanonicalName(tc.label, tc.rawURL); got != tc.want {
				t.Errorf("CanonicalName(%q, %q) = %q, want %q", tc.label, tc.rawURL, got, tc.want)
			}
		})
	}
}
