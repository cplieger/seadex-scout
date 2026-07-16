// Package scout orchestrates one compare cycle: load state, walk the library,
// refresh the ID map, pull SeaDex, match entries to library items, compare, and
// report findings, then persist the caches.
//
// Cycle health follows the library ingest: a failed arr walk is unhealthy (a
// restart or config fix could recover it), while a SeaDex, mapping, or AniList
// failure is degraded but healthy (a restart cannot fix an upstream outage) and
// leaves prior findings untouched rather than falsely resolving them.
package scout

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/cplieger/seadex-scout/internal/audit"
	"github.com/cplieger/seadex-scout/internal/compare"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/match"
	"github.com/cplieger/seadex-scout/internal/report"
	"github.com/cplieger/seadex-scout/internal/seadex"
	"github.com/cplieger/seadex-scout/internal/state"
)

// FeedWriter rebuilds and persists the indexer's Torznab feed from the cycle's
// shared SeaDex snapshot, so the findings and the RSS feed the arrs grab from
// are produced by one data engine from a single fetch. The indexer's feed writer
// implements it; Deps.Feed is nil when no Torznab feed is configured and the
// cycle then does no feed work. Because Rebuild persists the feed, a cycle run
// by the `poll` subcommand refreshes a resident daemon's feed too.
type FeedWriter interface {
	Rebuild(ctx context.Context, entries []seadex.Entry, isMovie func(alID int) bool) error
}

// SeaDexSource supplies the SeaDex entries snapshot a cycle compares and
// rebuilds the feed from. It is the consumer-side seam over the concrete
// *seadex.Client (which implements it; build.go injects it), so orchestration
// tests can drive cycle outcomes with a fake instead of standing up the
// PocketBase adapter over an httptest server.
type SeaDexSource interface {
	FetchEntries(ctx context.Context) ([]seadex.Entry, error)
}

// The concrete PocketBase client must keep satisfying the cycle's seam.
var _ SeaDexSource = (*seadex.Client)(nil)

// StateStore loads and saves the persisted cross-cycle state a cycle reads and
// writes. It is the consumer-side seam over the concrete *state.Store (which
// implements it; build.go injects it), so orchestration tests can drive state
// transitions with an in-memory fake instead of performing atomic disk I/O
// (the state package's own suite covers the file adapter round-trip).
type StateStore interface {
	Load(ctx context.Context) (state.State, error)
	Save(ctx context.Context, st *state.State) error
}

// The concrete file-backed store must keep satisfying the cycle's seam.
var _ StateStore = (*state.Store)(nil)

// Deps are the assembled components a Scout runs a cycle with.
type Deps struct {
	Logger   *slog.Logger
	Store    StateStore
	Library  *library.Walker
	Mapping  *mapping.Loader
	SeaDex   SeaDexSource
	Matcher  *match.Matcher
	Comparer *compare.Comparer
	Auditor  *audit.Auditor
	Reporter *report.Reporter
	// AniListStats reports the AniList client's cumulative request counters
	// (calls, rate-limit waits) for the cycle completion logs. The scout only
	// needs these two counters, so it takes a narrow callback instead of the
	// concrete client (build.go injects a closure over the client's Stats);
	// nil when no AniList client is wired (the early-return degradation paths
	// and unit tests) - the daemon always wires it.
	AniListStats func() (calls, rateLimitWaits int64)
	// Feed rebuilds and persists the indexer's Torznab feed from each cycle's
	// SeaDex snapshot. Nil when no Torznab feed is configured (the cycle then
	// skips all feed work).
	Feed FeedWriter
}

// libraryShrinkFactor sets the library shrink guard's trigger fraction: a
// non-failed walk (partial included - Failed placeholders keep the item
// count, so a shrink means the arr's series list itself shrank) returning
// fewer than 1/libraryShrinkFactor of the prior snapshot's items (below
// half, at the default 2) is treated as
// suspicious (a misconfigured arr_tags filter, an emptied or fresh arr) rather
// than a real library change, mirroring the mapping loader's below-half-size
// refresh guard. The zero-items case is the extreme of the same shrink.
const libraryShrinkFactor = 2

// shrunkWalkEscalationThreshold is the consecutive-shrunk-walk streak
// (state.State.ShrunkWalks) at which the scout escalates its shrunk-walk log
// from WARN to ERROR (firing the existing SeadexScoutCycleError Loki rule):
// 8 cycles is about a day at the default 3h cadence - long enough to ride out
// a transient arr oddity, short enough that a persistent misconfiguration
// (arr_tags leaving one item) alerts instead of silently skipping the compare
// forever. Mirrors mapping.RejectionEscalationThreshold. The remedy is
// operator-driven: fix the arr/tags, or remove state.json to accept the
// smaller library - the guard never auto-accepts a shrunken walk.
const shrunkWalkEscalationThreshold = 8

// Scout runs compare cycles from its assembled dependencies.
type Scout struct {
	deps Deps
	log  *slog.Logger
}

// cycleDegraded emits the degraded-cycle completion line. Every
// degraded-but-healthy early return (unusable map, failed or empty SeaDex
// fetch, AniList degradation, the library shrink guard) and the two degraded
// compare paths (a partial walk, a stale-but-usable map) end the cycle with
// this single WARN, so the cycle-deadman
// alert (which counts completion lines) stays satisfied during a long
// upstream outage instead of firing as if the daemon died. reason
// distinguishes the gate; the healthy path keeps "cycle complete" as-is, and
// a shutdown-interrupted cycle emits neither (it did not complete).
func (s *Scout) cycleDegraded(reason string, attrs ...any) {
	s.log.Warn("cycle degraded", append([]any{"reason", reason}, attrs...)...)
}

// New builds a Scout from deps.
func New(deps *Deps) *Scout {
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Scout{deps: *deps, log: log}
}

// Cycle runs one full compare cycle and reports whether the run was healthy
// (the library ingest succeeded). It never returns an error: a failed ingest
// returns false, and an upstream (SeaDex/mapping/AniList) failure returns true
// but degraded.
func (s *Scout) Cycle(ctx context.Context) bool {
	start := time.Now()
	startStats := s.aniStats()
	st := s.loadState(ctx)

	snap, walkErr := s.deps.Library.Walk(ctx)
	if s.stopAfterWalkFailure(ctx, walkErr) {
		return false
	}

	// The shared SeaDex + Fribb snapshot feeds BOTH halves: the Torznab feed
	// (arr-independent) and the compare pass below. Fetching once here is what
	// keeps a notification and what the arrs see in the feed on the same data.
	mapCache, idx, mapErr := s.loadMapping(ctx, &st)
	entries, seaErr := s.deps.SeaDex.FetchEntries(ctx)

	// Rebuild the Torznab feed from the shared snapshot, independent of the arr
	// walk (see rebuildFeed): a notification and what the arrs see in the feed
	// come from this one fetch.
	s.rebuildFeed(ctx, entries, idx, mapErr, seaErr)

	// From here the compare pass is gated on the arr walk (the health signal): a
	// failed walk is unhealthy and leaves findings untouched (only the refreshed
	// mapping cache is persisted), while the feed above was still refreshed. The
	// pre-compare degradation gate (failed walk, unusable map, failed/empty
	// SeaDex fetch) is factored into a helper so Cycle reads as the top-down
	// happy path.
	if handled, healthy := s.handlePreCompareGate(ctx, &st, snap, &mapCache, entries, walkErr, mapErr, seaErr); handled {
		return healthy
	}

	result := s.deps.Matcher.Match(ctx, entries, &snap, idx, st.Memo)
	if result.Degraded {
		return s.finishDegradedMatch(ctx, start, startStats, &st, snap, &mapCache, result)
	}
	return s.finishSuccessfulCycle(ctx, start, startStats, &st, snap, &mapCache, entries, result, mapErr)
}

// stopAfterWalkFailure logs a failed library walk and reports whether Cycle
// should stop immediately. A shutdown-cancelled walk is logged at WARN (a
// redeploy is routine, not an arr fault) and always stops. A genuine walk
// failure is unhealthy; an alert-only deployment (no Torznab feed) stops right
// away since nothing else remains to do, while a configured feed falls through
// so the arr-independent feed rebuild still runs (the pre-compare gate then
// returns unhealthy).
func (s *Scout) stopAfterWalkFailure(ctx context.Context, walkErr error) bool {
	if walkErr == nil {
		return false
	}
	if ctx.Err() != nil {
		// A shutdown/redeploy cancelled the cycle mid-walk: not an arr fault,
		// so do not log at ERROR (it would trip the SeadexScoutCycleError
		// alert on every redeploy landing mid-cycle - the same fault class
		// the detached state-save retry in save already closed).
		s.log.Warn("cycle interrupted by shutdown during library walk", "cause", context.Cause(ctx))
		return true
	}
	s.log.Error("library walk failed; cycle unhealthy", "error", walkErr)
	// Alert-only (no Torznab feed): a failed walk is unhealthy and there is
	// nothing else to do, so skip the SeaDex/Fribb fetch (the pre-fold
	// behaviour). With a feed configured, fall through to refresh it - it
	// needs only SeaDex + Fribb, not the arrs - before returning unhealthy.
	return s.deps.Feed == nil
}

// loadMapping refreshes the Fribb map from the persisted cache, logging a
// degraded load once. A cancelled load is the shutdown, not a Fribb fault; the
// pre-compare gate logs the interruption instead (same rule as the
// walk/matching paths). The degraded log is WARN, escalating to ERROR (which
// fires the existing SeadexScoutCycleError Loki rule) once the loader's
// acceptance guards have rejected mapping.RejectionEscalationThreshold
// consecutive refreshes: that state re-downloads the ~5.9MB body every cycle
// against an aging cache and never self-heals without the operator, so it
// must alert rather than WARN forever. The rejection streak rides on the
// *StaleMapError (ConsecutiveRejections) so this stays the single log site -
// no second log line in the mapping package, no double-logging.
func (s *Scout) loadMapping(ctx context.Context, st *state.State) (mapping.Cache, *mapping.Index, error) {
	mapCache, idx, mapErr := s.deps.Mapping.Load(ctx, &st.Mapping)
	if mapErr != nil && ctx.Err() == nil {
		attrs := mappingDegradedAttrs(mapErr, idx.Len())
		stale, ok := errors.AsType[*mapping.StaleMapError](mapErr)
		if ok && stale.ConsecutiveRejections() >= mapping.RejectionEscalationThreshold {
			// The attrs carry the streak (stale_consecutive_rejections) and
			// the rejecting guard (stale_reason).
			s.log.Error("mapping degraded: refresh rejected repeatedly; inspect upstream, or remove state.json to cold-start if the change is legitimate", attrs...)
		} else {
			s.log.Warn("mapping degraded", attrs...)
		}
	}
	return mapCache, idx, mapErr
}

// mappingDegradedAttrs builds the attribute set shared by the cycle and report
// mapping-degraded log sites: the existing error and usable_records attributes,
// plus StaleMapError's structured degradation fields (stale_reason,
// stale_age_seconds, stale_records) when the error carries them, so Loki can
// query the rejection class and stale age without parsing the message text.
func mappingDegradedAttrs(mapErr error, usableRecords int) []any {
	attrs := []any{"error", mapErr, "usable_records", usableRecords}
	if stale, ok := errors.AsType[*mapping.StaleMapError](mapErr); ok {
		attrs = append(attrs, stale.LogAttrs()...)
	}
	return attrs
}

// aniListStats snapshots the AniList request counters (via Deps.AniListStats)
// at a point in the cycle, so the completion line can log per-cycle deltas.
type aniListStats struct {
	calls          int64
	rateLimitWaits int64
}

// aniListCycleAttrs returns the cumulative and per-cycle AniList counters both
// cycle completion paths log.
func (s *Scout) aniListCycleAttrs(startStats aniListStats) []any {
	cur := s.aniStats()
	return []any{
		"anilist_calls", cur.calls,
		"anilist_calls_cycle", cur.calls - startStats.calls,
		"anilist_waits", cur.rateLimitWaits,
		"anilist_waits_cycle", cur.rateLimitWaits - startStats.rateLimitWaits,
	}
}

// finishDegradedMatch closes a cycle whose matching came back degraded: a
// transient AniList outage left needed fallback lookups incomplete, so some
// entries are missing matches they would normally resolve. Comparing now would
// treat those absent findings as resolved. Save the refreshed
// library/mapping/memo (the memo keeps the lookups that did succeed) but leave
// the finding dedupe table untouched. Always healthy: an upstream outage is
// not an ingest fault.
func (s *Scout) finishDegradedMatch(ctx context.Context, start time.Time, startStats aniListStats, st *state.State, snap library.Snapshot, mapCache *mapping.Cache, result match.Result) bool {
	st.Library, st.Mapping, st.Memo = snap, *mapCache, result.Memo
	s.save(ctx, st)
	attrs := append(s.aniListCycleAttrs(startStats),
		"duration", time.Since(start).Round(time.Millisecond).String())
	if ctx.Err() != nil {
		// A shutdown/redeploy cancelled the cycle mid-matching: Matcher flags
		// the result degraded so findings stay preserved, but the cause is
		// the shutdown, not AniList. Log it as such (matching the
		// library-walk path) so a routine redeploy is not attributed to an
		// AniList outage.
		s.log.Warn("cycle interrupted by shutdown during matching",
			append([]any{"cause", context.Cause(ctx)}, attrs...)...)
		return true
	}
	s.log.Warn("anilist degraded; skipping comparison, findings preserved", attrs...)
	s.cycleDegraded("anilist-degraded", attrs...)
	return true
}

// finishSuccessfulCycle runs the compare over the completed match result,
// emits (or cold-start baselines) the findings, logs the completion line
// ("cycle complete", or "cycle degraded" for a partial walk or a
// stale-but-usable map), and persists the full refreshed state. On a partial
// walk the compare runs on the items that walked cleanly only: matches linked
// to Failed items are excluded (their file state is missing, not empty), and
// finding resolution is scoped so those items' prior findings are preserved
// rather than falsely resolved. Always healthy.
func (s *Scout) finishSuccessfulCycle(ctx context.Context, start time.Time, startStats aniListStats, st *state.State, snap library.Snapshot, mapCache *mapping.Cache, entries []seadex.Entry, result match.Result, mapErr error) bool {
	cleanMatches, failedItems := splitFailedMatches(result.Matches)
	findings := s.deps.Comparer.Compare(cleanMatches)

	// A cold start (a fresh install, or a lost/reset cache) has no dedupe table
	// yet: baseline the current findings silently so the whole pre-existing
	// backlog is not dumped as notifications at once. Steady-state emission
	// resumes next cycle via Report. The len(Findings) guard keeps an upgrade of
	// an already-running instance (state predating the flag but already holding
	// findings) on the normal emit path. One cell stays conservative: a state
	// with no findings and Baselined unset (an upgraded fully-aligned instance,
	// or an install whose first cycles were all degraded) is indistinguishable
	// from a cold start and baselines, preferring a one-cycle silent seed over
	// bursting a whole backlog - a finding first appearing in exactly that
	// cycle is seeded, not emitted. The full list is always available on
	// demand via report mode.
	var newFindings map[string]report.Alerted
	if !st.Baselined && len(st.Findings) == 0 {
		if snap.Partial {
			// A cold-start baseline must cover the complete library: baselining
			// only the clean subset of a partial first walk would mark
			// Baselined=true, and the failed items' pre-existing findings would
			// burst as "new" when they recover on the next complete cycle. Keep
			// the unseeded state until a complete walk can establish the
			// baseline.
			newFindings = st.Findings
		} else {
			newFindings = s.deps.Reporter.Baseline(findings, time.Now())
			st.Baselined = true
		}
	} else {
		newFindings = s.deps.Reporter.Report(findings, st.Findings, failedItems, time.Now())
		st.Baselined = true
	}

	diff := library.DiffSnapshots(&st.Library, &snap)
	attrs := make([]any, 0, 26)
	attrs = append(attrs,
		"seadex_entries", len(entries),
		"library_items", len(snap.Items),
		"findings", len(findings),
		"mapped", sumCounts(result.Coverage.Hits),
		"unmapped", sumCounts(result.Coverage.Unmapped),
	)
	attrs = append(attrs, s.aniListCycleAttrs(startStats)...)
	attrs = append(attrs,
		"added", diff.Added, "removed", diff.Removed, "changed", diff.Changed,
		"duration", time.Since(start).Round(time.Millisecond).String())
	switch {
	case snap.Partial:
		// A partial walk compared only the clean items, so the cycle closed
		// degraded: report the degraded coverage on the completion line the
		// deadman alert counts alongside "cycle complete".
		s.cycleDegraded("partial-walk", append([]any{"failed_items", len(failedItems)}, attrs...)...)
	case mapErr != nil:
		// Only a stale-but-usable mapping error reaches this point; unusable and
		// cancelled loads returned at the pre-compare gate. The compare ran on
		// the cached map, but the cycle is still upstream-degraded, so it must
		// not read as fully successful.
		s.cycleDegraded("mapping-stale", attrs...)
	default:
		s.log.Info("cycle complete", attrs...)
	}

	st.Library, st.Mapping, st.Memo, st.Findings = snap, *mapCache, result.Memo, newFindings
	s.save(ctx, st)
	return true
}

// splitFailedMatches partitions the match set for a partial walk: a match
// linked to an item whose episode fetch failed is excluded from the compare
// (its file state is missing, not empty, so comparing would misread every
// recommendation as unmet), and the failed items' AniList IDs are returned so
// finding resolution can preserve their prior findings. A clean walk returns
// the matches untouched and a nil set.
func splitFailedMatches(matches []match.Match) (clean []match.Match, failedItems map[int]struct{}) {
	clean = make([]match.Match, 0, len(matches))
	for i := range matches {
		if m := &matches[i]; m.InLibrary() && m.Item.Failed {
			if failedItems == nil {
				failedItems = make(map[int]struct{})
			}
			failedItems[m.Entry.AniListID] = struct{}{}
			continue
		}
		clean = append(clean, matches[i])
	}
	if failedItems == nil {
		return matches, nil
	}
	return clean, failedItems
}

// mapUsable reports whether a compare or feed rebuild can proceed on the loaded
// map: a nil load error, or a stale-but-usable cache (*mapping.StaleMapError,
// which carries the cached index). Any other load error means no usable map.
func mapUsable(mapErr error) bool {
	if mapErr == nil {
		return true
	}
	_, stale := errors.AsType[*mapping.StaleMapError](mapErr)
	return stale
}

// rebuildFeed refreshes the indexer's Torznab feed from the cycle's shared
// SeaDex snapshot, independent of the arr walk (the feed needs only SeaDex +
// Fribb, so an arr outage must not freeze it). It is a no-op when no feed is
// configured, the SeaDex fetch failed, or the map is unusable (a load error
// that is NOT a mapping.StaleMapError) - the last-good feed is then kept:
// rebuilding against an unusable map would categorize every entry as anime and
// silently drop all SeaDex movies from Radarr's RSS view. A stale-but-usable
// map (mapErr matches mapping.StaleMapError, which carries a usable cached
// index) still rebuilds, exactly like the pre-compare gate's discrimination.
func (s *Scout) rebuildFeed(ctx context.Context, entries []seadex.Entry, idx *mapping.Index, mapErr, seaErr error) {
	if s.deps.Feed == nil || seaErr != nil || len(entries) == 0 || !mapUsable(mapErr) {
		return
	}
	// The feed writer consumes exactly one bit of the Fribb map (movie or not),
	// so it takes a closure instead of the whole index; Lookup is nil-safe.
	isMovie := func(alID int) bool {
		rec, ok := idx.Lookup(alID)
		return ok && rec.IsMovie()
	}
	if err := s.deps.Feed.Rebuild(ctx, entries, isMovie); err != nil && ctx.Err() == nil {
		// A cancelled rebuild is the shutdown, not a feed fault; the pre-compare
		// gate logs the interruption (the last-good feed is kept either way).
		s.log.Warn("indexer feed rebuild failed; keeping previous feed", "error", err)
	}
}

// logFeedOutageOnWalkFail surfaces a concurrent SeaDex outage when the arr
// walk already failed but a feed is configured, so a multi-dependency outage
// does not read as arr-only. During a shutdown the SeaDex failure is the
// cancellation (the walk path already logged the interruption), so it stays
// silent then.
func (s *Scout) logFeedOutageOnWalkFail(ctx context.Context, entries []seadex.Entry, seaErr error) {
	if s.deps.Feed == nil || ctx.Err() != nil {
		return
	}
	switch {
	case seaErr != nil:
		s.log.Warn("seadex fetch failed; indexer feed kept previous feed", "error", seaErr)
	case len(entries) == 0:
		s.log.Warn("seadex returned zero entries; indexer feed kept previous feed")
	}
}

// handlePreCompareGate applies the pre-compare degradation gate: it reports
// whether the cycle should stop before the compare pass (handled) and, when it
// should, the health outcome to return. The library gate (failed walk,
// suspicious shrunken walk) runs first, then the upstream gate
// (shutdown cancellation, unusable map, failed/empty SeaDex fetch); see each
// helper for the per-branch policy. A stale-but-usable map (mapErr matches
// mapping.StaleMapError) is degraded-but-comparable and flows into the normal
// compare path (handled=false).
func (s *Scout) handlePreCompareGate(ctx context.Context, st *state.State, snap library.Snapshot, mapCache *mapping.Cache, entries []seadex.Entry, walkErr, mapErr, seaErr error) (handled, healthy bool) {
	if handled, healthy := s.handleLibraryGate(ctx, st, snap, mapCache, entries, walkErr, seaErr); handled {
		return true, healthy
	}
	return s.handleUpstreamGate(ctx, st, snap, mapCache, entries, mapErr, seaErr)
}

// handleLibraryGate gates the compare pass on the library ingest. A failed arr
// walk is unhealthy and persists only the refreshed mapping cache (findings,
// memo, and the prior library snapshot ride along untouched). A
// non-failed walk (partial included) that shrank below half the prior snapshot's items
// (libraryShrinkFactor; zero items is the extreme case) is degraded but
// healthy: it persists ONLY the refreshed mapping cache plus the consecutive
// shrunk-walk streak, so a shrunken snapshot can never replace st.Library and
// mass-resolve findings (now or a cycle later), and never auto-accepts. A
// partial snapshot (per-series episode-fetch failures) is NOT gated here: the
// compare proceeds on the items that walked cleanly, with the Failed items'
// findings preserved by resolution scoping (see finishSuccessfulCycle).
func (s *Scout) handleLibraryGate(ctx context.Context, st *state.State, snap library.Snapshot, mapCache *mapping.Cache, entries []seadex.Entry, walkErr, seaErr error) (handled, healthy bool) {
	if walkErr != nil {
		// With a feed configured, Cycle fell through the walk failure so the
		// arr-independent feed could still refresh. If SeaDex ALSO failed (or
		// returned nothing), rebuildFeed silently kept the previous feed and this
		// early return would swallow that second outage - surface it here so a
		// multi-dependency outage does not read as arr-only. Single SeaDex
		// failures (walk healthy) keep their own WARNs in the upstream gate, so
		// no duplicates.
		s.logFeedOutageOnWalkFail(ctx, entries, seaErr)
		// Persist only the refreshed mapping cache, like the shrunk-walk arm
		// below: discarding it re-downloads an updated Fribb
		// body next cycle. Findings, memo, and the prior library snapshot
		// ride along untouched (an unusable-map load returns the prior cache,
		// making this persist a no-op then).
		st.Mapping = *mapCache
		s.save(ctx, st)
		return true, false
	}
	if len(st.Library.Items) > 0 && len(snap.Items)*libraryShrinkFactor < len(st.Library.Items) {
		// A non-failed walk that shrank far below the prior snapshot
		// (zero items, or a misconfigured arr_tags.include leaving a handful)
		// would mass-resolve most findings. Do NOT degradedSave here:
		// persisting the shrunken snapshot would make this a one-cycle ratchet
		// (next cycle the prior snapshot is shrunken too and the mass-resolve
		// happens anyway). Persist only the refreshed mapping cache plus the
		// consecutive-shrunk streak: the ratchet danger is the shrunken
		// snapshot, not the map, and dropping the cache re-downloads an
		// updated Fribb body next cycle. The single log site below escalates
		// to ERROR (the SeadexScoutCycleError rule) once the persisted streak
		// reaches shrunkWalkEscalationThreshold - a shrink that persists for a
		// day is a misconfiguration, not a blip, and must alert rather than
		// WARN forever. Never auto-accepted: recovery is a genuinely recovered
		// walk, or the operator removing state.json.
		st.ShrunkWalks++
		st.Mapping = *mapCache
		s.save(ctx, st)
		attrs := []any{
			"items", len(snap.Items),
			"prior_items", len(st.Library.Items),
			"consecutive_shrunk_walks", st.ShrunkWalks,
		}
		if st.ShrunkWalks >= shrunkWalkEscalationThreshold {
			s.log.Error("library walk shrank repeatedly; skipping comparison, findings preserved - inspect the arrs and arr_tags, or remove state.json to accept the smaller library", attrs...)
		} else {
			s.log.Warn("library walk shrank below half the prior snapshot; skipping comparison, findings preserved", attrs...)
		}
		s.cycleDegraded("library-shrunk", "items", len(snap.Items), "prior_items", len(st.Library.Items))
		return true, true
	}
	// The walk passed the shrink guard: any shrunk-walk streak ends here (a
	// recovered walk resumes normal resolution), persisted by whichever save
	// closes this cycle.
	st.ShrunkWalks = 0
	return false, true
}

// handleUpstreamGate gates the compare pass on the map's usability and the
// SeaDex fetch. An unusable map (no stale cache either - a load error that is
// NOT a mapping.StaleMapError; the loader owns that discrimination, so a
// handful of operator overrides overlaid on an empty index cannot defeat the
// gate and let the compare pass falsely resolve findings against an
// overrides-only map), a failed SeaDex fetch, or a successful-but-empty fetch
// are each degraded but healthy: they preserve prior findings and save only
// the refreshed library snapshot/map (degradedSave) so a transient upstream
// outage does not falsely resolve live findings. A shutdown cancellation
// during the load or fetch is attributed to the shutdown, not the upstream.
func (s *Scout) handleUpstreamGate(ctx context.Context, st *state.State, snap library.Snapshot, mapCache *mapping.Cache, entries []seadex.Entry, mapErr, seaErr error) (handled, healthy bool) {
	if ctx.Err() != nil && (mapErr != nil || seaErr != nil) {
		// A shutdown/redeploy cancelled the cycle during the mapping load or
		// SeaDex fetch: the errors are the cancellation, not an upstream fault.
		// Preserve findings exactly like an upstream outage (degradedSave) but
		// attribute the interruption to the shutdown instead of blaming Fribb
		// or SeaDex (matching the library-walk and matching paths).
		s.degradedSave(ctx, st, snap, mapCache)
		s.log.Warn("cycle interrupted by shutdown before comparison; findings preserved",
			"cause", context.Cause(ctx))
		return true, true
	}
	if !mapUsable(mapErr) {
		s.degradedSave(ctx, st, snap, mapCache)
		s.log.Warn("mapping unusable; skipping comparison, findings preserved", "error", mapErr)
		s.cycleDegraded("mapping-unusable", "error", mapErr)
		return true, true
	}
	if seaErr != nil {
		s.degradedSave(ctx, st, snap, mapCache)
		s.log.Warn("seadex fetch failed; skipping comparison, findings preserved", "error", seaErr)
		s.cycleDegraded("seadex-fetch-failed", "error", seaErr)
		return true, true
	}
	if len(entries) == 0 {
		s.degradedSave(ctx, st, snap, mapCache)
		s.log.Warn("seadex returned zero entries; skipping comparison, findings preserved")
		s.cycleDegraded("seadex-zero-entries")
		return true, true
	}
	return false, true
}

// reportSnapshot walks the library for a one-shot report, failing on a walk
// error or a partial snapshot: auditing an incomplete snapshot would publish a
// successful, timestamped report that silently omits the skipped series,
// contradicting the whole-library audit contract.
func (s *Scout) reportSnapshot(ctx context.Context) (library.Snapshot, error) {
	snap, err := s.deps.Library.Walk(ctx)
	if err != nil {
		return library.Snapshot{}, fmt.Errorf("library walk: %w", err)
	}
	if snap.Partial {
		// The walk skipped series after episode-fetch failures - fail instead,
		// like a failed walk.
		return library.Snapshot{}, errors.New("library walk: partial snapshot after episode-fetch failures")
	}
	return snap, nil
}

// reportMapping loads the Fribb map for a one-shot report. An unusable map
// (no stale cache either) fails the report: ID matching, season scoping, and
// the not_on_seadex catalogue all depend on it, so publishing would contradict
// the whole-library audit contract (the daemon gate refuses to compare on this
// too). A stale-but-usable map proceeds with a single degraded WARN. A
// cancelled load is the shutdown, not a Fribb fault (the SeaDex fetch after
// this then fails with the cancellation and Report returns it).
func (s *Scout) reportMapping(ctx context.Context, st *state.State) (*mapping.Index, error) {
	_, idx, mapErr := s.deps.Mapping.Load(ctx, &st.Mapping)
	if mapErr == nil || ctx.Err() != nil {
		return idx, nil
	}
	if !mapUsable(mapErr) {
		return nil, fmt.Errorf("mapping unusable: %w", mapErr)
	}
	s.log.Warn("report: mapping degraded", mappingDegradedAttrs(mapErr, idx.Len())...)
	return idx, nil
}

// Report runs a one-shot SeaDex-alignment audit over the current library and
// returns the report. It is read-only on persisted state (it loads the mapping
// cache and AniList memo to avoid needless refetching, but never saves), so it
// is safe to run on demand while the daemon's cycle runs: the shared clients are
// concurrency-safe and each run carries its own state copy. It returns an error
// when the library walk, mapping load, SeaDex fetch, or matching cannot produce
// a complete audit.
func (s *Scout) Report(ctx context.Context) (audit.Report, error) {
	start := time.Now()
	st := s.loadState(ctx)

	snap, err := s.reportSnapshot(ctx)
	if err != nil {
		return audit.Report{}, err
	}

	idx, err := s.reportMapping(ctx, &st)
	if err != nil {
		return audit.Report{}, err
	}

	entries, err := s.deps.SeaDex.FetchEntries(ctx)
	if err != nil {
		return audit.Report{}, fmt.Errorf("seadex fetch: %w", err)
	}
	if len(entries) == 0 {
		// Defense in depth: FetchEntries errors on an empty completed
		// catalogue, but a future client regression returning (nil, nil) would
		// otherwise publish a successful report marking every library item
		// not_on_seadex - refuse instead, mirroring Cycle's zero-entries
		// degradation gate.
		return audit.Report{}, errors.New("seadex fetch: returned zero entries")
	}

	result := s.deps.Matcher.Match(ctx, entries, &snap, idx, st.Memo)
	if result.Degraded {
		// An incomplete match would publish a successful, timestamped audit
		// whose unmatched entries are silently omitted or misfiled - the same
		// completeness contract the partial-snapshot gate enforces above.
		if ctx.Err() != nil {
			return audit.Report{}, fmt.Errorf("report interrupted: %w", context.Cause(ctx))
		}
		return audit.Report{}, errors.New("anilist lookups degraded: matching incomplete")
	}
	rep := s.deps.Auditor.Audit(result.Matches, &snap, idx)
	s.log.Info("report generated",
		"seadex_entries", len(entries),
		"library_items", len(snap.Items),
		"rows", len(rep.Rows),
		"duration", time.Since(start).Round(time.Millisecond).String())
	return rep, nil
}

// aniStats returns the AniList client's cumulative stats via the injected
// callback, or zero stats when none is wired (the early-return degradation
// paths and unit tests build Deps without one; the daemon always wires it).
func (s *Scout) aniStats() aniListStats {
	if s.deps.AniListStats == nil {
		return aniListStats{}
	}
	calls, waits := s.deps.AniListStats()
	return aniListStats{calls: calls, rateLimitWaits: waits}
}

// loadState loads persisted state, falling back to an empty state on error.
// A load cut short by shutdown/redeploy cancellation is not a state fault:
// it returns empty silently (no ERROR, which would trip the cycle-error alert
// on a routine redeploy) and the immediately following context-aware cycle
// stage reports the shutdown once at WARN.
func (s *Scout) loadState(ctx context.Context) state.State {
	st, err := s.deps.Store.Load(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			return state.State{}
		}
		s.log.Error("state load failed; starting from empty state", "error", err)
		return state.State{}
	}
	return st
}

// degradedSave persists the caches refreshed before the compare pass was
// skipped (library snapshot and map), leaving the AniList memo and finding
// dedupe untouched so a degraded upstream (unusable map, failed or empty
// SeaDex fetch) or a shutdown mid-cycle cannot falsely resolve live findings.
func (s *Scout) degradedSave(ctx context.Context, st *state.State, snap library.Snapshot, mapCache *mapping.Cache) {
	st.Library = snap
	st.Mapping = *mapCache
	s.save(ctx, st)
}

// saveGrace bounds the detached shutdown save. It stays inside Docker's default
// 10s stop grace (the public compose example sets no stop_grace_period), so the
// write completes before SIGKILL. atomicfile's temp+rename means a SIGKILL
// mid-write cannot corrupt state - the only cost of a missed save is losing the
// AniList memo, which self-heals over one cold cycle.
const saveGrace = 5 * time.Second

// save persists state, tolerating a shutdown mid-cycle. When the run context is
// cancelled (SIGTERM during a redeploy), the atomic write fails with
// context.Canceled and the caches are lost — so a cancellation is retried once
// with a detached, briefly-bounded context (context.WithoutCancel keeps the
// values, drops the cancellation), letting the write finish so the expensive
// AniList memo survives the restart. A cancellation is not a fault (a redeploy
// is routine), so only a genuine write failure is logged at ERROR — which keeps
// it off the cycle-error alert.
func (s *Scout) save(ctx context.Context, st *state.State) {
	err := s.deps.Store.Save(ctx, st)
	if err != nil && (errors.Is(err, context.Canceled) || ctx.Err() != nil) {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), saveGrace)
		defer cancel()
		err = s.deps.Store.Save(dctx, st)
	}
	if err != nil {
		s.log.Error("state save failed", "error", err)
	}
}

// sumCounts totals a per-arr count map for a flat log field.
func sumCounts(m map[string]int) int {
	total := 0
	for _, n := range m {
		total += n
	}
	return total
}
