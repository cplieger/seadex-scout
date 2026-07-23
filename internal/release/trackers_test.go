package release

import (
	"net/url"
	"testing"

	"github.com/cplieger/urlform"
)

// TestLookupTrackerByHostFailClosed pins the fail-closed guards of the
// URL-host tracker resolver consumed by the seadex link-safety gate
// (usableAbsolute) and the host twins (IsAnimeBytesHost/IsNyaaHost): an
// empty host, a bare DNS-root dot, whitespace-only input, an empty-labeled
// host (a leading dot or an inner ".." - no resolvable DNS name has an empty
// label), and a non-ASCII homograph label never match, and neither a
// suffix-confusion host nor a parent-domain spoof survives the dot-delimited
// comparison. Positive cases pin the documented tolerance: exact host,
// real dot-delimited subdomain, case folding, and one DNS-root trailing dot.
func TestLookupTrackerByHostFailClosed(t *testing.T) {
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
		{host: "nyaa.si", wantName: TrackerNameNyaa, wantOK: true},
		{host: "sub.nyaa.si", wantName: TrackerNameNyaa, wantOK: true},
		{host: "sukebei.nyaa.si", wantName: TrackerNameNyaa, wantOK: true},
		{host: "NYAA.SI", wantName: TrackerNameNyaa, wantOK: true},
		{host: "nyaa.si.", wantName: TrackerNameNyaa, wantOK: true},
		{host: "animebytes.tv", wantName: TrackerNameAnimeBytes, wantOK: true},
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
			got, ok := LookupTrackerByHost(tc.host)
			if ok != tc.wantOK {
				t.Errorf("LookupTrackerByHost(%q) ok = %v, want %v", tc.host, ok, tc.wantOK)
				return
			}
			if ok && got.Name != tc.wantName {
				t.Errorf("LookupTrackerByHost(%q) = %q, want %q", tc.host, got.Name, tc.wantName)
			}
		})
	}
}

// TestLookupTrackerByHostPinsHostSet pins the host allowlist the URL-host
// resolver derives from the tracker table (one https site host per canonical
// tracker, order-insensitive by construction), so a table edit that drops or
// respells a tracker's site cannot silently shrink the allowlist the seadex
// link-safety gate keys on; an unknown host never matches.
func TestLookupTrackerByHostPinsHostSet(t *testing.T) {
	wantHosts := map[string]string{
		"animebytes.tv":  TrackerNameAnimeBytes,
		"animetosho.org": TrackerNameAnimeTosho,
		"nyaa.si":        TrackerNameNyaa,
		"rutracker.org":  TrackerNameRuTracker,
	}
	for host, wantName := range wantHosts {
		got, ok := LookupTrackerByHost(host)
		if !ok {
			t.Errorf("LookupTrackerByHost(%q) not found, want %q", host, wantName)
			continue
		}
		if got.Name != wantName {
			t.Errorf("LookupTrackerByHost(%q) = %q, want %q", host, got.Name, wantName)
		}
	}
	if len(trackerByHost) != len(wantHosts) {
		t.Errorf("trackerByHost has %d entries, want %d: a tracker site was added or dropped without updating this pin", len(trackerByHost), len(wantHosts))
	}
	if _, ok := LookupTrackerByHost("example.com"); ok {
		t.Error("LookupTrackerByHost(example.com) found, want not found")
	}
}

// TestTrackerTableBaseURLsAreHTTPS pins the shape of every canonical table
// entry's BaseURL: it must parse, carry the https scheme, and yield a
// non-empty hostname. The BaseURLs seed both the host allowlist
// (trackerByHost) and the link/download-URL builders, so a table edit that
// downgrades a tracker to http or breaks its URL would silently weaken every
// consumer; the host-set pin above does not guard the scheme.
func TestTrackerTableBaseURLsAreHTTPS(t *testing.T) {
	for _, tr := range trackerTable {
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

// TestIsASCIIHost pins the ASCII homograph gate's byte-boundary contract as
// a consumer contract (the gate now lives in the urlform library;
// LookupTrackerByHost keys its fail-closed rejection on it, and
// filter.ABVisible calls it
// without going through LookupTrackerByHost, since its fail direction
// inverts the lookup): every byte below utf8.RuneSelf is ASCII - 0x7F (DEL),
// the last ASCII byte, passes - while 0x80 (utf8.RuneSelf itself, the first
// non-ASCII byte and a UTF-8 continuation-byte value) and
// any multi-byte sequence are rejected; the empty string is vacuously ASCII
// (the callers own the empty-host policy).
func TestIsASCIIHost(t *testing.T) {
	tests := []struct {
		name string
		host string
		want bool
	}{
		{name: "plain tracker host", host: "animebytes.tv", want: true},
		{name: "digits and hyphen", host: "sub-01.nyaa.si", want: true},
		{name: "empty string is vacuously ASCII", host: "", want: true},
		{name: "DEL 0x7F is the last ASCII byte", host: "del\x7f.example", want: true},
		{name: "0x80 the first non-ASCII byte is rejected", host: "a\x80b", want: false},
		{name: "latin-1 accented label is rejected", host: "x\u00e9.nyaa.si", want: false},
		{name: "fullwidth dot spelling is rejected", host: "animebytes\uff0etv", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := urlform.IsASCIIHost(tc.host); got != tc.want {
				t.Errorf("urlform.IsASCIIHost(%q) = %v, want %v", tc.host, got, tc.want)
			}
		})
	}
}

// TestLookupTrackerByRelativeURL pins the structural relative-URL tracker
// resolver consumed by filter's AB evidence gate and seadex's link publisher:
// only SeaDex's documented AnimeBytes relative page shape - a rooted
// "/torrents.php" path carrying a "torrentid" query parameter - resolves (to
// the canonical AnimeBytes table entry), case-insensitively on the path.
// Everything else fails closed: an absolute URL (tracker identity must then
// come from the host gate, never this shape), a protocol-relative or
// schemeless-host form, a different relative path, a torrentid-less
// torrents.php query, and the empty string.
func TestLookupTrackerByRelativeURL(t *testing.T) {
	tests := []struct {
		name   string
		raw    string
		wantOK bool
	}{
		{name: "documented AB relative shape", raw: "/torrents.php?id=12345&torrentid=1167293", wantOK: true},
		{name: "torrentid alone", raw: "/torrents.php?torrentid=1", wantOK: true},
		{name: "path case-insensitive", raw: "/TORRENTS.PHP?torrentid=1", wantOK: true},
		{name: "surrounding whitespace tolerated", raw: "  /torrents.php?torrentid=1  ", wantOK: true},
		{name: "missing torrentid", raw: "/torrents.php?id=12345", wantOK: false},
		{name: "no query at all", raw: "/torrents.php", wantOK: false},
		{name: "different relative path", raw: "/view/1918784", wantOK: false},
		{name: "subpath is not the AB page", raw: "/torrents.php/extra?torrentid=1", wantOK: false},
		{name: "absolute AB URL is not a relative shape", raw: "https://animebytes.tv/torrents.php?torrentid=1", wantOK: false},
		{name: "protocol-relative form is not relative", raw: "//animebytes.tv/torrents.php?torrentid=1", wantOK: false},
		{name: "schemeless host form is not relative", raw: "animebytes.tv/torrents.php?torrentid=1", wantOK: false},
		{name: "empty string", raw: "", wantOK: false},
		{name: "whitespace only", raw: "   ", wantOK: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := LookupTrackerByRelativeURL(tc.raw)
			if ok != tc.wantOK {
				t.Errorf("LookupTrackerByRelativeURL(%q) ok = %v, want %v", tc.raw, ok, tc.wantOK)
				return
			}
			if ok && got.Name != TrackerNameAnimeBytes {
				t.Errorf("LookupTrackerByRelativeURL(%q) = %q, want %q", tc.raw, got.Name, TrackerNameAnimeBytes)
			}
		})
	}
}
