package indexer

import (
	"strings"
	"testing"
)

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

// FuzzExtractID_roundTripsNumericIDs pins the acceptance side of extractID that
// the digits-or-empty target above cannot: a numeric id of any width embedded in
// each of the three supported URL forms (Nyaa /view, AB permalink, AB
// torrentid=) round-trips intact, so a reject-all parser cannot pass.
func FuzzExtractID_roundTripsNumericIDs(f *testing.F) {
	f.Add(byte(3), byte(7))
	f.Add(byte(0), byte(0))
	f.Fuzz(func(t *testing.T, digit, width byte) {
		id := strings.Repeat(string(rune('0'+digit%10)), int(width%32)+1)
		for _, tc := range []struct {
			raw, needle string
		}{
			{"https://nyaa.si/view/" + id + "?x=1", "/view/"},
			{"https://animebytes.tv/torrent/" + id + "/group", "/torrent/"},
			{"/torrents.php?id=1&torrentid=" + id + "&x=1", "torrentid="},
		} {
			if got := extractID(tc.raw, tc.needle); got != id {
				t.Errorf("extractID(%q, %q) = %q, want %q", tc.raw, tc.needle, got, id)
			}
		}
	})
}
