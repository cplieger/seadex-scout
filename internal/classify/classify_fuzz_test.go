package classify

import (
	"slices"
	"strconv"
	"testing"

	"github.com/cplieger/seadex-scout/internal/seadex"
)

// FuzzPayloadNames is the coverage-guided twin of TestPayloadNamesProperty:
// the rapid property samples six fixed base names, so the type-gate
// predicates (extension table, creditless regex) are never explored over
// arbitrary untrusted SeaDex names. The fuzz target feeds arbitrary names and
// int64 lengths (negative, zero, and MaxInt64 are all constructible upstream)
// and asserts the rule's structural invariants: the output is an in-order
// subsequence of the eligible pool (modeled with the exported type gate, so
// with any content survivor no sidecar or creditless extra ever votes), a
// named input never loses ALL its evidence (totality), every maximum-length
// pool file survives, and no pool file below the ceil-half threshold does.
// Names are made unique per index so presence checks are sound; the prefix
// changes neither the extension nor a creditless token.
func FuzzPayloadNames(f *testing.F) {
	f.Add("a.mkv", int64(1000), "NCED01 [BDRemux].mkv", int64(900), "sub.ass", int64(10))
	f.Add("movie.iso", int64(1000), "Sample.iso", int64(10), "", int64(0))
	f.Add("Show - 01 [1080p][HEVC].mkv", int64(9223372036854775807), "Making Of [BDRemux].mkv", int64(50_000_000), "b.mkv", int64(-5))
	f.Add("primary.mkv", int64(3), "extra.mkv", int64(1), "extra2.mkv", int64(2))
	f.Add("", int64(0), "", int64(0), "", int64(0))
	f.Fuzz(func(t *testing.T, n1 string, l1 int64, n2 string, l2 int64, n3 string, l3 int64) {
		files := []seadex.File{{Name: n1, Length: l1}, {Name: n2, Length: l2}, {Name: n3, Length: l3}}
		named := 0
		for i := range files {
			if files[i].Name != "" {
				files[i].Name = strconv.Itoa(i) + "-" + files[i].Name
				named++
			}
		}

		// Model the eligible pool with the exported type gate: content files
		// when any exist, every named file otherwise.
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

		// Totality: a torrent with any named file never loses all evidence.
		if named > 0 && len(got) == 0 {
			t.Fatalf("PayloadNames(%+v) = empty, want evidence whenever a named file exists", files)
		}
		// In-order subsequence of the eligible pool.
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
			minPrimary := maxLength/2 + maxLength%2
			for i := range pool {
				if pool[i].Length == maxLength && !slices.Contains(got, pool[i].Name) {
					t.Fatalf("PayloadNames(%+v) = %v, dropped the primary payload %q", files, got, pool[i].Name)
				}
				if pool[i].Length < minPrimary && slices.Contains(got, pool[i].Name) {
					t.Fatalf("PayloadNames(%+v) = %v, kept the sub-primary extra %q (len %d vs max %d)", files, got, pool[i].Name, pool[i].Length, maxLength)
				}
			}
		}
		if maxLength <= 0 && !slices.Equal(got, poolNames) {
			t.Fatalf("PayloadNames(%+v) = %v, want the whole eligible pool %v when no pool file has a positive length", files, got, poolNames)
		}
	})
}
