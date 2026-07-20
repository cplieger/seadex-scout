package mapping

import (
	"testing"

	"pgregory.net/rapid"
)

// TestDeduplicateRecordsIndexOracle property-checks deduplicateRecords against
// buildIndex, the consumer whose semantics it exists to mirror: for any record
// list, the deduplicated slice must index bijectively (len == index len) and
// produce exactly the same effective index as the raw input, every surviving
// ID must be non-zero and unique, surviving records must be the LAST
// occurrence of their ID, and the operation must be idempotent. This is the
// invariant the acceptance guards depend on (row counts and identifier
// coverage are measured on the deduplicated set so they match what consumers
// receive).
func TestDeduplicateRecordsIndexOracle(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		records := rapid.SliceOfN(rapid.Custom(func(t *rapid.T) Record {
			return Record{
				// A small ID range forces duplicate and zero IDs.
				AniListID: rapid.IntRange(0, 5).Draw(t, "anilist_id"),
				TvdbID:    rapid.IntRange(0, 1000).Draw(t, "tvdb_id"),
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
		lastByID := make(map[int]Record, len(records))
		for _, r := range records {
			if r.AniListID != 0 {
				lastByID[r.AniListID] = r
			}
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
			if last := lastByID[r.AniListID]; r.TvdbID != last.TvdbID {
				t.Fatalf("survivor for ID %d = %+v, want last occurrence %+v", r.AniListID, r, last)
			}
			got, ok := rawIdx.Lookup(r.AniListID)
			if !ok || got.TvdbID != r.TvdbID {
				t.Fatalf("raw index disagrees for ID %d: index %+v ok=%v, deduplicated %+v", r.AniListID, got, ok, r)
			}
		}
		again := deduplicateRecords(out)
		if len(again) != len(out) {
			t.Fatalf("deduplicateRecords not idempotent: %d -> %d records", len(out), len(again))
		}
		for i := range again {
			if again[i].AniListID != out[i].AniListID || again[i].TvdbID != out[i].TvdbID {
				t.Fatalf("idempotence broke at [%d]: %+v vs %+v", i, again[i], out[i])
			}
		}
	})
}
