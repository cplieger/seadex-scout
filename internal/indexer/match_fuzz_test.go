package indexer

import (
	"net/url"
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
	f.Add("https://nyaa.si/view/999999999999999999999")
	f.Add("")
	f.Fuzz(func(t *testing.T, raw string) {
		for _, needle := range []string{"/view/", "/torrent/", "torrentid="} {
			if id := extractID(raw, needle); id != "" && (!isAllDigits(id) || len(id) > maxTrackerIDDigits) {
				t.Fatalf("extractID(%q, %q) = %q, want a bounded run of digits or empty", raw, needle, id)
			}
		}
		if id := animeBytesID(raw); id != "" && (!isAllDigits(id) || len(id) > maxTrackerIDDigits) {
			t.Fatalf("animeBytesID(%q) = %q, want a bounded run of digits or empty", raw, id)
		}
		if k := trackerKeyFromURL(raw); k != "" {
			_, id, found := strings.Cut(k, ":")
			if !found || !isAllDigits(id) || len(id) > maxTrackerIDDigits {
				t.Fatalf("trackerKeyFromURL(%q) = %q, want scope:<bounded digits>", raw, k)
			}
		}
	})
}

// FuzzExtractID_roundTripsNumericIDs pins the acceptance side of extractID that
// the digits-or-empty target above cannot: a numeric id up to maxTrackerIDDigits wide embedded in
// each of the three supported URL forms (Nyaa /view, AB permalink, AB
// torrentid=) round-trips intact, so a reject-all parser cannot pass.
func FuzzExtractID_roundTripsNumericIDs(f *testing.F) {
	f.Add(byte(3), byte(7))
	f.Add(byte(0), byte(0))
	f.Fuzz(func(t *testing.T, digit, width byte) {
		id := strings.Repeat(string(rune('0'+digit%10)), int(width)%maxTrackerIDDigits+1)
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

// FuzzTrackerKeyFromURL_neverKeysFromQueryOrFragment pins the no-smuggling
// invariant the digits-or-empty target cannot: arbitrary content placed in a
// query value or fragment of a genuine tracker host must never yield a
// curation key, because only the path (Nyaa /view, AB permalink) and the
// torrentid query parameter may key.
func FuzzTrackerKeyFromURL_neverKeysFromQueryOrFragment(f *testing.F) {
	f.Add("/view/1234567")
	f.Add("/torrent/1167293/group")
	f.Add("torrentid=1143533")
	f.Fuzz(func(t *testing.T, payload string) {
		esc := url.QueryEscape(payload)
		for _, raw := range []string{
			"https://nyaa.si/?next=" + esc,
			"https://nyaa.si/#" + esc,
			"https://animebytes.tv/?next=" + esc,
			"https://animebytes.tv/#" + esc,
		} {
			if k := trackerKeyFromURL(raw); k != "" {
				t.Fatalf("trackerKeyFromURL(%q) = %q, want empty (query/fragment content must never key)", raw, k)
			}
		}
	})
}

// FuzzTrackerKey_keysOnlyTrackerOwnCanonicalURLs pins the SeaDex-side half
// of the curation trust boundary (trackerKey runs on tracker labels and URLs
// from untrusted SeaDex records; the Prowlarr-side twin is
// FuzzTrackerKeyFromURL_neverKeysFromQueryOrFragment): any non-empty key is
// scope:<bounded digits> for a supported scope, and - under the consumer's
// own interpretation (url.Parse) - the source URL is either an absolute URL
// on exactly that tracker's canonical host, or (AnimeBytes only) a true
// relative reference, so a tracker label can never authorize an id extracted
// from a foreign, subdomain, or opaque URL.
func FuzzTrackerKey_keysOnlyTrackerOwnCanonicalURLs(f *testing.F) {
	f.Add("Nyaa", "https://nyaa.si/view/1234567")
	f.Add("AB", "/torrents.php?id=1&torrentid=456")
	f.Add("AB", "https://animebytes.tv/torrent/1167293/group")
	f.Add("Nyaa", "https://evil.example/view/123")
	f.Add("Nyaa", "https://sukebei.nyaa.si/view/123")
	f.Add("AnimeTosho", "https://animetosho.org/view/1")
	f.Add("AB", "javascript:/torrents.php?torrentid=456")
	f.Fuzz(func(t *testing.T, tracker, raw string) {
		key := trackerKey(tracker, raw)
		if key == "" {
			return
		}
		scope, id, found := strings.Cut(key, ":")
		if !found || !isAllDigits(id) || len(id) > maxTrackerIDDigits {
			t.Fatalf("trackerKey(%q, %q) = %q, want scope:<bounded digits>", tracker, raw, key)
		}
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("keyed URL %q does not parse: %v", raw, err)
		}
		host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
		switch scope {
		case upstreamNyaa:
			if host != "nyaa.si" {
				t.Fatalf("nyaa key %q minted from host %q, want exactly nyaa.si", key, u.Hostname())
			}
		case upstreamAB:
			if host != "animebytes.tv" && (u.Scheme != "" || u.Host != "" || u.Opaque != "") {
				t.Fatalf("ab key %q minted from %q, want the canonical host or a true relative reference", key, raw)
			}
		default:
			t.Fatalf("trackerKey(%q, %q) = %q, want scope nyaa or ab", tracker, raw, key)
		}
	})
}
