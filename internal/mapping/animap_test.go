package mapping

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"testing"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func animapDoc(records string) string {
	return `{"version":1,"records":` + records + `}`
}

func animapBody(records string) []byte { return []byte(animapDoc(records)) }

func keyedRecordsBody(n int) []byte {
	var b strings.Builder
	b.WriteByte('[')
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"anilist_id":%d}`, i+1)
	}
	b.WriteByte(']')
	return animapBody(b.String())
}

func TestParseAnimap_decodesTheFieldsConsumersRead(t *testing.T) {
	body := animapBody(`[
		{"anilist_id":1,"anidb_id":23,"mal_id":1,"type":"TV","episodes":26,"tvdb_id":76885,"tvdb_season":1,"tmdb_tv_id":30991,"tmdb_season":1},
		{"anilist_id":2,"anidb_id":69,"type":"tv","tvdb_id":81797,"tvdb_absolute":true},
		{"anilist_id":3,"anidb_id":70,"type":"MOVIE","tvdb_id":500,"tvdb_season":0,"tmdb_movie_ids":[0,42],"imdb_ids":["tt0000042"]},
		{"anilist_id":4,"type":"SPECIAL","episodes":1},
		{"anidb_id":99,"tvdb_id":600,"tvdb_season":2}
	]`)
	parsed, err := parseAnimap(body, discardLogger())
	if err != nil {
		t.Fatalf("parseAnimap error: %v", err)
	}
	want := []Record{
		{AniListID: 1, AniDBID: 23, Type: "TV", TvdbID: 76885, SeasonKind: SeasonPresent, SeasonTvdb: 1},
		{AniListID: 2, AniDBID: 69, Type: "TV", TvdbID: 81797, SeasonKind: SeasonAbsent},
		{AniListID: 3, AniDBID: 70, Type: "MOVIE", TvdbID: 500, SeasonKind: SeasonPresent, TmdbMovies: []int{42}, IMDbIDs: []string{"tt0000042"}},
		{AniListID: 4, Type: "SPECIAL", SeasonKind: SeasonAbsent},
	}
	if len(parsed.records) != len(want) {
		t.Fatalf("parseAnimap records = %+v, want %d AniList-keyed records", parsed.records, len(want))
	}
	for i := range want {
		got, w := parsed.records[i], want[i]
		if got.AniListID != w.AniListID || got.AniDBID != w.AniDBID || got.Type != w.Type || got.TvdbID != w.TvdbID ||
			got.SeasonKind != w.SeasonKind || got.SeasonTvdb != w.SeasonTvdb ||
			!slices.Equal(got.TmdbMovies, w.TmdbMovies) || !slices.Equal(got.IMDbIDs, w.IMDbIDs) {
			t.Errorf("parseAnimap record %d = %+v, want %+v", i, got, w)
		}
	}
	if parsed.elements != 5 {
		t.Errorf("parseAnimap elements = %d, want 5 (the AniDB-keyed record counts)", parsed.elements)
	}
}

func TestParseAnimap_filmEpisode(t *testing.T) {
	tests := []struct {
		name   string
		record string
		want   int
	}{
		{
			name: "one pair names the episode", want: 8,
			record: `{"anidb_id":7,"tvdb_id":1,"tvdb_season":0,"mapping_list":[{"anidb_season":1,"tvdb_season":0,"episodes":[[1,8]]}]}`,
		},
		{
			name: "every pair names the same episode", want: 3,
			record: `{"anidb_id":7,"tvdb_id":1,"tvdb_season":0,"mapping_list":[{"anidb_season":1,"tvdb_season":0,"episodes":[[1,3],[2,3]]}]}`,
		},
		{
			name: "pairs disagree", want: 0,
			record: `{"anidb_id":7,"tvdb_id":1,"tvdb_season":0,"mapping_list":[{"anidb_season":1,"tvdb_season":0,"episodes":[[1,3],[2,4]]}]}`,
		},
		{
			name: "no counterpart", want: 0,
			record: `{"anidb_id":7,"tvdb_id":1,"tvdb_season":0,"mapping_list":[{"anidb_season":1,"tvdb_season":0,"episodes":[[1]]}]}`,
		},
		{
			name: "a pair with no counterpart beside one that names an episode", want: 0,
			record: `{"anidb_id":7,"tvdb_id":1,"tvdb_season":0,"mapping_list":[{"anidb_season":1,"tvdb_season":0,"episodes":[[1,8],[2]]}]}`,
		},
		{
			name: "one episode spans two", want: 0,
			record: `{"anidb_id":7,"tvdb_id":1,"tvdb_season":0,"mapping_list":[{"anidb_season":1,"tvdb_season":0,"episodes":[[1,5,6]]}]}`,
		},
		{
			name: "series filed under a real season", want: 0,
			record: `{"anidb_id":7,"tvdb_id":1,"tvdb_season":1,"mapping_list":[{"anidb_season":1,"tvdb_season":0,"episodes":[[10,1]]}]}`,
		},
		{
			name: "a range row is not the film's own row", want: 0,
			record: `{"anidb_id":7,"tvdb_id":1,"tvdb_season":0,"mapping_list":[{"anidb_season":1,"tvdb_season":0,"start":1,"end":2,"offset":4}]}`,
		},
		{
			name: "a range row's single episodes do not join the film's own row", want: 8,
			record: `{"anidb_id":7,"tvdb_id":1,"tvdb_season":0,"mapping_list":[{"anidb_season":1,"tvdb_season":0,"episodes":[[1,8]]},` +
				`{"anidb_season":1,"tvdb_season":0,"start":2,"end":3,"offset":4,"episodes":[[2,9]]}]}`,
		},
		{
			name: "a specials row is not the film's own row", want: 0,
			record: `{"anidb_id":7,"tvdb_id":1,"tvdb_season":0,"mapping_list":[{"anidb_season":0,"tvdb_season":0,"episodes":[[1,9]]}]}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := parseAnimap(animapBody(`[`+tc.record+`]`), discardLogger())
			if err != nil {
				t.Fatalf("parseAnimap(%s) error: %v", tc.record, err)
			}
			if got := parsed.mappings[7].SpecialEpisode; got != tc.want {
				t.Errorf("parseAnimap(%s) special episode = %d, want %d", tc.record, got, tc.want)
			}
		})
	}
}

func TestParseAnimap_seasonRanges(t *testing.T) {
	tests := []struct {
		name string
		rows string
		want []SeasonRange
	}{
		{
			name: "ranges sorted with an open end",
			rows: `{"anidb_season":1,"tvdb_season":2,"start":9,"end":30},{"anidb_season":1,"tvdb_season":1,"start":1,"end":8},{"anidb_season":1,"tvdb_season":23,"start":1156}`,
			want: []SeasonRange{{Season: 1, First: 1, Last: 8}, {Season: 2, First: 9, Last: 30}, {Season: 23, First: 1156}},
		},
		{
			name: "uncovered leading run is the season below the lowest named",
			rows: `{"anidb_season":1,"tvdb_season":2,"start":14,"end":30}`,
			want: []SeasonRange{{Season: 1, First: 1, Last: 13}, {Season: 2, First: 14, Last: 30}},
		},
		{
			name: "a TMDB row is not a TVDB range",
			rows: `{"anidb_season":1,"tmdb_season":1,"start":1,"end":61}`,
		},
		{
			name: "a season-0 target is not a range",
			rows: `{"anidb_season":1,"tvdb_season":0,"start":1,"end":2}`,
		},
		{
			name: "a specials row is not a range",
			rows: `{"anidb_season":0,"tvdb_season":2,"start":1,"end":2}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			record := `{"anidb_id":7,"tvdb_id":1,"tvdb_absolute":true,"mapping_list":[` + tc.rows + `]}`
			parsed, err := parseAnimap(animapBody(`[`+record+`]`), discardLogger())
			if err != nil {
				t.Fatalf("parseAnimap(%s) error: %v", record, err)
			}
			if got := parsed.mappings[7].Seasons; !slices.Equal(got, tc.want) {
				t.Errorf("parseAnimap(%s) seasons = %+v, want %+v", record, got, tc.want)
			}
		})
	}
}

// TestParseAnimap_keysFactsByTheirJoin pins where each record's facts land: under
// its AniDB id whether or not it carries an AniList id, under its AniList id
// when it is a specials-of-parent record with no AniDB id, and nowhere for an
// AniList-only record without a valid anidb_parent, whose facts have no join.
func TestParseAnimap_keysFactsByTheirJoin(t *testing.T) {
	special := `"tvdb_id":1,"tvdb_season":0,"mapping_list":[{"anidb_season":1,"tvdb_season":0,"episodes":[[1,8]]}]`
	body := animapBody(`[` +
		`{"anilist_id":10,"anidb_id":100,` + special + `},` +
		`{"anilist_id":11,"anidb_parent":{"anidb_id":500,"specials":[1]},` + special + `},` +
		`{"anilist_id":13,` + special + `},` +
		`{"anilist_id":14,"anidb_parent":{"specials":[1]},` + special + `},` +
		`{"anidb_id":200,` + special + `},` +
		`{"anilist_id":12,"anidb_id":300,"tvdb_id":1,"tvdb_season":1}]`)
	parsed, err := parseAnimap(body, discardLogger())
	if err != nil {
		t.Fatalf("parseAnimap error: %v", err)
	}
	want := map[int]Mapping{100: {SpecialEpisode: 8}, 200: {SpecialEpisode: 8}}
	if !maps.EqualFunc(parsed.mappings, want, sameMapping) {
		t.Errorf("parseAnimap mappings = %+v, want %+v (a record with no facts keeps no entry)", parsed.mappings, want)
	}
	wantParent := map[int]Mapping{11: {SpecialEpisode: 8}}
	if !maps.EqualFunc(parsed.parentMappings, wantParent, sameMapping) {
		t.Errorf("parseAnimap parent mappings = %+v, want %+v", parsed.parentMappings, wantParent)
	}
}

func sameMapping(a, b Mapping) bool {
	return a.SpecialEpisode == b.SpecialEpisode && slices.Equal(a.Seasons, b.Seasons)
}

func TestParseAnimap_refusesWhatIsNotAVersionOneDocument(t *testing.T) {
	tests := []struct {
		want error
		name string
		body string
	}{
		{name: "array", body: `[{"anilist_id":1}]`, want: errNotAnimapDocument},
		{name: "null", body: `null`, want: errNotAnimapDocument},
		{name: "no records", body: `{"version":1}`, want: errNotAnimapDocument},
		{name: "null records", body: `{"version":1,"records":null}`, want: errNotAnimapDocument},
		{name: "records not an array", body: `{"version":1,"records":{}}`, want: errNotAnimapDocument},
		{name: "repeated records", body: `{"version":1,"records":[],"records":[]}`, want: errNotAnimapDocument},
		{name: "version not a number", body: `{"version":"1","records":[]}`, want: errNotAnimapDocument},
		{name: "a later version", body: `{"version":2,"records":[{"anilist_id":1}]}`, want: errUnsupportedVersion},
		{name: "no version", body: `{"records":[{"anilist_id":1}]}`, want: errUnsupportedVersion},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := parseAnimap([]byte(tc.body), discardLogger())
			if !errors.Is(err, tc.want) {
				t.Fatalf("parseAnimap(%s) error = %v, want %v", tc.body, err, tc.want)
			}
			if parsed.records != nil || parsed.mappings != nil || parsed.elements != 0 {
				t.Errorf("parseAnimap(%s) returned a partial result %+v alongside its error", tc.body, parsed)
			}
		})
	}
}

// TestParseAnimap_transientFailuresCarryNoSentinel pins that an empty body and a
// body truncated mid-stream are not content-shape evidence: both can succeed on
// the next attempt, so neither may carry a sentinel that advances the streak.
func TestParseAnimap_transientFailuresCarryNoSentinel(t *testing.T) {
	for _, body := range []string{"", "  \n", `{"version":1,"records":[{"anilist_id":1,`, `{"version":1,"records":[]} trailing`} {
		_, err := parseAnimap([]byte(body), discardLogger())
		if err == nil {
			t.Fatalf("parseAnimap(%q) error = nil, want a parse failure", body)
		}
		for _, sentinel := range []error{errNotAnimapDocument, errUnsupportedVersion, errRecordCapExceeded, errIdentifierBudgetExceeded} {
			if errors.Is(err, sentinel) {
				t.Errorf("parseAnimap(%q) error = %v, want no persistent sentinel (%v)", body, err, sentinel)
			}
		}
	}
	if _, err := parseAnimap(nil, discardLogger()); !errors.Is(err, io.EOF) {
		t.Errorf("parseAnimap(empty) error = %v, want it to wrap io.EOF", err)
	}
}

// TestParseAnimap_toleratesMembersItDoesNotRead pins that additive schema
// members, at the top level and on a record, neither fail nor change a record.
func TestParseAnimap_toleratesMembersItDoesNotRead(t *testing.T) {
	body := []byte(`{"sources":{"overlay":{"entries":3}},"records":[{"anilist_id":1,"type":"TV","tvdb_id":5,"future":[1,{"x":2}]}],"version":1,"attribution":{}}`)
	parsed, err := parseAnimap(body, discardLogger())
	if err != nil {
		t.Fatalf("parseAnimap error: %v", err)
	}
	if len(parsed.records) != 1 || parsed.records[0].TvdbID != 5 {
		t.Errorf("parseAnimap records = %+v, want the one record with tvdb 5", parsed.records)
	}
}

// TestParseAnimap_skipsMalformedRecords pins the per-record tolerance: a member of
// the wrong JSON type, an oversized record and an identifier list over its cap
// each skip their record and keep the rest, and every element still counts.
func TestParseAnimap_skipsMalformedRecords(t *testing.T) {
	overList := `"imdb_ids":["tt0000001"` + strings.Repeat(`,"tt0000001"`, maxRecordIdentifiers) + `]`
	oversized := `{"anilist_id":5,"type":"` + strings.Repeat("x", maxRecordBytes) + `"}`
	body := animapBody(`[{"anilist_id":1,"tvdb_id":"100"},{"anilist_id":2,` + overList + `},` + oversized + `,{"anilist_id":3,"tvdb_id":300}]`)
	parsed, err := parseAnimap(body, discardLogger())
	if err != nil {
		t.Fatalf("parseAnimap error: %v", err)
	}
	if len(parsed.records) != 1 || parsed.records[0].AniListID != 3 {
		t.Errorf("parseAnimap records = %+v, want only the well-formed record 3", parsed.records)
	}
	if parsed.elements != 4 {
		t.Errorf("parseAnimap elements = %d, want 4", parsed.elements)
	}
}

func TestParseAnimap_recordCap(t *testing.T) {
	if _, err := parseAnimap(keyedRecordsBody(maxRecords), discardLogger()); err != nil {
		t.Fatalf("parseAnimap(exactly %d records) error: %v, want acceptance", maxRecords, err)
	}
	if _, err := parseAnimap(keyedRecordsBody(maxRecords+1), discardLogger()); !errors.Is(err, errRecordCapExceeded) {
		t.Fatalf("parseAnimap(%d records) error = %v, want errRecordCapExceeded", maxRecords+1, err)
	}
}

// TestParseAnimap_retainedBudgetCountsSeasonRanges pins that season ranges join
// the identifiers in the retained budget: a document whose ids alone fit but
// whose ranges push it over must be refused.
func TestParseAnimap_retainedBudgetCountsSeasonRanges(t *testing.T) {
	const idsPerRecord = maxRecordIdentifiers
	n := maxRetainedTotal / idsPerRecord
	var b strings.Builder
	b.WriteByte('[')
	ids := strings.TrimSuffix(strings.Repeat("1,", idsPerRecord), ",")
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"anilist_id":%d,"tmdb_movie_ids":[%s]}`, i+1, ids)
	}
	b.WriteByte(']')
	if _, err := parseAnimap(animapBody(b.String()), discardLogger()); err != nil {
		t.Fatalf("parseAnimap(exactly the budget in ids) error: %v, want acceptance", err)
	}
	withRange := strings.TrimSuffix(b.String(), "]") +
		`,{"anidb_id":1,"tvdb_id":1,"mapping_list":[{"anidb_season":1,"tvdb_season":1,"start":1}]}]`
	if _, err := parseAnimap(animapBody(withRange), discardLogger()); !errors.Is(err, errIdentifierBudgetExceeded) {
		t.Fatalf("parseAnimap(budget plus one season range) error = %v, want errIdentifierBudgetExceeded", err)
	}
}
