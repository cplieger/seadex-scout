package mapping

import (
	"reflect"
	"testing"

	"pgregory.net/rapid"
)

// TestDeduplicateRecordsIndexOracle property-checks deduplicateRecords against
// buildIndex, the consumer whose semantics it exists to mirror: for any record
// list, the deduplicated slice must index bijectively (len == index len) and
// produce exactly the same effective index as the raw input, every surviving
// ID must be non-zero and unique, each survivor must be the WHOLE last
// occurrence of its ID (every field, not a projection - routing and refresh
// acceptance consume Type, TmdbMovies, IMDbIDs, and SeasonTvdb too), and the
// operation must be idempotent. This is the invariant the acceptance guards
// depend on (row counts and identifier coverage are measured on the
// deduplicated set so they match what consumers receive).
func TestDeduplicateRecordsIndexOracle(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		records := rapid.SliceOfN(rapid.Custom(func(t *rapid.T) Record {
			return Record{
				// A small ID range forces duplicate and zero IDs.
				AniListID:  rapid.IntRange(0, 5).Draw(t, "anilist_id"),
				Type:       rapid.SampledFrom([]string{"", "TV", "MOVIE", "OVA", "SPECIAL"}).Draw(t, "type"),
				TvdbID:     rapid.IntRange(0, 1000).Draw(t, "tvdb_id"),
				SeasonTvdb: rapid.IntRange(0, 5).Draw(t, "season_tvdb"),
				TmdbMovies: rapid.SliceOfN(rapid.IntRange(1, 9), 0, 3).Draw(t, "tmdb_movies"),
				IMDbIDs:    rapid.SliceOfN(rapid.SampledFrom([]string{"tt1", "tt2", "tt3"}), 0, 3).Draw(t, "imdb_ids"),
			}
		}), 0, 20).Draw(t, "records")

		out := deduplicateRecords(records)

		if got, want := buildIndex(out).Len(), len(out); got != want {
			t.Fatalf("deduplicated set indexes to %d entries, want bijective %d", got, want)
		}
		rawIdx, outIdx := buildIndex(records), buildIndex(out)
		if rawIdx.Len() != outIdx.Len() {
			t.Fatalf("index size diverged: raw %d, deduplicated %d", rawIdx.Len(), outIdx.Len())
		}
		seen := make(map[int]struct{}, len(out))
		for _, r := range out {
			if r.AniListID == 0 {
				t.Fatalf("deduplicated set retained a zero-ID record: %+v", r)
			}
			if _, dup := seen[r.AniListID]; dup {
				t.Fatalf("deduplicated set repeats ID %d", r.AniListID)
			}
			seen[r.AniListID] = struct{}{}
			// buildIndex is the last-write-wins oracle: the survivor must be
			// the WHOLE last occurrence, every field intact.
			got, ok := rawIdx.Lookup(r.AniListID)
			if !ok || !reflect.DeepEqual(got, r) {
				t.Fatalf("raw index disagrees for ID %d: index %+v ok=%v, deduplicated %+v", r.AniListID, got, ok, r)
			}
		}
		again := deduplicateRecords(out)
		if !reflect.DeepEqual(again, out) {
			t.Fatalf("deduplicateRecords not idempotent: %+v -> %+v", out, again)
		}
	})
}
