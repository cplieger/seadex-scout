package release

import "testing"

// TestLookupTrackerByHostFailClosed pins the fail-closed guards of the
// URL-host tracker resolver consumed by the seadex link-safety gate
// (usableAbsolute) and the host twins (IsAnimeBytesHost/IsNyaaHost): an
// empty host, a bare DNS-root dot, and whitespace-only input never match,
// and neither a suffix-confusion host nor a parent-domain spoof survives the
// dot-delimited comparison. Positive cases pin the documented tolerance:
// exact host, dot-delimited subdomain, case folding, and one DNS-root
// trailing dot.
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
		{host: "NYAA.SI", wantName: TrackerNameNyaa, wantOK: true},
		{host: "nyaa.si.", wantName: TrackerNameNyaa, wantOK: true},
		{host: "animebytes.tv", wantName: TrackerNameAnimeBytes, wantOK: true},
		// Fail-closed lookalikes: suffix confusion and parent-domain spoof.
		{host: "evil-nyaa.si", wantOK: false},
		{host: "evilnyaa.si", wantOK: false},
		{host: "nyaa.si.evil.com", wantOK: false},
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
