package scout

import (
	"strings"

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
//     synthesis source. A blank or whitespace-only arr title counts as ABSENT
//     here, matching synthesizeTitle's own trimmed check downstream, so it
//     falls through to the memo instead of suppressing it.
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
			if it := find(&rec); it != nil && strings.TrimSpace(it.Title) != "" {
				info.Title, info.Year = it.Title, it.Year
				return info
			}
		}
		if title, year, titled := memo.StaleTitle(alID); titled {
			info.Title = title
			info.Year = year
		}
		// The memo's typing fills a gap; it never overrides the record's own
		// arr routing. An untyped record that still routes an id routes the
		// SERIES arm (RoutedIDs), which is Fribb/override evidence that the
		// entry is a series - so a stale AniList MOVIE format must not send it
		// to Movies/2000, where Sonarr never sees it.
		if !ok || (rec.Type == "" && !rec.HasArrIdentifier()) {
			applyMemoTyping(memo, alID, &info)
		}
		return info
	}
}

// applyMemoTyping fills the media typing - and the season that typing implies -
// from the persisted AniList memo. It runs only when Fribb supplied no ARR
// ROUTING EVIDENCE at all: no record, or a record that is BOTH untyped and
// id-less (the tolerant Fribb decoder and an override without `type` both yield
// an empty Type; HasArrIdentifier is what rules out a routed id). An untyped
// record that still routes a positive TVDB id through RoutedIDs' series arm is
// itself Fribb/override evidence of a series, so the caller's gate keeps it out
// of here - which matters because a memoized format OUTLIVES the id-less shape
// it was fetched for: pruneExpired deliberately retains an expired memo entry
// for an id-less record that later gained an arr id, and re-typing such a
// record from that stale MOVIE format is the routing bug h-f12 closed.
// Reading the memoized format fixes the case where the app KNEW an entry was a
// movie and still routed its feed item to Anime/5070: Radarr filters on
// Movies/2000, so it never saw that movie in the RSS feed at all (l-f70). The
// typing is only ever taken from the memo when Fribb had nothing to say, so a
// mapped record's typing still wins, and an entry with no memoized format keeps
// the documented unmapped-to-Anime default.
//
// The format is already gated to a real AniList enum member at the client
// boundary (anilist.knownFormat), so an unrecognized upstream token cannot
// route anything here.
func applyMemoTyping(memo match.Memo, alID int, info *indexer.EntryInfo) {
	format, hasFormat := memo.StaleFormat(alID)
	if !hasFormat {
		return
	}
	typed := mapping.RecordFromFormat(format)
	info.IsMovie = typed.IsMovie()
	switch {
	case info.IsMovie:
		// A movie pins no season at all (resolvedSeason's Radarr-first arm),
		// so a season the record resolved before the memo typed it as a
		// movie must not survive: no consumer may see IsMovie with a
		// resolved season.
		info.Season, info.SeasonKnown = 0, false
	case !info.SeasonKnown:
		// A positive Fribb season already resolved by the caller wins: the
		// memo's format can only ever add the specials bucket.
		info.Season, info.SeasonKnown = resolvedSeason(&typed)
	}
}

// resolvedSeason resolves the season a Fribb record pins, once, for the feed:
// its positive TVDB season, or the specials bucket for a Fribb-typed special
// (a MAPPED season zero, not an absent one - a special IS filed under season 0
// by the arrs). An absolute-numbered run, a title-only match, and an entry with
// no Fribb typing at all pin no season.
//
// A movie pins no season at all, mirroring align's Radarr-first scope dispatch:
// a MOVIE-typed record's season.tvdb is not the season the arr files it under
// (Radarr has none), so a broken upstream mapping that carries one must not
// reach a consumer as a resolved season.
//
// It reads the same three Record predicates align's scope resolution dispatches on
// (IsMovie, HasMappedSeason, IsSpecial), and exists so the indexer receives a resolved
// season instead of raw Fribb fields it would have to re-interpret - the
// duplication l-f4 named, in a package that deliberately imports neither align
// nor mapping.
func resolvedSeason(rec *mapping.Record) (season int, known bool) {
	switch {
	case rec.IsMovie():
		return 0, false
	case rec.HasMappedSeason():
		return rec.SeasonTvdb, true
	case rec.IsSpecial():
		return specialSeason, true
	default:
		return 0, false
	}
}
