package mapping

import (
	"reflect"
	"slices"
	"sort"
	"testing"
)

func TestRecord_IsMovie(t *testing.T) {
	if !(&Record{Type: "MOVIE"}).IsMovie() {
		t.Error("Record{MOVIE}.IsMovie() = false, want true")
	}
	if (&Record{Type: "TV"}).IsMovie() {
		t.Error("Record{TV}.IsMovie() = true, want false")
	}
}

func TestRecord_IsSpecial(t *testing.T) {
	tests := map[string]bool{
		"OVA": true, "ONA": true, "SPECIAL": true, "MUSIC": true,
		"TV": false, "MOVIE": false, "": false,
	}
	for typ, want := range tests {
		if got := (&Record{Type: typ}).IsSpecial(); got != want {
			t.Errorf("Record{%q}.IsSpecial() = %v, want %v", typ, got, want)
		}
	}
}

// TestRecord_HasArrIdentifier pins the arr-routed identifier predicate: only
// the fields the record's routed arr consumes count (TMDB-movie/IMDb for
// movies, TVDB for series), so a wrong-arm identifier can neither satisfy the
// refresh coverage floor nor catalogue an item for the opposite arr.
func TestRecord_HasArrIdentifier(t *testing.T) {
	tests := []struct {
		name string
		rec  Record
		want bool
	}{
		{"series with tvdb", Record{Type: "TV", TvdbID: 100}, true},
		{"series with only movie ids", Record{Type: "TV", TmdbMovies: []int{4}, IMDbIDs: []string{"tt1"}}, false},
		{"movie with tmdb", Record{Type: "MOVIE", TmdbMovies: []int{4}}, true},
		{"movie with imdb", Record{Type: "MOVIE", IMDbIDs: []string{"tt1"}}, true},
		{"movie with only tvdb", Record{Type: "MOVIE", TvdbID: 100}, false},
		{"no ids", Record{Type: "TV"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.rec.HasArrIdentifier(); got != tt.want {
				t.Errorf("HasArrIdentifier() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestArrIdentifierCountIgnoresWrongArmIdentifiers pins the refresh coverage
// guard to the same arr-routed predicate the matcher uses: a TV record
// carrying only movie ids (or a MOVIE record carrying only a TVDB id) cannot
// count toward the acceptance floor, because findByID would never consume
// those fields for that record's arr.
func TestArrIdentifierCountIgnoresWrongArmIdentifiers(t *testing.T) {
	records := []Record{
		{AniListID: 1, Type: "TV", TmdbMovies: []int{4}, IMDbIDs: []string{"tt1"}},
		{AniListID: 2, Type: "MOVIE", TvdbID: 100},
		{AniListID: 3, Type: "TV", TvdbID: 100},
		{AniListID: 4, Type: "MOVIE", IMDbIDs: []string{"tt2"}},
	}
	if got := arrIdentifierCount(records); got != 2 {
		t.Errorf("arrIdentifierCount = %d, want 2 (wrong-arm identifiers must not count)", got)
	}
}

func TestBuildIndex_dedupAndZeroSkip(t *testing.T) {
	idx := buildIndex([]Record{
		{AniListID: 1, Type: "TV"},
		{AniListID: 0, Type: "TV"},
		{AniListID: 1, Type: "MOVIE"},
	})
	if idx.Len() != 1 {
		t.Fatalf("buildIndex Len = %d, want 1 (zero-id skipped, dup collapsed)", idx.Len())
	}
	rec, ok := idx.Lookup(1)
	if !ok {
		t.Fatal("Lookup(1) not found")
	}
	if rec.Type != "MOVIE" {
		t.Errorf("Lookup(1).Type = %q, want MOVIE (last write wins)", rec.Type)
	}
}

func TestIndex_nilSafe(t *testing.T) {
	var idx *Index
	if _, ok := idx.Lookup(1); ok {
		t.Error("nil Index Lookup returned ok=true")
	}
	if idx.Len() != 0 {
		t.Error("nil Index Len != 0")
	}
	called := false
	idx.ForEachRecord(func(Record) { called = true })
	if called {
		t.Error("nil Index ForEachRecord invoked fn")
	}
}

func TestIndex_ForEachRecordAndNewIndex(t *testing.T) {
	idx := NewIndex([]Record{{AniListID: 1}, {AniListID: 2}})
	var got []int
	idx.ForEachRecord(func(r Record) { got = append(got, r.AniListID) })
	sort.Ints(got)
	if !reflect.DeepEqual(got, []int{1, 2}) {
		t.Errorf("ForEachRecord visited %v, want [1 2]", got)
	}
}

func TestParseOverrides(t *testing.T) {
	recs, unknown, err := parseOverrides([]byte(`[{"anilist_id":5,"type":"  movie  "}]`))
	if err != nil {
		t.Fatalf("parseOverrides error: %v", err)
	}
	if len(recs) != 1 || recs[0].Type != "MOVIE" {
		t.Fatalf("parseOverrides = %+v, want one record with Type MOVIE", recs)
	}
	if len(unknown) != 0 {
		t.Errorf("unknown keys = %v, want none for a well-formed override", unknown)
	}
	if _, _, err := parseOverrides([]byte(`{bad`)); err == nil {
		t.Error("parseOverrides(malformed) = nil error, want error")
	}
}

// TestParseOverridesReportsUnknownKeys pins the unknown-key detection: an
// operator writing the upstream Fribb field names (imdb_id, themoviedb_id,
// season) instead of the override names gets them reported (sorted, deduped)
// while the records still parse.
func TestParseOverridesReportsUnknownKeys(t *testing.T) {
	data := []byte(`[{"anilist_id":5,"imdb_id":"tt1","season":1},{"anilist_id":6,"imdb_id":"tt2","themoviedb_id":9}]`)
	recs, unknown, err := parseOverrides(data)
	if err != nil {
		t.Fatalf("parseOverrides error: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("records = %d, want 2 (unknown keys do not reject the record)", len(recs))
	}
	want := []string{"imdb_id", "season", "themoviedb_id"}
	if !slices.Equal(unknown, want) {
		t.Errorf("unknown keys = %v, want %v (sorted, deduped)", unknown, want)
	}
}

// TestParseOverridesAcceptsCaseVariantKeys pins the diagnostic's key matching
// to encoding/json's: a case-variant canonical key (e.g. "ANILIST_ID", "TYPE")
// is decoded and applied by the typed unmarshal, so it must not be reported as
// unknown and "ignored" - that would tell the operator an accepted field was
// discarded.
func TestParseOverridesAcceptsCaseVariantKeys(t *testing.T) {
	recs, unknown, err := parseOverrides([]byte(`[{"ANILIST_ID":5,"TYPE":"movie"}]`))
	if err != nil {
		t.Fatalf("parseOverrides error: %v", err)
	}
	if len(recs) != 1 || recs[0].AniListID != 5 || recs[0].Type != "MOVIE" {
		t.Fatalf("parseOverrides = %+v, want one record with AniListID 5 and Type MOVIE", recs)
	}
	if len(unknown) != 0 {
		t.Errorf("unknown keys = %v, want none for case-variant canonical keys (encoding/json accepts them)", unknown)
	}
}
