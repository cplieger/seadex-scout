package mapping

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// FuzzParseFribb exercises the tolerant Fribb JSON decoder against arbitrary
// bytes. Seeds cover the shape variance the decoders tolerate: a numeric or
// string id, a scalar or array imdb_id, a {tv}/{movie} or non-object
// themoviedb_id, a zero-id record, malformed array elements, and non-array
// top-level shapes. Invariants hold for any input: parseFribb never panics, and
// every returned record is keyed (non-zero AniListID), type-normalized, and
// carries only trimmed non-empty imdb ids and non-zero tmdb movie ids.
func FuzzParseFribb(f *testing.F) {
	log := discardLogger()
	f.Add([]byte(`[{"anilist_id":1,"type":"tv","tvdb_id":"100","imdb_id":"tt1","themoviedb_id":{"tv":5}}]`))
	f.Add([]byte(`[{"anilist_id":"2","type":"MOVIE","imdb_id":["tt2","tt3"],"themoviedb_id":{"movie":[7,8]}}]`))
	f.Add([]byte(`[{"anilist_id":10,"type":"MOVIE","themoviedb_id":603}]`))
	f.Add([]byte(`[{"anilist_id":11,"type":" movie ","themoviedb_id":"603"}]`))
	f.Add([]byte(`[{"anilist_id":0}]`))
	f.Add([]byte(`[[],"x",5,{"anilist_id":9,"themoviedb_id":"unknown"}]`))
	f.Add([]byte(`null`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`[]`))
	f.Add([]byte(`[{"anilist_id":1}`))
	f.Add([]byte(`[{"anilist_id":1},!!!]`))
	f.Add([]byte(`[{"anilist_id":1}] {"extra":true}`))
	f.Add([]byte(`[{"anilist_id":5,"type":"tv","season":{"tvdb":2},"episode_offset":{"tvdb":12}}]`))
	f.Add([]byte(`[{"anilist_id":6,"tvdb_id":"2147483648","imdb_id":["tt1",5,null]}]`))
	f.Add([]byte(`[{"anilist_id":7,"themoviedb_id":{"movie":[1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,20,21,22,23,24,25,26,27,28,29,30,31,32,33]}}]`))
	f.Fuzz(func(t *testing.T, data []byte) {
		records, err := parseFribb(data, log)
		if err != nil {
			return
		}
		for _, r := range records {
			if r.AniListID == 0 {
				t.Errorf("parseFribb kept a record with zero AniListID: %+v", r)
			}
			if want := strings.ToUpper(strings.TrimSpace(r.Type)); r.Type != want {
				t.Errorf("parseFribb Type = %q, want normalized %q", r.Type, want)
			}
			for _, id := range r.TmdbMovies {
				if id == 0 {
					t.Errorf("parseFribb TmdbMovies contains zero: %+v", r)
				}
			}
			for _, s := range r.IMDbIDs {
				if s == "" || s != strings.TrimSpace(s) {
					t.Errorf("parseFribb IMDbIDs entry not trimmed/non-empty: %q", s)
				}
			}
		}
	})
}

// FuzzParseFribb_numericIDFormsEquivalent pins a cross-representation
// property on the number-or-string flexInt decoder sitting on the externally
// supplied Fribb JSON path: every int32 AniList/TVDB id must produce the same
// records whether upstream sends JSON numbers or numeric strings (including
// ids the validity invariant rejects, which must drop identically from both
// forms).
func FuzzParseFribb_numericIDFormsEquivalent(f *testing.F) {
	f.Add(int32(0))
	f.Add(int32(1))
	f.Add(int32(-1))
	f.Add(int32(2147483647))

	log := discardLogger()
	f.Fuzz(func(t *testing.T, id int32) {
		numberJSON := fmt.Appendf(nil, `[{"anilist_id":%d,"tvdb_id":%d,"type":"tv"}]`, id, id)
		stringJSON := fmt.Appendf(nil, `[{"anilist_id":%q,"tvdb_id":%q,"type":"tv"}]`, fmt.Sprint(id), fmt.Sprint(id))

		numberRecords, numberErr := parseFribb(numberJSON, log)
		stringRecords, stringErr := parseFribb(stringJSON, log)
		if numberErr != nil || stringErr != nil {
			t.Fatalf("parseFribb equivalent numeric forms: number error=%v, string error=%v", numberErr, stringErr)
		}
		if !reflect.DeepEqual(numberRecords, stringRecords) {
			t.Errorf("parseFribb numeric form = %#v, string form = %#v for id %d", numberRecords, stringRecords, id)
		}
	})
}
