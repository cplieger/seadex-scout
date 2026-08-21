package mapping

import (
	"testing"

	"github.com/cplieger/seadex-scout/internal/mediatype"
)

// FuzzParseOverrides exercises the operator-overrides parse boundary against
// arbitrary file bytes. Seeds cover the accepted array form, upstream-Fribb
// key spellings, case-variant canonical keys, the rejected null/object/scalar
// top levels, and typed-decode failures. Invariants hold for any input: an
// error yields a zero result (never a partial one); a success returns
// deduplicated, positively-keyed records with normalized types.
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
	f.Add([]byte(`[{"anilist_id":-7,"type":"ova"},{"anilist_id":-1,"tvdb_id":-5,"season_tvdb":-2}]`))
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
			if set.records != nil || set.unknown != 0 || set.applied != 0 || set.skipped != 0 {
				t.Errorf("parseOverrides error with non-empty result: %+v", set)
			}
			return
		}
		for _, r := range set.records {
			if r.AniListID <= 0 {
				t.Errorf("parseOverrides retained a non-positive-AniList-ID record: %+v", r)
			}
			if r.Type != mediatype.Normalize(r.Type) {
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
	})
}
