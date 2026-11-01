package match

import (
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
)

// Catalogue tells a recognized anime from an arbitrary library entry: the
// mapping's route ids, TVDB series for Sonarr and TMDB movies for Radarr.
type Catalogue struct {
	tvdb map[int]struct{}
	tmdb map[int]struct{}
}

// NewCatalogue builds the reverse ID sets from the mapping records. A nil
// index yields an empty catalogue (nothing is considered catalogued). With
// excludeSpecials a record whose Sonarr copy is a special
// (mapping.Record.SonarrSpecial) catalogues no TVDB id, though another record
// on the same series still does; its TMDB movie ids stay, because a Radarr
// copy is never a special.
func NewCatalogue(idx *mapping.Index, excludeSpecials bool) *Catalogue {
	c := &Catalogue{tvdb: map[int]struct{}{}, tmdb: map[int]struct{}{}}
	idx.ForEachRecord(func(r mapping.Record) {
		if r.SonarrRoute() && (!excludeSpecials || !r.SonarrSpecial()) {
			c.tvdb[r.TvdbID] = struct{}{}
		}
		for _, id := range r.TmdbMovies {
			c.tmdb[id] = struct{}{}
		}
	})
	return c
}

// Has reports whether a library item corresponds to any catalogued route: a
// Radarr movie by its TMDB id, a Sonarr series by its TVDB id.
func (c *Catalogue) Has(it *library.Item) bool {
	switch it.Arr {
	case library.ArrRadarr:
		if it.TmdbID <= 0 {
			return false
		}
		_, ok := c.tmdb[it.TmdbID]
		return ok
	case library.ArrSonarr:
		if it.TvdbID <= 0 {
			return false
		}
		_, ok := c.tvdb[it.TvdbID]
		return ok
	default:
		return false
	}
}
