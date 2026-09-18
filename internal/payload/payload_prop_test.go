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

// TestPopulationProperty pins the EPISODE-CENSUS floor over the full untrusted
// length space, which the seven fixed length shapes of
// TestPopulationMedianAnchoredFloor cannot cover. A max-anchored floor deletes
// every regular episode of a pack carrying one over-long file, collapsing a
// season pack into a single-episode release, so the floor is median-anchored.
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
			// Stated against the two CENTRAL lengths, not one chosen middle: the
			// even-pool tie-break is not part of the contract. Reading the upper
			// middle as "the median" re-asserts the max-anchored floor on a
			// two-file pool, where the upper middle IS the maximum.
			upperMid := lengths[len(lengths)/2]
			lowerMid := lengths[(len(lengths)-1)/2]
			if upperMid > 0 {
				floor := max(0, lowerMid)
				floor = floor/2 + floor%2
				for i := range pool {
					if pool[i].Length >= upperMid && !slices.Contains(got, pool[i].Name) {
						t.Fatalf("Population(%+v) = %v, dropped the episode %q at or above the median length %d", files, got, pool[i].Name, upperMid)
					}
					// The sub-floor claim only bites where a floor exists: on a
					// zero lower middle the rule filtered nothing, so a negative
					// wire length surviving is the no-positive-median fallback.
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

// TestNamesProperty pins the layered eligibility rule over the full untrusted
// input space: SeaDex names carry arbitrary extensions and creditless markers,
// and lengths are upstream int64s where negative, zero and math.MaxInt64 are all
// constructible. The eligible POOL is modeled with the rule's own exported type
// gate, so the four numbered invariants below check the size layer structurally
// against it. Names are unique per index, or presence checks prove nothing.
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

// TestTypeGateASCIICaseInsensitiveProperty pins the type gate's case contract:
// swapping the ASCII case of every letter changes none of the four predicates.
// A global (?i) is unusable, since Go regexp's SimpleFold diverges from
// strings.ToLower on U+0130 and U+017F, so the markers render through
// nametoken.Literal's case classes and a hand-typed [N] where the class renders
// [nN] would leave a lowercase "nced" extra voting as content evidence. Only
// ASCII letters swap: a non-ASCII fold can legitimately move the extraMarkerEdge
// boundary (U+212A lowercases onto ASCII 'k' but is not ASCII-alphanumeric).
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
