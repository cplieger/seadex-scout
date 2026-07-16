package mapping

import (
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
	f.Add([]byte(`[{"anilist_id":0}]`))
	f.Add([]byte(`[[],"x",5,{"anilist_id":9,"themoviedb_id":"unknown"}]`))
	f.Add([]byte(`null`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`[]`))
	f.Add([]byte(`[{"anilist_id":1}`))
	f.Add([]byte(`[{"anilist_id":1},!!!]`))
	f.Add([]byte(`[{"anilist_id":1}] {"extra":true}`))
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
