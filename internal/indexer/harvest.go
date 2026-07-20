package indexer

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// --- Budget, paging, and stats ---

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

// --- Harvest orchestration ---

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
// remaining budget stays synthetic and retries next cycle. A SCOPE-WIDE query
// failure (status/transport - see harvestShow) skips the scope's remaining
// shows this rebuild, while a show-local malformed response only skips that
// show, so one poison result set cannot freeze an otherwise healthy tracker's
// whole harvest on synthesized titles; a run of consecutiveMalformedLatch
// malformed shows — or of consecutiveRejectedLatch request-scoped rejections
// — on one scope latches it scope-wide anyway, since systematic 2xx garbage
// (e.g. a proxy answering HTML to everything) or an upstream deterministically
// rejecting every query shape is upstream-wide breakage that would otherwise
// burn the whole budget with zero progress.
func (w *FeedWriter) harvestTitles(ctx context.Context, feeds map[string][]item, titles map[string]string, infoFor func(alID int) EntryInfo) (stats harvestStats) {
	defer func() { stats.pending = syntheticCount(feeds, titles) }()
	groups, index := pendingHarvest(feeds, titles, infoFor)
	if len(groups) == 0 || len(w.upstreams) == 0 {
		return stats
	}
	budget := harvestSearchBudget
	failed := make(map[string]bool, len(w.upstreams))
	malformed := make(map[string]int, len(w.upstreams))
	rejected := make(map[string]int, len(w.upstreams))
	for _, g := range groups {
		if ctx.Err() != nil || budget == 0 {
			break
		}
		if !groupPending(g, titles) {
			// An earlier page already titled this group's items
			// opportunistically (matchHarvest matches the global index);
			// spend no query on a satisfied group.
			w.log.Debug("indexer title harvest group already satisfied; skipping query",
				"upstream", g.scope, "al_id", g.alID, "items", len(g.keys))
			continue
		}
		u := availableHarvestUpstream(w.upstreams, failed, g.scope)
		if u == nil {
			continue
		}
		var outcome harvestOutcome
		budget, outcome = w.harvestShow(ctx, u, g, infoFor(g.alID), index, titles, budget, &stats)
		w.updateHarvestScopeState(g.scope, outcome, failed, malformed, rejected)
	}
	return stats
}

// updateHarvestScopeState applies one queried show's outcome to the per-scope
// failure latch and the two consecutive-run counters: harvestScopeFailed
// latches the scope, harvestShowMalformed counts toward
// consecutiveMalformedLatch (latching the scope when the run trips it), a
// show-local request rejection (harvestShowFailed) resets the malformed run
// but counts toward its own consecutiveRejectedLatch (latching the scope on a
// run of systematic rejections), and any other outcome - a success - resets
// both runs.
func (w *FeedWriter) updateHarvestScopeState(scope string, outcome harvestOutcome, failed map[string]bool, malformed, rejected map[string]int) {
	switch outcome {
	case harvestScopeFailed:
		failed[scope] = true
	case harvestShowMalformed:
		rejected[scope] = 0
		malformed[scope]++
		if malformed[scope] >= consecutiveMalformedLatch {
			w.log.Warn("indexer title harvest: repeated malformed responses; skipping this upstream's remaining shows this rebuild",
				"upstream", scope, "consecutive", malformed[scope])
			failed[scope] = true
		}
	case harvestShowFailed:
		// A request-scoped rejection is a definitive upstream answer for
		// ONE show (reset the malformed run), but a consecutive RUN of
		// rejections is the signature of an upstream deterministically
		// rejecting this app's query shape - latch it like systematic
		// malformed bodies, or the whole budget burns with zero progress
		// on every rebuild.
		malformed[scope] = 0
		rejected[scope]++
		if rejected[scope] >= consecutiveRejectedLatch {
			w.log.Warn("indexer title harvest: repeated request rejections; skipping this upstream's remaining shows this rebuild",
				"upstream", scope, "consecutive", rejected[scope])
			failed[scope] = true
		}
	default:
		malformed[scope] = 0
		rejected[scope] = 0
	}
}

// availableHarvestUpstream returns the upstream serving scope, or nil when
// the scope's upstream already failed this rebuild (keep synthesized titles,
// retry next cycle) or the tracker is not configured for searches (never
// queried).
func availableHarvestUpstream(upstreams []*upstream, failed map[string]bool, scope string) *upstream {
	if failed[scope] {
		return nil
	}
	return upstreamForScope(upstreams, scope)
}

// --- Failure classification ---

// harvestOutcome classifies how one show's harvest ended, deciding what
// harvestTitles latches for the show's scope: harvestScopeFailed condemns the
// whole scope this rebuild, harvestShowMalformed counts toward the
// consecutive-malformed latch, harvestShowFailed ends only that show's
// harvest (a request-scoped Torznab rejection - the upstream answered, so it
// resets the malformed run but counts toward the consecutive-rejected
// latch), and harvestOK resets both runs.
type harvestOutcome int

const (
	harvestOK harvestOutcome = iota
	harvestScopeFailed
	harvestShowMalformed
	harvestShowFailed
)

// requestScopedHarvestError reports whether err is a Torznab <error> document
// naming a request/parameter failure (Newznab codes 200-299): the upstream
// deliberately rejected THIS show's query, so the failure is show-local -
// terminal for the show (retrying the same invalid request cannot help, which
// is why terminalTorznabCode already fails it fast) but never evidence the
// upstream itself is down, so one rejection stays show-local (a consecutive
// run of them may still trip consecutiveRejectedLatch and latch the scope).
// Auth/account codes (100-199) stay scope-wide: bad credentials fail every
// show's query identically.
func requestScopedHarvestError(err error) bool {
	docErr, ok := errors.AsType[*upstreamDocError](err)
	if !ok {
		return false
	}
	code, parseErr := strconv.Atoi(docErr.code)
	return parseErr == nil && code >= 200 && code < 300
}

// consecutiveMalformedLatch is how many CONSECUTIVE shows on one scope may
// fail with a persistently malformed 2xx body before the scope is treated as
// upstream-wide broken (e.g. a reverse proxy answering an HTML error page to
// every request) and its remaining shows are skipped this rebuild. One poison
// result set stays show-local; a show whose harvest ends without a malformed
// page - a success (even an empty one) or a request-scoped rejection - resets
// the run. The reset is per show outcome, not per page: a show whose LATER
// offset page is malformed after a successful first page still counts toward
// the latch.
const consecutiveMalformedLatch = 3

// consecutiveRejectedLatch is how many CONSECUTIVE shows on one scope may
// fail with a request-scoped Torznab rejection (codes 200-299) before the
// scope is treated as systematically rejecting this app's query shape (e.g.
// an indexer definition without tvsearch caps answering 203 to every
// season-form query) and its remaining shows are skipped this rebuild. One
// rejected query stays show-local; a show whose harvest ends without a
// request-scoped rejection - a success (even an empty one) or a malformed
// show - resets the run.
const consecutiveRejectedLatch = 3

// harvestShow runs one show's query (plus offset pages while its items remain
// unmatched, full pages keep coming, and budget remains) against its tracker's
// upstream, returning the remaining budget. A query failure warns and ends the
// show's harvest for this rebuild (the next rebuild retries). Failures are
// classified before condemning the whole scope: a SCOPE-WIDE failure
// (429/5xx, an auth/config status, a transport error - the upstream is likely
// down or refusing service) reports harvestScopeFailed so the caller skips the
// scope's remaining groups this rebuild, while a persistently malformed
// SUCCESSFUL body (malformedUpstreamBody) is specific to this one show's
// result set and reports harvestShowMalformed, so the scope's other shows are
// still harvested within the remaining budget instead of one poison response
// freezing the whole tracker on synthesized titles indefinitely - unless a
// RUN of malformed shows trips the caller's consecutiveMalformedLatch, the
// signature of an upstream answering 2xx garbage to everything. A Torznab
// <error> document naming a request/parameter code (200-299) is likewise
// show-local (requestScopedHarvestError -> harvestShowFailed): the upstream
// deliberately rejected this one show's query, so its siblings' valid queries
// still run — unless a run of rejections trips the caller's
// consecutiveRejectedLatch.
func (w *FeedWriter) harvestShow(ctx context.Context, u *upstream, g harvestGroup, meta EntryInfo, index, titles map[string]string, budget int, stats *harvestStats) (int, harvestOutcome) {
	params := harvestParams(meta, g.scope)
	for offset := 0; budget > 0 && ctx.Err() == nil; offset += harvestPageSize {
		budget--
		stats.queries++
		page := harvestPage(params, offset)
		results, err := u.search(ctx, page)
		if err != nil {
			if ctx.Err() != nil {
				return budget, harvestScopeFailed
			}
			return budget, w.classifyHarvestError(err, u, g.alID)
		}
		stats.matched += matchHarvest(results, g.scope, index, titles)
		if !groupPending(g, titles) || len(results) < harvestPageSize {
			return budget, harvestOK
		}
	}
	return budget, harvestOK
}

// classifyHarvestError warns about one show's failed (non-cancelled) harvest
// query and maps it to the outcome harvestTitles latches: a persistently
// malformed SUCCESSFUL body stays show-local (harvestShowMalformed, counted
// toward the consecutive-malformed latch), a request-scoped Torznab rejection
// (codes 200-299) stays show-local and counts toward the consecutive-rejected
// latch (harvestShowFailed), and
// anything else - a status/transport/auth failure - condemns the scope
// (harvestScopeFailed).
func (w *FeedWriter) classifyHarvestError(err error, u *upstream, alID int) harvestOutcome {
	if malformedUpstreamBody(err) {
		w.log.Warn("indexer title harvest response malformed; show keeps its synthesized title this rebuild",
			"upstream", u.name, "al_id", alID, "error", err)
		return harvestShowMalformed
	}
	if requestScopedHarvestError(err) {
		w.log.Warn("indexer title harvest request rejected; show keeps its synthesized title this rebuild",
			"upstream", u.name, "al_id", alID, "error", err)
		return harvestShowFailed
	}
	w.log.Warn("indexer title harvest query failed; skipping this upstream's remaining shows this rebuild",
		"upstream", u.name, "al_id", alID, "error", err)
	return harvestScopeFailed
}

// --- Pending-group collection ---

// harvestGroupKey identifies one show's pending harvest group on one
// tracker: the per-show, per-tracker bucket pendingHarvest collects journal
// keys into before materializing the sorted harvestGroup list.
type harvestGroupKey struct {
	scope string
	alID  int
}

// indexHarvestItem records one harvestable journal item: it appends the
// item's key to its show's per-tracker group and registers the item's
// identity forms (tracker key and info hash) in the global index that maps a
// matched Prowlarr result back to the journal key whose title it supplies.
// A non-harvestable item is left out (see harvestable).
func indexHarvestItem(it *item, scope string, titles map[string]string, infoFor func(int) EntryInfo, byShow map[harvestGroupKey][]string, index map[string]string) {
	if !harvestable(it, titles, infoFor) {
		return
	}
	key := harvestGroupKey{scope: scope, alID: it.AniListID}
	byShow[key] = append(byShow[key], it.Key)
	index[it.Key] = it.Key
	if it.InfoHash != "" {
		index[it.InfoHash] = it.Key
	}
}

// compareHarvestGroups orders harvest groups by tracker scope then AniList ID
// for deterministic query order; cmp.Compare avoids the overflow a plain int
// subtraction could hit on extreme untrusted AniList IDs.
func compareHarvestGroups(a, b harvestGroup) int {
	if c := strings.Compare(a.scope, b.scope); c != 0 {
		return c
	}
	return cmp.Compare(a.alID, b.alID)
}

// pendingHarvest collects the journal items lacking a cached title into
// per-show, per-tracker groups (sorted for deterministic query order) plus a
// global identity index (tracker key and info hash forms) mapping a matched
// Prowlarr result back to the journal key whose title it supplies. Items
// whose show has no synthesis title source are left out: there is nothing to
// query with, and they retry once the library or the AniList memo knows the
// show.
func pendingHarvest(feeds map[string][]item, titles map[string]string, infoFor func(alID int) EntryInfo) (groups []harvestGroup, index map[string]string) {
	byShow := make(map[harvestGroupKey][]string)
	index = make(map[string]string)
	for scope, feed := range feeds {
		for i := range feed {
			indexHarvestItem(&feed[i], scope, titles, infoFor, byShow, index)
		}
	}
	groups = make([]harvestGroup, 0, len(byShow))
	for k, keys := range byShow {
		groups = append(groups, harvestGroup{keys: keys, scope: k.scope, alID: k.alID})
	}
	slices.SortFunc(groups, compareHarvestGroups)
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

// --- Query building ---

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
	page := maps.Clone(params)
	page.Set("limit", strconv.Itoa(harvestPageSize))
	if offset > 0 {
		page.Set("offset", strconv.Itoa(offset))
	}
	return page
}

// --- Result matching ---

// harvestMaxTitleLen bounds a cached harvested title: real tracker release
// titles are well under this, and the titles map is persisted verbatim into
// the snapshot and rendered into every RSS response, so an oversized title
// from a tampered/garbled upstream body must never enter the cache.
const harvestMaxTitleLen = 512

// matchHarvest matches one page of Prowlarr results back to pending journal
// items by the single journal key each result's identity signals agree on -
// the tracker id parsed from its page URLs (comments/guid, the same
// numeric-validated extraction the search curation match uses) and its info
// hash; contradictory signals fail closed and title nothing - caching each
// matched real title. A resolved key must belong to the queried tracker's
// scope: a result from one upstream must never title the other tracker's
// journal item (the same scope binding the search curation match applies in
// acceptScopedKeys). An already-cached key is never overwritten: torrents
// are immutable, so the first harvested title stands.
func matchHarvest(results []item, scope string, index, titles map[string]string) int {
	n := 0
	for i := range results {
		title := strings.TrimSpace(results[i].Title)
		if title == "" || len(title) > harvestMaxTitleLen {
			continue
		}
		key := resolveHarvestKey(&results[i], index)
		if key == "" || !strings.HasPrefix(key, scope+":") {
			continue
		}
		if _, done := titles[key]; done {
			continue
		}
		titles[key] = title
		n++
	}
	return n
}

// resolveHarvestKey resolves a Prowlarr result to the single journal key its
// identity signals - the tracker keys parsed from its page URLs and its
// (already validated) info hash - agree on. It fails closed - returning "" -
// when the keys parsed from the result's two page URLs name different
// releases, or when two signals resolve to different journal items: a healthy
// Prowlarr emits one consistent identity per item, so a contradictory result
// is an untrusted response that must not title anything (the same fail-closed
// rule the search curation match applies in acceptScopedKeys).
func resolveHarvestKey(it *item, index map[string]string) string {
	kc, kg := trackerKeyFromURL(it.InfoURL), trackerKeyFromURL(it.GUID)
	if kc != "" && kg != "" && kc != kg {
		return ""
	}
	var key string
	for _, id := range []string{kc, kg, it.InfoHash} {
		if id == "" {
			continue
		}
		k, ok := index[id]
		if !ok {
			continue
		}
		if key != "" && k != key {
			return ""
		}
		key = k
	}
	return key
}

// --- Pending accounting ---

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
