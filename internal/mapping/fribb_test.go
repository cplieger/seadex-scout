package mapping

import (
	"errors"
	"io"
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

// oversizedFribbRecord builds one encoded Fribb record whose imdb_id array
// alone pushes it past maxFribbRecordBytes - the per-record byte cap
// decodeFribbRecord rejects before the tolerant decode allocates.
func oversizedFribbRecord(aniListID int) string {
	var b strings.Builder
	b.WriteString(`{"anilist_id":` + strconv.Itoa(aniListID) + `,"imdb_id":[`)
	for i := 0; b.Len() <= maxFribbRecordBytes; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`"tt` + strconv.Itoa(i) + `"`)
	}
	b.WriteString(`]}`)
	return b.String()
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

// TestParseFribb_atCapRecordCountAccepted pins the INCLUSIVE side of the
// record-count cap: a list of exactly maxFribbRecords elements is accepted in
// full, so an off-by-one in decodeFribbRecords' guard cannot start rejecting a
// body the documented cap admits.
func TestParseFribb_atCapRecordCountAccepted(t *testing.T) {
	var b strings.Builder
	b.WriteByte('[')
	for i := range maxFribbRecords {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"anilist_id":`)
		b.WriteString(strconv.Itoa(i + 1))
		b.WriteByte('}')
	}
	b.WriteByte(']')
	records, err := parseFribb([]byte(b.String()), discardLogger())
	if err != nil {
		t.Fatalf("parseFribb(exactly %d records) error: %v, want acceptance", maxFribbRecords, err)
	}
	if len(records) != maxFribbRecords {
		t.Fatalf("parseFribb kept %d records, want the full at-cap %d", len(records), maxFribbRecords)
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

// TestParseFribb_idRangeAppliedEndToEnd pins the identifier range policy at
// the application boundary: an at-limit AniList/TVDB id survives the parse
// unchanged, an over-range AniList id drops the whole record (its key is
// unusable), and an over-range TVDB id decodes as absent (0) while the record
// itself is retained.
func TestParseFribb_idRangeAppliedEndToEnd(t *testing.T) {
	data := []byte(`[
		{"anilist_id":2147483647,"tvdb_id":2147483647},
		{"anilist_id":2147483648,"tvdb_id":1},
		{"anilist_id":7,"tvdb_id":2147483648}
	]`)
	records, err := parseFribb(data, discardLogger())
	if err != nil {
		t.Fatalf("parseFribb error: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("parseFribb kept %d records, want 2 (over-range AniList ID dropped)", len(records))
	}
	if records[0].AniListID != 2147483647 || records[0].TvdbID != 2147483647 {
		t.Errorf("at-limit record = %+v, want both IDs 2147483647", records[0])
	}
	if records[1].AniListID != 7 || records[1].TvdbID != 0 {
		t.Errorf("over-range TVDB record = %+v, want AniListID 7 / TvdbID 0", records[1])
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
	big := oversizedFribbRecord(2)

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

	data := []byte(`[{"anilist_id":1,"tvdb_id":100},` + big + `,` + wide.String() + `,{"anilist_id":4,"tvdb_id":400}]`)
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
	big := oversizedFribbRecord(9)
	data := []byte(`[5,` + big + `,{"anilist_id":0},{"anilist_id":1,"type":"tv","tvdb_id":100}]`)

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
	if rec.CountLevel(slog.LevelWarn, "mapping: skipped malformed records") != 1 {
		t.Fatalf("skipped-records line is not at WARN (logs = %v); demoted to DEBUG it vanishes from the deployed info-level stream and dropped upstream rows go unseen", rec.Messages())
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
	if rec.CountLevel(slog.LevelDebug, "mapping: dropped records without anilist_id") != 1 {
		t.Fatalf("dropped-records line is not at DEBUG (logs = %v); a keyless-row count promoted to WARN is per-cycle noise on a shape Fribb always carries", rec.Messages())
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

// TestParseFribb_emptyBodyIsNotTheNonArraySentinel pins the empty-body carve-out:
// a zero-length or whitespace-only 200 has NO first token, so it is a TRANSIENT
// parse failure and must NOT carry errNotJSONArray - that sentinel routes through
// rejectRefresh and advances the persisted rejection streak toward escalation,
// which a body that can succeed on the next attempt has not earned. The object
// and null documents stay on the sentinel.
func TestParseFribb_emptyBodyIsNotTheNonArraySentinel(t *testing.T) {
	for _, body := range []string{"", "   ", "\n\t"} {
		_, err := parseFribbForRefresh([]byte(body), discardLogger())
		if err == nil {
			t.Fatalf("parseFribbForRefresh(%q) error = nil, want an empty-body error", body)
		}
		if errors.Is(err, errNotJSONArray) {
			t.Errorf("parseFribbForRefresh(%q) error = %v, want a transient parse failure, not the errNotJSONArray sentinel (it advances the persisted rejection streak)", body, err)
		}
		if !errors.Is(err, io.EOF) {
			t.Errorf("parseFribbForRefresh(%q) error = %v, want it to wrap io.EOF", body, err)
		}
	}
	if _, err := parseFribbForRefresh([]byte(`{"a":1}`), discardLogger()); !errors.Is(err, errNotJSONArray) {
		t.Errorf("object body error = %v, want the errNotJSONArray sentinel to be unaffected by the empty-body carve-out", err)
	}
	if _, err := parseFribbForRefresh([]byte(`null`), discardLogger()); !errors.Is(err, errNotJSONArray) {
		t.Errorf("null body error = %v, want the errNotJSONArray sentinel to be unaffected by the empty-body carve-out", err)
	}
}

// TestParseFribb_approachingRecordCapWarns pins the operator's only advance
// notice before the record cap becomes a hard refusal: at three quarters of
// maxFribbRecords the parse still succeeds but WARNs with the element count
// and the cap, while one element below the threshold it stays silent. Without
// the warning a growing upstream list crosses the cap with no prior signal and
// the map freezes stale, because acceptRefresh routes the breach through
// rejectRefresh and every later cycle re-downloads and re-rejects the body.
func TestParseFribb_approachingRecordCapWarns(t *testing.T) {
	const threshold = maxFribbRecords / 4 * 3
	const msg = "mapping: Fribb list approaching record cap"
	// Keyless elements are the cheapest way to reach the threshold: each one
	// counts toward the element total the guard reads while keeping the body
	// small (they are dropped, so no records are retained).
	build := func(n int) []byte {
		var b strings.Builder
		b.Grow(3 * n)
		b.WriteByte('[')
		for i := range n {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`{}`)
		}
		b.WriteByte(']')
		return []byte(b.String())
	}

	logger, rec := capture.New()
	if _, err := parseFribb(build(threshold), logger); err != nil {
		t.Fatalf("parseFribb(threshold body) error: %v", err)
	}
	if rec.CountLevel(slog.LevelWarn, msg) != 1 {
		t.Fatalf("logs = %v, want exactly one WARN approaching-cap line at %d elements", rec.Messages(), threshold)
	}
	if !rec.HasAttr(msg, "elements", strconv.Itoa(threshold)) {
		t.Errorf("approaching-cap log = %v, want elements=%d", rec.Messages(), threshold)
	}
	if !rec.HasAttr(msg, "cap", strconv.Itoa(maxFribbRecords)) {
		t.Errorf("approaching-cap log = %v, want cap=%d", rec.Messages(), maxFribbRecords)
	}

	belowLogger, belowRec := capture.New()
	if _, err := parseFribb(build(threshold-1), belowLogger); err != nil {
		t.Fatalf("parseFribb(below-threshold body) error: %v", err)
	}
	if belowRec.CountExact(msg) != 0 {
		t.Errorf("below-threshold logs = %v, want no approaching-cap warning one element below the threshold", belowRec.Messages())
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

// TestParseFribbForRefresh_elementsCountsEverySourceElement pins the counted
// denominator the refresh acceptance floors validate coverage against: every
// top-level element counts, whether it survived, was skipped as malformed, or
// was dropped for a missing AniList key. Without the skipped term a body of
// malformed elements plus one valid record would read as a healthy 1/1 map.
func TestParseFribbForRefresh_elementsCountsEverySourceElement(t *testing.T) {
	data := []byte(`[` +
		`{"anilist_id":1,"type":"tv","tvdb_id":100},` +
		`5,` +
		`[],` +
		`{"anilist_id":0,"type":"tv"}` +
		`]`)
	parsed, err := parseFribbForRefresh(data, discardLogger())
	if err != nil {
		t.Fatalf("parseFribbForRefresh error: %v", err)
	}
	if len(parsed.records) != 1 {
		t.Fatalf("parseFribbForRefresh kept %d records, want 1", len(parsed.records))
	}
	if parsed.elements != 4 {
		t.Errorf("parseFribbForRefresh elements = %d, want 4 (1 survivor + 2 skipped-malformed + 1 dropped-keyless)", parsed.elements)
	}
}

// TestFribbDecodeCounts_aggregateIdentifierBudget pins the aggregate retained-
// identifier budget. maxFribbIdentifiers bounds EACH of the two retained lists
// on one record (imdb_id and themoviedb_id.movie), so without this budget the
// per-record caps multiply (maxFribbRecords x 2 x maxFribbIdentifiers admits
// ~4.2M retained ids from a body under maxMapBytes). A record that
// would breach the budget is rejected inside the EXISTING per-record tolerance
// boundary - counted separately as overBudget (not as a malformed record), so
// the "records refused by identifier budget" WARN names the app-side cap.
func TestFribbDecodeCounts_aggregateIdentifierBudget(t *testing.T) {
	atCap := Record{AniListID: 1, IMDbIDs: make([]string, maxFribbIdentifiers)}
	var c fribbDecodeCounts
	for range maxFribbIdentifiersTotal / maxFribbIdentifiers {
		c.add(&atCap, true, nil)
	}
	if c.skipped != 0 || c.dropped != 0 || c.overBudget != 0 {
		t.Fatalf("filling the budget skipped=%d dropped=%d overBudget=%d, want 0/0/0", c.skipped, c.dropped, c.overBudget)
	}
	if c.identifiers != maxFribbIdentifiersTotal {
		t.Fatalf("charged %d identifiers, want the full budget %d", c.identifiers, maxFribbIdentifiersTotal)
	}

	retained := len(c.records)
	c.add(&atCap, true, nil)
	if len(c.records) != retained {
		t.Fatalf("over-budget record retained (%d records, want %d)", len(c.records), retained)
	}
	if c.overBudget != 1 {
		t.Fatalf("over-budget record counted overBudget=%d, want 1", c.overBudget)
	}
	if c.skipped != 0 {
		t.Fatalf("over-budget record counted skipped=%d, want 0 (a budget breach is not a malformed record)", c.skipped)
	}
	if c.identifiers != maxFribbIdentifiersTotal {
		t.Fatalf("over-budget record charged the budget to %d, want %d", c.identifiers, maxFribbIdentifiersTotal)
	}
	if c.firstErr != nil {
		t.Fatalf("firstErr = %v, want nil (the budget breach reports on its own line)", c.firstErr)
	}
}

// TestParseFribb_ordinaryBodyUnaffectedByIdentifierBudget shows the aggregate
// budget does not fire on an ordinary body: a compact stand-in for real Fribb
// (which carries ~40k records with a handful of ids each, an order of
// magnitude under maxFribbIdentifiersTotal) keeps every record and every
// identifier.
func TestParseFribb_ordinaryBodyUnaffectedByIdentifierBudget(t *testing.T) {
	const n = 500
	var b strings.Builder
	b.WriteByte('[')
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		id := strconv.Itoa(i + 1)
		b.WriteString(`{"anilist_id":` + id + `,"type":"movie","imdb_id":["tt` + id + `"],"themoviedb_id":{"movie":[` + id + `]}}`)
	}
	b.WriteByte(']')

	records, err := parseFribb([]byte(b.String()), discardLogger())
	if err != nil {
		t.Fatalf("parseFribb error: %v", err)
	}
	if len(records) != n {
		t.Fatalf("parseFribb kept %d records, want all %d", len(records), n)
	}
	for _, rec := range records {
		if len(rec.IMDbIDs) != 1 || len(rec.TmdbMovies) != 1 {
			t.Fatalf("record %d retained %d imdb / %d tmdb ids, want 1/1", rec.AniListID, len(rec.IMDbIDs), len(rec.TmdbMovies))
		}
	}
}

// TestRecordFromFormat_normalizesRoutingType pins the exported normalization
// seam scout.applyMemoTyping and match.formatArr route an unmapped entry
// through. anilist.knownFormat deliberately preserves an accepted LOWERCASE
// format token verbatim, and every existing consumer test supplies uppercase
// MOVIE/OVA, so dropping the normalizeType call here would silently route a
// movie to Sonarr/Anime and lose an OVA's season-zero classification without
// failing any other test.
func TestRecordFromFormat_normalizesRoutingType(t *testing.T) {
	tests := map[string]struct {
		format      string
		wantType    string
		wantMovie   bool
		wantSpecial bool
	}{
		"movie is canonicalized for Radarr routing":        {format: " movie ", wantType: "MOVIE", wantMovie: true},
		"special is canonicalized for season-zero routing": {format: " ova ", wantType: "OVA", wantSpecial: true},
		"blank format remains unknown":                     {format: "   ", wantType: ""},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := RecordFromFormat(tc.format)
			if got.Type != tc.wantType {
				t.Errorf("RecordFromFormat(%q).Type = %q, want %q", tc.format, got.Type, tc.wantType)
			}
			if got.IsMovie() != tc.wantMovie {
				t.Errorf("RecordFromFormat(%q).IsMovie() = %v, want %v", tc.format, got.IsMovie(), tc.wantMovie)
			}
			if got.IsSpecial() != tc.wantSpecial {
				t.Errorf("RecordFromFormat(%q).IsSpecial() = %v, want %v", tc.format, got.IsSpecial(), tc.wantSpecial)
			}
		})
	}
}
