package scout

import (
	"strings"

	"github.com/cplieger/seadex-scout/internal/align"
	"github.com/cplieger/seadex-scout/internal/indexer"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/match"
)

// feedInfo builds the feed writer's metadata from a library snapshot, the
// order set (NonAiredOrder's answer) and the memo, with no arr call, so the
// rebuild stays arr-independent. Entry resolves an AniList id's title, in
// order: the arr's OWN title, which it parses back, from the snapshot, keyed
// through the record's routed ids (a blank one counts as ABSENT); the memo's
// AniList canonical title, expiry ignored; else none, leaving the file-name
// derivation. The media kind (KindUnknown when neither the record nor the memo
// types the entry) and the RESOLVED season ride along.
func feedInfo(idx *mapping.Index, lib *library.Snapshot, nonAiredTvdbIDs []int, memo match.Memo) indexer.FeedInfo {
	nonAired := nonAiredOrderSet(nonAiredTvdbIDs)
	return indexer.FeedInfo{
		Entry:         entryInfo(idx, lib, nonAired, memo),
		NonAiredOrder: func(tvdbID int) bool { return nonAired[tvdbID] },
	}
}

// entryInfo is feedInfo's per-show half.
func entryInfo(idx *mapping.Index, lib *library.Snapshot, nonAired map[int]bool, memo match.Memo) indexer.EntryInfoFunc {
	// match.NewLibIndex applies the matcher's arr-consistent ID routing, so a film
	// carrying its parent series' IMDb id cannot take that Sonarr series' title.
	// Failed placeholder items keep their ids, so a partial walk still supplies titles.
	li := match.NewLibIndex(lib)
	return func(alID int) indexer.EntryInfo {
		var info indexer.EntryInfo
		rec, ok := idx.Lookup(alID)
		if ok {
			it := applyRecordFacts(idx, &rec, li, nonAired, &info)
			// The item's title is taken only when the item is the entry's own work.
			// FindByID resolves a MOVIE record to the Sonarr series TVDB files it
			// under, and that series is a DIFFERENT work, so a film on a Sonarr item
			// falls through to the memo tier and keeps its own name.
			if it != nil && (info.Kind != indexer.KindMovie || it.Arr == library.ArrRadarr) && strings.TrimSpace(it.Title) != "" {
				info.Title, info.Year = it.Title, it.Year
				return info
			}
		}
		if title, year, titled := memo.StaleTitle(alID); titled {
			info.Title = title
			info.Year = year
		}
		// The memo's typing fills only a gap recordKind leaves: a stale AniList
		// MOVIE format must not send a routed series to Movies/2000, where Sonarr
		// never sees it.
		if info.Kind == indexer.KindUnknown {
			applyMemoTyping(memo, alID, &info)
		}
		return info
	}
}

// nonAiredOrderSet indexes the TVDB ids of the series whose releases get no
// TheTVDB season fact: Sonarr reads a release's season tokens in the series'
// own order with scene mappings cleared (Sonarr develop
// src/NzbDrone.Core/Parser/ParsingService.cs:236-246), so a TVDB season or S00
// special would be imported into whatever that slot holds there.
func nonAiredOrderSet(tvdbIDs []int) map[int]bool {
	set := make(map[int]bool, len(tvdbIDs))
	for _, id := range tvdbIDs {
		set[id] = true
	}
	return set
}

// An entry resolved to a Radarr film is served as one whatever its type, or
// its film-titled release lands in the series categories Radarr never reads.
func applyRecordFacts(idx *mapping.Index, rec *mapping.Record, li *match.LibIndex, nonAired map[int]bool, info *indexer.EntryInfo) *library.Item {
	it := li.FindByID(rec)
	if it == nil {
		it = li.PlacedFilm(idx, rec)
	}
	film := rec.IsMovie() || it != nil && it.Arr == library.ArrRadarr
	info.Kind = recordKind(rec)
	if film {
		info.Kind = indexer.KindMovie
	}
	info.TvdbID = rec.TvdbID
	info.NonAiredOrder = rec.TvdbID > 0 && nonAired[rec.TvdbID]
	if !info.NonAiredOrder {
		if !film {
			info.Season, info.SeasonKnown = resolvedSeason(rec)
		}
		applyEpisodeFacts(idx, rec, film, li, info)
	}
	return it
}

// recordKind is the typing rec carries: its type, else the series arm a
// routed id implies; KindUnknown for an untyped record that routes nothing.
func recordKind(rec *mapping.Record) indexer.MediaKind {
	switch {
	case rec.IsMovie():
		return indexer.KindMovie
	case rec.Type != "" || rec.HasArrIdentifier():
		return indexer.KindSeries
	default:
		return indexer.KindUnknown
	}
}

// applyEpisodeFacts projects the map's episode facts onto the feed metadata.
// The season ranges ride along whatever the target (the indexer picks each
// pack's season token), except for a film, which pins no season. The twin's
// season-0 run is stamped only for the OFFERED class (season scope 0) with the
// titled Sonarr series it is filed under, whichever arr FindByID resolves the
// record to: a film also in Radarr still needs its twin.
func applyEpisodeFacts(idx *mapping.Index, rec *mapping.Record, film bool, li *match.LibIndex, info *indexer.EntryInfo) {
	m, mapped := idx.MappingFor(rec)
	if !mapped {
		return
	}
	if len(m.Seasons) > 0 && !film {
		info.Seasons = make([]indexer.SeasonRange, len(m.Seasons))
		for i, r := range m.Seasons {
			info.Seasons[i] = indexer.SeasonRange{Season: r.Season, First: r.First, Last: r.Last}
		}
	}
	run, entryEpisodes := m.SpecialRun()
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
	info.SpecialEpisodes, info.SpecialsEpisodes, info.SeriesTitle = run, entryEpisodes, series.Title
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
	info.Kind = recordKind(&typed)
	switch {
	case info.Kind == indexer.KindMovie:
		// A movie pins no season at all, so a season resolved before the memo typed
		// it as a movie must not survive: no consumer may see KindMovie with one.
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
