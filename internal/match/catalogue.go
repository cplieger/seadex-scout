package match

import (
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
)

// Catalogue is the reverse (item -> any record) side of the arr-consistent
// ID bridge FindByID resolves forward: the set of TVDB, TMDB-movie, and IMDb
// IDs any kept mapping record references, used to tell a recognized anime
// from an arbitrary library entry. Both directions of the item<->record
// pairing rule (a Radarr item is claimed by a record's routed TMDB-movie/IMDb
// ids or by its cross-type movie ids, a Sonarr item only by its TVDB id) live
// here beside FindByID/findMovie, and both read the same two mapping
// accessors (RoutedIDs for the type-routed set, MovieTMDBIDs for the cross-type
// movie evidence), so a change to the pairing - a new id kind, an id becoming
// valid for the other arr - cannot drift between the forward and reverse
// lookups.
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
		// Insert only the identifiers the record's routed arr consumes
		// (mapping.Record.RoutedIDs): a MOVIE record must not catalogue a
		// Sonarr item through a stray TVDB id, nor a series record a Radarr
		// item through its IMDb id (TVDB reuses a film's IMDb id on the parent
		// series).
		tvdb, _, imdbIDs := r.RoutedIDs()
		// Presence checks over RoutedIDs' already-canonicalized ids: the
		// usability policy's one home is RoutedIDs (l-f37/l-f109), for the id
		// slices as much as for the scalar id.
		if tvdb > 0 {
			c.tvdb[tvdb] = struct{}{}
		} else {
			// No routed series id: the record's unambiguous movie TMDB ids
			// claim a Radarr movie whatever its type label says, mirroring
			// FindByID's secondary movie lookup so the two directions of the
			// bridge cannot drift (l-f73). For a MOVIE record this IS the
			// routed set (RoutedIDs' movie arm returns exactly these ids and
			// zero tvdb), so the two arms stay one rule rather than two.
			for _, id := range r.MovieTMDBIDs() {
				c.tmdb[id] = struct{}{}
			}
		}
		for _, im := range imdbIDs { // RoutedIDs returns only canonical, usable ids
			c.imdb[im] = struct{}{}
		}
	})
	return c
}

// Has reports whether a library item corresponds to any kept mapping record:
// a Radarr movie by its TMDB or IMDb id, a Sonarr series by its TVDB id. The
// switch is exhaustive over the known arr values and answers false for any
// other Arr, mirroring the forward side (indexIDs indexes each ID map with only
// the items of the arr that consumes it), so an unknown or future arr value can
// never be misclassified through the Sonarr TVDB branch.
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
