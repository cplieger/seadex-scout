package mapping

import (
	"reflect"
	"testing"
)

// TestFlexInt_nonIntegerStringDecodesZero pins the string branch's Atoi
// fallback: a validly-quoted but non-integer numeric string ("12.5") decodes
// to 0 (a tolerated placeholder) rather than erroring or truncating to 12.
func TestFlexInt_nonIntegerStringDecodesZero(t *testing.T) {
	var f flexInt
	if err := f.UnmarshalJSON([]byte(`"12.5"`)); err != nil {
		t.Fatalf("UnmarshalJSON(%q) error: %v", `"12.5"`, err)
	}
	if int(f) != 0 {
		t.Errorf("flexInt(%q) = %d, want 0 (non-integer string tolerated as placeholder)", `"12.5"`, int(f))
	}
}

// TestFlexInt_malformedStringErrors pins that the string branch propagates a
// JSON syntax error (an unterminated string) instead of tolerating it.
func TestFlexInt_malformedStringErrors(t *testing.T) {
	var f flexInt
	if err := f.UnmarshalJSON([]byte(`"unterminated`)); err == nil {
		t.Error("UnmarshalJSON(unterminated string) = nil error, want syntax error")
	}
}

// TestStringList_numberScalarTolerated pins the scalar branch's tolerance: a
// non-string scalar (a bare number) decodes to an empty list rather than
// failing the record.
func TestStringList_numberScalarTolerated(t *testing.T) {
	var s stringList
	if err := s.UnmarshalJSON([]byte(`5`)); err != nil {
		t.Fatalf("UnmarshalJSON(5) error: %v", err)
	}
	if s != nil {
		t.Errorf("stringList(5) = %v, want nil (non-string scalar tolerated as empty)", []string(s))
	}
}

// TestStringList_mixedArrayKeepsStrings pins the ARRAY branch's tolerance
// (matching the scalar branch and the sibling flexInt/tmdbID decoders): a
// mixed-type imdb_id array keeps its valid string entries and drops the rest,
// and an array with no usable strings decodes to an empty list - never an
// error that would fail the whole record.
func TestStringList_mixedArrayKeepsStrings(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{name: "mixed types keep strings", in: `["tt1",5,"tt2"]`, want: []string{"tt1", "tt2"}},
		{name: "no usable strings decode empty", in: `[5,{"x":1},null]`, want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var s stringList
			if err := s.UnmarshalJSON([]byte(tc.in)); err != nil {
				t.Fatalf("UnmarshalJSON(%s) error: %v", tc.in, err)
			}
			if !reflect.DeepEqual([]string(s), tc.want) {
				t.Errorf("stringList(%s) = %v, want %v (non-string entries dropped, record never failed)", tc.in, []string(s), tc.want)
			}
		})
	}
}

// TestParseFribb_mixedImdbArrayKeepsRecord pins the record-level consequence
// of stringList's tolerant array branch: a record whose imdb_id array mixes
// types survives with its valid string entries, instead of the whole record
// being skipped.
func TestParseFribb_mixedImdbArrayKeepsRecord(t *testing.T) {
	data := []byte(`[
		{"anilist_id":1,"type":"tv","tvdb_id":100},
		{"anilist_id":2,"type":"tv","imdb_id":["tt1",5]}
	]`)
	records, err := parseFribb(data, discardLogger())
	if err != nil {
		t.Fatalf("parseFribb error: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("parseFribb kept %d records, want 2 (mixed imdb_id record survives)", len(records))
	}
	if !reflect.DeepEqual(records[1].IMDbIDs, []string{"tt1"}) {
		t.Errorf("mixed-array record IMDbIDs = %v, want [tt1] (valid entry kept, non-string dropped)", records[1].IMDbIDs)
	}
}

// TestParseFribb_fractionalAndNegativeIDsAbsent pins the record-level
// consequence of setNumber's validity invariant: a fractional id decodes as
// absent (not truncated - 9.9 truncated to 9 would point at a different
// anime) and a negative id decodes as absent (it must not count toward the
// arr-identifier acceptance floor), while both records survive.
func TestParseFribb_fractionalAndNegativeIDsAbsent(t *testing.T) {
	data := []byte(`[
		{"anilist_id":7,"type":"tv","tvdb_id":9.9},
		{"anilist_id":8,"type":"tv","tvdb_id":-5}
	]`)
	records, err := parseFribb(data, discardLogger())
	if err != nil {
		t.Fatalf("parseFribb error: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("parseFribb kept %d records, want 2 (odd ids zero the field, never fail the record)", len(records))
	}
	for _, rec := range records {
		if rec.TvdbID != 0 {
			t.Errorf("record %d TvdbID = %d, want 0 (fractional/negative treated absent)", rec.AniListID, rec.TvdbID)
		}
		if rec.HasArrIdentifier() {
			t.Errorf("record %d HasArrIdentifier = true, want false (absent id must not count toward the acceptance floor)", rec.AniListID)
		}
	}
}

// TestParseFribb_oddSeasonShapesSurvive pins the season tolerance boundary:
// an odd upstream season shape (a bare number, a float or quoted interior, a
// garbage string) zeroes or best-effort-decodes the field instead of failing
// json.Unmarshal for the whole record - the record survives, and SeasonTvdb 0
// falls back to whole-series/season-0 scoping.
func TestParseFribb_oddSeasonShapesSurvive(t *testing.T) {
	tests := []struct {
		name   string
		season string
		want   int
	}{
		{name: "object form decodes", season: `{"tvdb":2}`, want: 2},
		{name: "quoted interior decodes via flexInt", season: `{"tvdb":"3"}`, want: 3},
		{name: "float interior treated absent", season: `{"tvdb":1.5}`, want: 0},
		{name: "bare number treated absent", season: `1`, want: 0},
		{name: "garbage string treated absent", season: `"x"`, want: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data := []byte(`[{"anilist_id":5,"type":"tv","tvdb_id":100,"season":` + tc.season + `}]`)
			records, err := parseFribb(data, discardLogger())
			if err != nil {
				t.Fatalf("parseFribb error: %v", err)
			}
			if len(records) != 1 {
				t.Fatalf("parseFribb kept %d records, want 1 (odd season must not fail the record)", len(records))
			}
			if records[0].SeasonTvdb != tc.want {
				t.Errorf("SeasonTvdb = %d, want %d", records[0].SeasonTvdb, tc.want)
			}
		})
	}
}

// TestParseFribb_oddTypeShapesSurvive pins the type tolerance boundary: a
// non-string type (a bare number, a float) decodes as empty - routing the
// record as a non-movie series, the safe default - instead of failing the
// whole record; a real string still normalizes.
func TestParseFribb_oddTypeShapesSurvive(t *testing.T) {
	data := []byte(`[
		{"anilist_id":1,"type":1,"tvdb_id":100},
		{"anilist_id":2,"type":1.5,"tvdb_id":200},
		{"anilist_id":3,"type":"movie","tvdb_id":300}
	]`)
	records, err := parseFribb(data, discardLogger())
	if err != nil {
		t.Fatalf("parseFribb error: %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("parseFribb kept %d records, want 3 (odd type must not fail the record)", len(records))
	}
	if records[0].Type != "" || records[1].Type != "" {
		t.Errorf("odd-type records Type = %q, %q, want empty (safe non-movie default)", records[0].Type, records[1].Type)
	}
	if records[2].Type != "MOVIE" {
		t.Errorf("string type = %q, want MOVIE", records[2].Type)
	}
}

// TestTmdbID_badInteriorTolerated pins the object branch's tolerance: an
// object whose movie field has the wrong type decodes to an empty tmdbID
// rather than failing the record.
func TestTmdbID_badInteriorTolerated(t *testing.T) {
	var got tmdbID
	if err := got.UnmarshalJSON([]byte(`{"movie":"x"}`)); err != nil {
		t.Fatalf("UnmarshalJSON({movie:x}) error: %v", err)
	}
	if len(got.Movie) != 0 {
		t.Errorf("tmdbID({movie:x}) = %+v, want empty (bad interior tolerated)", got)
	}
}
