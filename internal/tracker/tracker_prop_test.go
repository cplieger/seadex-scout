package tracker_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/cplieger/seadex-scout/internal/tracker"
	"pgregory.net/rapid"
)

// TestLookupByHostProperties gives the untrusted-host gate the
// every-PR property layer its fuzz target only explores in the weekly run:
// across generated label chains a real dot-delimited subdomain of a known
// tracker domain resolves to that tracker, while suffix-confusion,
// empty-label, and non-ASCII hosts stay rejected.
func TestLookupByHostProperties(t *testing.T) {
	label := rapid.StringMatching(`[A-Za-z0-9]{1,12}`)
	labels := rapid.SliceOfN(label, 1, 4)

	rapid.Check(t, func(t *rapid.T) {
		prefix := strings.Join(labels.Draw(t, "labels"), ".")

		nyaaHost := prefix + ".nyaa.si"
		if got, ok := tracker.LookupByHost(nyaaHost); !ok || got.Name != tracker.NameNyaa {
			t.Fatalf("LookupByHost(%q) = %q/%v, want Nyaa/true", nyaaHost, got.Name, ok)
		}
		abHost := prefix + ".animebytes.tv"
		if got, ok := tracker.LookupByHost(abHost); !ok || got.Name != tracker.NameAnimeBytes {
			t.Fatalf("LookupByHost(%q) = %q/%v, want AnimeBytes/true", abHost, got.Name, ok)
		}
		if _, ok := tracker.LookupByHost(prefix + "nyaa.si"); ok {
			t.Fatalf("LookupByHost accepted suffix-confusion host %q", prefix+"nyaa.si")
		}
		if _, ok := tracker.LookupByHost(prefix + "..nyaa.si"); ok {
			t.Fatalf("LookupByHost accepted empty-label host %q", prefix+"..nyaa.si")
		}
		if _, ok := tracker.LookupByHost(prefix + "\u00a0.nyaa.si"); ok {
			t.Fatalf("LookupByHost accepted non-ASCII host %q", prefix+"\u00a0.nyaa.si")
		}
	})
}

// TestLookupByRelativeURLProperties covers the structural AB
// torrent-page classifier across the separator, key-case, percent-encoding,
// and id variants SeaDex and the trackers really emit: every rooted-relative
// torrents.php form carrying a torrentid key resolves to AnimeBytes, while the
// absolute form and a torrentid-less form do not.
func TestLookupByRelativeURLProperties(t *testing.T) {
	path := rapid.SampledFrom([]string{"/torrents.php", "/TORRENTS.PHP"})
	separator := rapid.SampledFrom([]string{"&", ";"})
	key := rapid.SampledFrom([]string{"torrentid", "TORRENTID", "%74orrentid"})
	id := rapid.IntRange(1, 2_000_000)

	rapid.Check(t, func(t *rapid.T) {
		raw := fmt.Sprintf("%s?id=%d%s%s=%d", path.Draw(t, "path"), id.Draw(t, "groupID"), separator.Draw(t, "separator"), key.Draw(t, "key"), id.Draw(t, "torrentID"))
		got, ok := tracker.LookupByRelativeURL(raw)
		if !ok || got.Name != tracker.NameAnimeBytes {
			t.Fatalf("LookupByRelativeURL(%q) = %q/%v, want AnimeBytes/true", raw, got.Name, ok)
		}
		if _, ok := tracker.LookupByRelativeURL("https://animebytes.tv" + raw); ok {
			t.Fatalf("LookupByRelativeURL accepted absolute form %q", "https://animebytes.tv"+raw)
		}
		withoutKey := fmt.Sprintf("%s?id=%d", path.Draw(t, "pathWithoutKey"), id.Draw(t, "groupIDWithoutKey"))
		if _, ok := tracker.LookupByRelativeURL(withoutKey); ok {
			t.Fatalf("LookupByRelativeURL accepted torrentid-less form %q", withoutKey)
		}
	})
}
