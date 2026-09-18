package mapping

import (
	"reflect"
	"testing"
)

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
// consequence of the tolerant decode's validity invariant: a fractional id decodes as
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

// TestParseFribb_oddSeasonShapesSurvive pins the season tolerance boundary AND
// the presence bound with it: an odd upstream season shape zeroes the number
// instead of failing json.Unmarshal for the whole record, and only a tvdb member
// that is a JSON number >= 0 reads present. Two shapes with no live rows are
// pinned here rather than argued: a quoted "3" is not a JSON number so it reads
// ABSENT with no season, and a non-negative fractional reads PRESENT at the
// season flexInt collapses it to.
func TestParseFribb_oddSeasonShapesSurvive(t *testing.T) {
	tests := []struct {
		name   string
		season string
		want   int
		kind   SeasonKind
	}{
		{name: "object form decodes present", season: `{"tvdb":2}`, want: 2, kind: SeasonPresent},
		{name: "mapped zero is present", season: `{"tvdb":0}`, want: 0, kind: SeasonPresent},
		{name: "quoted interior is absent", season: `{"tvdb":"3"}`, want: 0, kind: SeasonAbsent},
		{name: "float interior is present at season zero", season: `{"tvdb":1.5}`, want: 0, kind: SeasonPresent},
		{name: "negative interior is absent", season: `{"tvdb":-1}`, want: 0, kind: SeasonAbsent},
		{name: "null interior is absent", season: `{"tvdb":null}`, want: 0, kind: SeasonAbsent},
		{name: "tmdb-only object is absent", season: `{"tmdb":1}`, want: 0, kind: SeasonAbsent},
		{name: "null season is absent", season: `null`, want: 0, kind: SeasonAbsent},
		{name: "bare number is absent", season: `1`, want: 0, kind: SeasonAbsent},
		{name: "garbage string is absent", season: `"x"`, want: 0, kind: SeasonAbsent},
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
			if records[0].SeasonKind != tc.kind {
				t.Errorf("SeasonKind = %q, want %q", records[0].SeasonKind, tc.kind)
			}
		})
	}
}

// TestParseFribb_absentSeasonKeyDecodesAbsent proves the kind is set in toRecord
// rather than in seasonObject.UnmarshalJSON: encoding/json never calls an
// UnmarshalJSON for a key the object does not carry, and 31,889 of the live
// map's 39,304 records carry no season key at all.
func TestParseFribb_absentSeasonKeyDecodesAbsent(t *testing.T) {
	records, err := parseFribb([]byte(`[{"anilist_id":820,"type":"ova","tvdb_id":78964}]`), discardLogger())
	if err != nil {
		t.Fatalf("parseFribb error: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("parseFribb kept %d records, want 1", len(records))
	}
	if got := records[0].SeasonKind; got != SeasonAbsent {
		t.Errorf("record with no season key: SeasonKind = %q, want %q", got, SeasonAbsent)
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

// TestSeasonObject_malformedObjectTolerated pins the object branch's tolerance:
// syntactically broken object bytes decode to a zero seasonObject (SeasonTvdb 0,
// the whole-series/season-0 fallback) with a nil error rather than failing.
func TestSeasonObject_malformedObjectTolerated(t *testing.T) {
	var o seasonObject
	if err := o.UnmarshalJSON([]byte(`{"tvdb":`)); err != nil {
		t.Fatalf("UnmarshalJSON(malformed object) error: %v", err)
	}
	if season, present := o.season(); season != 0 || present {
		t.Errorf("seasonObject(malformed object).season() = %d, %v, want 0, false", season, present)
	}
}

// TestFlexString_malformedStringErrors pins that the string branch propagates
// a JSON syntax error (an unterminated string) instead of tolerating it,
// matching the sibling flexInt malformed-string contract.
func TestFlexString_malformedStringErrors(t *testing.T) {
	var s flexString
	if err := s.UnmarshalJSON([]byte(`"unterminated`)); err == nil {
		t.Error("UnmarshalJSON(unterminated string) = nil error, want syntax error")
	}
	if s != "" {
		t.Errorf("flexString(unterminated) = %q, want empty (no partial value retained)", string(s))
	}
}

// TestStringList_malformedArrayTolerated pins the array branch's outer
// tolerance: syntactically broken array bytes decode to an empty list with a
// nil error rather than failing the record.
func TestStringList_malformedArrayTolerated(t *testing.T) {
	var s stringList
	if err := s.UnmarshalJSON([]byte(`["tt1"`)); err != nil {
		t.Fatalf("UnmarshalJSON(malformed array) error: %v", err)
	}
	if s != nil {
		t.Errorf("stringList(malformed array) = %v, want nil", []string(s))
	}
}

// TestTolerantDecoders_resetOnReuse pins the duplicate-key reset invariant on
// each app-owned tolerant decoder directly (flexInt is covered in jsonx):
// encoding/json processes duplicate object keys in order against the SAME
// field receiver, so a later tolerated-odd value must clear the earlier
// decode, not silently retain it.
func TestTolerantDecoders_resetOnReuse(t *testing.T) {
	var s flexString
	if err := s.UnmarshalJSON([]byte(`"MOVIE"`)); err != nil || string(s) != "MOVIE" {
		t.Fatalf("flexString first decode = %q, %v, want MOVIE, nil", string(s), err)
	}
	if err := s.UnmarshalJSON([]byte(`7`)); err != nil || string(s) != "" {
		t.Errorf("flexString reused with odd value = %q, %v, want reset to empty, nil", string(s), err)
	}

	var l stringList
	if err := l.UnmarshalJSON([]byte(`["tt1"]`)); err != nil || len(l) != 1 {
		t.Fatalf("stringList first decode = %v, %v, want [tt1], nil", []string(l), err)
	}
	if err := l.UnmarshalJSON([]byte(`false`)); err != nil || l != nil {
		t.Errorf("stringList reused with odd value = %v, %v, want reset to nil, nil", []string(l), err)
	}

	var tm tmdbID
	if err := tm.UnmarshalJSON([]byte(`{"movie":[9]}`)); err != nil || len(tm.Movie) != 1 {
		t.Fatalf("tmdbID first decode = %+v, %v, want one movie id, nil", tm, err)
	}
	if err := tm.UnmarshalJSON([]byte(`3`)); err != nil || len(tm.Movie) != 0 {
		t.Errorf("tmdbID reused with odd value = %+v, %v, want reset to empty, nil", tm, err)
	}

	var o seasonObject
	if err := o.UnmarshalJSON([]byte(`{"tvdb":3}`)); err != nil {
		t.Fatalf("seasonObject first decode error = %v, want nil", err)
	}
	if season, present := o.season(); season != 3 || !present {
		t.Fatalf("seasonObject first decode = %d, %v, want 3, true", season, present)
	}
	if err := o.UnmarshalJSON([]byte(`4`)); err != nil {
		t.Fatalf("seasonObject reused with odd value error = %v, want nil", err)
	}
	if season, present := o.season(); season != 0 || present {
		t.Errorf("seasonObject reused with odd value = %d, %v, want reset to 0, false", season, present)
	}
}

// TestParseFribb_duplicateKeysLaterOddValueWins pins the documented per-field
// tolerance semantics under duplicate JSON keys end-to-end: a later odd
// anilist_id zeroes the key and drops the record, and later odd type/tvdb_id
// values decode as empty/zero instead of retaining the earlier valid values.
func TestParseFribb_duplicateKeysLaterOddValueWins(t *testing.T) {
	data := []byte(`[
		{"anilist_id":1,"anilist_id":false,"type":"MOVIE","tvdb_id":42},
		{"anilist_id":2,"type":"MOVIE","type":7,"tvdb_id":42,"tvdb_id":false}
	]`)
	records, err := parseFribb(data, discardLogger())
	if err != nil {
		t.Fatalf("parseFribb: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("parseFribb kept %d records, want 1 (later odd anilist_id drops its record)", len(records))
	}
	if records[0].AniListID != 2 {
		t.Errorf("surviving record AniListID = %d, want 2", records[0].AniListID)
	}
	if records[0].Type != "" {
		t.Errorf("duplicate-key Type = %q, want empty (later odd value wins)", records[0].Type)
	}
	if records[0].TvdbID != 0 {
		t.Errorf("duplicate-key TvdbID = %d, want 0 (later odd value wins)", records[0].TvdbID)
	}
}
