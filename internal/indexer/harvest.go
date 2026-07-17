package indexer

import (
	"context"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// harvestSearchBudget hard-caps the Prowlarr Torznab queries one rebuild may
// spend harvesting real release titles (each offset page counts as one
// query). The rebuild runs every poll_interval against community-backed
// trackers, so the budget keeps the harvest polite and bounded: over-budget
// shows keep their synthesized titles this cycle and retry next rebuild -
// harvesting is enrichment, never a dependency.
const harvestSearchBudget = 15

// harvestPageSize is the per-query result window requested from Prowlarr and
// the paging stride: a page returning fewer results than this ends the show's
// offset paging (there is nothing older left to reach).
const harvestPageSize = 100

// harvestStats summarizes one rebuild's title harvest for the snapshot log
// line: queries spent, titles matched into the cache, and journal items still
// on a synthesized title afterwards (over budget, unmatched, no query source,
// or no upstream for their tracker).
type harvestStats struct {
	queries int
	matched int
	pending int
}

// harvestGroup is one show's pending harvest work on one tracker: the journal
// keys still lacking a cached real title, queried with a single Torznab search
// (plus offset pages) built from the show's synthesis title source.
type harvestGroup struct {
	scope string
	keys  []string
	alID  int
}

// harvestTitles fetches real release titles for journal items still serving a
// synthesized title: ONE Prowlarr Torznab query per show and tracker (q = the
// show's synthesis title source), matching the returned items back to curated
// torrents by tracker id / info hash - the same identity extraction the search
// curation match uses - and caching each match in titles (torrents are
// immutable, so a title is harvested once, ever). AnimeBytes search is
// series-level (one query returns the show's whole torrent set, validated
// live); Nyaa uses the season form and pages by offset under the indexer's
// default created/desc ordering (see harvestParams). Failures warn and never
// fail the rebuild; a show with no known title, no configured upstream, or no
// remaining budget stays synthetic and retries next cycle.
func (w *FeedWriter) harvestTitles(ctx context.Context, feeds map[string][]item, titles map[string]string, infoFor func(alID int) EntryInfo) (stats harvestStats) {
	defer func() { stats.pending = syntheticCount(feeds, titles) }()
	groups, index := pendingHarvest(feeds, titles, infoFor)
	if len(groups) == 0 || len(w.upstreams) == 0 {
		return stats
	}
	budget := harvestSearchBudget
	for _, g := range groups {
		if ctx.Err() != nil || budget == 0 {
			break
		}
		u := upstreamForScope(w.upstreams, g.scope)
		if u == nil {
			continue // tracker not configured for searches: never queried
		}
		budget = w.harvestShow(ctx, u, g, infoFor(g.alID), index, titles, budget, &stats)
	}
	return stats
}

// harvestShow runs one show's query (plus offset pages while its items remain
// unmatched, full pages keep coming, and budget remains) against its tracker's
// upstream, returning the remaining budget. A query failure warns and ends the
// show's harvest for this rebuild (the next rebuild retries).
func (w *FeedWriter) harvestShow(ctx context.Context, u *upstream, g harvestGroup, meta EntryInfo, index, titles map[string]string, budget int, stats *harvestStats) int {
	params := harvestParams(meta, g.scope)
	for offset := 0; budget > 0 && ctx.Err() == nil; offset += harvestPageSize {
		budget--
		stats.queries++
		page := harvestPage(params, offset)
		results, err := u.search(ctx, page)
		if err != nil {
			if ctx.Err() == nil {
				w.log.Warn("indexer title harvest query failed; keeping synthesized titles",
					"upstream", u.name, "al_id", g.alID, "error", err)
			}
			return budget
		}
		stats.matched += matchHarvest(results, index, titles)
		if !groupPending(g, titles) || len(results) < harvestPageSize {
			return budget
		}
	}
	return budget
}

// pendingHarvest collects the journal items lacking a cached title into
// per-show, per-tracker groups (sorted for deterministic query order) plus a
// global identity index (tracker key and info hash forms) mapping a matched
// Prowlarr result back to the journal key whose title it supplies. Items
// whose show has no synthesis title source are left out: there is nothing to
// query with, and they retry once the library or the AniList memo knows the
// show.
func pendingHarvest(feeds map[string][]item, titles map[string]string, infoFor func(alID int) EntryInfo) (groups []harvestGroup, index map[string]string) {
	type groupKey struct {
		scope string
		alID  int
	}
	byShow := make(map[groupKey][]string)
	index = make(map[string]string)
	for scope, feed := range feeds {
		for i := range feed {
			it := &feed[i]
			if !harvestable(it, titles, infoFor) {
				continue
			}
			gk := groupKey{scope: scope, alID: it.AniListID}
			byShow[gk] = append(byShow[gk], it.Key)
			index[it.Key] = it.Key
			if it.InfoHash != "" {
				index[it.InfoHash] = it.Key
			}
		}
	}
	groups = make([]harvestGroup, 0, len(byShow))
	for k, keys := range byShow {
		groups = append(groups, harvestGroup{keys: keys, scope: k.scope, alID: k.alID})
	}
	slices.SortFunc(groups, func(a, b harvestGroup) int {
		if a.scope != b.scope {
			return strings.Compare(a.scope, b.scope)
		}
		return a.alID - b.alID
	})
	return groups, index
}

// harvestable reports whether a journal item is due a harvest query: it still
// serves a synthesized title, carries its journal bookkeeping, and its show
// has a title source to query with.
func harvestable(it *item, titles map[string]string, infoFor func(alID int) EntryInfo) bool {
	if it.Key == "" || it.AniListID <= 0 {
		return false
	}
	if _, done := titles[it.Key]; done {
		return false
	}
	return strings.TrimSpace(infoFor(it.AniListID).Title) != ""
}

// harvestParams builds the one Torznab query for a show on a tracker, from the
// show's synthesis title source. AnimeBytes search is series-level - a plain
// q returns the show's whole torrent set - so a basic search suffices. Nyaa is
// a flat search, so a mapped season uses the season form (q + season): the
// season token surfaces both packs (named "... S01 ...") and SxxExx-named
// episodes (S01 prefixes S01E07), which is what SeaDex curates; offset paging
// under the indexer's default created/desc ordering then reaches older items.
func harvestParams(meta EntryInfo, scope string) url.Values {
	q := url.Values{"t": {"search"}, "q": {strings.TrimSpace(meta.Title)}}
	if scope == upstreamNyaa && !meta.IsMovie && meta.SeasonTvdb > 0 {
		q.Set("t", "tvsearch")
		q.Set("season", strconv.Itoa(meta.SeasonTvdb))
	}
	return q
}

// harvestPage clones the show query with the paging window applied.
func harvestPage(params url.Values, offset int) url.Values {
	page := url.Values{}
	maps.Copy(page, params)
	page.Set("limit", strconv.Itoa(harvestPageSize))
	if offset > 0 {
		page.Set("offset", strconv.Itoa(offset))
	}
	return page
}

// matchHarvest matches one page of Prowlarr results back to pending journal
// items by every identity the result carries - the tracker id parsed from its
// page URLs (comments/guid, the same numeric-validated extraction the search
// curation match uses) and its info hash - caching each matched real title.
// An already-cached key is never overwritten: torrents are immutable, so the
// first harvested title stands.
func matchHarvest(results []item, index, titles map[string]string) int {
	n := 0
	for i := range results {
		title := strings.TrimSpace(results[i].Title)
		if title == "" {
			continue
		}
		for _, id := range harvestIdentity(&results[i]) {
			key, ok := index[id]
			if !ok {
				continue
			}
			if _, done := titles[key]; done {
				continue
			}
			titles[key] = title
			n++
		}
	}
	return n
}

// harvestIdentity returns the identity forms a Prowlarr result can be matched
// under: tracker keys parsed from its page URLs and its (already validated)
// info hash.
func harvestIdentity(it *item) []string {
	var ids []string
	if k := trackerKeyFromURL(it.InfoURL); k != "" {
		ids = append(ids, k)
	}
	if k := trackerKeyFromURL(it.GUID); k != "" {
		ids = append(ids, k)
	}
	if it.InfoHash != "" {
		ids = append(ids, it.InfoHash)
	}
	return ids
}

// groupPending reports whether any of the group's journal keys still lacks a
// cached title (more paging could still help).
func groupPending(g harvestGroup, titles map[string]string) bool {
	for _, k := range g.keys {
		if _, ok := titles[k]; !ok {
			return true
		}
	}
	return false
}

// syntheticCount totals the journal items across all feeds still serving a
// synthesized title (no cached harvested title), for the snapshot log line -
// whatever the reason: over budget, unmatched, no query source, or no
// configured upstream for their tracker.
func syntheticCount(feeds map[string][]item, titles map[string]string) int {
	n := 0
	for _, feed := range feeds {
		for i := range feed {
			if feed[i].Key == "" {
				continue
			}
			if _, ok := titles[feed[i].Key]; !ok {
				n++
			}
		}
	}
	return n
}
