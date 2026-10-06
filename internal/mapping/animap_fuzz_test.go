package mapping

import (
	"slices"
	"testing"

	"github.com/cplieger/seadex-scout/internal/mediatype"
)

// FuzzParseAnimap exercises the animap.json decode boundary against arbitrary
// bytes. Invariants hold for any input: an error yields a zero result; a
// success retains only canonical, positively keyed records, never more records
// than elements, never more than the retained budget, and facts only under
// positive keys.
func FuzzParseAnimap(f *testing.F) {
	f.Add([]byte(animapDoc(`[{"anilist_id":1,"anidb_id":2,"type":" tv ","tvdb_id":3,"tvdb_season":0,` +
		`"mapping_list":[{"anidb_season":1,"tvdb_season":0,"episodes":[[1,4]]}]}]`)))
	f.Add([]byte(animapDoc(`[{"anidb_id":9,"tvdb_id":1,"mapping_list":[{"anidb_season":1,"tvdb_season":2,"start":14,"end":30}]}]`)))
	f.Add([]byte(animapDoc(`[{"anilist_id":5,"anidb_parent":{"anidb_id":6,"specials":[1]},"tvdb_id":1,"tvdb_season":0,` +
		`"mapping_list":[{"anidb_season":1,"tvdb_season":0,"episodes":[[1,8]]}]}]`)))
	f.Add([]byte(animapDoc(`[{"anilist_id":-1,"tmdb_movie_ids":[-3,0,4],"imdb_ids":[" ",""]},{"anilist_id":"x"},5,null]`)))
	f.Add([]byte(`{"version":2,"records":[]}`))
	f.Add([]byte(`{"records":[],"records":[]}`))
	f.Add([]byte(`[{"anilist_id":1}]`))
	f.Add([]byte(`null`))
	f.Add([]byte(``))
	f.Add([]byte(`{"version":1,"records":[{"anilist_id":1,`))
	f.Add([]byte(`{"version":1,"records":[]} {}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		parsed, err := parseAnimap(data, discardLogger())
		if err != nil {
			if parsed.records != nil || parsed.mappings != nil || parsed.parentMappings != nil || parsed.elements != 0 {
				t.Errorf("parseAnimap error %v with a non-empty result: %+v", err, parsed)
			}
			return
		}
		if len(parsed.records) > parsed.elements || parsed.elements > maxRecords {
			t.Errorf("parseAnimap records %d, elements %d: want records <= elements <= %d", len(parsed.records), parsed.elements, maxRecords)
		}
		retained := 0
		for _, r := range parsed.records {
			if r.AniListID <= 0 {
				t.Errorf("parseAnimap retained a non-positive AniList id: %+v", r)
			}
			if r.Type != mediatype.Normalize(r.Type) || slices.ContainsFunc(r.TmdbMovies, func(v int) bool { return v <= 0 }) {
				t.Errorf("parseAnimap record not canonical: %+v", r)
			}
			if r.SeasonKind != SeasonPresent && r.SeasonKind != SeasonAbsent {
				t.Errorf("parseAnimap record season kind %q, want present or absent", r.SeasonKind)
			}
			retained += len(r.IMDbIDs) + len(r.TmdbMovies)
		}
		if retained > maxRetainedTotal {
			t.Errorf("parseAnimap retained %d identifiers, want at most %d", retained, maxRetainedTotal)
		}
		for _, facts := range []map[int]Mapping{parsed.mappings, parsed.parentMappings} {
			for k, m := range facts {
				if k <= 0 || m.empty() {
					t.Errorf("parseAnimap kept facts %+v under key %d, want non-empty facts under a positive key", m, k)
				}
			}
		}
	})
}
