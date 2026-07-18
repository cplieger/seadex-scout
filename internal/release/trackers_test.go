package release

import (
	"net/url"
	"testing"
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

// TestIsASCIIHost pins the exported ASCII homograph gate's byte-boundary
// contract directly in its defining package (filter.ABVisible calls it
// without going through LookupTrackerByHost, since its fail direction
// inverts the lookup): every byte below utf8.RuneSelf is ASCII - 0x7F (DEL),
// the last ASCII byte, passes - while 0x80 (utf8.RuneSelf itself, the first
// non-ASCII byte and the lead byte of many UTF-8 homograph encodings) and
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
			if got := IsASCIIHost(tc.host); got != tc.want {
				t.Errorf("IsASCIIHost(%q) = %v, want %v", tc.host, got, tc.want)
			}
		})
	}
}
