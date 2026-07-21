package classify

import (
	"math"
	"slices"
	"strconv"
	"testing"

	"github.com/cplieger/seadex-scout/internal/seadex"
	"pgregory.net/rapid"
)

// TestPayloadNamesProperty pins the layered eligibility rule's invariants
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
func TestPayloadNamesProperty(t *testing.T) {
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

		got := PayloadNames(files)

		// (1) In-order subsequence of the eligible pool.
		j := 0
		for _, name := range got {
			for j < len(poolNames) && poolNames[j] != name {
				j++
			}
			if j == len(poolNames) {
				t.Fatalf("PayloadNames(%+v) = %v, not an in-order subsequence of the eligible pool %v", files, got, poolNames)
			}
			j++
		}
		if maxLength > 0 {
			for i := range pool {
				// (2) Every maximum-length pool file survives.
				if pool[i].Length == maxLength && !slices.Contains(got, pool[i].Name) {
					t.Fatalf("PayloadNames(%+v) = %v, dropped the primary payload %q", files, got, pool[i].Name)
				}
				// (3) A pool file under half the primary size never survives.
				if pool[i].Length < maxLength/2 && slices.Contains(got, pool[i].Name) {
					t.Fatalf("PayloadNames(%+v) = %v, kept the sub-primary extra %q (len %d vs max %d)", files, got, pool[i].Name, pool[i].Length, maxLength)
				}
			}
		}
		// (4) No positive length in the pool: the whole pool is kept.
		if maxLength <= 0 && !slices.Equal(got, poolNames) {
			t.Fatalf("PayloadNames(%+v) = %v, want the whole eligible pool %v when no pool file has a positive length", files, got, poolNames)
		}
	})
}
