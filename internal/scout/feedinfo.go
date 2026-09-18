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
			info.TvdbID = rec.TvdbID
			info.Season, info.SeasonKnown = resolvedSeason(&rec)
			it := find(&rec)
			info.Target = arrTarget(it)
			applyMappingList(idx, &rec, it, &info)
			// The item's title is taken only when the item is the entry's own work.
			// FindByID resolves a MOVIE record to the Sonarr series TVDB files it
			// under, and that series is a DIFFERENT work, so a film on a Sonarr item
			// falls through to the memo tier and keeps its own name.
			ownWork := !info.IsMovie || info.Target != indexer.TargetSonarr
			if it != nil && ownWork && strings.TrimSpace(it.Title) != "" {
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

// applyMappingList projects the Anime-Lists mapping-list's two facts onto the
// feed metadata. The entry's TVDB season ranges ride along whatever the target
// (a pack's season token is a per-torrent decision the indexer makes over
// them). The film's special episode is stamped only for the OFFERED class - a
// record whose season scope is the season-0 bucket - resolved to a Sonarr series
// with a title: that is the one shape where the feed serves a second title the
// series' Sonarr can match, and a series node carrying an identically shaped
// row for one of its own specials must not gain a twin on every pack.
func applyMappingList(idx *mapping.Index, rec *mapping.Record, it *library.Item, info *indexer.EntryInfo) {
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
	if m.SpecialEpisode <= 0 || it == nil || info.Target != indexer.TargetSonarr || strings.TrimSpace(it.Title) == "" {
		return
	}
	if kind, _ := align.RecordSeason(rec); kind != align.ScopeOffered {
		return
	}
	info.SpecialEpisode = m.SpecialEpisode
	info.SeriesTitle = it.Title
}

// arrTarget names which arr a resolved library item belongs to, three-valued so
// "not in the library" stays distinguishable from "Radarr". It is what decides
// the feed categories, which the movie flag alone cannot: measured, 131 curated
// MOVIE records carry a tvdb id without resolving to Sonarr against 50 that do.
func arrTarget(it *library.Item) indexer.ArrTarget {
	switch {
	case it == nil:
		return indexer.TargetNone
	case it.Arr == library.ArrRadarr:
		return indexer.TargetRadarr
	default:
		return indexer.TargetSonarr
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

// resolvedSeason resolves the season a Fribb record pins, once, for the feed: its
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
