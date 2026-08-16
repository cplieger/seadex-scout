package scout

import (
	"context"
	"time"

	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/match"
	"github.com/cplieger/seadex-scout/internal/seadexapi"
	"github.com/cplieger/seadex-scout/internal/state"
)

// The TICK: one bounded recent-changes pass, the loop iteration that is not a
// reconcile. Cycle (scout.go) dispatches between the two; everything specific
// to the cheap path lives here.

// The tick's window and its two wedge diagnostics.
const (
	// changeWindow is how far back a tick looks. It is deliberately much wider
	// than the tick interval: the window is the only thing that recovers a missed
	// tick, a restart, or a small clock skew, and a wider window costs bytes
	// proportional to the upstream's change RATE, not to its size.
	changeWindow = 48 * time.Hour

	// frozenFastPathTolerance is how long the fast path may stay frozen before the
	// diagnostic ERRORs. It pages at ERROR because nothing in this stack alerts on
	// WARN, and a frozen fast path means no new RSS items and no new findings, with
	// only the reconcile still working. It is the tolerance BOTH latches use; the
	// unreachable one is the tick's half of the persisted SeadexFailures streak,
	// which advances only inside a reconcile.
	frozenFastPathTolerance = 2 * time.Hour

	// minLatchTicks is the floor latchTicks applies, so one blip is always
	// tolerated however long the interval is.
	minLatchTicks = 2
)

// latchTicks converts a wall-clock tolerance into the number of CONSECUTIVE
// ticks that spans at this loop's own interval, floored at minLatchTicks so one
// blip is always tolerated. Raw iteration counts only meant what they claimed at
// the 15m default: poll_interval is accepted up to 30 days, so an 8-tick "2h"
// escalation is 24h at a 3h interval. A zero or negative interval runs no ticks.
func (s *Scout) latchTicks(d time.Duration) int {
	if s.pollInterval <= 0 {
		return minLatchTicks
	}
	return max(minLatchTicks, int(d/s.pollInterval))
}

// tick runs one bounded recent-changes pass. It is healthy whenever it completed,
// including when it found nothing: an empty window is a successful tick, and the
// marker it commits attests that the loop is alive, not that anything changed.
// Every exit that RAN emits one completion line the loop deadman counts and
// re-states the finding set; a tick a SHUTDOWN cancelled emits NEITHER, because
// publishing its truncated match set would resolve entries it never evaluated.
// It costs one ~88-byte probe plus one small window fetch, walks no arr, touches
// no search curation index, and ADVANCES the feed rather than rebuilding it.
func (s *Scout) tick(ctx context.Context) bool {
	if !s.ready {
		// No complete pass has established the finding set yet, so this tick's
		// handful of findings would publish as the app's whole state. Emit the
		// liveness line and nothing else: an empty "findings reported" line reads as
		// "no findings", a claim this tick cannot make.
		s.logTickDegraded("awaiting-first-reconcile", "reconcile_attempts", s.reconcileRetries)
		return true
	}
	since := time.Now().Add(-changeWindow)
	count, err := s.seadex.CountWindow(ctx, since)
	if err != nil {
		if ctx.Err() != nil {
			// A redeploy cancelled the probe. The CAUSE is the shutdown, not SeaDex;
			// the outcome decides every effect that follows from it.
			return s.tickInterrupted(ctx, "during the change probe", nil, nil)
		}
		// A failed probe is a failed tick, and the fault is the upstream's.
		return s.tickUnreadableUpstream(ctx,
			"change probe failed; skipping tick", "probe-failed", err, nil, nil)
	}
	switch {
	case count == 0:
		s.oversizeRun, s.unreachableRun = 0, 0
		// A complete tick: the probe answered, and the answer was "nothing". Nothing
		// was compared, so the standing set is re-stated rather than replaced.
		s.notifier.Reemit()
		return s.tickComplete(ctx, 0, 0, nil, nil)
	case count >= seadexapi.MaxWindowEntries:
		// This EXIT's own evidence settles the other counter: the probe read the
		// upstream, and the answer was not empty.
		s.unreachableRun = 0
		return s.tickOversizeWindow(ctx, count)
	}
	// Not an exit: the tick continues into a window fetch, so only the counter that
	// measures upstream STATE is settled here. Reachability is not established
	// until that fetch returns, and resetting unreachableRun here would cancel the
	// increment the same tick's fetch failure makes.
	s.oversizeRun = 0
	return s.tickChanged(ctx, since, count)
}

// tickInterrupted closes a tick a shutdown cancelled - CAUSE: shutdown. No
// streak advances, because a redeploy is not an upstream fault. It ALWAYS
// persists what the pass already learned (the revalidated Fribb validators, the
// memo entries that did resolve, the persisted refresh-rejection streak - never
// the library snapshot, since a tick performs no walk). It emits NO completion
// line and no re-statement, because its match set is truncated. st and mapCache
// are nil at the one boundary reached before the state load. Always healthy.
func (s *Scout) tickInterrupted(ctx context.Context, stage string, st *state.State, mapCache *mapping.Cache) bool {
	s.log.Warn("tick interrupted by shutdown "+stage, "cause", context.Cause(ctx))
	s.saveTick(ctx, st, mapCache)
	return true
}

// tickUnreadableUpstream closes a tick that could not read SeaDex at either of
// its two reads - CAUSE: upstream fault. It advances the fast path's own
// unreachability streak (the tick-cadence half of the persisted SeadexFailures
// streak), persists what the pass had learned, and emits the degraded line.
func (s *Scout) tickUnreadableUpstream(ctx context.Context, msg, reason string, err error, st *state.State, mapCache *mapping.Cache, attrs ...any) bool {
	s.unreachableRun++
	s.warnUnreachableUpstream(msg, err)
	return s.closeDegradedTick(ctx, reason, st, mapCache, attrs...)
}

// tickOversizeWindow closes a tick whose window is too large to fetch - CAUSE:
// upstream fault, the second one the fast path has. It advances the oversize
// streak rather than the unreachability one because the two have different
// remedies, has nothing to persist (this exit precedes the state load), and
// emits the degraded completion line.
func (s *Scout) tickOversizeWindow(ctx context.Context, count int) bool {
	s.oversizeRun++
	s.warnOversizeWindow(count)
	return s.closeDegradedTick(ctx, "window-oversize", nil, nil, "window_entries", count)
}

// tickMappingUnusable closes a tick with no usable Fribb index - CAUSE: mapping
// unusable. No upstream streak advances (the mapping loader's own persisted
// streak was already advanced by the load), the refreshed cache IS persisted so
// that streak survives a restart, and the line carries reason=mapping-unusable.
// Nothing can be matched without an index, so a compare would report an empty
// finding set for entries that may well be misaligned. The feed advance is
// skipped for the same reason rebuildFeed skips it, so the caller gates first.
func (s *Scout) tickMappingUnusable(ctx context.Context, mapErr error, st *state.State, mapCache *mapping.Cache) bool {
	s.log.Warn("mapping unusable; skipping tick comparison",
		"error", logSafeUpstreamError(mapErr))
	return s.closeDegradedTick(ctx, "mapping-unusable", st, mapCache)
}

// tickComplete closes a tick that did its whole job - CAUSE: healthy, which
// includes a tick whose window was empty. No streak advances: the arm that
// reached here already settled the counters its own evidence covers. It is the
// ONE site that emits the completed-tick line, whose message and attribute set
// alerts.yaml's loop deadman pins.
func (s *Scout) tickComplete(ctx context.Context, entries, findings int, st *state.State, mapCache *mapping.Cache) bool {
	s.saveTick(ctx, st, mapCache)
	s.log.Info("tick complete",
		"seadex_entries", entries, "findings", findings,
		"window", changeWindow.String())
	return true
}

// closeDegradedTick is the shared tail of the three degraded CAUSES above, and
// is reached only through them: it persists what the pass learned, then re-states
// the finding set and emits the completion line. The re-statement is uniform
// because a degraded exit compared nothing, and the completion line keeps the
// loop deadman satisfied through an upstream outage, so its absence means only
// "loop wedged". Always healthy: a tick performs no walk.
func (s *Scout) closeDegradedTick(ctx context.Context, reason string, st *state.State, mapCache *mapping.Cache, attrs ...any) bool {
	s.saveTick(ctx, st, mapCache)
	s.notifier.Reemit()
	s.logTickDegraded(reason, attrs...)
	return true
}

// logTickDegraded is the ONE site that emits the degraded-tick line. It is
// separate from closeDegradedTick because the not-ready arm needs the line
// without the re-statement, and alerts.yaml's stall rule pins this message.
func (s *Scout) logTickDegraded(reason string, attrs ...any) {
	s.log.Warn("tick degraded", append([]any{"reason", reason}, attrs...)...)
}

// warnUnreachableUpstream is the unreachable-upstream fault's diagnostic: it
// reports a tick that could not read SeaDex and escalates a sustained run to
// ERROR. The streak itself is advanced by tickUnreadableUpstream, so no exit
// picks a counter. It re-fires at and above the threshold, so a count-based rule
// keeps firing while the condition holds.
func (s *Scout) warnUnreachableUpstream(msg string, err error) {
	s.escalate(s.unreachableRun, s.latchTicks(frozenFastPathTolerance), msg,
		"SeaDex has been unreadable on every recent tick; the fast path is blind and only the daily reconcile is refreshing - inspect releases.moe reachability and egress",
		attrError, logSafeUpstreamError(err),
		"consecutive_unreachable_ticks", s.unreachableRun)
}

// warnOversizeWindow reports a window too large to fetch, escalating a sustained
// run to ERROR: while it holds, the fast path is frozen and only the reconcile
// is working. The remedy is to wait for the reconcile or check the clock - one
// running BEHIND widens every window the same way a bulk upstream edit does.
func (s *Scout) warnOversizeWindow(count int) {
	s.escalate(s.oversizeRun, s.latchTicks(frozenFastPathTolerance),
		"SeaDex change window too large to fetch; deferring to the reconcile",
		"SeaDex change window has been too large to fetch repeatedly; the fast path is frozen and only the daily reconcile is refreshing - check this container's clock, then wait for the reconcile",
		"window_entries", count, "max", seadexapi.MaxWindowEntries,
		"consecutive_oversize_ticks", s.oversizeRun, "window", changeWindow.String())
}

// tickChanged runs the tick's work once the probe has said there is something to
// do and that it fits: fetch the window, advance the RSS journal from it, and
// report the findings its entries produce. It compares against the CACHED
// library snapshot rather than walking the arrs - the whole reason a tick is
// cheap, and the source of one accepted regression: an upgrade the operator
// performs is noticed only by the next reconcile. A bulk upstream edit landing
// between the two requests can make the window multi-page after all, and the
// walk pages it rather than refusing evidence the upstream already sent.
func (s *Scout) tickChanged(ctx context.Context, since time.Time, count int) bool {
	st := s.loadState(ctx)
	mapCache, idx, mapErr := s.loadMapping(ctx, &st)
	entries, err := s.seadex.FetchEntries(ctx,
		seadexapi.Options{Mode: seadexapi.FetchWindow, Since: since})
	if err != nil {
		if ctx.Err() != nil {
			// A redeploy cancelled the fetch: the CAUSE is the shutdown, not
			// SeaDex. The symmetric sibling of the probe's arm in tick.
			return s.tickInterrupted(ctx, "during the change window fetch", &st, &mapCache)
		}
		// The mapping load above may have accepted a freshly revalidated Fribb body;
		// the outcome persists it, because discarding it re-downloads ~5.9 MB.
		return s.tickUnreadableUpstream(ctx,
			"change window fetch failed; skipping tick", "window-fetch-failed", err,
			&st, &mapCache, "window_entries", count)
	}
	// The fast path READ the upstream, so this is where its unreachability streak
	// ends - a reset at the probe would cancel this boundary's own increment.
	s.unreachableRun = 0
	if !mapUsable(mapErr) {
		// The gate runs BEFORE the feed advance deliberately; see
		// tickMappingUnusable for the rule and for all three of its effects.
		return s.tickMappingUnusable(ctx, mapErr, &st, &mapCache)
	}
	s.advanceFeed(ctx, entries, idx, &st)
	result := s.matcher.Match(ctx, entries, &st.Library, idx, st.Memo)
	if ctx.Err() != nil {
		// Match returns early on a cancellation, so the match set is truncated and
		// publishing it would resolve entries this tick never finished evaluating.
		// The memo still holds the lookups that DID resolve.
		st.Memo = result.Memo
		return s.tickInterrupted(ctx, "before comparison", &st, &mapCache)
	}
	cleanMatches, failedItems := splitFailedMatches(result.Matches)
	findings := s.comparer.Compare(cleanMatches)
	// The report REPLACES only the rows of the entries this window EVALUATED and
	// preserves every row whose entry had incomplete evidence, which is what makes
	// a partial pass safe here at all.
	s.notifier.ReportScoped(findings, evaluatedIDs(result.Matches),
		unionIDs(failedItems, result.IncompleteIDs))
	st.Memo = result.Memo
	return s.tickComplete(ctx, len(entries), len(findings), &st, &mapCache)
}

// saveTick persists what a tick legitimately learned - the refreshed mapping
// cache and the AniList memo - and ONLY when it learned something. It never
// writes the library snapshot: a tick performs no walk. A nil st or mapCache
// means the exit was reached BEFORE the state load, so persistence is vacuous.
// The skip matters because state.json is one ~2.5 MB document dominated by that
// snapshot, so persisting a tick's few KB rewrites the whole file - ~230 MB/day
// of writes at ~92 productive ticks a day, buying nothing on a tick that
// renewed nothing.
func (s *Scout) saveTick(ctx context.Context, st *state.State, mapCache *mapping.Cache) {
	if st == nil || mapCache == nil {
		return
	}
	if !st.Memo.Changed() && !mappingWorthPersisting(&st.Mapping, mapCache) {
		s.log.Debug("tick learned nothing to persist; skipping the whole state.json write")
		return
	}
	st.Mapping = *mapCache
	s.save(ctx, st)
}

// mappingWorthPersisting reports whether a refreshed mapping cache differs from
// the persisted one in a way a future load would miss. FetchedAt is deliberately
// excluded, and is the whole reason this exists: almost every tick gets a 304
// and a fresh FetchedAt, so comparing whole Cache values would report a change
// every single tick and the skip would never fire. Validators plus the record
// count catch an accepted refresh, and RejectedRefreshes catches a refusal - the
// streak the mapping-rejection escalation reads, which must survive a restart.
func mappingWorthPersisting(prev, next *mapping.Cache) bool {
	return prev.ETag != next.ETag ||
		prev.LastModified != next.LastModified ||
		len(prev.Records) != len(next.Records) ||
		prev.RejectedRefreshes != next.RejectedRefreshes
}

// evaluatedIDs is the tick's deletion-authority set: the AniList IDs this window
// actually EVALUATED, which is not the same as the IDs it fetched. Deletion by
// absence is only sound where absence is evidence: an entry can end without a
// finding because it was compared and is aligned (absence means resolved), or
// because the linkage to a library item was lost (absence proves nothing - every
// such case yields match.SourceUnmapped, which is NOT in IncompleteIDs). A match
// linked to an item whose walk failed IS included, where preservation takes
// precedence. Library removal is not a tick's authority: it walked nothing.
func evaluatedIDs(matches []match.Match) map[int]struct{} {
	ids := make(map[int]struct{}, len(matches))
	for i := range matches {
		if m := &matches[i]; m.InLibrary() {
			ids[m.Entry.AniListID] = struct{}{}
		}
	}
	return ids
}
