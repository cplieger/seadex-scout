package mapping

import (
	"slices"
	"strings"
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
		{"series with negative tvdb", Record{Type: "TV", TvdbID: -1}, false},
		{"movie with zero tmdb entry", Record{Type: "MOVIE", TmdbMovies: []int{0}}, false},
		{"movie with blank imdb entry", Record{Type: "MOVIE", IMDbIDs: []string{"  "}}, false},
		{"movie with zero then valid tmdb", Record{Type: "MOVIE", TmdbMovies: []int{0, 4}}, true},
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
// count toward the acceptance floor, because FindByID would never consume
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
	slices.Sort(got)
	if !slices.Equal(got, []int{1, 2}) {
		t.Errorf("ForEachRecord visited %v, want [1 2]", got)
	}
}

func TestParseOverrides(t *testing.T) {
	set, err := parseOverrides([]byte(`[{"anilist_id":5,"type":"  movie  ","imdb_ids":[" tt2222222 ",""]}]`))
	if err != nil {
		t.Fatalf("parseOverrides error: %v", err)
	}
	if len(set.records) != 1 || set.records[0].Type != "MOVIE" {
		t.Fatalf("parseOverrides = %+v, want one record with Type MOVIE", set.records)
	}
	// IMDb ids must be normalized like Fribb's (trimmed, blanks dropped) so
	// HasArrIdentifier, findMovie, and the report catalogue agree on the
	// exact lookup key.
	if got := set.records[0].IMDbIDs; !slices.Equal(got, []string{"tt2222222"}) {
		t.Errorf("IMDbIDs = %v, want [tt2222222] (trimmed, blank dropped)", got)
	}
	if len(set.unknown) != 0 {
		t.Errorf("unknown keys = %v, want none for a well-formed override", set.unknown)
	}
	if _, err := parseOverrides([]byte(`{bad`)); err == nil {
		t.Error("parseOverrides(malformed) = nil error, want error")
	}
	if _, err := parseOverrides([]byte(`null`)); err == nil {
		t.Error("parseOverrides(null) = nil error, want error (a non-array top level must not read as an empty overlay)")
	}
	if _, err := parseOverrides([]byte(`[] trailing`)); err == nil {
		t.Error("parseOverrides(trailing data) = nil error, want error (json.Unmarshal parity)")
	}
}

// TestParseOverridesReportsUnknownKeys pins the unknown-key detection: an
// operator writing the upstream Fribb field names (imdb_id, themoviedb_id,
// season) instead of the override names gets them reported (sorted, deduped)
// while the records still parse.
func TestParseOverridesReportsUnknownKeys(t *testing.T) {
	data := []byte(`[{"anilist_id":5,"imdb_id":"tt1","season":1},{"anilist_id":6,"imdb_id":"tt2","themoviedb_id":9}]`)
	set, err := parseOverrides(data)
	if err != nil {
		t.Fatalf("parseOverrides error: %v", err)
	}
	if len(set.records) != 2 {
		t.Fatalf("records = %d, want 2 (unknown keys do not reject the record)", len(set.records))
	}
	want := []string{"imdb_id", "season", "themoviedb_id"}
	if !slices.Equal(set.unknown, want) {
		t.Errorf("unknown keys = %v, want %v (sorted, deduped)", set.unknown, want)
	}
}

// TestParseOverridesAcceptsCaseVariantKeys pins the diagnostic's key matching
// to encoding/json's: a case-variant canonical key (e.g. "ANILIST_ID", "TYPE")
// is decoded and applied by the typed unmarshal, so it must not be reported as
// unknown and "ignored" - that would tell the operator an accepted field was
// discarded.
func TestParseOverridesAcceptsCaseVariantKeys(t *testing.T) {
	set, err := parseOverrides([]byte(`[{"ANILIST_ID":5,"TYPE":"movie"}]`))
	if err != nil {
		t.Fatalf("parseOverrides error: %v", err)
	}
	if len(set.records) != 1 || set.records[0].AniListID != 5 || set.records[0].Type != "MOVIE" {
		t.Fatalf("parseOverrides = %+v, want one record with AniListID 5 and Type MOVIE", set.records)
	}
	if len(set.unknown) != 0 {
		t.Errorf("unknown keys = %v, want none for case-variant canonical keys (encoding/json accepts them)", set.unknown)
	}
}

// TestNewIndex_ignoresZeroAndKeepsLastDuplicate pins the public NewIndex
// contract consumers rely on: zero AniList IDs are omitted (unkeyable) and
// the last duplicate wins, so upstream ordering cannot silently retain a
// stale record.
func TestNewIndex_ignoresZeroAndKeepsLastDuplicate(t *testing.T) {
	idx := NewIndex([]Record{
		{AniListID: 0, Type: "TV", TvdbID: 99},
		{AniListID: 42, Type: "TV", TvdbID: 100},
		{AniListID: 42, Type: "TV", TvdbID: 200},
	})

	if got := idx.Len(); got != 1 {
		t.Errorf("NewIndex length = %d, want 1", got)
	}
	got, ok := idx.Lookup(42)
	if !ok {
		t.Fatal("NewIndex lookup 42 missing")
	}
	if got.TvdbID != 200 {
		t.Errorf("NewIndex duplicate TVDB ID = %d, want last value 200", got.TvdbID)
	}
	if _, ok := idx.Lookup(0); ok {
		t.Error("NewIndex retained zero AniList ID")
	}
}

// TestParseOverrides_reportsEachDuplicateIDOnce pins the duplicate diagnostic
// population: each distinct duplicated AniList ID is reported once (on its
// first repeated occurrence), so a heavily repeated first ID cannot fill the
// bounded log prefix and hide later duplicated IDs, while the effective set
// keeps last-record-wins and applied still counts every keyed transport row.
func TestParseOverrides_reportsEachDuplicateIDOnce(t *testing.T) {
	set, err := parseOverrides([]byte(`[
		{"anilist_id":1,"type":"TV","tvdb_id":10},
		{"anilist_id":1,"type":"TV","tvdb_id":11},
		{"anilist_id":1,"type":"TV","tvdb_id":12},
		{"anilist_id":2,"type":"TV","tvdb_id":20},
		{"anilist_id":2,"type":"TV","tvdb_id":21}
	]`))
	if err != nil {
		t.Fatalf("parseOverrides error: %v", err)
	}
	if set.applied != 5 {
		t.Errorf("applied = %d, want 5 (every keyed record applies)", set.applied)
	}
	if !slices.Equal(set.duplicates, []int{1, 2}) {
		t.Errorf("duplicates = %v, want [1 2] (each distinct duplicated ID once)", set.duplicates)
	}
	if len(set.records) != 2 {
		t.Fatalf("effective records = %d, want 2 (deduplicated during the stream)", len(set.records))
	}
	idx := NewIndex(nil)
	for _, r := range set.records {
		idx.byAniList[r.AniListID] = r
	}
	if got, ok := idx.Lookup(1); !ok || got.TvdbID != 12 {
		t.Errorf("Lookup(1) = %+v, %v, want last record with TvdbID 12", got, ok)
	}
	if got, ok := idx.Lookup(2); !ok || got.TvdbID != 21 {
		t.Errorf("Lookup(2) = %+v, %v, want last record with TvdbID 21", got, ok)
	}
}

// TestParseOverrides_discardsSemanticallyEmptyRowsDuringStream pins the
// memory-amplification regression (a valid compact array of empty objects
// fits under maxOverrideBytes but used to be materialized whole three times
// before every row was discarded): a large all-empty-object array parses to
// an EMPTY effective overlay with the exact skipped count, allocating no
// []Record growth per transport row.
func TestParseOverrides_discardsSemanticallyEmptyRowsDuringStream(t *testing.T) {
	const rows = 100_000
	data := []byte("[" + strings.Repeat("{},", rows-1) + "{}]")
	set, err := parseOverrides(data)
	if err != nil {
		t.Fatalf("parseOverrides error: %v", err)
	}
	if len(set.records) != 0 || cap(set.records) != 0 {
		t.Errorf("effective records len=%d cap=%d, want 0/0 (zero-ID rows discarded during the stream)", len(set.records), cap(set.records))
	}
	if set.skipped != rows {
		t.Errorf("skipped = %d, want the exact discarded row count %d", set.skipped, rows)
	}
	if set.applied != 0 || len(set.duplicates) != 0 || len(set.unknown) != 0 {
		t.Errorf("applied=%d duplicates=%v unknown=%v, want all empty", set.applied, set.duplicates, set.unknown)
	}
}

// TestParseOverrides_partialRawDecodeYieldsNoUnknownKeys pins the documented
// error contract the old whole-document key scan carried: for a partially
// decodable array ([{"weird":1},5]) the typed decode rejects the file, so no
// unknown keys from the decodable prefix may leak out - the error return
// carries an empty set and readOverrides logs the malformed-file WARN.
func TestParseOverrides_partialRawDecodeYieldsNoUnknownKeys(t *testing.T) {
	set, err := parseOverrides([]byte(`[{"weird":1},5]`))
	if err == nil {
		t.Fatal("parseOverrides(partially decodable array) = nil error, want typed-decode error")
	}
	if set.unknown != nil {
		t.Errorf("unknown keys on error = %v, want nil", set.unknown)
	}
}

// TestParseOverrides_typedDecodeErrorPropagates pins the typed-decode error
// return: a document that passes the top-level array check but fails
// json.Unmarshal into []Record must surface the error so readOverrides logs
// the malformed-file WARN, never a silently-empty overlay.
func TestParseOverrides_typedDecodeErrorPropagates(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{name: "non-object element", in: `[5]`},
		{name: "wrong-typed field", in: `[{"anilist_id":"not-a-number"}]`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			set, err := parseOverrides([]byte(tc.in))
			if err == nil {
				t.Fatalf("parseOverrides(%s) = nil error, want typed-decode error", tc.in)
			}
			if set.records != nil || set.unknown != nil {
				t.Errorf("parseOverrides(%s) = %v, %v, want nil records and nil unknown keys on error", tc.in, set.records, set.unknown)
			}
		})
	}
}

// TestParseOverrides_streamDecodeErrorsPropagate pins the two element-stream
// error branches of parseOverrides that the array-guard and typed-decode
// tests cannot reach: a malformed array element that fails dec.Decode (a bare
// token or a row truncated mid-stream) and a truncated document that ends
// before the closing ']'. Each must surface an error with an empty result set
// so readOverrides logs the malformed-file WARN instead of applying a partial
// overlay.
func TestParseOverrides_streamDecodeErrorsPropagate(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{name: "malformed element token", in: `[bad]`},
		{name: "element truncated after comma", in: `[{"anilist_id":1},`},
		{name: "array missing closing bracket", in: `[{"anilist_id":1,"type":"tv"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			set, err := parseOverrides([]byte(tc.in))
			if err == nil {
				t.Fatalf("parseOverrides(%q) = nil error, want stream-decode error", tc.in)
			}
			if set.records != nil || set.unknown != nil || set.duplicates != nil || set.applied != 0 || set.skipped != 0 {
				t.Errorf("parseOverrides(%q) error carried a partial result: %+v", tc.in, set)
			}
		})
	}
}
