package scout

import (
	"github.com/cplieger/seadex-scout/internal/indexer"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/match"
)

// specialSeason is the TVDB season number the arrs file specials under, the
// season a Fribb-typed special resolves to. align has its own copy for the
// comparison scope; this one is the feed's, and both are the same immutable
// TVDB fact rather than a policy either package could change.
const specialSeason = 0

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
// The Fribb movie typing rides along for category routing, and the entry's
// season is RESOLVED here (see resolvedSeason) rather than projected as raw
// Fribb fields, so the indexer never re-interprets Fribb season semantics.
// Only persisted state is consulted - never this cycle's walk - so the feed
// rebuild stays arr-independent.
func feedEntryInfo(idx *mapping.Index, lib *library.Snapshot, memo match.Memo) indexer.EntryInfoFunc {
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
			info.Season, info.SeasonKnown = resolvedSeason(&rec)
			if it := find(&rec); it != nil && it.Title != "" {
				info.Title, info.Year = it.Title, it.Year
				return info
			}
		}
		if title, year, titled := memo.StaleTitle(alID); titled {
			info.Title = title
			info.Year = year
		}
		if !ok {
			// No Fribb record, so no Fribb typing - but the AniList memo may
			// carry the entry's own media format, which the matcher itself
			// routes on (match.formatArr). Reading it here fixes the case where
			// the app KNEW an unmapped entry was a movie and still routed its
			// feed item to Anime/5070: Radarr filters on Movies/2000, so it
			// never saw that movie in the RSS feed at all (l-f70). The typing is
			// only ever taken from the memo when Fribb had nothing to say, so a
			// mapped record's typing still wins, and an entry with no memoized
			// format keeps the documented unmapped-to-Anime default.
			//
			// The format is already gated to a real AniList enum member at the
			// client boundary (anilist.knownFormat), so an unrecognized upstream
			// token cannot route anything here.
			if format, hasFormat := memo.StaleFormat(alID); hasFormat {
				typed := mapping.Record{Type: mapping.NormalizeType(format)}
				info.IsMovie = typed.IsMovie()
				info.Season, info.SeasonKnown = resolvedSeason(&typed)
			}
		}
		return info
	}
}

// resolvedSeason resolves the season a Fribb record pins, once, for the feed:
// its positive TVDB season, or the specials bucket for a Fribb-typed special
// (a MAPPED season zero, not an absent one - a special IS filed under season 0
// by the arrs). An absolute-numbered run, a title-only match, and an entry with
// no Fribb typing at all pin no season.
//
// It reads the same two Record predicates align.Scope dispatches on
// (HasMappedSeason, IsSpecial), and exists so the indexer receives a resolved
// season instead of raw Fribb fields it would have to re-interpret - the
// duplication l-f4 named, in a package that deliberately imports neither align
// nor mapping.
func resolvedSeason(rec *mapping.Record) (season int, known bool) {
	switch {
	case rec.HasMappedSeason():
		return rec.SeasonTvdb, true
	case rec.IsSpecial():
		return specialSeason, true
	default:
		return 0, false
	}
}
