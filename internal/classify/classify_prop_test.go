package classify

import (
	"math"
	"slices"
	"strconv"
	"testing"

	"pgregory.net/rapid"

	"github.com/cplieger/seadex-scout/internal/seadex"
)

// TestTorrentFileNamesProperty pins the primary-payload selection's
// invariants over the full untrusted input space (SeaDex file lengths are
// upstream int64s: negative, zero, and math.MaxInt64 are all constructible by
// the upstream): (1) the output is an in-order subsequence of the non-empty
// input names; (2) whenever any positively-sized named file exists, every
// file carrying the maximum length survives (the primary payload can never
// be filtered out); (3) a named file strictly smaller than half the maximum
// is always dropped (the invariant the MaxInt64 ceil-half overflow would
// violate by letting every small extra survive); (4) with no positive length
// at all, every non-empty name is kept (the fixture-preserving contract).
// Names are made unique per index so presence/absence checks are sound.
func TestTorrentFileNamesProperty(t *testing.T) {
	baseGen := rapid.SampledFrom([]string{"", "a", "b", "NCED [BDRemux]"})
	lenGen := rapid.Int64Range(math.MinInt64, math.MaxInt64)
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(0, 8).Draw(t, "n")
		files := make([]seadex.File, n)
		var maxLength int64
		var nonEmpty []string
		for i := range files {
			if base := baseGen.Draw(t, "name"+strconv.Itoa(i)); base != "" {
				files[i].Name = base + "-" + strconv.Itoa(i) + ".mkv"
			}
			files[i].Length = lenGen.Draw(t, "len"+strconv.Itoa(i))
			if files[i].Name != "" {
				nonEmpty = append(nonEmpty, files[i].Name)
				if files[i].Length > maxLength {
					maxLength = files[i].Length
				}
			}
		}

		got := torrentFileNames(files)

		// (1) In-order subsequence of the non-empty names.
		j := 0
		for _, name := range got {
			for j < len(nonEmpty) && nonEmpty[j] != name {
				j++
			}
			if j == len(nonEmpty) {
				t.Fatalf("torrentFileNames(%+v) = %v, not an in-order subsequence of %v", files, got, nonEmpty)
			}
			j++
		}
		if maxLength > 0 {
			for i := range files {
				if files[i].Name == "" {
					continue
				}
				// (2) Every maximum-length named file survives.
				if files[i].Length == maxLength && !slices.Contains(got, files[i].Name) {
					t.Fatalf("torrentFileNames(%+v) = %v, dropped the primary payload %q", files, got, files[i].Name)
				}
				// (3) A file under half the primary size never survives.
				if files[i].Length < maxLength/2 && slices.Contains(got, files[i].Name) {
					t.Fatalf("torrentFileNames(%+v) = %v, kept the sub-primary extra %q (len %d vs max %d)", files, got, files[i].Name, files[i].Length, maxLength)
				}
			}
		}
		// (4) No positive length: every non-empty name is kept.
		if maxLength <= 0 && !slices.Equal(got, nonEmpty) {
			t.Fatalf("torrentFileNames(%+v) = %v, want all non-empty names %v when no file has a positive length", files, got, nonEmpty)
		}
	})
}
