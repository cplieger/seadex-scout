package mapping

import (
	"reflect"
	"slices"
	"testing"

	"pgregory.net/rapid"
)

// TestDeduplicateRecordsIndexOracle property-checks deduplicateRecords against
// NewIndex, the consumer whose semantics it exists to mirror: for any record
// list, the deduplicated slice must index bijectively (len == index len) and
// produce exactly the same effective index as the raw input, every surviving ID
// must be positive and unique, each survivor must be the WHOLE last occurrence of
// its ID (every field, since routing and refresh acceptance consume
// TmdbMovies, SeasonKind and SeasonTvdb too), and the operation must be idempotent.
// The acceptance guards measure their counts on the deduplicated set.
func TestDeduplicateRecordsIndexOracle(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		records := rapid.SliceOfN(rapid.Custom(func(t *rapid.T) Record {
			return Record{
				// A small ID range forces duplicate, zero, and negative IDs.
				AniListID:  rapid.IntRange(-2, 5).Draw(t, "anilist_id"),
				SeasonKind: rapid.SampledFrom([]SeasonKind{seasonUnknown, SeasonPresent, SeasonAbsent}).Draw(t, "season_kind"),
				TvdbID:     rapid.IntRange(0, 1000).Draw(t, "tvdb_id"),
				SeasonTvdb: rapid.IntRange(0, 5).Draw(t, "season_tvdb"),
				TmdbMovies: rapid.SliceOfN(rapid.IntRange(1, 9), 0, 3).Draw(t, "tmdb_movies"),
			}
		}), 0, 20).Draw(t, "records")

		// Freeze the oracle BEFORE the call under test, from a deep copy:
		// the Record copies would otherwise alias the generated TmdbMovies
		// backing arrays, so an in-place deduplicator regression that
		// mutates a survivor's fields could contaminate the oracle and the
		// output alike, and the parity check would still pass.
		frozen := make([]Record, len(records))
		for i, r := range records {
			r.TmdbMovies = slices.Clone(r.TmdbMovies)
			frozen[i] = r
		}
		rawIdx := NewIndex(Source{Records: frozen})

		out := deduplicateRecords(records)

		if got, want := NewIndex(Source{Records: out}).Len(), len(out); got != want {
			t.Fatalf("deduplicated set indexes to %d entries, want bijective %d", got, want)
		}
		outIdx := NewIndex(Source{Records: out})
		if rawIdx.Len() != outIdx.Len() {
			t.Fatalf("index size diverged: raw %d, deduplicated %d", rawIdx.Len(), outIdx.Len())
		}
		seen := make(map[int]struct{}, len(out))
		for _, r := range out {
			if r.AniListID <= 0 {
				t.Fatalf("deduplicated set retained a non-positive-ID record: %+v", r)
			}
			if _, dup := seen[r.AniListID]; dup {
				t.Fatalf("deduplicated set repeats ID %d", r.AniListID)
			}
			seen[r.AniListID] = struct{}{}
			// NewIndex is the last-write-wins oracle: the survivor must be
			// the WHOLE last occurrence, every field intact. It canonicalizes on
			// insertion, so the comparison is against the canonical form of the
			// survivor - canonicalize allocates fresh slices, so this cannot
			// disturb the deduplicated set.
			got, ok := rawIdx.Lookup(r.AniListID)
			want := r
			want.TmdbMovies = slices.Clone(r.TmdbMovies)
			want.canonicalize()
			if !ok || !reflect.DeepEqual(got, want) {
				t.Fatalf("raw index disagrees for ID %d: index %+v ok=%v, deduplicated %+v", r.AniListID, got, ok, want)
			}
		}
		again := deduplicateRecords(out)
		if !reflect.DeepEqual(again, out) {
			t.Fatalf("deduplicateRecords not idempotent: %+v -> %+v", out, again)
		}
	})
}

// TestValidateRefreshedRecordsUnchangedRefreshAlwaysAccepted property-checks
// the steady-state acceptance invariant none of the instance tests generalize:
// for ANY previously accepted cache shape (a deduplicated, cacheUsable record
// set), a byte-identical upstream refresh must always be accepted. A rejection
// here would be a self-inflicted permanent rejection loop (the stale map is
// kept every cycle and the streak escalates to ERROR) with a healthy upstream,
// so no floor, shrink guard, or population guard may fire when nothing changed.
func TestValidateRefreshedRecordsUnchangedRefreshAlwaysAccepted(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		raw := rapid.SliceOfN(rapid.Custom(func(t *rapid.T) Record {
			return Record{
				AniListID:  rapid.IntRange(-2, 50).Draw(t, "anilist_id"),
				SeasonKind: rapid.SampledFrom([]SeasonKind{seasonUnknown, SeasonPresent, SeasonAbsent}).Draw(t, "season_kind"),
				TvdbID:     rapid.IntRange(0, 1000).Draw(t, "tvdb_id"),
				SeasonTvdb: rapid.IntRange(0, 5).Draw(t, "season_tvdb"),
				TmdbMovies: rapid.SliceOfN(rapid.IntRange(1, 9), 0, 3).Draw(t, "tmdb_movies"),
			}
		}), 0, 30).Draw(t, "records")
		cache := deduplicateRecords(raw)
		if !cacheUsable(cache) {
			t.Skip("not a usable accepted-cache shape")
		}
		if err := validateRefreshedRecords(cache, cache); err != nil {
			t.Fatalf("unchanged refresh rejected: %v (records %+v)", err, cache)
		}
	})
}
