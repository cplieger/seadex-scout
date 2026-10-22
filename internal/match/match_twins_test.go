package match

import (
	"slices"
	"testing"

	"github.com/cplieger/seadex-scout/internal/align"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/seadex"
)

var seasonFilm = mapping.Record{
	AniListID: 2, AniDBID: 80, Type: "MOVIE", TvdbID: 600, TmdbMovies: []int{43},
	SeasonKind: mapping.SeasonPresent, SeasonTvdb: 1,
}

var seasonOVA = mapping.Record{
	AniListID: 3, AniDBID: 90, Type: "OVA", TvdbID: 700, TmdbMovies: []int{44},
	SeasonKind: mapping.SeasonPresent, SeasonTvdb: 2,
}

var seriesFilm = mapping.Record{
	AniListID: 4, AniDBID: 100, Type: "MOVIE", TvdbID: 800, TmdbMovies: []int{45},
	SeasonKind: mapping.SeasonAbsent,
}

var unplacedFilm = mapping.Record{
	AniListID: 5, AniDBID: 110, Type: "MOVIE", TvdbID: 900, TmdbMovies: []int{91},
	SeasonKind: mapping.SeasonAbsent,
}

func twinIndex(rec mapping.Record) *mapping.Index {
	facts := map[int]mapping.Mapping{
		seasonFilm.AniDBID: {
			Seasons: []mapping.SeasonRange{{Season: 1, First: 1, Last: 1}}, SeasonsTvdb: 600,
			Episodes: []int{1}, EpisodesTvdb: 600, EpisodesSeason: 1,
		},
		seasonOVA.AniDBID:  {Seasons: []mapping.SeasonRange{{Season: 2, First: 1, Last: 2}}, SeasonsTvdb: 700},
		placedFilm.AniDBID: {Specials: []int{9}, SpecialsTvdb: placedFilm.TvdbID},
		seriesFilm.AniDBID: {Seasons: []mapping.SeasonRange{{Season: 3, First: 1, Last: 12}}, SeasonsTvdb: 800},
	}
	return mapping.NewIndex(mapping.Source{Records: []mapping.Record{rec}, Mappings: facts})
}

// filmSeries has AniList 2593's shape (animap_links_test.go): a season of
// seven films, so the placed episode is a proper part of it and Decide judges
// the entry on that episode alone.
func filmSeries(fileOnPlaced bool) library.Item {
	listed := map[int]library.ListedEpisode{}
	for ep := 1; ep <= 7; ep++ {
		listed[ep] = library.ListedEpisode{}
	}
	listed[2] = library.ListedEpisode{Group: "oz", HasFile: true}
	groups := []string{"oz"}
	if fileOnPlaced {
		listed[1] = library.ListedEpisode{Group: "mtbb", HasFile: true}
		groups = []string{"mtbb", "oz"}
	}
	return library.Item{
		Arr: library.ArrSonarr, ArrID: 8, TvdbID: 600, ImdbID: "tt0000600",
		SeasonGroups: map[int][]string{1: groups}, Listed: map[int]map[int]library.ListedEpisode{1: listed},
	}
}

func ovaSeries(fileOnPlaced bool) library.Item {
	it := library.Item{Arr: library.ArrSonarr, ArrID: 9, TvdbID: 700, ImdbID: "tt0000700", SeasonGroups: map[int][]string{1: {"oz"}}}
	if fileOnPlaced {
		it.SeasonGroups[2] = []string{"mtbb"}
	}
	return it
}

func movie(arrID, tmdb int, hasFile bool) library.Item {
	it := library.Item{Arr: library.ArrRadarr, ArrID: arrID, TmdbID: tmdb, HasFile: hasFile}
	if hasFile {
		it.Groups = []string{"mtbb"}
	}
	return it
}

type judged struct {
	arr, uncompared string
	outcome         align.Outcome
}

func judgeCopies(t *testing.T, rec mapping.Record, items ...library.Item) []judged {
	t.Helper()
	snap := &library.Snapshot{Items: items}
	res := New(&countingAniList{}, nil).Match(t.Context(), []seadex.Entry{{AniListID: rec.AniListID}}, snap, twinIndex(rec), Memo{})
	if hits := res.Coverage.Hits[library.ArrRadarr] + res.Coverage.Hits[library.ArrSonarr]; hits != 1 {
		t.Errorf("coverage hits = %v, want the entry counted once", res.Coverage.Hits)
	}
	out := make([]judged, 0, len(res.Matches))
	for i := range res.Matches {
		m := &res.Matches[i]
		if !m.InLibrary() {
			t.Fatalf("Match[%d] = %+v, want an in-library copy", i, m)
		}
		if m.Arr != m.Item.Arr {
			t.Errorf("Match[%d].Arr = %q, item arr %q; want them equal", i, m.Arr, m.Item.Arr)
		}
		j := judged{arr: m.Arr, outcome: align.Decide(m.Item, m.AlignEntry(), &align.Listing{Best: []string{"mtbb"}}).Outcome}
		if m.Uncompared != nil {
			j.uncompared = m.Uncompared.Arr
		}
		out = append(out, j)
	}
	return out
}

func TestMatchJudgesBothCopiesOfASeasonPlacedFilm(t *testing.T) {
	for _, tc := range []struct {
		name   string
		want   []judged
		radarr bool
		sonarr bool
	}{
		{"both hold a file", []judged{{library.ArrRadarr, "", align.OutcomeAligned}, {library.ArrSonarr, "", align.OutcomeAligned}}, true, true},
		{"only the Radarr film holds one", []judged{{library.ArrRadarr, library.ArrSonarr, align.OutcomeAligned}}, true, false},
		{"only the Sonarr episode holds one", []judged{{library.ArrSonarr, library.ArrRadarr, align.OutcomeAligned}}, false, true},
		{"neither holds one", []judged{{library.ArrRadarr, library.ArrSonarr, align.OutcomeNoFile}}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := judgeCopies(t, seasonFilm, movie(4, 43, tc.radarr), filmSeries(tc.sonarr))
			if !slices.Equal(got, tc.want) {
				t.Errorf("copies = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestMatchJudgesTheSonarrCopyOnThePlacedEpisodes(t *testing.T) {
	snap := &library.Snapshot{Items: []library.Item{movie(4, 43, true), filmSeries(true)}}
	res := New(&countingAniList{}, nil).Match(t.Context(), []seadex.Entry{{AniListID: 2}}, snap, twinIndex(seasonFilm), Memo{})
	if len(res.Matches) != 2 || res.Matches[1].Arr != library.ArrSonarr {
		t.Fatalf("Match = %+v, want the Radarr film then the Sonarr series", res.Matches)
	}
	m := &res.Matches[1]
	d := align.Decide(m.Item, m.AlignEntry(), &align.Listing{Best: []string{"mtbb"}})
	if d.Kind != align.ScopeEpisodes || d.Season != 1 || !slices.Equal(d.Episodes, []int{1}) || !slices.Equal(d.Groups, []string{"mtbb"}) {
		t.Errorf("Decide(Sonarr copy) = %v season %d episodes %v groups %v, want episodes S01E01 holding mtbb", d.Kind, d.Season, d.Episodes, d.Groups)
	}
}

func TestMatchJudgesBothCopiesOfASeasonPlacedSeries(t *testing.T) {
	imdbOnly := seasonOVA
	imdbOnly.TmdbMovies, imdbOnly.IMDbIDs = nil, []string{"tt0000044"}
	excluded := ovaSeries(false)
	excluded.AniListIDs = []int{999}
	for _, tc := range []struct {
		name   string
		rec    mapping.Record
		series library.Item
		want   []judged
		film   bool
	}{
		{"both hold a file", seasonOVA, ovaSeries(true), []judged{{library.ArrSonarr, "", align.OutcomeAligned}, {library.ArrRadarr, "", align.OutcomeAligned}}, true},
		{"only the Radarr film holds one", seasonOVA, ovaSeries(false), []judged{{library.ArrRadarr, library.ArrSonarr, align.OutcomeAligned}}, true},
		{"only the Sonarr season holds one", seasonOVA, ovaSeries(true), []judged{{library.ArrSonarr, library.ArrRadarr, align.OutcomeAligned}}, false},
		{"Sonarr's AniList ids exclude the entry", seasonOVA, excluded, []judged{{library.ArrSonarr, "", align.OutcomeWithheld}, {library.ArrRadarr, "", align.OutcomeAligned}}, true},
		{"the film is known only by an IMDb id", imdbOnly, ovaSeries(true), []judged{{library.ArrSonarr, "", align.OutcomeAligned}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			film := movie(5, 44, tc.film)
			film.ImdbID = "tt0000044"
			got := judgeCopies(t, tc.rec, tc.series, film)
			if !slices.Equal(got, tc.want) {
				t.Errorf("copies = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestMatchWithholdsATwinInANonAiredOrder(t *testing.T) {
	offeredOVA := placedFilm
	offeredOVA.AniListID, offeredOVA.AniDBID, offeredOVA.Type = 7, 130, "OVA"
	for _, tc := range []struct {
		name   string
		rec    mapping.Record
		film   library.Item
		series library.Item
		want   []judged
	}{
		{"the Radarr film holds a file", seasonFilm, movie(4, 43, true), filmSeries(false), []judged{{library.ArrRadarr, "", align.OutcomeAligned}, {library.ArrSonarr, "", align.OutcomeWithheld}}},
		{"the Radarr film holds none", seasonFilm, movie(4, 43, false), filmSeries(false), []judged{{library.ArrSonarr, library.ArrRadarr, align.OutcomeWithheld}}},
		{"a specials-placed film, no file on the placed special", placedFilm, radarrFilm(true), sonarrSeries(false), []judged{{library.ArrRadarr, "", align.OutcomeAligned}, {library.ArrSonarr, "", align.OutcomeWithheld}}},
		{"an OVA offered on season 0, placed on no special", offeredOVA, radarrFilm(true), sonarrSeries(false), []judged{{library.ArrSonarr, "", align.OutcomeWithheld}, {library.ArrRadarr, "", align.OutcomeAligned}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			series := tc.series
			series.Order = &library.Order{Type: "dvd", Name: "DVD Order"}
			got := judgeCopies(t, tc.rec, tc.film, series)
			if !slices.Equal(got, tc.want) {
				t.Errorf("copies = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestMatchJudgesAnEntryHeldInOneArrOnce(t *testing.T) {
	for _, tc := range []struct {
		name string
		rec  mapping.Record
		item library.Item
		want []judged
	}{
		{"a season-placed film in Radarr alone", seasonFilm, movie(4, 43, true), []judged{{library.ArrRadarr, "", align.OutcomeAligned}}},
		{"a season-placed film in Sonarr alone", seasonFilm, filmSeries(true), []judged{{library.ArrSonarr, "", align.OutcomeAligned}}},
		{"a season-placed film in Sonarr alone, no file", seasonFilm, filmSeries(false), []judged{{library.ArrSonarr, "", align.OutcomeNoFile}}},
		{"a season-placed OVA in Sonarr alone", seasonOVA, ovaSeries(true), []judged{{library.ArrSonarr, "", align.OutcomeAligned}}},
		{"a specials-placed film in Radarr alone", placedFilm, radarrFilm(true), []judged{{library.ArrRadarr, "", align.OutcomeAligned}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := judgeCopies(t, tc.rec, tc.item)
			if !slices.Equal(got, tc.want) {
				t.Errorf("copies = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestMatchJudgesBothCopiesOfAWholeSeriesFilm(t *testing.T) {
	for _, tc := range []struct {
		name    string
		seasons map[int][]string
		want    []judged
	}{
		{"the series holds a file on the entry's season", map[int][]string{1: {"oz"}, 3: {"mtbb"}}, []judged{{library.ArrRadarr, "", align.OutcomeAligned}, {library.ArrSonarr, "", align.OutcomeAligned}}},
		{"the series holds files on other seasons only", map[int][]string{1: {"oz"}}, []judged{{library.ArrRadarr, library.ArrSonarr, align.OutcomeAligned}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			series := library.Item{Arr: library.ArrSonarr, ArrID: 10, TvdbID: 800, SeasonGroups: tc.seasons}
			got := judgeCopies(t, seriesFilm, movie(6, 45, true), series)
			if !slices.Equal(got, tc.want) {
				t.Errorf("copies = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestMatchFindsNoSonarrCopyOfAFilmPlacedOnNothingOfTheSeries(t *testing.T) {
	parent := library.Item{Arr: library.ArrSonarr, ArrID: 12, TvdbID: 900, SeasonGroups: map[int][]string{1: {"oz"}, 2: {"oz"}}}
	excluding := parent
	excluding.AniListIDs = []int{999}
	dvd := parent
	dvd.Order = &library.Order{Type: "dvd", Name: "DVD Order"}
	offered := placedFilm
	offered.AniListID, offered.AniDBID, offered.TvdbID, offered.TmdbMovies = 6, 120, 900, []int{91}
	for _, shape := range []struct {
		name   string
		rec    mapping.Record
		series library.Item
	}{
		{"a film placed on no season", unplacedFilm, parent},
		{"a film placed on no season, the series links other AniList entries", unplacedFilm, excluding},
		{"a film placed on no season, the series in a DVD order", unplacedFilm, dvd},
		{"a film offered on season 0, placed on no special, the series in a DVD order", offered, dvd},
	} {
		for _, tc := range []struct {
			name string
			want []judged
			film bool
		}{
			{"the Radarr film holds a file", []judged{{library.ArrRadarr, "", align.OutcomeAligned}}, true},
			{"the Radarr film holds none", []judged{{library.ArrRadarr, "", align.OutcomeNoFile}}, false},
		} {
			t.Run(shape.name+", "+tc.name, func(t *testing.T) {
				got := judgeCopies(t, shape.rec, movie(11, 91, tc.film), shape.series)
				if !slices.Equal(got, tc.want) {
					t.Errorf("copies = %+v, want %+v", got, tc.want)
				}
			})
		}
	}
}
