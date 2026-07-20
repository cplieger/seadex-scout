package compare

import (
	"slices"
	"strings"
	"testing"

	"github.com/cplieger/seadex-scout/internal/release"
	"github.com/cplieger/seadex-scout/internal/seadex"
	"pgregory.net/rapid"
)

// splitEscapedParts is the test-local inverse of escapeJoinParts: split on
// unescaped commas, unescaping '\'-escaped runes. A genuine round-trip oracle,
// not a copy of the encoder.
func splitEscapedParts(s string) []string {
	var parts []string
	var cur strings.Builder
	escaped := false
	for _, r := range s {
		if escaped {
			cur.WriteRune(r)
			escaped = false
			continue
		}
		switch r {
		case '\\':
			escaped = true
		case ',':
			parts = append(parts, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	parts = append(parts, cur.String())
	return parts
}

// TestEscapeJoinPartsRoundTripProperty pins the dedupe-key component encoding
// with a round-trip: decoding the escaped join recovers the exact original
// element boundaries for any non-empty parts drawn from a delimiter-heavy
// alphabet (production groups are non-empty by the release.NoGroup invariant).
// A boundary-collapsing encoding (the naive strings.Join this replaced, under
// which ["a,b"] and ["a","b"] collide and suppress a distinct finding as
// already alerted) cannot survive the round-trip.
func TestEscapeJoinPartsRoundTripProperty(t *testing.T) {
	part := rapid.StringOfN(rapid.RuneFrom([]rune{'a', 'b', ',', '|', '\\'}), 1, 4, -1)
	gen := rapid.SliceOfN(part, 1, 4)
	rapid.Check(t, func(t *rapid.T) {
		parts := gen.Draw(t, "parts")
		encoded := escapeJoinParts(parts)
		back := splitEscapedParts(encoded)
		if !slices.Equal(back, parts) {
			t.Errorf("round-trip lost element boundaries: %q -> %q -> %q", parts, encoded, back)
		}
	})
}

// TestHashKeyPartsPreservesElementBoundariesProperty pins the
// length-prefixed hashing of oversized dedupe-key components: merging two
// adjacent elements (["a","b"] -> ["ab"]) always changes the hash, so
// ["a,b"] and ["a","b"] cannot collide in the hashed regime any more than in
// the escaped one. A naive join-then-hash collapses exactly this boundary
// and would reintroduce the finding-suppression collision class the bounding
// exists to keep out of hostile bulk SeaDex data.
func TestHashKeyPartsPreservesElementBoundariesProperty(t *testing.T) {
	part := rapid.StringOfN(rapid.RuneFrom([]rune{'a', 'b', ',', '|', '\\'}), 0, 4, -1)
	gen := rapid.SliceOfN(part, 2, 4)
	rapid.Check(t, func(t *rapid.T) {
		parts := gen.Draw(t, "parts")
		merged := append([]string{parts[0] + parts[1]}, parts[2:]...)
		if hashKeyParts(parts) == hashKeyParts(merged) {
			t.Errorf("hashKeyParts collapsed element boundaries: %q and %q share a hash", parts, merged)
		}
	})
}

// TestRepresentativePermutationInvariantProperty pins the headline selection's
// order independence over pools of any size: representative must pick a
// content-identical candidate whatever order PocketBase returned the torrents
// relation in, because the headline's identity enters the dedupe key (an
// order-dependent pick emits a different key for an unchanged finding - a
// duplicate alert plus a false resolution). The pairwise tests pin 2-candidate
// reversals; this property covers N-candidate pools, where a single-pass max
// is order-independent ONLY while betterCandidate stays a total order (a
// transitive lexicographic chain) - the invariant a future tie-break edit
// could silently break. Small alphabets deliberately force rank ties so the
// stable-key tie-break is exercised.
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
