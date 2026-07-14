package indexer

import (
	"strings"
	"testing"
)

// TestTrackerScope pins the two documented contracts of trackerScope that the
// other indexer tests only exercise indirectly: the defensive "animebytes"
// alias for the "AB" spelling, and the tail-drop default (any unknown tracker
// maps to "") that makes feedItemFor/downloadURL exclude those releases from
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

// FuzzExtractID_alwaysDigitsOrEmpty pins the security-relevant invariant of the id
// extraction that runs on Prowlarr-supplied (tracker-controlled) URL strings: every id
// it returns is a non-empty run of ASCII digits, or it returns "" - a bogus tracker key
// (a non-numeric id) must never reach the curation match set. The seed corpus covers the
// Nyaa /view, AnimeBytes permalink, and AnimeBytes torrentid= forms plus a non-numeric id.
func FuzzExtractID_alwaysDigitsOrEmpty(f *testing.F) {
	f.Add("https://nyaa.si/view/1234567")
	f.Add("https://animebytes.tv/torrent/1167293/group?nh=709E38EC")
	f.Add("/torrents.php?id=70543&torrentid=1143533")
	f.Add("https://nyaa.si/view/12a45")
	f.Add("")
	f.Fuzz(func(t *testing.T, raw string) {
		for _, needle := range []string{"/view/", "/torrent/", "torrentid="} {
			if id := extractID(raw, needle); id != "" && !isAllDigits(id) {
				t.Fatalf("extractID(%q, %q) = %q, want all digits or empty", raw, needle, id)
			}
		}
		if id := animeBytesID(raw); id != "" && !isAllDigits(id) {
			t.Fatalf("animeBytesID(%q) = %q, want all digits or empty", raw, id)
		}
		if k := trackerKeyFromURL(raw); k != "" {
			_, id, found := strings.Cut(k, ":")
			if !found || !isAllDigits(id) {
				t.Fatalf("trackerKeyFromURL(%q) = %q, want scope:<digits>", raw, k)
			}
		}
	})
}
