package match

import (
	"testing"

	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/seadex"
)

var placedFilm = mapping.Record{
	AniListID: 1, AniDBID: 70, Type: "MOVIE", TvdbID: 500, TmdbMovies: []int{42}, IMDbIDs: []string{"tt0000042"},
	SeasonKind: mapping.SeasonPresent,
}

func copiesIndex(rec mapping.Record, specials []int) *mapping.Index {
	return mapping.NewIndex(mapping.Source{Records: []mapping.Record{rec}, Mappings: map[int]mapping.Mapping{rec.AniDBID: {Specials: specials, SpecialsTvdb: rec.TvdbID}}})
}

func radarrFilm(hasFile bool) library.Item {
	it := library.Item{Arr: library.ArrRadarr, ArrID: 3, TmdbID: 42, ImdbID: "tt0000042", HasFile: hasFile}
	if hasFile {
		it.Groups = []string{"mtbb"}
	}
	return it
}

func sonarrSeries(fileOnSpecial bool) library.Item {
	it := library.Item{
		Arr: library.ArrSonarr, ArrID: 7, TvdbID: 500, ImdbID: "tt0000500",
		SeasonGroups: map[int][]string{0: {"oz"}},
		Specials:     map[int]library.SpecialEpisode{4: {Group: "oz", HasFile: true}, 9: {}},
	}
	if fileOnSpecial {
		it.Specials[9] = library.SpecialEpisode{Group: "mtbb", HasFile: true}
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

func TestMatchJudgesEveryCopyThatHoldsAFile(t *testing.T) {
	tvOVA := placedFilm
	tvOVA.Type = "OVA"
	for _, tc := range []struct {
		name   string
		rec    mapping.Record
		radarr bool
		sonarr bool
		want   []string
	}{
		{"both hold a file", placedFilm, true, true, []string{library.ArrRadarr, library.ArrSonarr}},
		{"only the special holds one", placedFilm, false, true, []string{library.ArrSonarr}},
		{"only the film holds one", placedFilm, true, false, []string{library.ArrRadarr}},
		{"neither holds one, a film routes to Radarr", placedFilm, false, false, []string{library.ArrRadarr}},
		{"an OVA held by both", tvOVA, true, true, []string{library.ArrSonarr, library.ArrRadarr}},
		{"an OVA held only in Radarr", tvOVA, true, false, []string{library.ArrRadarr}},
		{"neither holds one, an OVA routes to Sonarr", tvOVA, false, false, []string{library.ArrSonarr}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap := &library.Snapshot{Items: []library.Item{radarrFilm(tc.radarr), sonarrSeries(tc.sonarr)}}
			res := New(&countingAniList{}, nil).Match(t.Context(), []seadex.Entry{{AniListID: 1}}, snap, copiesIndex(tc.rec, []int{9}), Memo{})
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
			if hits := res.Coverage.Hits[library.ArrRadarr] + res.Coverage.Hits[library.ArrSonarr]; hits != 1 {
				t.Errorf("Match(%s) coverage hits = %v, want the entry counted once", tc.name, res.Coverage.Hits)
			}
		})
	}
}

func TestMatchFindsAPlacedOVAHeldOnlyInRadarr(t *testing.T) {
	ova := placedFilm
	ova.Type = "OVA"
	imdbOnly := ova
	imdbOnly.TmdbMovies = nil
	otherIMDb := imdbOnly
	otherIMDb.IMDbIDs = []string{"tt0000099"}
	season := ova
	season.SeasonTvdb = 1
	for _, tc := range []struct {
		name     string
		rec      mapping.Record
		specials []int
		want     bool
	}{
		{"placed", ova, []int{9}, true},
		{"not placed", ova, nil, false},
		{"placed, known only by IMDb id", imdbOnly, []int{9}, true},
		{"placed, known only by an IMDb id Radarr lacks", otherIMDb, []int{9}, false},
		{"placed, filed under a real season", season, []int{9}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap := &library.Snapshot{Items: []library.Item{radarrFilm(true)}}
			res := New(&countingAniList{}, nil).Match(t.Context(), []seadex.Entry{{AniListID: 1}}, snap, copiesIndex(tc.rec, tc.specials), Memo{})
			if len(res.Matches) != 1 {
				t.Fatalf("Match(%s) = %d matches, want 1", tc.name, len(res.Matches))
			}
			m := res.Matches[0]
			if got := m.InLibrary() && m.Arr == library.ArrRadarr; got != tc.want {
				t.Errorf("Match(%s) = in library %v arr %q, want matched in Radarr %v", tc.name, m.InLibrary(), m.Arr, tc.want)
			}
		})
	}
}

func TestMatchCopiesStayWithinThePlacedOfferedClass(t *testing.T) {
	season := placedFilm
	season.SeasonTvdb = 1
	for _, tc := range []struct {
		name     string
		rec      mapping.Record
		specials []int
	}{
		{"not placed", placedFilm, nil},
		{"filed under a real season", season, []int{9}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap := &library.Snapshot{Items: []library.Item{radarrFilm(true), sonarrSeries(true)}}
			res := New(&countingAniList{}, nil).Match(t.Context(), []seadex.Entry{{AniListID: 1}}, snap, copiesIndex(tc.rec, tc.specials), Memo{})
			if got := matchedArrs(res.Matches); len(res.Matches) != 1 || len(got) != 1 || got[0] != library.ArrRadarr {
				t.Errorf("Match(%s) arrs = %v over %d matches, want only the Radarr film", tc.name, got, len(res.Matches))
			}
		})
	}
}

func TestMatchDropsThePlacementOfARecordFiledUnderARealSeason(t *testing.T) {
	ova := placedFilm
	ova.Type, ova.SeasonTvdb = "OVA", 1
	series := sonarrSeries(false)
	series.SeasonGroups, series.Specials = map[int][]string{0: {"oz"}, 1: {"mtbb"}}, nil
	snap := &library.Snapshot{Items: []library.Item{series}}
	res := New(&countingAniList{}, nil).Match(t.Context(), []seadex.Entry{{AniListID: 1}}, snap, copiesIndex(ova, []int{9}), Memo{})
	if len(res.Matches) != 1 || !res.Matches[0].InLibrary() {
		t.Fatalf("Match(season-1 OVA) = %d matches, want the Sonarr series", len(res.Matches))
	}
	if m := res.Matches[0]; m.Specials != nil || m.SpecialsUnknown() {
		t.Errorf("Match(season-1 OVA) specials %v, SpecialsUnknown %v; want nil, false", m.Specials, m.SpecialsUnknown())
	}
}

func TestOtherCopySkipsTheSeriesOwnIMDbID(t *testing.T) {
	series := sonarrSeries(true)
	reused := library.Item{Arr: library.ArrRadarr, ArrID: 4, ImdbID: series.ImdbID, HasFile: true}
	own := library.Item{Arr: library.ArrRadarr, ArrID: 5, ImdbID: "tt0000042", HasFile: true}
	special := mapping.Record{AniDBID: 70, Type: "SPECIAL", TvdbID: 500, IMDbIDs: []string{series.ImdbID}}
	li := NewLibIndex(&library.Snapshot{Items: []library.Item{series, reused}})
	if got := li.otherCopy(&special, &series); got != nil {
		t.Errorf("otherCopy(series' own IMDb id) = %+v, want nil", got)
	}
	special.IMDbIDs = []string{series.ImdbID, "tt0000042"}
	li = NewLibIndex(&library.Snapshot{Items: []library.Item{series, reused, own}})
	if got := li.otherCopy(&special, &series); got == nil || got.ArrID != 5 {
		t.Errorf("otherCopy(series' id, then the film's) = %+v, want the film ArrID 5", got)
	}
}

func TestHoldsCopyReadsUnknownAsHeld(t *testing.T) {
	unread := sonarrSeries(false)
	unread.Specials = nil
	empty := unread
	empty.SeasonGroups = map[int][]string{1: {"oz"}}
	for _, tc := range []struct {
		name string
		it   library.Item
		want bool
	}{
		{"a failed placeholder", library.Item{Arr: library.ArrRadarr, Failed: true}, true},
		{"season-0 episodes unread while season 0 holds a file", unread, true},
		{"season-0 episodes unread and season 0 empty", empty, false},
		{"another work on season 0", sonarrSeries(false), false},
	} {
		if got := holdsCopy(&tc.it, []int{9}); got != tc.want {
			t.Errorf("holdsCopy(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSpecialsSeriesFindsTheSonarrCopyFindByIDHides(t *testing.T) {
	film, series := radarrFilm(true), sonarrSeries(false)
	li := NewLibIndex(&library.Snapshot{Items: []library.Item{film, series}})
	if got := li.FindByID(&placedFilm); got == nil || got.Arr != library.ArrRadarr {
		t.Fatalf("FindByID(both-arr film) = %+v, want the Radarr copy", got)
	}
	if got := li.SpecialsSeries(&placedFilm); got == nil || got.Arr != library.ArrSonarr || got.ArrID != 7 {
		t.Errorf("SpecialsSeries(both-arr film) = %+v, want the Sonarr series ArrID 7", got)
	}
	noTvdb := placedFilm
	noTvdb.TvdbID = 0
	if got := li.SpecialsSeries(&noTvdb); got != nil {
		t.Errorf("SpecialsSeries(no tvdb id) = %+v, want nil", got)
	}
	radarrOnly := NewLibIndex(&library.Snapshot{Items: []library.Item{film}})
	if got := radarrOnly.SpecialsSeries(&placedFilm); got != nil {
		t.Errorf("SpecialsSeries(film in Radarr only) = %+v, want nil", got)
	}
}
