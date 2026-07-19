package match

import (
	"testing"

	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
)

// TestCatalogueHas covers the reverse-catalogue predicate directly (moved from
// audit alongside the catalogue itself), exercising every id path: the Sonarr
// TVDB match and zero-TVDB short-circuit, and the Radarr TMDB-match plus IMDb
// fallback. audit only ever reaches Has through the TMDB/TVDB paths, so the
// IMDb fallback and the zero-TVDB guard are otherwise untested.
func TestCatalogueHas(t *testing.T) {
	cat := NewCatalogue(mapping.NewIndex([]mapping.Record{
		{AniListID: 1, Type: "TV", TvdbID: 100},
		{AniListID: 2, Type: "MOVIE", TmdbMovies: []int{400}, IMDbIDs: []string{"tt777"}},
		// Wrong-arm identifiers must not be catalogued (the HasArrIdentifier
		// contract): a MOVIE record's stray TVDB id must not recognize a
		// Sonarr item, nor a series record's movie ids a Radarr item.
		{AniListID: 3, Type: "MOVIE", TvdbID: 555},
		{AniListID: 4, Type: "TV", TmdbMovies: []int{600}, IMDbIDs: []string{"tt888"}},
	}), nil)
	tests := []struct {
		name string
		item library.Item
		want bool
	}{
		{"sonarr tvdb matches", library.Item{Arr: library.ArrSonarr, TvdbID: 100}, true},
		{"sonarr tvdb absent", library.Item{Arr: library.ArrSonarr, TvdbID: 999}, false},
		{"sonarr tvdb zero is not catalogued", library.Item{Arr: library.ArrSonarr, TvdbID: 0}, false},
		{"radarr tmdb matches", library.Item{Arr: library.ArrRadarr, TmdbID: 400}, true},
		{"radarr tmdb miss falls through to imdb match", library.Item{Arr: library.ArrRadarr, TmdbID: 401, ImdbID: "tt777"}, true},
		{"radarr imdb only matches", library.Item{Arr: library.ArrRadarr, ImdbID: "tt777"}, true},
		{"radarr neither id matches", library.Item{Arr: library.ArrRadarr, TmdbID: 402, ImdbID: "tt000"}, false},
		{"radarr no ids is not catalogued", library.Item{Arr: library.ArrRadarr}, false},
		{"sonarr not catalogued via a movie record's tvdb id", library.Item{Arr: library.ArrSonarr, TvdbID: 555}, false},
		{"radarr not catalogued via a series record's movie ids", library.Item{Arr: library.ArrRadarr, TmdbID: 600, ImdbID: "tt888"}, false},
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

// TestCatalogueKeep pins the keep predicate: a rejected record contributes no
// IDs, a kept sibling sharing the same TVDB id still catalogues the item, and
// a nil predicate keeps everything. The caller-facing policy this predicate
// carries (audit's exclude_specials symmetry) stays pinned end to end in
// audit's TestAuditNotOnSeaDexHonorsExcludeSpecials.
func TestCatalogueKeep(t *testing.T) {
	records := []mapping.Record{
		{AniListID: 1, Type: "OVA", TvdbID: 500},
		{AniListID: 2, Type: "OVA", TvdbID: 600},
		{AniListID: 3, Type: "TV", TvdbID: 600},
	}
	keepTV := func(r mapping.Record) bool { return r.Type == "TV" }
	cat := NewCatalogue(mapping.NewIndex(records), keepTV)

	ovaOnly := library.Item{Arr: library.ArrSonarr, TvdbID: 500}
	if cat.Has(&ovaOnly) {
		t.Error("an item whose only records are rejected by keep must not be catalogued")
	}
	mixed := library.Item{Arr: library.ArrSonarr, TvdbID: 600}
	if !cat.Has(&mixed) {
		t.Error("a kept sibling record sharing the TVDB id must keep the item catalogued")
	}

	all := NewCatalogue(mapping.NewIndex(records), nil)
	if !all.Has(&ovaOnly) {
		t.Error("a nil keep predicate must catalogue every record")
	}
}
