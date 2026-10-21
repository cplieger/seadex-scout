package match

import (
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
)

// Catalogue tells a recognized anime from an arbitrary library entry: the
// kept records' routed TVDB and IMDb ids and their TMDB movie ids, whatever
// their type.
type Catalogue struct {
	tvdb map[int]struct{}
	tmdb map[int]struct{}
	imdb map[string]struct{}
}

// NewCatalogue builds the reverse ID sets from the mapping records. A nil
// index yields an empty catalogue (nothing is considered catalogued). keep
// filters which records are catalogued (nil keeps all): a record rejected by
// keep contributes no IDs, but another kept record sharing an ID (e.g. a TV
// sibling of an excluded special on the same TVDB id) still catalogues it.
func NewCatalogue(idx *mapping.Index, keep func(mapping.Record) bool) *Catalogue {
	c := &Catalogue{tvdb: map[int]struct{}{}, tmdb: map[int]struct{}{}, imdb: map[string]struct{}{}}
	idx.ForEachRecord(func(r mapping.Record) {
		if keep != nil && !keep(r) {
			return
		}
		// A MOVIE record's TVDB id names its parent series, and TVDB reuses a
		// film's IMDb id on that series, so those ids come only from RoutedIDs;
		// a TMDB movie id is a Radarr film whatever the type, as in FindByID.
		tvdb, _, imdbIDs := r.RoutedIDs()
		if tvdb > 0 {
			c.tvdb[tvdb] = struct{}{}
		}
		for _, id := range r.TmdbMovies {
			c.tmdb[id] = struct{}{}
		}
		for _, im := range imdbIDs { // RoutedIDs returns only canonical, usable ids
			c.imdb[im] = struct{}{}
		}
	})
	return c
}

// Has reports whether a library item corresponds to any kept mapping record:
// a Radarr movie by its TMDB or IMDb id, a Sonarr series by its TVDB id.
func (c *Catalogue) Has(it *library.Item) bool {
	switch it.Arr {
	case library.ArrRadarr:
		if it.TmdbID > 0 {
			if _, ok := c.tmdb[it.TmdbID]; ok {
				return true
			}
		}
		if key := imdbKey(it.ImdbID); key != "" {
			if _, ok := c.imdb[key]; ok {
				return true
			}
		}
		return false
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
