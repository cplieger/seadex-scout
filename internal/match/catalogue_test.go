package match

import (
	"testing"

	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
)

// TestCatalogueHas covers the reverse-catalogue predicate directly, exercising
// every id path: a Sonarr series by its TVDB id, a Radarr film by its TMDB id,
// each only in its own arr, and the zero-id short-circuits.
func TestCatalogueHas(t *testing.T) {
	cat := NewCatalogue(mapping.NewIndex(mapping.Source{Records: []mapping.Record{
		{AniListID: 1, TvdbID: 100},
		{AniListID: 2, TmdbMovies: []int{400}},
		// A film's record with both routes catalogues its series too.
		{AniListID: 5, TvdbID: 700, TmdbMovies: []int{800}, SeasonKind: mapping.SeasonPresent},
	}}), false)
	tests := []struct {
		name string
		item library.Item
		want bool
	}{
		{"sonarr tvdb matches", library.Item{Arr: library.ArrSonarr, TvdbID: 100}, true},
		{"sonarr tvdb absent", library.Item{Arr: library.ArrSonarr, TvdbID: 999}, false},
		{"sonarr tvdb zero is not catalogued", library.Item{Arr: library.ArrSonarr, TvdbID: 0}, false},
		{"radarr tmdb matches", library.Item{Arr: library.ArrRadarr, TmdbID: 400}, true},
		{"radarr tmdb absent", library.Item{Arr: library.ArrRadarr, TmdbID: 402}, false},
		{"radarr no ids is not catalogued", library.Item{Arr: library.ArrRadarr}, false},
		{"radarr catalogued via a both-routes record's tmdb id", library.Item{Arr: library.ArrRadarr, TmdbID: 800}, true},
		{"sonarr catalogued via a both-routes record's tvdb id", library.Item{Arr: library.ArrSonarr, TvdbID: 700}, true},
		{"a radarr item is not catalogued by a tvdb id", library.Item{Arr: library.ArrRadarr, TvdbID: 100}, false},
		{"a sonarr item is not catalogued by a tmdb id", library.Item{Arr: library.ArrSonarr, TmdbID: 400}, false},
		{"unknown arr never matches even a catalogued tvdb id", library.Item{Arr: "lidarr", TvdbID: 100}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			it := tt.item
			if got := cat.Has(&it); got != tt.want {
				t.Errorf("Has() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestCatalogueExcludeSpecials pins the specials policy: a record whose Sonarr
// copy is a special catalogues no TVDB id, a sibling record on the same series
// still does, and the special's TMDB movie id stays catalogued because its
// Radarr copy is never a special. The end-to-end policy is pinned in audit's
// TestAuditNotOnSeaDexHonorsExcludeSpecials.
func TestCatalogueExcludeSpecials(t *testing.T) {
	records := []mapping.Record{
		{AniListID: 1, TvdbID: 500, TmdbMovies: []int{80518}, SeasonKind: mapping.SeasonPresent},
		{AniListID: 2, TvdbID: 600, SeasonKind: mapping.SeasonPresent},
		{AniListID: 3, TvdbID: 600, SeasonKind: mapping.SeasonPresent, SeasonTvdb: 1},
	}
	specialsOnly := library.Item{Arr: library.ArrSonarr, TvdbID: 500}
	mixed := library.Item{Arr: library.ArrSonarr, TvdbID: 600}
	film := library.Item{Arr: library.ArrRadarr, TmdbID: 80518}

	cat := NewCatalogue(mapping.NewIndex(mapping.Source{Records: records}), true)
	if cat.Has(&specialsOnly) {
		t.Error("a series whose only record is a special must not be catalogued with specials excluded")
	}
	if !cat.Has(&mixed) {
		t.Error("a regular-season sibling sharing the TVDB id must keep the series catalogued")
	}
	if !cat.Has(&film) {
		t.Error("a special's TMDB movie id must stay catalogued: its Radarr copy is no special")
	}

	all := NewCatalogue(mapping.NewIndex(mapping.Source{Records: records}), false)
	if !all.Has(&specialsOnly) {
		t.Error("with specials kept, every record's TVDB id must be catalogued")
	}
}
