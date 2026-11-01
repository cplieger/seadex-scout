package match

import (
	"testing"

	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/seadex"
)

var placedFilm = mapping.Record{
	AniListID: 1, AniDBID: 70, TvdbID: 500, TmdbMovies: []int{42},
	SeasonKind: mapping.SeasonPresent,
}

func copiesIndex(rec mapping.Record, specials []int) *mapping.Index {
	return mapping.NewIndex(mapping.Source{Records: []mapping.Record{rec}, Mappings: map[int]mapping.Mapping{rec.AniDBID: {Specials: specials, SpecialsTvdb: rec.TvdbID}}})
}

func radarrFilm(hasFile bool) library.Item {
	it := library.Item{Arr: library.ArrRadarr, ArrID: 3, TmdbID: 42, HasFile: hasFile}
	if hasFile {
		it.Groups = []string{"mtbb"}
	}
	return it
}

func sonarrSeries(fileOnSpecial bool) library.Item {
	it := library.Item{
		Arr: library.ArrSonarr, ArrID: 7, TvdbID: 500,
		SeasonGroups: map[int][]string{0: {"oz"}},
		Specials:     map[int]library.ListedEpisode{4: {Group: "oz", HasFile: true}, 9: {}},
	}
	if fileOnSpecial {
		it.Specials[9] = library.ListedEpisode{Group: "mtbb", HasFile: true}
	}
	return it
}

func matchedArrs(ms []Match) []string {
	var out []string
	for i := range ms {
		if ms[i].InLibrary() {
			out = append(out, ms[i].Item.Arr)
		}
	}
	return out
}

func inLibraryCopy(ms []Match, arr string) *Match {
	for i := range ms {
		if ms[i].InLibrary() && ms[i].Arr == arr {
			return &ms[i]
		}
	}
	return nil
}

func TestMatchJudgesEveryCopyThatHoldsAFile(t *testing.T) {
	for _, tc := range []struct {
		name   string
		want   []string
		radarr bool
		sonarr bool
	}{
		{"both hold a file", []string{library.ArrRadarr, library.ArrSonarr}, true, true},
		{"only the special holds one", []string{library.ArrSonarr}, false, true},
		{"only the film holds one", []string{library.ArrRadarr}, true, false},
		{"neither holds one, the Radarr copy is judged", []string{library.ArrRadarr}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap := &library.Snapshot{Items: []library.Item{radarrFilm(tc.radarr), sonarrSeries(tc.sonarr)}}
			res := New(Config{AniList: &countingAniList{}}).Match(t.Context(), []seadex.Entry{{AniListID: 1}}, snap, copiesIndex(placedFilm, []int{9}), Memo{})
			got := matchedArrs(res.Matches)
			if len(got) != len(tc.want) || len(res.Matches) != len(tc.want) {
				t.Fatalf("Match(%s) arrs = %v over %d matches, want %v", tc.name, got, len(res.Matches), tc.want)
			}
			for i := range got {
				m := res.Matches[i]
				if got[i] != tc.want[i] || m.Arr != tc.want[i] || len(m.Specials) != 1 || m.Specials[0] != 9 {
					t.Errorf("Match(%s)[%d] = arr %q (item %q) specials %v, want %q with specials [9]", tc.name, i, m.Arr, got[i], m.Specials, tc.want[i])
				}
			}
			if res.Coverage.Hits[library.ArrRadarr] != 1 || res.Coverage.Hits[library.ArrSonarr] != 1 {
				t.Errorf("Match(%s) coverage hits = %v, want one hit in each arr", tc.name, res.Coverage.Hits)
			}
		})
	}
}

func TestMatchFindsTheRadarrCopyByItsTMDBMovieID(t *testing.T) {
	season := placedFilm
	season.SeasonTvdb = 1
	seriesOnly := placedFilm
	seriesOnly.TmdbMovies = nil
	otherFilm := placedFilm
	otherFilm.TmdbMovies = []int{99}
	for _, tc := range []struct {
		name     string
		rec      mapping.Record
		specials []int
		want     bool
	}{
		{"placed on season 0", placedFilm, []int{9}, true},
		{"offered on season 0, placed on no episode", placedFilm, nil, true},
		{"filed under a real season", season, []int{9}, true},
		{"no TMDB movie id", seriesOnly, []int{9}, false},
		{"a TMDB movie id Radarr lacks", otherFilm, []int{9}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap := &library.Snapshot{Items: []library.Item{radarrFilm(true)}}
			res := New(Config{AniList: &countingAniList{}}).Match(t.Context(), []seadex.Entry{{AniListID: 1}}, snap, copiesIndex(tc.rec, tc.specials), Memo{})
			if got := inLibraryCopy(res.Matches, library.ArrRadarr) != nil; got != tc.want {
				t.Errorf("Match(%s) = %+v, want the Radarr film matched %v", tc.name, res.Matches, tc.want)
			}
		})
	}
}

func TestMatchJudgesOnlyTheRadarrFilmWhenSonarrHoldsNoPlacedFile(t *testing.T) {
	season := placedFilm
	season.SeasonTvdb = 1
	for _, tc := range []struct {
		name           string
		rec            mapping.Record
		specials       []int
		wantUncompared string
	}{
		{"a film offered on season 0, placed on no episode", placedFilm, nil, ""},
		{"filed under a real season it holds no file on", season, []int{9}, library.ArrSonarr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap := &library.Snapshot{Items: []library.Item{radarrFilm(true), sonarrSeries(true)}}
			res := New(Config{AniList: &countingAniList{}}).Match(t.Context(), []seadex.Entry{{AniListID: 1}}, snap, copiesIndex(tc.rec, tc.specials), Memo{})
			if got := matchedArrs(res.Matches); len(res.Matches) != 1 || len(got) != 1 || got[0] != library.ArrRadarr {
				t.Fatalf("Match(%s) arrs = %v over %d matches, want only the Radarr film", tc.name, got, len(res.Matches))
			}
			uncompared := ""
			if u := res.Matches[0].Uncompared; u != nil {
				uncompared = u.Arr
			}
			if uncompared != tc.wantUncompared {
				t.Errorf("Match(%s).Uncompared arr = %q, want %q", tc.name, uncompared, tc.wantUncompared)
			}
		})
	}
}

func TestMatchDropsThePlacementOfARecordFiledUnderARealSeason(t *testing.T) {
	film := placedFilm
	film.SeasonTvdb = 1
	series := sonarrSeries(false)
	series.SeasonGroups, series.Specials = map[int][]string{0: {"oz"}, 1: {"mtbb"}}, nil
	snap := &library.Snapshot{Items: []library.Item{series}}
	res := New(Config{AniList: &countingAniList{}}).Match(t.Context(), []seadex.Entry{{AniListID: 1}}, snap, copiesIndex(film, []int{9}), Memo{})
	m := inLibraryCopy(res.Matches, library.ArrSonarr)
	if m == nil {
		t.Fatalf("Match(season-1 film) = %+v, want the Sonarr series", res.Matches)
	}
	if m.Specials != nil || m.SpecialsUnknown() {
		t.Errorf("Match(season-1 film) specials %v, SpecialsUnknown %v; want nil, false", m.Specials, m.SpecialsUnknown())
	}
}

func TestMatchLeavesOutAnExcludedSpecialCopy(t *testing.T) {
	sonarrOnly := placedFilm
	sonarrOnly.TmdbMovies = nil
	season := placedFilm
	season.SeasonTvdb = 1
	for _, tc := range []struct {
		name       string
		rec        mapping.Record
		want       []string
		wantHidden bool
	}{
		{"a film in season 0 keeps its Radarr copy", placedFilm, []string{library.ArrRadarr}, true},
		{"a Sonarr-only special leaves no copy", sonarrOnly, nil, true},
		{"a film on a real season is no special", season, []string{library.ArrRadarr, library.ArrSonarr}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			series := sonarrSeries(true)
			series.SeasonGroups[1] = []string{"mtbb"}
			snap := &library.Snapshot{Items: []library.Item{radarrFilm(true), series}}
			res := New(Config{AniList: &countingAniList{}, ExcludeSpecials: true}).Match(t.Context(), []seadex.Entry{{AniListID: 1}}, snap, copiesIndex(tc.rec, []int{9}), Memo{})
			got := matchedArrs(res.Matches)
			if len(got) != len(tc.want) || len(res.Matches) != len(tc.want) {
				t.Fatalf("Match(%s) arrs = %v over %d matches, want %v", tc.name, got, len(res.Matches), tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] || res.Matches[i].Uncompared != nil {
					t.Errorf("Match(%s)[%d] = %q uncompared %v, want %q judged alone", tc.name, i, got[i], res.Matches[i].Uncompared, tc.want[i])
				}
			}
			if _, hidden := res.HiddenIDs[1]; hidden != tc.wantHidden {
				t.Errorf("Match(%s) HiddenIDs = %v, want entry 1 hidden %v", tc.name, res.HiddenIDs, tc.wantHidden)
			}
		})
	}
}

func TestMatchRecordsAnUnplacedSeriesCopyAsHidden(t *testing.T) {
	snap := &library.Snapshot{Items: []library.Item{sonarrSeries(true)}}
	res := New(Config{AniList: &countingAniList{}}).Match(t.Context(), []seadex.Entry{{AniListID: 1}}, snap, copiesIndex(placedFilm, nil), Memo{})
	if got := matchedArrs(res.Matches); len(got) != 0 {
		t.Errorf("Match(unplaced film, absent from Radarr) in-library copies = %v, want none", got)
	}
	if _, hidden := res.HiddenIDs[1]; !hidden {
		t.Errorf("HiddenIDs = %v, want entry 1: its series was in the library and is not its copy", res.HiddenIDs)
	}
	absent := New(Config{AniList: &countingAniList{}}).Match(t.Context(), []seadex.Entry{{AniListID: 1}}, &library.Snapshot{}, copiesIndex(placedFilm, nil), Memo{})
	if len(absent.HiddenIDs) != 0 {
		t.Errorf("HiddenIDs with no series in the library = %v, want none", absent.HiddenIDs)
	}
}

func TestMatchSpecialIsTheSonarrCopyOnSeasonZero(t *testing.T) {
	season := placedFilm
	season.SeasonTvdb = 1
	for _, tc := range []struct {
		name string
		m    Match
		want bool
	}{
		{"the Sonarr copy of a film in season 0", Match{Arr: library.ArrSonarr, Record: placedFilm}, true},
		{"the Radarr copy of the same film", Match{Arr: library.ArrRadarr, Record: placedFilm}, false},
		{"the Sonarr copy of a film on a real season", Match{Arr: library.ArrSonarr, Record: season}, false},
		{"a title-fallback match", Match{Arr: library.ArrSonarr, Source: sourceTitle}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.m.Special(); got != tc.want {
				t.Errorf("Special() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLibIndexFindsEachRouteInItsOwnArr(t *testing.T) {
	film, series := radarrFilm(true), sonarrSeries(false)
	// A Sonarr series carrying the film's TMDB id and a Radarr film carrying
	// the series' TVDB id: neither is the other arr's copy.
	series.TmdbID, film.TvdbID = 42, 500
	li := NewLibIndex(&library.Snapshot{Items: []library.Item{film, series}})
	if got := li.Radarr(&placedFilm); got == nil || got.Arr != library.ArrRadarr || got.ArrID != 3 {
		t.Errorf("Radarr(both-routes film) = %+v, want the Radarr film ArrID 3", got)
	}
	if got := li.Sonarr(&placedFilm); got == nil || got.Arr != library.ArrSonarr || got.ArrID != 7 {
		t.Errorf("Sonarr(both-routes film) = %+v, want the Sonarr series ArrID 7", got)
	}
	none := mapping.Record{AniListID: 1}
	if got := li.Radarr(&none); got != nil {
		t.Errorf("Radarr(no TMDB movie id) = %+v, want nil", got)
	}
	if got := li.Sonarr(&none); got != nil {
		t.Errorf("Sonarr(no TVDB id) = %+v, want nil", got)
	}
	radarrOnly := NewLibIndex(&library.Snapshot{Items: []library.Item{film}})
	if got := radarrOnly.Sonarr(&placedFilm); got != nil {
		t.Errorf("Sonarr(film in Radarr only) = %+v, want nil", got)
	}
}
