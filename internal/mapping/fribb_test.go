package mapping

import (
	"log/slog"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/cplieger/slogx/capture"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func TestParseFribb(t *testing.T) {
	data := []byte(`[
		{"anilist_id":1,"type":"tv","tvdb_id":100},
		[],
		{"anilist_id":0,"type":"tv"},
		{"anilist_id":"3","type":"  movie  ","imdb_id":"tt3","themoviedb_id":{"movie":[42]}}
	]`)
	records, err := parseFribb(data, discardLogger())
	if err != nil {
		t.Fatalf("parseFribb returned error: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("parseFribb kept %d records, want 2 (malformed element and zero-id dropped)", len(records))
	}
	if records[0].AniListID != 1 || records[0].Type != "TV" || records[0].TvdbID != 100 {
		t.Errorf("record 0 = %+v, want AniListID 1 / Type TV / TvdbID 100", records[0])
	}
	if records[1].AniListID != 3 || records[1].Type != "MOVIE" {
		t.Errorf("record 1 = %+v, want AniListID 3 / Type MOVIE", records[1])
	}
	if len(records[1].TmdbMovies) != 1 || records[1].TmdbMovies[0] != 42 {
		t.Errorf("record 1 TmdbMovies = %v, want [42]", records[1].TmdbMovies)
	}
}

func TestParseFribb_nonArrayErrors(t *testing.T) {
	if _, err := parseFribb([]byte(`{"anilist_id":1}`), discardLogger()); err == nil {
		t.Fatal("parseFribb(object) = nil error, want error")
	}
}

// TestParseFribb_recordCap pins the hard acceptance cap: a list exceeding
// maxFribbRecords is rejected (so refreshCache keeps the stale cache) rather
// than amplifying an upstream-controlled body into a huge in-memory record set,
// while a below-cap list the size of the real ~40k-record Fribb file is still
// accepted in full.
func TestParseFribb_recordCap(t *testing.T) {
	build := func(n int) []byte {
		var b strings.Builder
		b.WriteByte('[')
		for i := range n {
			if i > 0 {
				b.WriteByte(',')
			}
			// Tiny but valid records with a non-zero AniList ID so they survive
			// toRecord (the amplification path the cap defends against).
			b.WriteString(`{"anilist_id":`)
			b.WriteString(strconv.Itoa(i + 1))
			b.WriteByte('}')
		}
		b.WriteByte(']')
		return []byte(b.String())
	}

	if _, err := parseFribb(build(maxFribbRecords+1), discardLogger()); err == nil {
		t.Fatalf("parseFribb(%d records) = nil error, want over-cap error", maxFribbRecords+1)
	}

	const below = 40000 // ~ the real Fribb file size, comfortably under the cap
	records, err := parseFribb(build(below), discardLogger())
	if err != nil {
		t.Fatalf("parseFribb(%d records) returned error: %v", below, err)
	}
	if len(records) != below {
		t.Fatalf("parseFribb kept %d records, want %d (real-size body must be accepted in full)", len(records), below)
	}
}

// TestParseFribb_overCapStopsEarly pins that the record cap is enforced while
// STREAMING: an over-cap array of tiny elements is rejected before the rest of
// the body is consumed or materialized. The tail after the cap point is
// deliberately invalid JSON — a decoder that materialized the whole top-level
// array first would surface a syntax error instead of the over-cap error.
func TestParseFribb_overCapStopsEarly(t *testing.T) {
	var b strings.Builder
	b.WriteByte('[')
	for i := 0; i <= maxFribbRecords; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{}`)
	}
	b.WriteString(`,!!!not-json`)
	_, err := parseFribb([]byte(b.String()), discardLogger())
	if err == nil {
		t.Fatal("parseFribb(over-cap tiny elements) = nil error, want over-cap error")
	}
	if !strings.Contains(err.Error(), "cap") {
		t.Errorf("parseFribb over-cap error = %v, want the record-cap error, not a syntax error", err)
	}
}

func TestFribbRecord_toRecord(t *testing.T) {
	if _, ok := (&fribbRecord{}).toRecord(); ok {
		t.Error("toRecord with zero AniListID returned ok=true, want false")
	}
	fr := &fribbRecord{
		Type:      "  ova  ",
		AniListID: 7,
		TvdbID:    12,
		IMDbID:    stringList{"tt9"},
		TmdbID:    tmdbID{Movie: []flexInt{0, 8}},
	}
	rec, ok := fr.toRecord()
	if !ok {
		t.Fatal("toRecord with AniListID 7 returned ok=false")
	}
	if rec.Type != "OVA" {
		t.Errorf("toRecord Type = %q, want OVA", rec.Type)
	}
	if !reflect.DeepEqual(rec.TmdbMovies, []int{8}) {
		t.Errorf("toRecord TmdbMovies = %v, want [8] (zero dropped)", rec.TmdbMovies)
	}
}

func TestFlexInt_UnmarshalJSON(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int
	}{
		{"number", `123`, 123},
		{"numeric string", `"456"`, 456},
		{"padded string", `"  78  "`, 78},
		{"null", `null`, 0},
		{"unknown string", `"unknown"`, 0},
		{"fractional treated absent", `9.9`, 0},
		{"negative treated absent", `-5`, 0},
		{"out of range", `1e300`, 0},
		{"quoted out of range", `"2147483648"`, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var f flexInt
			if err := f.UnmarshalJSON([]byte(tc.in)); err != nil {
				t.Fatalf("UnmarshalJSON(%s) error: %v", tc.in, err)
			}
			if int(f) != tc.want {
				t.Errorf("flexInt(%s) = %d, want %d", tc.in, int(f), tc.want)
			}
		})
	}
}

func TestStringList_UnmarshalJSON(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"array", `["tt1","tt2"]`, []string{"tt1", "tt2"}},
		{"scalar", `"tt9"`, []string{"tt9"}},
		{"null", `null`, nil},
		{"blanks dropped", `["  x  ",""]`, []string{"x"}},
		{"empty array", `[]`, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var s stringList
			if err := s.UnmarshalJSON([]byte(tc.in)); err != nil {
				t.Fatalf("UnmarshalJSON(%s) error: %v", tc.in, err)
			}
			if !reflect.DeepEqual([]string(s), tc.want) {
				t.Errorf("stringList(%s) = %v, want %v", tc.in, []string(s), tc.want)
			}
		})
	}
}

func TestTmdbID_UnmarshalJSON(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		wantMovie []int
	}{
		{name: "tv object ignored", in: `{"tv":5}`},
		{name: "movie array", in: `{"movie":[7,8]}`, wantMovie: []int{7, 8}},
		{name: "bare number tolerated", in: `123`},
		{name: "string tolerated", in: `"unknown"`},
		{name: "null", in: `null`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got tmdbID
			if err := got.UnmarshalJSON([]byte(tc.in)); err != nil {
				t.Fatalf("UnmarshalJSON(%s) error: %v", tc.in, err)
			}
			if !reflect.DeepEqual(intSlice(got.Movie), tc.wantMovie) {
				t.Errorf("tmdbID(%s).Movie = %v, want %v", tc.in, intSlice(got.Movie), tc.wantMovie)
			}
		})
	}
}

func TestIntSliceAndTrimmed(t *testing.T) {
	if got := intSlice([]flexInt{0, 3, 0, 4}); !reflect.DeepEqual(got, []int{3, 4}) {
		t.Errorf("intSlice = %v, want [3 4]", got)
	}
	if got := trimmed([]string{" a ", "", "b"}); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("trimmed = %v, want [a b]", got)
	}
}

// TestParseFribb_seasonDecoded pins the season.tvdb decode path:
// offsetPair.tvdbOrZero's non-nil branch and Record.SeasonTvdb, which existing
// parseFribb tests never populate. SeasonTvdb is load-bearing for the audit
// season-scoping logic. The upstream episode_offset field is deliberately not
// decoded (no consumer reads it); it rides along here to prove an unknown
// field is ignored.
func TestParseFribb_seasonDecoded(t *testing.T) {
	data := []byte(`[{"anilist_id":5,"type":"tv","season":{"tvdb":2},"episode_offset":{"tvdb":12}}]`)
	records, err := parseFribb(data, discardLogger())
	if err != nil {
		t.Fatalf("parseFribb error: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("parseFribb kept %d records, want 1", len(records))
	}
	if records[0].SeasonTvdb != 2 {
		t.Errorf("SeasonTvdb = %d, want 2", records[0].SeasonTvdb)
	}
}

// TestFlexInt_rangeClampBoundaries pins the inclusive validity endpoints of
// the tolerant number decode: 0 and the int32 maximum decode as themselves,
// while any negative value and anything above the int32 maximum are treated
// as absent (0). Real AniList/TVDB/TMDB ids are never negative, and a
// negative value must not count toward the arr-identifier acceptance floor.
func TestFlexInt_rangeClampBoundaries(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int
	}{
		{name: "zero accepted", in: `0`, want: 0},
		{name: "negative one treated absent", in: `-1`, want: 0},
		{name: "int32 minimum treated absent", in: `-2147483648`, want: 0},
		{name: "maximum accepted", in: `2147483647`, want: 2147483647},
		{name: "above maximum treated absent", in: `2147483648`, want: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got flexInt
			if err := got.UnmarshalJSON([]byte(tc.in)); err != nil {
				t.Fatalf("UnmarshalJSON(%s) error: %v", tc.in, err)
			}
			if int(got) != tc.want {
				t.Errorf("flexInt(%s) = %d, want %d", tc.in, int(got), tc.want)
			}
		})
	}
}

// TestFlexInt_nonNumericJSONTolerated pins the non-string sibling of the
// placeholder policy: valid JSON of a non-numeric type (a boolean) is
// tolerated as 0 rather than surfacing the decode error.
func TestFlexInt_nonNumericJSONTolerated(t *testing.T) {
	var got flexInt
	if err := got.UnmarshalJSON([]byte(`true`)); err != nil {
		t.Fatalf("UnmarshalJSON(true) error: %v", err)
	}
	if int(got) != 0 {
		t.Errorf("flexInt(true) = %d, want 0 (non-numeric placeholder tolerated)", int(got))
	}
}

// TestParseFribb_malformedDocumentErrors pins the strict document-level
// boundary of the otherwise tolerant decoder: per-record shape oddities are
// skipped, but a document-level defect (empty input, a garbage first token, an
// unterminated array, an invalid token mid-array, or trailing data after the
// closing bracket) fails the whole parse so refreshCache keeps the stale map.
func TestParseFribb_malformedDocumentErrors(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{name: "empty input", in: ``},
		{name: "garbage first token", in: `!!!`},
		{name: "unterminated array", in: `[{"anilist_id":1}`},
		{name: "invalid token mid-array", in: `[{"anilist_id":1},!!!]`},
		{name: "trailing data after array", in: `[{"anilist_id":1}] {"extra":true}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseFribb([]byte(tc.in), discardLogger()); err == nil {
				t.Fatalf("parseFribb(%q) = nil error, want document-level error", tc.in)
			}
		})
	}
}

// TestParseFribb_oversizedRecordSkipped pins the per-record amplification
// guards: a record whose encoded form exceeds maxFribbRecordBytes, and a
// small record whose nested identifier list exceeds maxFribbIdentifiers, are
// each skipped as malformed while sibling records survive - so a hostile body
// below maxMapBytes cannot amplify nested arrays into an unbounded decoded
// working set.
func TestParseFribb_oversizedRecordSkipped(t *testing.T) {
	// A record over the byte cap: one imdb_id array whose encoded size alone
	// exceeds maxFribbRecordBytes.
	var big strings.Builder
	big.WriteString(`{"anilist_id":2,"imdb_id":[`)
	for i := 0; big.Len() <= maxFribbRecordBytes; i++ {
		if i > 0 {
			big.WriteByte(',')
		}
		big.WriteString(`"tt` + strconv.Itoa(i) + `"`)
	}
	big.WriteString(`]}`)

	// A record well under the byte cap but over the identifier cap.
	var wide strings.Builder
	wide.WriteString(`{"anilist_id":3,"themoviedb_id":{"movie":[`)
	for i := range maxFribbIdentifiers + 1 {
		if i > 0 {
			wide.WriteByte(',')
		}
		wide.WriteString(strconv.Itoa(i + 1))
	}
	wide.WriteString(`]}}`)

	data := []byte(`[{"anilist_id":1,"tvdb_id":100},` + big.String() + `,` + wide.String() + `,{"anilist_id":4,"tvdb_id":400}]`)
	if len(data) >= maxMapBytes {
		t.Fatalf("test body is %d bytes, must stay under maxMapBytes %d", len(data), maxMapBytes)
	}
	records, err := parseFribb(data, discardLogger())
	if err != nil {
		t.Fatalf("parseFribb error: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("parseFribb kept %d records, want 2 (oversized records skipped, siblings survive)", len(records))
	}
	if records[0].AniListID != 1 || records[1].AniListID != 4 {
		t.Errorf("surviving records = %d, %d, want 1 and 4", records[0].AniListID, records[1].AniListID)
	}
}

// TestParseFribb_identifierSlicesCapped pins the aggregate bound: across a
// many-record body every identifier slice a caller receives is at or below
// maxFribbIdentifiers - at-cap lists are retained in full, over-cap lists
// reject their record.
func TestParseFribb_identifierSlicesCapped(t *testing.T) {
	const n = 64
	var b strings.Builder
	b.WriteByte('[')
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		ids := maxFribbIdentifiers // at the cap: retained in full
		if i%4 == 0 {
			ids = maxFribbIdentifiers + 1 // over the cap: record skipped
		}
		b.WriteString(`{"anilist_id":` + strconv.Itoa(i+1) + `,"imdb_id":[`)
		for j := range ids {
			if j > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`"tt` + strconv.Itoa(j) + `"`)
		}
		b.WriteString(`]}`)
	}
	b.WriteByte(']')
	records, err := parseFribb([]byte(b.String()), discardLogger())
	if err != nil {
		t.Fatalf("parseFribb error: %v", err)
	}
	want := n - n/4 // every i%4==0 record is over-cap and skipped
	if len(records) != want {
		t.Fatalf("parseFribb kept %d records, want %d", len(records), want)
	}
	for _, rec := range records {
		if len(rec.IMDbIDs) > maxFribbIdentifiers {
			t.Fatalf("record %d retained %d imdb ids, want <= %d", rec.AniListID, len(rec.IMDbIDs), maxFribbIdentifiers)
		}
		if len(rec.IMDbIDs) != maxFribbIdentifiers {
			t.Fatalf("record %d retained %d imdb ids, want the full at-cap %d", rec.AniListID, len(rec.IMDbIDs), maxFribbIdentifiers)
		}
	}
}

// TestParseFribb_toleratesVariantRecords characterizes the tolerant decode of
// one record mixing every upstream shape variant at once: padded string ids,
// a padded type, a scalar imdb_id, a tv-keyed themoviedb_id (ignored — only
// the movie half feeds a lookup), and a season object; beside it, an array
// imdb_id with blanks, a movie-array themoviedb_id with a quoted number and
// an "unknown" placeholder, and an unkeyable record (odd anilist_id) that is
// omitted.
func TestParseFribb_toleratesVariantRecords(t *testing.T) {
	data := []byte(`[
		{"anilist_id":" 42 ","tvdb_id":101,"type":" tv ","imdb_id":" tt001 ","themoviedb_id":{"tv":"202"},"season":{"tvdb":3},"episode_offset":{"tvdb":12}},
		{"anilist_id":43,"type":"movie","imdb_id":["tt002","  "," tt003 "],"themoviedb_id":{"movie":[303,"404","unknown"]}},
		{"anilist_id":{"unexpected":true},"type":"TV"}
	]`)

	got, err := parseFribb(data, discardLogger())
	if err != nil {
		t.Fatalf("parseFribb: %v", err)
	}
	want := []Record{
		{Type: "TV", IMDbIDs: []string{"tt001"}, AniListID: 42, TvdbID: 101, SeasonTvdb: 3},
		{Type: "MOVIE", IMDbIDs: []string{"tt002", "tt003"}, TmdbMovies: []int{303, 404}, AniListID: 43},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseFribb variant records = %#v, want %#v", got, want)
	}
}

// TestParseFribb_logsSkippedAndDroppedCounts pins the operator-facing decode
// diagnostics, the only observable signal for malformed upstream records: the
// WARN carries the skipped count, the surviving parsed count, and the FIRST
// per-record decode error (not a later one), and the zero-id drop count rides
// a separate Debug line.
func TestParseFribb_logsSkippedAndDroppedCounts(t *testing.T) {
	logger, rec := capture.New()
	// Element order: a type-mismatch element (the first, retained error), an
	// over-cap record (a later, different error), a zero-id drop, a survivor.
	var big strings.Builder
	big.WriteString(`{"anilist_id":9,"imdb_id":[`)
	for i := 0; big.Len() <= maxFribbRecordBytes; i++ {
		if i > 0 {
			big.WriteByte(',')
		}
		big.WriteString(`"tt` + strconv.Itoa(i) + `"`)
	}
	big.WriteString(`]}`)
	data := []byte(`[5,` + big.String() + `,{"anilist_id":0},{"anilist_id":1,"type":"tv","tvdb_id":100}]`)

	records, err := parseFribb(data, logger)
	if err != nil {
		t.Fatalf("parseFribb error: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("parseFribb kept %d records, want 1", len(records))
	}
	if rec.CountExact("mapping: skipped malformed records") != 1 {
		t.Fatalf("logs = %v, want one skipped-records warning", rec.Messages())
	}
	if !attrRendered(rec, "skipped", "2") {
		t.Errorf("skipped-records logs = %v, want skipped=2", rec.Messages())
	}
	if !attrRendered(rec, "parsed", "1") {
		t.Errorf("skipped-records logs = %v, want parsed=1", rec.Messages())
	}
	if !attrContains(rec, "error", "cannot unmarshal") {
		t.Errorf("skipped-records logs = %v, want the FIRST decode error (a type mismatch), not the later over-cap error", rec.Messages())
	}
	if rec.CountExact("mapping: dropped records without anilist_id") != 1 {
		t.Fatalf("logs = %v, want one dropped-records debug line", rec.Messages())
	}
	if !attrRendered(rec, "dropped", "1") {
		t.Errorf("dropped-records logs = %v, want dropped=1", rec.Messages())
	}
}

// attrContains reports whether any captured record carries an attribute with
// the given key whose rendered value contains want.
func attrContains(rec *capture.Recorder, key, want string) bool {
	for _, r := range rec.Records() {
		found := false
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == key && strings.Contains(a.Value.String(), want) {
				found = true
				return false
			}
			return true
		})
		if found {
			return true
		}
	}
	return false
}
