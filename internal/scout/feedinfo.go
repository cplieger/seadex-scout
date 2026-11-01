package scout

import (
	"slices"
	"strings"

	"github.com/cplieger/seadex-scout/internal/align"
	"github.com/cplieger/seadex-scout/internal/indexer"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/match"
	"github.com/cplieger/seadex-scout/internal/mediatype"
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
	// Failed placeholder items keep their ids, so a partial walk still supplies titles.
	li := match.NewLibIndex(lib)
	return func(alID int) indexer.EntryInfo {
		var info indexer.EntryInfo
		rec, ok := idx.Lookup(alID)
		if ok {
			it := applyRecordFacts(idx, &rec, li, nonAired, &info)
			if it != nil && strings.TrimSpace(it.Title) != "" {
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

// applyRecordFacts sets rec's routing facts on info and returns the library
// copy whose title the original item carries: the Radarr film for an entry
// with a Radarr route, else the Sonarr series.
func applyRecordFacts(idx *mapping.Index, rec *mapping.Record, li *match.LibIndex, nonAired map[int]bool, info *indexer.EntryInfo) *library.Item {
	film := rec.RadarrRoute()
	info.Kind = recordKind(rec)
	info.TvdbID = rec.TvdbID
	info.NonAiredOrder = rec.TvdbID > 0 && nonAired[rec.TvdbID]
	if !info.NonAiredOrder {
		if !film {
			info.Season, info.SeasonKnown = resolvedSeason(rec)
		}
		applyEpisodeFacts(idx, rec, film, li, info)
	}
	if film {
		return li.Radarr(rec)
	}
	return li.Sonarr(rec)
}

// recordKind is the kind rec's routes give: a Radarr route is a film, a
// Sonarr route alone a series; KindUnknown for a record that routes nothing.
func recordKind(rec *mapping.Record) indexer.MediaKind {
	switch {
	case rec.RadarrRoute():
		return indexer.KindMovie
	case rec.SonarrRoute():
		return indexer.KindSeries
	default:
		return indexer.KindUnknown
	}
}

// applyEpisodeFacts projects the map's episode facts onto the feed metadata.
// The season ranges ride along for a series (the indexer picks each pack's
// season token); a film pins no season. The Sonarr item's run is stamped,
// with the titled Sonarr series it is filed under, only where the map states
// one: an offered entry's season-0 run, or a film's episodes on its one
// positive season. A Sonarr-only entry on a positive season needs none,
// because its original already carries the series title and season.
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
	series := li.Sonarr(rec)
	if series == nil || strings.TrimSpace(series.Title) == "" {
		return
	}
	switch kind, season := align.RecordSeason(rec); {
	case kind == align.ScopeOffered:
		run, entryEpisodes := m.SpecialRun()
		if len(run) == 0 {
			return
		}
		info.TwinSeason, info.TwinEpisodes, info.TwinEntryEpisodes = 0, run, entryEpisodes
	case film && kind == align.ScopeSeason && len(m.Episodes) > 0:
		info.TwinSeason, info.TwinEpisodes, info.TwinEntryEpisodes = season, slices.Clone(m.Episodes), 0
	default:
		return
	}
	info.SeriesTitle = series.Title
}

// applyMemoTyping types an entry the mapping routes nothing for from the
// persisted AniList memo's format: a stale MOVIE goes to Movies/2000, which
// Radarr reads, and any other format to the series categories. Without a
// route there is no placement, so it pins no season.
func applyMemoTyping(memo match.Memo, alID int, info *indexer.EntryInfo) {
	format, hasFormat := memo.StaleFormat(alID)
	if !hasFormat || strings.TrimSpace(format) == "" {
		return
	}
	info.Kind = indexer.KindSeries
	if mediatype.IsMovie(mediatype.Normalize(format)) {
		// No consumer may see KindMovie with a season.
		info.Kind = indexer.KindMovie
		info.Season, info.SeasonKnown = 0, false
	}
}

// resolvedSeason resolves the season a mapping record pins, once, for the feed:
// its positive TVDB season, or the specials bucket for a MAPPED season zero.
// The rule itself is align.RecordSeason, so the feed and the comparison scope
// cannot drift; the caller withholds it from a film.
func resolvedSeason(rec *mapping.Record) (season int, known bool) {
	kind, season := align.RecordSeason(rec)
	return season, kind != align.ScopeWholeSeries
}
