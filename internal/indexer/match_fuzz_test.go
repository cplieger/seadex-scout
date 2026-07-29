package indexer

import (
	"net/url"
	"strings"
	"testing"

	"github.com/cplieger/urlform"
)

// boundedTrackerID reports whether id is a non-empty, width-bounded run of
// ASCII digits - the charset/width half of validTrackerID's contract (it
// deliberately does not re-assert the canonical-decimal-form rule, which
// TestTrackerIDExtractionRejectsNonCanonicalDecimalForms pins, so this stays a
// necessary condition on every returned id). The digit test is
// an INDEPENDENT oracle (strings.Trim), never the production isAllDigits
// helper: sharing that helper would let a mutation loosening it govern both
// the code under test and the assertion, so a parser admitting a non-digit id
// would still pass.
func boundedTrackerID(id string) bool {
	return id != "" && len(id) <= maxTrackerIDDigits && strings.Trim(id, "0123456789") == ""
}

// FuzzExtractID_alwaysDigitsOrEmpty pins the security-relevant invariant of the id
// extraction that runs on Prowlarr-supplied (tracker-controlled) URL strings: every id
// it returns is a non-empty run of ASCII digits, or it returns "" - a bogus tracker key
// (a non-numeric id) must never reach the curation match set. The seed corpus covers the
// Nyaa /view, AnimeBytes permalink, and AnimeBytes torrentid= forms plus a non-numeric id.
// The digit check is the INDEPENDENT boundedTrackerID oracle, not the
// production isAllDigits helper: sharing that helper would let a mutation
// loosening it govern both the code under test and the assertion, so the
// property would still pass on a parser that admits a non-digit id.
func FuzzExtractID_alwaysDigitsOrEmpty(f *testing.F) {
	f.Add("https://nyaa.si/view/1234567")
	f.Add("https://animebytes.tv/torrent/1167293/group?nh=709E38EC")
	f.Add("/torrents.php?id=70543&torrentid=1143533")
	f.Add("https://nyaa.si/view/12a45")
	f.Add("https://nyaa.si/view/999999999999999999999")
	f.Add("")
	f.Fuzz(func(t *testing.T, raw string) {
		assertValid := func(name, id string) {
			t.Helper()
			if id != "" && !boundedTrackerID(id) {
				t.Fatalf("%s(%q) = %q, want a bounded run of digits or empty", name, raw, id)
			}
		}
		for _, needle := range []string{"/view/", "/torrent/", "torrentid="} {
			assertValid("extractID", extractID(raw, needle))
		}
		assertValid("animeBytesID", animeBytesID(raw))
		if k := trackerKeyFromURL(raw); k != "" {
			_, id, found := strings.Cut(k, ":")
			if !found || !boundedTrackerID(id) {
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
		// Digits 1-9 only: a leading zero is a non-canonical decimal form that
		// validTrackerID fails closed on by design (one torrent must not key
		// under two identity strings), so it belongs to the rejection side.
		id := strings.Repeat(string(rune('1'+digit%9)), int(width)%maxTrackerIDDigits+1)
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
// scope:<bounded digits> for a supported scope, and under the package's urlform
// structural vocabulary the source is either an absolute URL on exactly that
// tracker's canonical host or, for AnimeBytes only, a rooted relative reference,
// so a tracker label can never authorize an id extracted from a foreign,
// subdomain, or opaque URL.
func FuzzTrackerKey_keysOnlyTrackerOwnCanonicalURLs(f *testing.F) {
	f.Add("Nyaa", "https://nyaa.si/view/1234567")
	f.Add("AB", "/torrents.php?id=1&torrentid=456")
	f.Add("AB", "https://animebytes.tv/torrent/1167293/group")
	f.Add("Nyaa", "https://evil.example/view/123")
	f.Add("Nyaa", "https://sukebei.nyaa.si/view/123")
	f.Add("Nyaa", "https://nyaa.si./view/123")
	f.Add("AnimeTosho", "https://animetosho.org/view/1")
	f.Add("AB", "javascript:/torrents.php?torrentid=456")
	f.Fuzz(func(t *testing.T, tracker, raw string) {
		key := trackerKey(tracker, raw)
		if key == "" {
			return
		}
		scope, id, found := strings.Cut(key, ":")
		if !found || !boundedTrackerID(id) {
			t.Fatalf("trackerKey(%q, %q) = %q, want scope:<bounded digits>", tracker, raw, key)
		}
		// Assert the admitted set in the vocabulary trackerOwnForm now reads
		// (urlform), not in net/url's: the two disagree on exactly the shape this
		// oracle used to describe as a "true relative reference" (a schemeless
		// host like "animebytes.tv/x" is triple-empty to net/url but host
		// evidence to urlform, l-f162), so pinning the old reading here would
		// re-assert the divergence the adoption removed.
		f := urlform.Classify(raw)
		// A trailing DNS-root dot is the fully-qualified spelling of the same
		// canonical host: tracker.LookupByHost tolerates it and
		// isCanonicalTrackerHost trims it, so trackerKey legitimately keys
		// "https://nyaa.si./view/1". The oracle must normalize the same way or a
		// legitimate input is reported as a crasher.
		host := strings.TrimSuffix(f.Host, ".")
		switch scope {
		case upstreamNyaa:
			if f.Class != urlform.ClassAbsolute || host != "nyaa.si" {
				t.Fatalf("nyaa key %q minted from %q (class %v, host %q), want an absolute URL on exactly nyaa.si",
					key, raw, f.Class, f.Host)
			}
		case upstreamAB:
			absoluteCanonical := f.Class == urlform.ClassAbsolute && host == "animebytes.tv"
			rootedRelative := f.Class == urlform.ClassRelative && f.Host == ""
			if !absoluteCanonical && !rootedRelative {
				t.Fatalf("ab key %q minted from %q (class %v, host %q), want the canonical host or a rooted relative reference",
					key, raw, f.Class, f.Host)
			}
		default:
			t.Fatalf("trackerKey(%q, %q) = %q, want scope nyaa or ab", tracker, raw, key)
		}
	})
}
