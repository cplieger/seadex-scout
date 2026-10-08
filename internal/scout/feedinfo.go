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
// arr's OWN title from the PERSISTED library snapshot, keyed through the mapping
// record's routed ids (the arr parses its own title back, and a blank one counts
// as ABSENT); then the AniList canonical title from the persisted memo, expiry
// ignored; then nothing, leaving the writer its file-name derivation. The mapping's
// movie typing rides along for category routing and the season is RESOLVED here.
// Only persisted state is consulted, so the rebuild stays arr-independent.
func feedEntryInfo(idx *mapping.Index, lib *library.Snapshot, memo match.Memo) indexer.EntryInfoFunc {
	// match.NewLibIndex applies the matcher's arr-consistent ID routing, so a film
	// carrying its parent series' IMDb id cannot take that Sonarr series' title.
	// Failed placeholder items keep their ids, so a partial walk still supplies titles.
	li := match.NewLibIndex(lib)
	return func(alID int) indexer.EntryInfo {
		var info indexer.EntryInfo
		rec, ok := idx.Lookup(alID)
		if ok {
			info.IsMovie = rec.IsMovie()
			info.TvdbID = rec.TvdbID
			info.Season, info.SeasonKnown = resolvedSeason(&rec)
			it := li.FindByID(&rec)
			applyMappingList(idx, &rec, li, &info)
			// The item's title is taken only when the item is the entry's own work.
			// FindByID resolves a MOVIE record to the Sonarr series TVDB files it
			// under, and that series is a DIFFERENT work, so a film on a Sonarr item
			// falls through to the memo tier and keeps its own name.
			if it != nil && (!info.IsMovie || it.Arr == library.ArrRadarr) && strings.TrimSpace(it.Title) != "" {
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

// applyMappingList projects the mapping facts onto the feed metadata. The
// entry's TVDB season ranges ride along whatever the target (a pack's season
// token is a per-torrent decision the indexer makes over them). The twin's
// season-0 run (mapping.Mapping.SpecialRun) is stamped only for the OFFERED
// class - a record whose season scope is the season-0 bucket - with the titled
// Sonarr series it is filed under, whichever arr FindByID resolved: a series
// node carrying an identically shaped row for one of its own specials must not
// gain a twin on every pack, and a film also in Radarr still needs its twin.
func applyMappingList(idx *mapping.Index, rec *mapping.Record, li *match.LibIndex, info *indexer.EntryInfo) {
	m, mapped := idx.MappingFor(rec)
	if !mapped {
		return
	}
	if len(m.Seasons) > 0 {
		info.Seasons = make([]indexer.SeasonRange, len(m.Seasons))
		for i, r := range m.Seasons {
			info.Seasons[i] = indexer.SeasonRange{Season: r.Season, First: r.First, Last: r.Last}
		}
	}
	run, anidbEpisodes := m.SpecialRun()
	if len(run) == 0 {
		return
	}
	if kind, _ := align.RecordSeason(rec); kind != align.ScopeOffered {
		return
	}
	series := li.SpecialsSeries(rec)
	if series == nil || strings.TrimSpace(series.Title) == "" {
		return
	}
	info.SpecialEpisodes, info.SpecialsEpisodes, info.SeriesTitle = run, anidbEpisodes, series.Title
}

// applyMemoTyping fills the media typing - and the season that typing implies -
// from the persisted AniList memo. It runs only when the mapping supplied no ARR
// ROUTING EVIDENCE at all: no record, or a record BOTH untyped and id-less. An
// untyped record that still routes a positive TVDB id is itself evidence of a
// series, so the caller's gate keeps it out of here - a memoized format OUTLIVES
// the id-less shape it was fetched for, and re-typing from a stale MOVIE format
// is a routing bug. Without the memo an entry the app KNEW was a movie routed to
// Anime/5070, which Radarr never sees.
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
		// A positive mapped season already resolved by the caller wins: the memo's
		// format can only ever add the specials bucket.
		info.Season, info.SeasonKnown = resolvedSeason(&typed)
	}
}

// resolvedSeason resolves the season a mapping record pins, once, for the feed: its
// positive TVDB season, or the specials bucket for a MAPPED season zero. An
// absolute-numbered run, a title-only match and an untyped entry pin no season,
// and so does a MOVIE, for the reason applyMemoTyping states. The rule itself is
// align.RecordSeason, so the feed and the comparison scope cannot drift.
func resolvedSeason(rec *mapping.Record) (season int, known bool) {
	if rec.IsMovie() {
		return 0, false
	}
	kind, season := align.RecordSeason(rec)
	return season, kind != align.ScopeWholeSeries
}
