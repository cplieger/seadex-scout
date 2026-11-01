package align_test

import (
	"testing"

	"github.com/cplieger/seadex-scout/internal/align"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
)

func TestPlacedInSeries(t *testing.T) {
	offered := &mapping.Record{TmdbMovies: []int{1}, TvdbID: 500, SeasonKind: mapping.SeasonPresent}
	offeredOVA := &mapping.Record{TvdbID: 500, SeasonKind: mapping.SeasonPresent}
	season := &mapping.Record{TmdbMovies: []int{1}, TvdbID: 500, SeasonKind: mapping.SeasonPresent, SeasonTvdb: 2}
	wholeTV := &mapping.Record{TvdbID: 500, SeasonKind: mapping.SeasonAbsent}
	wholeFilm := &mapping.Record{TmdbMovies: []int{1}, TvdbID: 500, SeasonKind: mapping.SeasonAbsent}
	own := []mapping.SeasonRange{{Season: 3, First: 1, Last: 1}}
	for _, tc := range []struct {
		name  string
		entry align.Entry
		want  bool
	}{
		{"a film placed on special episodes", align.Entry{Record: offered, Specials: []int{9}}, true},
		{"a film offered on season 0, placed on no episode", align.Entry{Record: offered}, false},
		{"a Sonarr-only entry offered on season 0, placed on no episode", align.Entry{Record: offeredOVA}, false},
		{"a film placed on a season", align.Entry{Record: season}, true},
		{"a film placed on episodes of a season", align.Entry{Record: season, Episodes: []int{1}}, true},
		{"a series-wide entry with no Radarr route and no own season", align.Entry{Record: wholeTV}, true},
		{"an entry with a Radarr route placed on no season", align.Entry{Record: wholeFilm}, false},
		{"a film placed on its own season of the series", align.Entry{Record: wholeFilm, Seasons: own}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := align.PlacedInSeries(&tc.entry); got != tc.want {
				t.Errorf("PlacedInSeries = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHoldsFile(t *testing.T) {
	offered := &mapping.Record{TmdbMovies: []int{1}, TvdbID: 500, SeasonKind: mapping.SeasonPresent}
	season := &mapping.Record{TvdbID: 500, SeasonKind: mapping.SeasonPresent, SeasonTvdb: 2}
	whole := &mapping.Record{TvdbID: 500, SeasonKind: mapping.SeasonAbsent}
	unplaced := &mapping.Record{TmdbMovies: []int{1}, TvdbID: 500, SeasonKind: mapping.SeasonAbsent}
	specials := map[int]library.ListedEpisode{4: {Group: "oz", HasFile: true}, 9: {}}
	listed := map[int]map[int]library.ListedEpisode{2: {1: {}, 2: {Group: "oz", HasFile: true}, 3: {}}}
	sonarr := func(seasons map[int][]string) library.Item {
		return library.Item{Arr: library.ArrSonarr, SeasonGroups: seasons}
	}
	withSpecials := func(it library.Item, eps map[int]library.ListedEpisode) library.Item {
		it.Specials = eps
		return it
	}
	withListed := func(it library.Item) library.Item {
		it.Listed = listed
		return it
	}
	ordered := sonarr(map[int][]string{1: {"oz"}})
	ordered.Order = &library.Order{Type: "dvd"}
	orderedSpecials := withSpecials(sonarr(map[int][]string{0: {"oz"}}), specials)
	orderedSpecials.Order = &library.Order{Type: "dvd"}
	excluded := sonarr(map[int][]string{1: {"oz"}})
	excluded.AniListIDs = []int{999}
	for _, tc := range []struct {
		name  string
		item  library.Item
		entry align.Entry
		want  bool
	}{
		{"a Radarr film with a file", library.Item{Arr: library.ArrRadarr, HasFile: true}, align.Entry{Record: offered, Specials: []int{9}}, true},
		{"a Radarr film with none", library.Item{Arr: library.ArrRadarr}, align.Entry{Record: offered, Specials: []int{9}}, false},
		{"a failed placeholder", library.Item{Arr: library.ArrSonarr, Failed: true}, align.Entry{Record: season}, true},
		{"a Radarr placeholder", library.Item{Arr: library.ArrRadarr, Failed: true}, align.Entry{Record: season}, true},
		{"a Sonarr placeholder the entry is placed on nothing of", library.Item{Arr: library.ArrSonarr, Failed: true}, align.Entry{Record: offered}, true},
		{"a file on a placed special", withSpecials(sonarr(map[int][]string{0: {"oz"}}), map[int]library.ListedEpisode{9: {Group: "mtbb", HasFile: true}}), align.Entry{Record: offered, Specials: []int{9}}, true},
		{"another work on season 0", withSpecials(sonarr(map[int][]string{0: {"oz"}}), specials), align.Entry{Record: offered, Specials: []int{9}}, false},
		{"season-0 episodes unread while season 0 holds a file", sonarr(map[int][]string{0: {"oz"}}), align.Entry{Record: offered, Specials: []int{9}}, true},
		{"season-0 episodes unread and season 0 empty", sonarr(map[int][]string{1: {"oz"}}), align.Entry{Record: offered, Specials: []int{9}}, false},
		{"a season-0 bucket the entry is placed on no episode of", sonarr(map[int][]string{0: {"oz"}}), align.Entry{Record: offered}, false},
		{"a file on the placed season", sonarr(map[int][]string{1: {"oz"}, 2: {"mtbb"}}), align.Entry{Record: season}, true},
		{"files on another season only", sonarr(map[int][]string{1: {"oz"}}), align.Entry{Record: season}, false},
		{"a file on a placed episode of the season", withListed(sonarr(map[int][]string{2: {"oz"}})), align.Entry{Record: season, Episodes: []int{2}}, true},
		{"files on the season's other episodes only", withListed(sonarr(map[int][]string{2: {"oz"}})), align.Entry{Record: season, Episodes: []int{1}}, false},
		{"placed episodes of an unread season with files", sonarr(map[int][]string{2: {"oz"}}), align.Entry{Record: season, Episodes: []int{1}}, true},
		{"a file on the entry's own season of the series", sonarr(map[int][]string{1: {"oz"}, 3: {"mtbb"}}), align.Entry{Record: whole, Seasons: []mapping.SeasonRange{{Season: 3, First: 1, Last: 12}}}, true},
		{"files on other seasons of the series only", sonarr(map[int][]string{1: {"oz"}}), align.Entry{Record: whole, Seasons: []mapping.SeasonRange{{Season: 3, First: 1, Last: 12}}}, false},
		{"files on seasons only siblings claim", sonarr(map[int][]string{1: {"oz"}}), align.Entry{Record: whole, SiblingSeasons: []int{1}}, false},
		{"a series-wide entry with no own season, files on an unclaimed season", sonarr(map[int][]string{1: {"oz"}}), align.Entry{Record: whole, SiblingSeasons: []int{2}}, true},
		{"a film placed on no season, files on an unclaimed season", sonarr(map[int][]string{1: {"oz"}, 2: {"oz"}}), align.Entry{Record: unplaced}, false},
		{"a film placed on its own season of the series", sonarr(map[int][]string{1: {"oz"}, 3: {"mtbb"}}), align.Entry{Record: unplaced, Seasons: []mapping.SeasonRange{{Season: 3, First: 1, Last: 1}}}, true},
		{"a non-aired order with no file on the placed season", ordered, align.Entry{Record: season}, true},
		{"a non-aired order with no file on a placed special", orderedSpecials, align.Entry{Record: offered, Specials: []int{9}}, true},
		{"a non-aired order the entry is placed on nothing of", ordered, align.Entry{Record: offered}, true},
		{"AniList ids excluding the entry, no file on the placed season", excluded, align.Entry{Record: season, AniListID: 7}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := align.HoldsFile(&tc.item, &tc.entry); got != tc.want {
				t.Errorf("HoldsFile = %v, want %v", got, tc.want)
			}
		})
	}
}
