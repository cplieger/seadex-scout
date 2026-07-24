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
		{"quoted integral float", `"9.0"`, 9},
		{"quoted exponent", `"1e3"`, 1000},
		{"quoted fractional treated absent", `"1.5"`, 0},
		{"quoted negative treated absent", `"-5"`, 0},
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
		name       string
		in         string
		wantMovie  []int
		wantScalar int
	}{
		{name: "tv object ignored", in: `{"tv":5}`},
		{name: "movie array", in: `{"movie":[7,8]}`, wantMovie: []int{7, 8}},
		{name: "bare number retained as scalar", in: `123`, wantScalar: 123},
		{name: "quoted number retained as scalar", in: `"123"`, wantScalar: 123},
		{name: "unknown string tolerated", in: `"unknown"`},
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
			if int(got.Scalar) != tc.wantScalar {
				t.Errorf("tmdbID(%s).Scalar = %d, want %d", tc.in, int(got.Scalar), tc.wantScalar)
			}
		})
	}
}

// TestParseFribb_bareNumberTmdbIDDisambiguatedByType pins the scalar
// themoviedb_id path end-to-end: a bare-number (or quoted-numeric)
// themoviedb_id carries no tv-vs-movie discrimination of its own, but a
// MOVIE-typed record's own type disambiguates it — a movie's tmdb id is
// necessarily a movie id — so the scalar becomes the record's movie TMDB id
// (without it, a MOVIE record with no imdb_id would lose its only arr
// identifier and could never resolve to Radarr). A non-movie or untyped
// record still discards the scalar, and the object form is unchanged.
func TestParseFribb_bareNumberTmdbIDDisambiguatedByType(t *testing.T) {
	tests := []struct {
		name string
		rec  string
		want []int
	}{
		{name: "movie with bare number sets movie id", rec: `{"anilist_id":1,"type":"movie","themoviedb_id":603}`, want: []int{603}},
		{name: "movie with quoted number sets movie id", rec: `{"anilist_id":1,"type":" Movie ","themoviedb_id":"603"}`, want: []int{603}},
		{name: "tv with bare number still discarded", rec: `{"anilist_id":1,"type":"tv","themoviedb_id":603}`},
		{name: "ova with bare number still discarded", rec: `{"anilist_id":1,"type":"ova","themoviedb_id":603}`},
		{name: "untyped with bare number still discarded", rec: `{"anilist_id":1,"themoviedb_id":603}`},
		{name: "movie object form unchanged", rec: `{"anilist_id":1,"type":"movie","themoviedb_id":{"movie":[7,8]}}`, want: []int{7, 8}},
		{name: "movie tv-object form still empty", rec: `{"anilist_id":1,"type":"movie","themoviedb_id":{"tv":5}}`},
		{name: "movie unknown placeholder still empty", rec: `{"anilist_id":1,"type":"movie","themoviedb_id":"unknown"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			records, err := parseFribb([]byte(`[`+tc.rec+`]`), discardLogger())
			if err != nil {
				t.Fatalf("parseFribb error: %v", err)
			}
			if len(records) != 1 {
				t.Fatalf("parseFribb kept %d records, want 1", len(records))
			}
			if !reflect.DeepEqual(records[0].TmdbMovies, tc.want) {
				t.Errorf("TmdbMovies = %v, want %v", records[0].TmdbMovies, tc.want)
			}
			// No record above carries any other id, so the arr-identifier
			// predicate must key entirely on the consumed movie ids: true
			// exactly when the scalar (or object form) was consumed.
			if got, want := records[0].HasArrIdentifier(), len(tc.want) > 0; got != want {
				t.Errorf("HasArrIdentifier = %v, want %v", got, want)
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
		{name: "top-level null", in: `null`},
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
	if !rec.HasAttr("", "skipped", "2") {
		t.Errorf("skipped-records logs = %v, want skipped=2", rec.Messages())
	}
	if !rec.HasAttr("", "parsed", "1") {
		t.Errorf("skipped-records logs = %v, want parsed=1", rec.Messages())
	}
	if !rec.AttrContains("", "error", "cannot unmarshal") {
		t.Errorf("skipped-records logs = %v, want the FIRST decode error (a type mismatch), not the later over-cap error", rec.Messages())
	}
	if rec.CountExact("mapping: dropped records without anilist_id") != 1 {
		t.Fatalf("logs = %v, want one dropped-records debug line", rec.Messages())
	}
	if !rec.HasAttr("", "dropped", "1") {
		t.Errorf("dropped-records logs = %v, want dropped=1", rec.Messages())
	}
}

// TestParseFribb_cleanParseEmitsNoLogs pins the log-gating conditions on the
// silent side: a fully-clean body (every record keyed, nothing skipped or
// dropped) must emit NO skipped-records warning and NO dropped-records debug
// line: a clean cycle must not imply upstream corruption by logging zero-count
// diagnostics.
func TestParseFribb_cleanParseEmitsNoLogs(t *testing.T) {
	logger, rec := capture.New()
	data := []byte(`[{"anilist_id":1,"type":"tv","tvdb_id":100},{"anilist_id":2,"type":"movie","themoviedb_id":603}]`)
	records, err := parseFribb(data, logger)
	if err != nil {
		t.Fatalf("parseFribb error: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("parseFribb kept %d records, want 2", len(records))
	}
	if msgs := rec.Messages(); len(msgs) != 0 {
		t.Errorf("clean parse logged %v, want no log lines (skipped=0 and dropped=0 must stay silent)", msgs)
	}
}

// TestParseFribb_atCapRecordAccepted pins the inclusive side of the
// per-record byte cap: a record whose encoded form is exactly
// maxFribbRecordBytes bytes is accepted (the guard is strictly
// greater-than), while one byte over is skipped.
func TestParseFribb_atCapRecordAccepted(t *testing.T) {
	// Build a record padded to exactly maxFribbRecordBytes bytes via one
	// long imdb_id string entry (a single string stays under the
	// maxFribbIdentifiers list cap).
	buildRecord := func(size int) string {
		const skeleton = `{"anilist_id":1,"imdb_id":"tt"}`
		pad := size - len(skeleton)
		if pad < 0 {
			t.Fatalf("cap %d smaller than skeleton %d", size, len(skeleton))
		}
		return `{"anilist_id":1,"imdb_id":"tt` + strings.Repeat("x", pad) + `"}`
	}

	atCap := buildRecord(maxFribbRecordBytes)
	if len(atCap) != maxFribbRecordBytes {
		t.Fatalf("at-cap record is %d bytes, want exactly %d", len(atCap), maxFribbRecordBytes)
	}
	records, err := parseFribb([]byte(`[`+atCap+`]`), discardLogger())
	if err != nil {
		t.Fatalf("parseFribb(at-cap record) error: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("parseFribb kept %d records, want 1 (an exactly-at-cap record is accepted)", len(records))
	}

	overCap := buildRecord(maxFribbRecordBytes + 1)
	records, err = parseFribb([]byte(`[`+overCap+`]`), discardLogger())
	if err != nil {
		t.Fatalf("parseFribb(over-cap record) error: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("parseFribb kept %d records, want 0 (one byte over the cap is skipped)", len(records))
	}
}

// TestTmdbID_atCapMovieListRetained pins the inclusive side of the
// themoviedb_id.movie identifier cap, matching the imdb_id at-cap coverage in
// TestParseFribb_identifierSlicesCapped: a movie list of exactly
// maxFribbIdentifiers entries is retained in full, one more rejects the
// record.
func TestTmdbID_atCapMovieListRetained(t *testing.T) {
	build := func(n int) string {
		var b strings.Builder
		b.WriteString(`{"movie":[`)
		for i := range n {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(strconv.Itoa(i + 1))
		}
		b.WriteString(`]}`)
		return b.String()
	}

	var at tmdbID
	if err := at.UnmarshalJSON([]byte(build(maxFribbIdentifiers))); err != nil {
		t.Fatalf("UnmarshalJSON(at-cap movie list) error: %v", err)
	}
	if len(at.Movie) != maxFribbIdentifiers {
		t.Errorf("at-cap movie list retained %d ids, want the full %d", len(at.Movie), maxFribbIdentifiers)
	}

	var over tmdbID
	if err := over.UnmarshalJSON([]byte(build(maxFribbIdentifiers + 1))); err == nil {
		t.Error("UnmarshalJSON(over-cap movie list) = nil error, want the record-rejecting cap error")
	}
}

// TestFribbRecord_toRecord_negativeAniListIDDropped pins the negative arm of
// the positive-key guard: a directly-constructed record with a negative
// AniList ID is dropped (ok=false), matching the documented contract that a
// zero or negative key can never resolve a SeaDex lookup. The branch is
// unreachable through parseFribb (flexInt zeroes negative wire values), so
// only this direct-construction case distinguishes the `<= 0` guard from an
// `== 0` form.
func TestFribbRecord_toRecord_negativeAniListIDDropped(t *testing.T) {
	if _, ok := (&fribbRecord{AniListID: -5, Type: "tv", TvdbID: 100}).toRecord(); ok {
		t.Error("toRecord with negative AniListID returned ok=true, want false (positive-key contract)")
	}
}
