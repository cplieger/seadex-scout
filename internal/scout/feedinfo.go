package scout

import (
	"github.com/cplieger/seadex-scout/internal/indexer"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/match"
)

// feedEntryInfo builds the per-show metadata closure the indexer's feed writer
// synthesizes RSS titles from. For each AniList id it resolves, in order:
//
//  1. The arr's OWN series/movie title from the PERSISTED library snapshot,
//     keyed through the Fribb record's routed ids (a series record's TVDB id
//     against Sonarr items, a movie record's TMDB-movie/IMDb ids against
//     Radarr items - the same arr-consistent routing the matcher uses). The
//     arr is guaranteed to parse its own title back, so this is the strongest
//     synthesis source.
//  2. The AniList canonical title (romaji-first, the memo's title order) from
//     the persisted AniList memo. Expiry is deliberately ignored: the memo's
//     expiry governs re-fetch cadence, and a stale show title still beats a
//     file-name derivation.
//  3. Neither: a zero title, which the writer resolves with its file-name
//     derivation (the permanent last resort).
//
// The Fribb typing (movie/special) and the mapped TVDB season ride along for
// category routing and the season marker. Only persisted state is consulted -
// never this cycle's walk - so the feed rebuild stays arr-independent.
func feedEntryInfo(idx *mapping.Index, lib *library.Snapshot, memo match.Memo) func(alID int) indexer.EntryInfo {
	// match.NewLibIndex applies the matcher's arr-consistent ID routing (a
	// series record resolves only against Sonarr items by TVDB, a movie record
	// only against Radarr items by TMDB movie then IMDb), so a movie whose
	// Fribb record carries a TV themoviedb_id can never take a same-named
	// Sonarr series' title. Failed placeholder items still carry their
	// identity fields (title included), so a partial prior walk keeps
	// supplying titles.
	find := match.NewLibIndex(lib).FindByID
	return func(alID int) indexer.EntryInfo {
		var info indexer.EntryInfo
		rec, ok := idx.Lookup(alID)
		if ok {
			info.IsMovie = rec.IsMovie()
			info.IsSpecial = rec.IsSpecial()
			info.SeasonTvdb = rec.SeasonTvdb
			if it := find(&rec); it != nil {
				info.Title, info.Year = it.Title, it.Year
				return info
			}
		}
		if ent, cached := memo.Entries[alID]; cached && !ent.NotFound && len(ent.Titles) > 0 {
			info.Title = ent.Titles[0]
			info.Year = ent.Year
		}
		return info
	}
}
