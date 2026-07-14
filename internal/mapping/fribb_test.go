package mapping

import (
	"io"
	"log/slog"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
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

func TestFribbRecord_toRecord(t *testing.T) {
	if _, ok := (&fribbRecord{}).toRecord(); ok {
		t.Error("toRecord with zero AniListID returned ok=true, want false")
	}
	fr := &fribbRecord{
		Type:      "  ova  ",
		AniListID: 7,
		TvdbID:    12,
		IMDbID:    stringList{"tt9"},
		TmdbID:    tmdbID{TV: 5, Movie: []flexInt{0, 8}},
	}
	rec, ok := fr.toRecord()
	if !ok {
		t.Fatal("toRecord with AniListID 7 returned ok=false")
	}
	if rec.Type != "OVA" {
		t.Errorf("toRecord Type = %q, want OVA", rec.Type)
	}
	if rec.TmdbTV != 5 {
		t.Errorf("toRecord TmdbTV = %d, want 5", rec.TmdbTV)
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
		{"float truncates", `9.9`, 9},
		{"negative", `-5`, -5},
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
		wantMovie []int
		name      string
		in        string
		wantTV    int
	}{
		{name: "tv object", in: `{"tv":5}`, wantTV: 5},
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
			if int(got.TV) != tc.wantTV {
				t.Errorf("tmdbID(%s).TV = %d, want %d", tc.in, int(got.TV), tc.wantTV)
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

// TestParseFribb_seasonAndOffsetDecoded pins the season.tvdb / episode_offset
// decode path: offsetPair.tvdbOr's non-nil branch and Record.SeasonTvdb /
// OffsetTvdb, which existing parseFribb tests never populate. SeasonTvdb is
// load-bearing for the audit season-scoping logic.
func TestParseFribb_seasonAndOffsetDecoded(t *testing.T) {
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
	if records[0].OffsetTvdb != 12 {
		t.Errorf("OffsetTvdb = %d, want 12", records[0].OffsetTvdb)
	}
}
