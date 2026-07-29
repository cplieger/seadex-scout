package payload

import (
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/cplieger/seadex-scout/internal/seadex"
	"pgregory.net/rapid"
)

// TestPopulationProperty pins the EPISODE-CENSUS floor over the full
// untrusted length space, which the seven fixed length shapes of
// TestPopulationMedianAnchoredFloor cannot cover. The floor is
// median-anchored precisely because a max-anchored one deleted every regular
// episode of a pack carrying one over-long file, collapsing a season pack into
// a single-episode release, so the invariants are stated against the pool's two
// CENTRAL lengths (not one chosen middle, since the even-pool tie-break is a
// free choice): the census is an in-order subsequence of the eligible pool,
// every pool file at or above the upper-middle length survives, nothing below
// half the lower-middle length does, a pool with no positive median is kept
// whole, and the census never excludes a file the primary-payload rule keeps (a
// median can never exceed a maximum, so its floor can never sit above
// primaryFiles').
func TestPopulationProperty(t *testing.T) {
	baseGen := rapid.SampledFrom([]string{"", "a.mkv", "b.mkv", "NCED [BDRemux].mkv", "movie.iso", "sub.ass"})
	lenGen := rapid.Int64Range(math.MinInt64, math.MaxInt64)
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(0, 8).Draw(t, "n")
		files := make([]seadex.File, n)
		for i := range files {
			if base := baseGen.Draw(t, "name"+strconv.Itoa(i)); base != "" {
				files[i].Name = strconv.Itoa(i) + "-" + base
			}
			files[i].Length = lenGen.Draw(t, "len"+strconv.Itoa(i))
		}

		// The eligible pool, modeled with the rule's own exported type gate:
		// content files when any exist, every named file otherwise.
		var pool []seadex.File
		for i := range files {
			if files[i].Name != "" && ContentMediaFile(files[i].Name) {
				pool = append(pool, files[i])
			}
		}
		if len(pool) == 0 {
			for i := range files {
				if files[i].Name != "" {
					pool = append(pool, files[i])
				}
			}
		}
		poolNames := make([]string, 0, len(pool))
		lengths := make([]int64, 0, len(pool))
		for i := range pool {
			poolNames = append(poolNames, pool[i].Name)
			lengths = append(lengths, pool[i].Length)
		}
		slices.Sort(lengths)

		got := censusNames(Population(files))

		j := 0
		for _, name := range got {
			for j < len(poolNames) && poolNames[j] != name {
				j++
			}
			if j == len(poolNames) {
				t.Fatalf("Population(%+v) = %v, not an in-order subsequence of the eligible pool %v", files, got, poolNames)
			}
			j++
		}
		if len(pool) > 0 {
			// The invariants are stated against the two CENTRAL lengths rather
			// than one chosen middle, because the even-pool tie-break is not
			// part of the contract: the census floor only has to sit at half of
			// some value between them. Reading the upper middle as "the median"
			// would re-assert the max-anchored floor on a two-file pool (there
			// the upper middle IS the maximum), which is exactly what the
			// median anchor exists to avoid.
			upperMid := lengths[len(lengths)/2]
			lowerMid := lengths[(len(lengths)-1)/2]
			if upperMid > 0 {
				floor := max(0, lowerMid)
				floor = floor/2 + floor%2
				for i := range pool {
					if pool[i].Length >= upperMid && !slices.Contains(got, pool[i].Name) {
						t.Fatalf("Population(%+v) = %v, dropped the episode %q at or above the median length %d", files, got, pool[i].Name, upperMid)
					}
					// The sub-floor claim only bites where a floor exists: a
					// zero lower middle means the rule filtered nothing (its
					// own median collapses to zero on such a pool), so a
					// negative wire length surviving is the documented
					// no-positive-median fallback, not a counted sample.
					if floor > 0 && pool[i].Length < floor && slices.Contains(got, pool[i].Name) {
						t.Fatalf("Population(%+v) = %v, counted the sub-half-median sample %q (len %d, floor %d)", files, got, pool[i].Name, pool[i].Length, floor)
					}
				}
			} else if !slices.Equal(got, poolNames) {
				t.Fatalf("Population(%+v) = %v, want the whole eligible pool %v when the median length is not positive", files, got, poolNames)
			}
		}
		for _, name := range censusNames(primaryFiles(files)) {
			if !slices.Contains(got, name) {
				t.Fatalf("Population(%+v) = %v, excludes the payload file %q: the census floor must never sit above the payload floor", files, got, name)
			}
		}
	})
}

// censusNames projects a file slice to its names, for the census property's
// presence checks.
func censusNames(files []seadex.File) []string {
	out := make([]string, 0, len(files))
	for i := range files {
		out = append(out, files[i].Name)
	}
	return out
}

// TestNamesProperty pins the layered eligibility rule's invariants
// over the full untrusted input space (SeaDex file names carry arbitrary
// extensions and creditless markers; lengths are upstream int64s where
// negative, zero, and math.MaxInt64 are all constructible). The eligible
// POOL is modeled with the rule's own exported type gate — content files
// (ContentMediaFile) when any exist, every named file otherwise (the
// unlisted-container / sidecar-only fallback) — and the size layer's
// invariants are then checked structurally against that pool:
//
//	(1) the output is an in-order subsequence of the pool's names — so with
//	    any content survivor, no sidecar or creditless extra ever appears,
//	    whatever its size (the type gate);
//	(2) whenever any pool file has positive length, every maximum-length
//	    pool file survives (the primary payload can never be filtered out);
//	(3) a pool file strictly smaller than half the maximum is always
//	    dropped (the invariant the MaxInt64 ceil-half overflow would
//	    violate by letting every small extra survive);
//	(4) with no positive length in the pool, the whole pool is kept (the
//	    fixture-preserving contract).
//
// Names are made unique per index so presence/absence checks are sound.
func TestNamesProperty(t *testing.T) {
	baseGen := rapid.SampledFrom([]string{"", "a.mkv", "b.mkv", "NCED [BDRemux].mkv", "movie.iso", "sub.ass"})
	lenGen := rapid.Int64Range(math.MinInt64, math.MaxInt64)
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(0, 8).Draw(t, "n")
		files := make([]seadex.File, n)
		for i := range files {
			if base := baseGen.Draw(t, "name"+strconv.Itoa(i)); base != "" {
				files[i].Name = strconv.Itoa(i) + "-" + base
			}
			files[i].Length = lenGen.Draw(t, "len"+strconv.Itoa(i))
		}

		var pool []seadex.File
		for i := range files {
			if files[i].Name != "" && ContentMediaFile(files[i].Name) {
				pool = append(pool, files[i])
			}
		}
		if len(pool) == 0 {
			for i := range files {
				if files[i].Name != "" {
					pool = append(pool, files[i])
				}
			}
		}
		var poolNames []string
		var maxLength int64
		for i := range pool {
			poolNames = append(poolNames, pool[i].Name)
			if pool[i].Length > maxLength {
				maxLength = pool[i].Length
			}
		}

		got := Names(files)

		// (1) In-order subsequence of the eligible pool.
		j := 0
		for _, name := range got {
			for j < len(poolNames) && poolNames[j] != name {
				j++
			}
			if j == len(poolNames) {
				t.Fatalf("Names(%+v) = %v, not an in-order subsequence of the eligible pool %v", files, got, poolNames)
			}
			j++
		}
		if maxLength > 0 {
			for i := range pool {
				// (2) Every maximum-length pool file survives.
				if pool[i].Length == maxLength && !slices.Contains(got, pool[i].Name) {
					t.Fatalf("Names(%+v) = %v, dropped the primary payload %q", files, got, pool[i].Name)
				}
				// (3) A pool file under the ceil-half primary threshold never
				// survives - the same bound FuzzPayloadNames asserts, so the two
				// twins cannot document different rules (a floor-half bound lets
				// an odd-maximum off-by-one slip past this property).
				if pool[i].Length < maxLength/2+maxLength%2 && slices.Contains(got, pool[i].Name) {
					t.Fatalf("Names(%+v) = %v, kept the sub-primary extra %q (len %d vs max %d)", files, got, pool[i].Name, pool[i].Length, maxLength)
				}
			}
		}
		// (4) No positive length in the pool: the whole pool is kept.
		if maxLength <= 0 && !slices.Equal(got, poolNames) {
			t.Fatalf("Names(%+v) = %v, want the whole eligible pool %v when no pool file has a positive length", files, got, poolNames)
		}
	})
}

// TestTypeGateASCIICaseInsensitiveProperty pins the file-eligibility type
// gate's case contract over generated marker-bearing names: swapping the ASCII
// case of every letter must change neither IsMediaFile, IsCreditlessExtra,
// IsSampleExtra, nor ContentMediaFile. The creditless marker spells out 30
// explicit case classes (a global (?i) is unusable - Go regexp's SimpleFold
// diverges from strings.ToLower on U+0130 and U+017F), the sample marker
// spells out six for the same reason, and the unit tables only exercise a few
// spellings of each token, so a single dropped alternative - [Nn] typed as
// [N] - would leave a lowercase "nced" extra voting as content evidence. Only ASCII letters are swapped: a non-ASCII fold can legitimately
// change the [^[:alnum:]] boundary (U+212A lowercases onto ASCII 'k', turning a
// delimiter into a word character), so it is outside the invariant.
func TestTypeGateASCIICaseInsensitiveProperty(t *testing.T) {
	tokenGen := rapid.SampledFrom([]string{
		"ncop", "NCOP", "NcOp", "nced", "NCED", "creditless", "CREDITLESS",
		"CRED\u0130TLESS", "sample", "SAMPLE", "SaMpLe", "01", "v2", "V2",
		"Show", "1080p", "x265",
		"[", "]", "_", "-", " ", ".", ".mkv", ".MKV", ".ass", ".WEBM",
	})
	rapid.Check(t, func(t *rapid.T) {
		name := strings.Join(rapid.SliceOfN(tokenGen, 0, 6).Draw(t, "tokens"), "")
		swapped := swapASCIICase(name)
		if got, want := IsMediaFile(swapped), IsMediaFile(name); got != want {
			t.Errorf("IsMediaFile(%q) = %v, but IsMediaFile(%q) = %v", swapped, got, name, want)
		}
		if got, want := IsCreditlessExtra(swapped), IsCreditlessExtra(name); got != want {
			t.Errorf("IsCreditlessExtra(%q) = %v, but IsCreditlessExtra(%q) = %v", swapped, got, name, want)
		}
		if got, want := IsSampleExtra(swapped), IsSampleExtra(name); got != want {
			t.Errorf("IsSampleExtra(%q) = %v, but IsSampleExtra(%q) = %v", swapped, got, name, want)
		}
		if got, want := ContentMediaFile(swapped), ContentMediaFile(name); got != want {
			t.Errorf("ContentMediaFile(%q) = %v, but ContentMediaFile(%q) = %v", swapped, got, name, want)
		}
	})
}

// swapASCIICase flips the case of every ASCII letter and leaves every other
// rune untouched: the transformation the type gate must be blind to.
func swapASCIICase(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r - 'a' + 'A'
		case r >= 'A' && r <= 'Z':
			return r - 'A' + 'a'
		}
		return r
	}, s)
}
