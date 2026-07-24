package mapping

import (
	"slices"
	"strings"
	"testing"
)

// FuzzParseOverrides exercises the operator-overrides parse boundary against
// arbitrary file bytes. Seeds cover the accepted array form, upstream-Fribb
// key spellings, case-variant canonical keys, the rejected null/object/scalar
// top levels, and typed-decode failures. Invariants hold for any input: an
// error yields nil records and nil unknown keys (never a partial result); a
// success returns normalized types and a sorted, deduplicated unknown-key set
// that never reports a case-variant canonical key encoding/json would accept.
func FuzzParseOverrides(f *testing.F) {
	f.Add([]byte(`[{"anilist_id":5,"type":"  movie  "}]`))
	f.Add([]byte(`[{"anilist_id":5,"imdb_id":"tt1","season":1},{"anilist_id":6,"themoviedb_id":9}]`))
	f.Add([]byte(`[{"ANILIST_ID":5,"TYPE":"movie"}]`))
	f.Add([]byte(`null`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`[]`))
	f.Add([]byte(`[5]`))
	f.Add([]byte(`[{"anilist_id":"not-a-number"}]`))
	f.Add([]byte(``))
	f.Add([]byte(`  [ ] `))
	f.Add([]byte(`[{"weird":1},5]`))
	f.Add([]byte(`[{"anilist_id":1,"tmdb_movies":[1,2],"imdb_ids":["a"],"season_tvdb":2,"tvdb_id":3}]`))
	f.Add([]byte(`[{"anilist_id":1},{"anilist_id":1},{"anilist_id":0}]`))
	f.Add([]byte(`[] trailing`))
	f.Add([]byte(`[bad]`))
	f.Add([]byte(`[{"anilist_id":1},`))
	f.Add([]byte(`[{"anilist_id":1,"type":"tv"}`))
	f.Add([]byte(`[{"anilist_id":5,"tmdb_movies":["x"]}]`))
	f.Add([]byte(`[{"anilist_id":5,"imdb_ids":[{}]}]`))
	f.Add([]byte(`[{"anilist_id":2,"type":"movie","e\u001bvil":1}]`))
	f.Fuzz(func(t *testing.T, data []byte) {
		set, err := parseOverrides(data)
		if err != nil {
			if set.records != nil || set.unknown != nil || set.duplicates != nil || set.applied != 0 || set.skipped != 0 || set.oversized != 0 || set.unknownOverflow {
				t.Errorf("parseOverrides error with non-empty result: %+v", set)
			}
			return
		}
		for _, r := range set.records {
			if r.AniListID == 0 {
				t.Errorf("parseOverrides retained a zero-AniList-ID record: %+v", r)
			}
			if r.Type != NormalizeType(r.Type) {
				t.Errorf("parseOverrides record Type %q not normalized", r.Type)
			}
		}
		seen := make(map[int]struct{}, len(set.records))
		for _, r := range set.records {
			if _, dup := seen[r.AniListID]; dup {
				t.Errorf("parseOverrides effective records not deduplicated: id %d repeats", r.AniListID)
			}
			seen[r.AniListID] = struct{}{}
		}
		if set.applied < len(set.records) {
			t.Errorf("applied %d < effective records %d", set.applied, len(set.records))
		}
		if !slices.IsSorted(set.unknown) {
			t.Errorf("parseOverrides unknown keys not sorted: %v", set.unknown)
		}
		for i := 1; i < len(set.unknown); i++ {
			if set.unknown[i] == set.unknown[i-1] {
				t.Errorf("parseOverrides unknown keys not deduped: %v", set.unknown)
			}
		}
		// The canonical key set is spelled out here as a test-local oracle
		// (Record's JSON tags), independent of the production dispatch in
		// decodeOverrideRecord.
		canonical := []string{"anilist_id", "type", "tvdb_id", "tmdb_movies", "imdb_ids", "season_tvdb"}
		for _, k := range set.unknown {
			for _, c := range canonical {
				if strings.EqualFold(k, c) {
					t.Errorf("parseOverrides reported canonical key %q as unknown", k)
				}
			}
		}
	})
}
