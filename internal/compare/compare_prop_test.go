package compare

import (
	"slices"
	"testing"

	"github.com/cplieger/seadex-scout/internal/release"
	"github.com/cplieger/seadex-scout/internal/seadex"
	"pgregory.net/rapid"
)

// TestRepresentativePermutationInvariantProperty pins the headline selection's
// order independence over pools of any size: representative must pick a
// content-identical candidate whatever order PocketBase returned the torrents
// relation in, because the headline's identity enters the dedupe key notify
// derives (an order-dependent pick emits a different key for an unchanged
// finding - a duplicate alert plus a false resolution). The pairwise tests pin
// 2-candidate reversals; this property covers N-candidate pools, where a
// single-pass max is order-independent ONLY while betterCandidate stays a
// total order (a transitive lexicographic chain) - the invariant a future
// tie-break edit could silently break. Small alphabets deliberately force
// rank ties so the stable-key tie-break is exercised.
func TestRepresentativePermutationInvariantProperty(t *testing.T) {
	resolutions := []string{"", "720p", "1080p", "2160p"}
	trackerTypes := []release.TrackerType{release.TrackerPublic, release.TrackerPrivate, release.TrackerUnknown}
	candGen := rapid.Custom(func(t *rapid.T) candidate {
		id := rapid.StringOfN(rapid.RuneFrom([]rune{'1', '2', '3'}), 1, 3, -1).Draw(t, "id")
		return candidate{
			rel: release.Release{
				Group:       rapid.StringOfN(rapid.RuneFrom([]rune{'a', 'b'}), 1, 2, -1).Draw(t, "group"),
				Tracker:     "Nyaa",
				Resolution:  rapid.SampledFrom(resolutions).Draw(t, "res"),
				TrackerType: rapid.SampledFrom(trackerTypes).Draw(t, "ttype"),
			},
			torrent: seadex.Torrent{
				Tracker:  "Nyaa",
				InfoHash: rapid.StringOfN(rapid.RuneFrom([]rune{'x', 'y'}), 0, 3, -1).Draw(t, "hash"),
				URL:      "https://nyaa.si/view/" + id,
			},
		}
	})
	rapid.Check(t, func(t *rapid.T) {
		pool := rapid.SliceOfN(candGen, 1, 6).Draw(t, "pool")
		shuffled := slices.Clone(pool)
		for i := len(shuffled) - 1; i > 0; i-- {
			j := rapid.IntRange(0, i).Draw(t, "j")
			shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
		}
		a, b := representative(pool), representative(shuffled)
		ka, kb := candidateStableKey(&a), candidateStableKey(&b)
		if ka != kb {
			t.Errorf("representative depends on candidate order: %q vs %q", ka, kb)
		}
	})
}
