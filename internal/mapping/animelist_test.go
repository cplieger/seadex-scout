package mapping

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/cplieger/xmlx"
)

// loadListFixture reads the real-shape Anime-Lists fixture: verbatim nodes from
// anime-list-master.xml, wrapped in one <anime-list>.
func loadListFixture(t *testing.T) []byte {
	t.Helper()
	body, err := os.ReadFile("testdata/anime-list-fixture.xml")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return body
}

// TestFilmEpisode pins the film reader over the row shapes the real list carries.
// The gates are what make it safe to stamp a Sonarr label from: a node filed
// under the parent's specials (defaulttvdbseason "0"), the film's own row
// (anidbseason "1", never the specials' own anidbseason "0" row that comes first
// in No Game No Life Zero), pair text with no start attribute, every target the
// same positive episode.
func TestFilmEpisode(t *testing.T) {
	film := func(text string) rowXML { return rowXML{AniDBSeason: "1", TVDBSeason: "0", Text: text} }
	tests := []struct {
		name          string
		defaultSeason string
		rows          []rowXML
		want          int
	}{
		{name: "single_pair", defaultSeason: "0", rows: []rowXML{film(";1-8;")}, want: 8},
		{name: "multi_part_film_one_special", defaultSeason: "0", rows: []rowXML{film(";1-1;2-1;")}, want: 1},
		{name: "differing_targets", defaultSeason: "0", rows: []rowXML{film(";1-3;2-4;")}, want: 0},
		{name: "plus_target", defaultSeason: "0", rows: []rowXML{film(";1-3+4;")}, want: 0},
		{name: "zero_target_no_tvdb_episode", defaultSeason: "0", rows: []rowXML{film(";1-0;")}, want: 0},
		// A zero pair beside positive ones: the refusal has to be the zero itself,
		// since the agreeing row leaves the differing-target rule nothing to catch.
		{name: "zero_target_beside_agreeing_positives", defaultSeason: "0", rows: []rowXML{film(";1-0;2-10;")}, want: 0},
		{name: "zero_target_beside_differing_positives", defaultSeason: "0", rows: []rowXML{film(";1-0;2-10;3-11;")}, want: 0},
		{name: "non_integer_target", defaultSeason: "0", rows: []rowXML{film(";1-x;")}, want: 0},
		{name: "specials_row_first_then_film_row", defaultSeason: "0", rows: []rowXML{
			{AniDBSeason: "0", TVDBSeason: "0", Text: ";1-7;"}, film(";1-8;"),
		}, want: 8},
		{name: "specials_row_alone_is_not_the_film", defaultSeason: "0", rows: []rowXML{
			{AniDBSeason: "0", TVDBSeason: "0", Text: ";1-0;"},
		}, want: 0},
		{name: "range_row_with_text_ignored", defaultSeason: "0", rows: []rowXML{
			{AniDBSeason: "1", TVDBSeason: "0", Start: "2", End: "9", Text: ";1-9;"},
		}, want: 0},
		{name: "series_node_gate", defaultSeason: "1", rows: []rowXML{film(";10-1;")}, want: 0},
		{name: "absolute_node_gate", defaultSeason: "a", rows: []rowXML{film(";1-8;")}, want: 0},
		{name: "positive_season_row_not_specials", defaultSeason: "0", rows: []rowXML{
			{AniDBSeason: "1", TVDBSeason: "1", Text: ";1-8;"},
		}, want: 0},
		{name: "no_rows", defaultSeason: "0", want: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := filmEpisode(tc.defaultSeason, tc.rows); got != tc.want {
				t.Errorf("filmEpisode(%q, %+v) = %d, want %d", tc.defaultSeason, tc.rows, got, tc.want)
			}
		})
	}
}

// TestSeasonRanges pins the season reader: only anidbseason "1" ranged rows with a
// tvdbseason of 1 or more count (a tmdbseason row has no tvdbseason, a season-0
// range would label a pack S00), an open-ended row keeps Last 0, rows sort by
// first episode, and a run starting above episode 1 gains the leading season.
func TestSeasonRanges(t *testing.T) {
	ranged := func(anidb, tvdb, start, end string) rowXML {
		return rowXML{AniDBSeason: anidb, TVDBSeason: tvdb, Start: start, End: end}
	}
	tests := []struct {
		name string
		rows []rowXML
		want []SeasonRange
	}{
		{name: "none", rows: nil, want: nil},
		{name: "pair_text_row_is_not_a_range", rows: []rowXML{{AniDBSeason: "1", TVDBSeason: "2", Text: ";1-1;"}}, want: nil},
		{name: "tmdb_row_excluded", rows: []rowXML{{AniDBSeason: "1", Start: "1", End: "61"}}, want: nil},
		{name: "season_zero_range_excluded", rows: []rowXML{ranged("1", "0", "1", "5")}, want: nil},
		{name: "anidbseason_zero_range_excluded", rows: []rowXML{ranged("0", "1", "1", "15")}, want: nil},
		{name: "unparseable_start_skipped", rows: []rowXML{ranged("1", "1", "x", "5")}, want: nil},
		{name: "non_positive_start_skipped", rows: []rowXML{ranged("1", "1", "0", "5")}, want: nil},
		{name: "unparseable_end_skipped", rows: []rowXML{ranged("1", "1", "1", "x"), ranged("1", "2", "9", "30")}, want: []SeasonRange{
			{Season: 1, First: 1, Last: 8}, {Season: 2, First: 9, Last: 30},
		}},
		{name: "open_ended_last_row", rows: []rowXML{ranged("1", "1", "1", "8"), ranged("1", "2", "9", "")}, want: []SeasonRange{
			{Season: 1, First: 1, Last: 8}, {Season: 2, First: 9},
		}},
		{name: "sorted_by_first_episode", rows: []rowXML{ranged("1", "2", "9", "30"), ranged("1", "1", "1", "8")}, want: []SeasonRange{
			{Season: 1, First: 1, Last: 8}, {Season: 2, First: 9, Last: 30},
		}},
		{name: "leading_run_is_the_season_below_the_lowest", rows: []rowXML{ranged("1", "2", "14", "28"), ranged("1", "3", "29", "68")}, want: []SeasonRange{
			{Season: 1, First: 1, Last: 13}, {Season: 2, First: 14, Last: 28}, {Season: 3, First: 29, Last: 68},
		}},
		{name: "leading_run_floors_at_season_one", rows: []rowXML{ranged("1", "1", "5", "10")}, want: []SeasonRange{
			{Season: 1, First: 1, Last: 4}, {Season: 1, First: 5, Last: 10},
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := seasonRanges(tc.rows); !slices.Equal(got, tc.want) {
				t.Errorf("seasonRanges(%+v) = %+v, want %+v", tc.rows, got, tc.want)
			}
		})
	}
}

// TestParseMappingList_fixture drives the decoder over verbatim nodes from the real
// list and pins what each yields. The fixture also carries every unmodeled child
// (<name>, <supplemental-info>, <before>), which must decode as skipped.
func TestParseMappingList_fixture(t *testing.T) {
	got, err := parseMappingList(loadListFixture(t))
	if err != nil {
		t.Fatalf("parseMappingList(fixture) error: %v", err)
	}
	for id, m := range got {
		if m.SpecialEpisode == 0 && len(m.Seasons) == 0 {
			t.Errorf("mappings[%d] = %+v, want no empty Mapping retained", id, m)
		}
	}
	// No Game No Life Zero: the specials' own ;1-7; row comes FIRST and the film's
	// ;1-8; row second, so a first-match reader answers 7.
	if got[12276].SpecialEpisode != 8 {
		t.Errorf("12276 (No Game No Life Zero) SpecialEpisode = %d, want 8 (the anidbseason 1 row, not the first row)", got[12276].SpecialEpisode)
	}
	// Street Fighter II: three film parts filed as one special.
	if got[313].SpecialEpisode != 1 {
		t.Errorf("313 (Street Fighter II) SpecialEpisode = %d, want 1", got[313].SpecialEpisode)
	}
	// Fairy Tail (2011): six pairs with six different targets is a specials run,
	// not a film.
	if _, ok := got[8132]; ok {
		t.Errorf("8132 (Fairy Tail 2011) = %+v, want absent (differing targets yield no episode)", got[8132])
	}
	// Anidb 73: its only row is ;1-0; on the specials' own anidbseason 0 row.
	if _, ok := got[73]; ok {
		t.Errorf("73 = %+v, want absent", got[73])
	}
	// Hourou Musuko: a series node (defaulttvdbseason 1) carrying an
	// anidbseason 1 / tvdbseason 0 row for one of its own specials.
	if _, ok := got[7949]; ok {
		t.Errorf("7949 (Hourou Musuko) = %+v, want absent (the node gate)", got[7949])
	}
	// Seitokai Yakuindomo * OAD: a tvdbseason 0 row carrying BOTH start and text
	// is a range row into the specials bucket, so neither reader takes it.
	if _, ok := got[10761]; ok {
		t.Errorf("10761 = %+v, want absent (a season-0 range row is neither a film nor a season)", got[10761])
	}
	// Master Keaton: an anidbseason 0 ranged row into season 1 is the specials'
	// row, not the run's.
	if _, ok := got[1439]; ok {
		t.Errorf("1439 (Master Keaton) = %+v, want absent (an anidbseason 0 range is excluded)", got[1439])
	}
	// Seikai no Senki II: a positive default season with only a specials row.
	if _, ok := got[5]; ok {
		t.Errorf("5 = %+v, want absent", got[5])
	}
	assertOnePiece(t, got[69])
	assertDragonBall(t, got)
	assertFairyTail(t, got)
}

// assertOnePiece pins the One Piece node: 22 tmdbseason rows and 23 tvdbseason
// rows, of which exactly the 23 tvdb rows survive, S1 = 1-8 (tvdb, not tmdb's
// 1-61), S23 open-ended.
func assertOnePiece(t *testing.T, m Mapping) {
	t.Helper()
	if m.SpecialEpisode != 0 {
		t.Errorf("69 (One Piece) SpecialEpisode = %d, want 0 (an absolute node names no film)", m.SpecialEpisode)
	}
	if len(m.Seasons) != 23 {
		t.Fatalf("69 (One Piece) Seasons = %d ranges, want exactly the 23 tvdb rows (no tmdb row)", len(m.Seasons))
	}
	if got, want := m.Seasons[0], (SeasonRange{Season: 1, First: 1, Last: 8}); got != want {
		t.Errorf("69 Seasons[0] = %+v, want %+v (tvdb S1, not tmdb's 1-61)", got, want)
	}
	if got, want := m.Seasons[21], (SeasonRange{Season: 22, First: 1086, Last: 1155}); got != want {
		t.Errorf("69 Seasons[21] = %+v, want %+v", got, want)
	}
	if got, want := m.Seasons[22], (SeasonRange{Season: 23, First: 1156}); got != want {
		t.Errorf("69 Seasons[22] = %+v, want %+v (open-ended)", got, want)
	}
	for i, r := range m.Seasons {
		if r.Season != i+1 {
			t.Errorf("69 Seasons[%d].Season = %d, want %d (one row per season, in order)", i, r.Season, i+1)
		}
	}
}

// assertDragonBall pins the leading-run rule on the fixture's three real nodes,
// each of which names season 2 first and leaves its earlier episodes to the
// prepended season 1.
func assertDragonBall(t *testing.T, got map[int]Mapping) {
	t.Helper()
	wantDB := []SeasonRange{
		{Season: 1, First: 1, Last: 13},
		{Season: 2, First: 14, Last: 28},
		{Season: 3, First: 29, Last: 68},
		{Season: 4, First: 69, Last: 101},
		{Season: 5, First: 102, Last: 132},
		{Season: 6, First: 133, Last: 153},
	}
	if !slices.Equal(got[231].Seasons, wantDB) {
		t.Errorf("231 (Dragon Ball) Seasons = %+v, want %+v", got[231].Seasons, wantDB)
	}
	wantGT := []SeasonRange{{Season: 1, First: 1, Last: 16}, {Season: 2, First: 17, Last: 40}, {Season: 3, First: 41, Last: 64}}
	if !slices.Equal(got[233].Seasons, wantGT) {
		t.Errorf("233 (Dragon Ball GT) Seasons = %+v, want %+v", got[233].Seasons, wantGT)
	}
	if got, want := got[312].Seasons[0], (SeasonRange{Season: 1, First: 1, Last: 25}); got != want {
		t.Errorf("312 (Yuu Yuu Hakusho) Seasons[0] = %+v, want %+v", got, want)
	}
}

// assertFairyTail pins the split show: three nodes sharing tvdbid 114801, each
// carrying only its own seasons.
func assertFairyTail(t *testing.T, got map[int]Mapping) {
	t.Helper()
	want := map[int][]SeasonRange{
		6662:  {{Season: 1, First: 1, Last: 48}, {Season: 2, First: 49, Last: 96}, {Season: 3, First: 97, Last: 150}, {Season: 4, First: 151, Last: 175}},
		9980:  {{Season: 5, First: 1, Last: 51}, {Season: 6, First: 52, Last: 90}, {Season: 7, First: 91, Last: 102}},
		13295: {{Season: 8, First: 1, Last: 51}},
	}
	for id, seasons := range want {
		if !slices.Equal(got[id].Seasons, seasons) {
			t.Errorf("%d (Fairy Tail) Seasons = %+v, want %+v", id, got[id].Seasons, seasons)
		}
	}
}

// TestParseMappingList_rejectsWrongDocuments pins the decode errors: a body with
// no <anime> node, a root that is not <anime-list>, malformed XML, and a node
// whose anidbid is not a positive integer (skipped, not fatal).
func TestParseMappingList_rejectsWrongDocuments(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr error
	}{
		{name: "empty_list", body: `<anime-list></anime-list>`, wantErr: errListNoNodes},
		{name: "wrong_root", body: `<rss><anime anidbid="1" defaulttvdbseason="0"/></rss>`, wantErr: errListRoot},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseMappingList([]byte(tc.body))
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("parseMappingList(%q) error = %v, want %v", tc.body, err, tc.wantErr)
			}
		})
	}
	if _, err := parseMappingList([]byte(`<anime-list><anime anidbid="1"`)); err == nil {
		t.Error("parseMappingList(truncated) = nil error, want a decode error")
	}
	got, err := parseMappingList([]byte(`<anime-list>` +
		`<anime anidbid="x" defaulttvdbseason="0"><mapping-list><mapping anidbseason="1" tvdbseason="0">;1-2;</mapping></mapping-list></anime>` +
		`<anime anidbid="-4" defaulttvdbseason="0"><mapping-list><mapping anidbseason="1" tvdbseason="0">;1-2;</mapping></mapping-list></anime>` +
		`<anime anidbid="0" defaulttvdbseason="0"><mapping-list><mapping anidbseason="1" tvdbseason="0">;1-2;</mapping></mapping-list></anime>` +
		`<anime anidbid="9" defaulttvdbseason="0"><mapping-list><mapping anidbseason="1" tvdbseason="0">;1-2;</mapping></mapping-list></anime>` +
		`</anime-list>`))
	if err != nil {
		t.Fatalf("parseMappingList(bad ids) error: %v", err)
	}
	if len(got) != 1 || got[9].SpecialEpisode != 2 {
		t.Errorf("parseMappingList(bad ids) = %+v, want only anidbid 9 retained", got)
	}
}

// TestParseMappingList_boundBreachIsClassifiable pins that an xmlx bound refusal
// surfaces through the chain as the library's *LimitError (matched on Kind, the
// cross-library acceptance assertion), for both a lexical breach the preflight
// catches and a decoded-value breach the budget catches.
func TestParseMappingList_boundBreachIsClassifiable(t *testing.T) {
	deep := strings.Repeat("<a>", listMaxDepth+1) + strings.Repeat("</a>", listMaxDepth+1)
	_, err := parseMappingList([]byte(`<anime-list>` + deep + `</anime-list>`))
	le, ok := errors.AsType[*xmlx.LimitError](err)
	if !ok || le.Kind != xmlx.KindDepth {
		t.Fatalf("parseMappingList(deep) error = %v, want an *xmlx.LimitError of KindDepth", err)
	}
	if !errors.Is(err, xmlx.ErrLimit) {
		t.Errorf("errors.Is(err, xmlx.ErrLimit) = false, want true")
	}
	// A run text under the raw 64 KiB run cap but over the 4 KiB decoded field cap.
	long := strings.Repeat(";1-8", listMaxFieldBytes/4+1) + ";"
	_, err = parseMappingList([]byte(`<anime-list><anime anidbid="1" defaulttvdbseason="0"><mapping-list>` +
		`<mapping anidbseason="1" tvdbseason="0">` + long + `</mapping></mapping-list></anime></anime-list>`))
	le, ok = errors.AsType[*xmlx.LimitError](err)
	if !ok || le.Kind != xmlx.KindField {
		t.Errorf("parseMappingList(long row) error = %v, want an *xmlx.LimitError of KindField", err)
	}
}
