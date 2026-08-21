package scout

import (
	"strings"

	"github.com/cplieger/seadex-scout/internal/align"
	"github.com/cplieger/seadex-scout/internal/indexer"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/match"
)

// feedEntryInfo builds the per-show metadata closure the indexer's feed writer
// synthesizes RSS titles from. For each AniList id it resolves, in order: the
// arr's OWN title from the PERSISTED library snapshot, keyed through the Fribb
// record's routed ids (the arr parses its own title back, and a blank one counts
// as ABSENT); then the AniList canonical title from the persisted memo, expiry
// ignored; then nothing, leaving the writer its file-name derivation. The Fribb
// movie typing rides along for category routing and the season is RESOLVED here.
// Only persisted state is consulted, so the rebuild stays arr-independent.
func feedEntryInfo(idx *mapping.Index, lib *library.Snapshot, memo match.Memo) indexer.EntryInfoFunc {
	// match.NewLibIndex applies the matcher's arr-consistent ID routing, so a
	// movie whose Fribb record carries a TV themoviedb_id can never take a
	// same-named Sonarr series' title. Failed placeholder items still carry their
	// identity fields, so a partial prior walk keeps supplying titles.
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
		// The memo's typing fills a gap; it never overrides the record's own arr
		// routing. An untyped record that still routes an id routes the SERIES arm,
		// which is evidence of a series - a stale AniList MOVIE format must not
		// send it to Movies/2000, where Sonarr never sees it.
		if !ok || (rec.Type == "" && !rec.HasArrIdentifier()) {
			applyMemoTyping(memo, alID, &info)
		}
		return info
	}
}

// resolvedSeason resolves the season a Fribb record pins, once, for the feed: its
// positive TVDB season, or the specials bucket for a Fribb-typed special (a MAPPED
// season zero - a special IS filed under season 0 by the arrs). An
// absolute-numbered run, a title-only match and an untyped entry pin no season,
// and so does a movie, mirroring align's Radarr-first dispatch. The season rule
// itself is align.RecordSeason, so the feed and the comparison scope cannot
// drift; this exists so the indexer never re-interprets raw Fribb fields.
func applyMemoTyping(memo match.Memo, alID int, info *indexer.EntryInfo) {
	format, hasFormat := memo.StaleFormat(alID)
	if !hasFormat {
		return
	}
	typed := mapping.RecordFromFormat(format)
	info.IsMovie = typed.IsMovie()
	switch {
	case info.IsMovie:
		// A movie pins no season at all, so a season resolved before the memo typed
		// it as a movie must not survive: no consumer may see IsMovie with one.
		info.Season, info.SeasonKnown = 0, false
	case !info.SeasonKnown:
		// A positive Fribb season already resolved by the caller wins: the memo's
		// format can only ever add the specials bucket.
		info.Season, info.SeasonKnown = resolvedSeason(&typed)
	}
}

// applyMemoTyping fills the media typing - and the season that typing implies -
// from the persisted AniList memo. It runs only when Fribb supplied no ARR
// ROUTING EVIDENCE at all: no record, or a record BOTH untyped and id-less. An
// untyped record that still routes a positive TVDB id is itself evidence of a
// series, so the caller's gate keeps it out of here - a memoized format OUTLIVES
// the id-less shape it was fetched for, and re-typing from a stale MOVIE format
// is a routing bug. Without the memo an entry the app KNEW was a movie routed to
// Anime/5070, which Radarr never sees.
func resolvedSeason(rec *mapping.Record) (season int, known bool) {
	if rec.IsMovie() {
		return 0, false
	}
	kind, season := align.RecordSeason(rec)
	return season, kind != align.ScopeWholeSeries
}
