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
	f.Fuzz(func(t *testing.T, data []byte) {
		records, unknown, err := parseOverrides(data)
		if err != nil {
			if records != nil || unknown != nil {
				t.Errorf("parseOverrides error with non-nil results: records=%v unknown=%v", records, unknown)
			}
			return
		}
		for _, r := range records {
			if r.Type != NormalizeType(r.Type) {
				t.Errorf("parseOverrides record Type %q not normalized", r.Type)
			}
		}
		if !slices.IsSorted(unknown) {
			t.Errorf("parseOverrides unknown keys not sorted: %v", unknown)
		}
		for i := 1; i < len(unknown); i++ {
			if unknown[i] == unknown[i-1] {
				t.Errorf("parseOverrides unknown keys not deduped: %v", unknown)
			}
		}
		for _, k := range unknown {
			for canonical := range overrideKeys {
				if strings.EqualFold(k, canonical) {
					t.Errorf("parseOverrides reported canonical key %q as unknown", k)
				}
			}
		}
	})
}
