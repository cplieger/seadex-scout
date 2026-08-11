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
// reconcile. Cycle (scout.go) dispatches between the two; everything specific to
// the cheap path lives here - its window, its wedge diagnostics, its completion
// vocabulary, and the deletion authority a partial pass is allowed.

// The tick's window and its three wedge diagnostics.
const (
	// changeWindow is how far back a tick looks. It is deliberately much wider
	// than the tick interval: the window is the only thing that recovers a
	// missed tick, a restart, or a clock skewed by less than this, and a wider
	// window costs bytes proportional to the upstream's change RATE (measured
	// at ~2 new entries and ~15 torrent edits a day), not to its size.
	changeWindow = 48 * time.Hour

	// emptyRunSilence is the wall-clock run of consecutive empty ticks that
	// WARNs. An empty 48h window means 48h of upstream silence has already
	// elapsed, so this tolerance is 48h of empty probes ON TOP of that, i.e.
	// ~96h of total silence. Measured against 90 days of upstream history the
	// longest genuine silence was 86.6h - so a shorter tolerance would fire on
	// healthy behaviour. It stays a WARN because it IS usually healthy.
	//
	// It fires ONCE, at ==, while the two latches below re-fire at >=. The
	// asymmetry is deliberate in both directions: this one is usually a healthy
	// upstream and per-tick WARN spam would be noise, while those two are faults
	// whose ERROR must keep the count-based Loki rule firing for as long as the
	// condition holds.
	emptyRunSilence = changeWindow

	// frozenFastPathTolerance is how long the fast path may stay frozen before
	// the diagnostic ERRORs. It pages, and at ERROR rather than WARN, because
	// nothing in this stack alerts on WARN: an oversize window or an unreadable
	// upstream means the fast path is frozen (no new RSS items, no new findings)
	// and only the reconcile is still working. 2h matches the fleet's other
	// escalation thresholds, and it is the tolerance BOTH the oversize latch and
	// the unreachable latch use - the unreachable one is the tick's half of the
	// persisted SeadexFailures streak (recordSeaDexFetch only runs inside the
	// reconcile), so without it a fast path that can never read SeaDex - a filter
	// the upstream rejects, an envelope larger than maxProbeBytes, an egress rule
	// that blocks the probe's query shape - would WARN indefinitely and reach the
	// ERROR this stack alerts on only after days.
	frozenFastPathTolerance = 2 * time.Hour

	// minLatchTicks is the floor latchTicks applies, so one blip is always
	// tolerated however long the interval is.
	minLatchTicks = 2
)

// latchTicks converts a wall-clock tolerance into the number of CONSECUTIVE
// ticks that spans at this loop's own interval, floored at minLatchTicks so one
// blip is always tolerated. The latches used to be raw iteration counts, which
// only meant what their comments claimed at the 15m default: poll_interval is
// accepted up to 30 days, so an 8-tick "2h" escalation is 24h at the deployed
// 3h interval and a 192-tick "48h of empty probes" is 24 days - the same
// cadence-relative drift reconcileEscalationThreshold was re-expressed for.
// A zero or negative interval (external mode, an unwired test) runs no ticks at
// all, so the floor is the whole answer there.
func (s *Scout) latchTicks(d time.Duration) int {
	if s.pollInterval <= 0 {
		return minLatchTicks
	}
	return max(minLatchTicks, int(d/s.pollInterval))
}

// tick runs one bounded recent-changes pass. It is healthy whenever it
// completed, including when it found nothing: an empty window is a successful
// tick, and the marker it commits attests that the loop is alive, not that
// anything changed.
//
// Every exit that RAN emits one completion line and re-states the finding set,
// because both are what the alerting contract reads:
//
//   - "tick complete" (the tick did its whole job, which includes finding
//     nothing to do) or "tick degraded" (it ran but could not) is what the
//     scan deadman counts, exactly as "cycle complete"/"cycle degraded" is for
//     a reconcile. A silent exit is indistinguishable from a wedged loop, and
//     the measured upstream has quiet runs of 154 consecutive empty ticks - so
//     a deadman watching only the productive path would page on healthy data
//     with a restart runbook.
//   - the finding set is re-emitted (Notifier.Reemit) on every exit that did
//     not compare, so the rules' lookback keeps seeing standing conditions.
//     Nothing was learned that could resolve them, and silence longer than the
//     lookback resolves all of them and then re-fires the whole set.
//
// A tick a SHUTDOWN cancelled emits NEITHER, and that is not an exception to the
// contract above but the other half of it: an interrupted pass did not run to an
// end, so counting it would turn a redeploy into a pass that ran and failed, and
// publishing its truncated match set would resolve entries it never finished
// evaluating. The reconcile applies the same rule at all four of its cancellable
// boundaries. Which effects each ending carries is decided in ONE place per
// cause - see "the tick's outcomes" below.
//
// It costs one ~88-byte probe plus, when there is something to fetch, one
// request of a few tens of KiB. It does NOT walk the arrs (that is ~1k requests
// and it is what gates container health), does NOT touch the search curation
// index (a window can only add to it, and an add-only index cannot express a
// de-curation), and does NOT rebuild the feed - it ADVANCES it (see
// FeedWriter.Advance).
func (s *Scout) tick(ctx context.Context) bool {
	if !s.ready {
		// No complete pass has established the finding set yet (the startup
		// reconcile failed or was gated, and the retry budget is spent), so
		// this tick's handful of findings would publish as the app's whole
		// state. Emit the liveness line and nothing else: the set is provably
		// empty here (only a reconcile's Report can fill it, and that is what
		// sets ready), and an empty "findings reported" line reads as "no
		// findings" - a claim this tick cannot make. The failed reconciles' own
		// degraded lines and their escalation are what tell the operator why.
		s.logTickDegraded("awaiting-first-reconcile", "reconcile_attempts", s.reconcileRetries)
		return true
	}
	since := time.Now().Add(-changeWindow)
	count, err := s.seadex.CountWindow(ctx, since)
	if err != nil {
		if ctx.Err() != nil {
			// A redeploy cancelled the probe. The CAUSE is the shutdown, not
			// SeaDex - naming it is the whole of what this arm does, and the
			// outcome decides every effect that follows from it.
			return s.tickInterrupted(ctx, "during the change probe", nil, nil)
		}
		// A failed probe is a failed tick, and the fault is the upstream's.
		return s.tickUnreadableUpstream(ctx,
			"change probe failed; skipping tick", "probe-failed", err, nil, nil)
	}
	switch {
	case count == 0:
		s.emptyRun++
		s.oversizeRun, s.unreachableRun = 0, 0
		if s.emptyRun == s.latchTicks(emptyRunSilence) {
			// Usually healthy - the upstream is simply quiet - but a window
			// that can NEVER hold anything looks identical, and there are two
			// ways that happens: a container clock running more than
			// changeWindow ahead, which puts every window in the upstream's
			// future, or something answering the probe with a plausible 200
			// that is not SeaDex (a captive portal, a challenge page). The
			// probe has no independent evidence to catch the second - the full
			// walk has warnCatalogueShrink, a count has nothing - so this latch
			// is the only signal for it, and the reconcile bounds the damage at
			// one day.
			s.log.Warn("no SeaDex change seen for a very long run of ticks; if this persists, check this container's clock against the upstream, and that the probe is reaching releases.moe rather than something answering for it",
				"consecutive_empty_ticks", s.emptyRun, "window", changeWindow.String())
		}
		// A complete tick: the probe answered, and the answer was "nothing".
		// Nothing was compared, so the standing set is re-stated rather than
		// replaced; the outcome owns the completion line.
		s.notifier.Reemit()
		return s.tickComplete(ctx, 0, 0, nil, nil)
	case count >= seadexapi.MaxWindowEntries:
		// This EXIT's own evidence settles the two other counters: the probe
		// read the upstream, and the answer was not empty. The oversize streak
		// is advanced by the outcome, where every effect of this cause lives.
		s.emptyRun, s.unreachableRun = 0, 0
		return s.tickOversizeWindow(ctx, count)
	}
	// Not an exit: the tick continues into a window fetch, so only the two
	// counters that measure upstream STATE are settled here. Reachability is not
	// established until that fetch returns, and unreachableRun is reset THERE -
	// a counter is reset only where its own evidence arrived. Resetting it here
	// cancelled the increment the same tick's fetch failure makes, capping that
	// half of the streak at 1 and putting frozenFastPathTolerance out of reach
	// through the fetch boundary entirely.
	s.emptyRun, s.oversizeRun = 0, 0
	return s.tickChanged(ctx, since, count)
}

// --- the tick's outcomes: one per CAUSE ---
//
// A tick exit names ONE cause, and the cause alone decides that exit's three
// effects: which degradation streak advances, whether the pass persists what it
// learned, and which completion line it emits. Every arm in tick and tickChanged
// does exactly one thing - name its cause and hand over what it is holding - so
// no call site chooses a streak, a persist, or a message, and a new exit cannot
// quietly assemble a different combination.
//
// This mirrors the reconcile, which already proves the model in this package:
// each of its outcomes is a named function owning its own effects
// (handleLibraryGate, handleUpstreamGate, finishInterruptedMatch,
// finishCompletedCycle), it asks ctx.Err() first at every cancellable boundary,
// and it emits its completion line from one place (logCompletedCycle). The tick
// used to borrow that vocabulary - the same warnUnreachableUpstream, the same
// reason strings - through a single thin emitter that took a reason STRING and
// hard-wired two effects, so cause was not an input, persistence was not an
// effect at all, and the one cause the tick never named (a shutdown) had no arm
// at two of its three boundaries.
//
// What these outcomes deliberately do NOT own is the finding set. Reemit-vs-
// Report is a statement about what the pass LEARNED rather than about why it
// ended, and the two healthy exits differ on it (an empty window re-states, a
// productive one reports what it compared), so each arm makes that call before
// its outcome closes. It IS uniform by cause on the degraded side - every
// degraded exit compared nothing - so closeDegradedTick re-states for all three.
//
// The not-ready gate in tick is not one of the four. It exits before the tick
// does any work at all, so it has no cause to name and nothing to persist; it
// reaches logTickDegraded directly for the line, deliberately without the
// re-statement (see tick for why an empty set must not be published).

// tickInterrupted closes a tick a shutdown cancelled - CAUSE: shutdown. It is
// the tick's half of the rule every interrupted reconcile arm follows, and all
// three effects follow from the cause alone:
//
//   - NO streak advances. A redeploy is not an upstream fault, so it must never
//     walk toward the "inspect releases.moe reachability and egress" escalation,
//     and the operator is not sent to look at their egress for something their
//     own restart caused.
//   - It ALWAYS persists what the pass already learned, exactly as the
//     reconcile's finishInterruptedMatch does: the revalidated Fribb validators
//     (a discarded revalidation re-downloads ~5.9 MB on the next boot), the memo
//     entries that did resolve, and the persisted refresh-rejection streak the
//     mapping escalation reads - a streak that only means anything if it
//     survives a restart. Never the library snapshot: a tick performs no walk.
//     Scout.save owns the cancelled-write retry, inside the container stop grace.
//   - It emits NO completion line and no re-statement, because an interrupted
//     pass did not complete, and its match set is truncated (match.Matcher.Match
//     returns early on cancellation) - publishing that set would hand
//     ReportScoped deletion authority over entries the tick never finished
//     evaluating.
//
// st and mapCache are nil at the one boundary the pass reaches before loading
// state; persistence is then vacuous rather than declined.
//
// Always healthy: a tick performs no walk, and health follows the library
// ingest.
func (s *Scout) tickInterrupted(ctx context.Context, stage string, st *state.State, mapCache *mapping.Cache) bool {
	s.log.Warn("tick interrupted by shutdown "+stage, "cause", context.Cause(ctx))
	s.saveTick(ctx, st, mapCache)
	return true
}

// tickUnreadableUpstream closes a tick that could not read SeaDex at either of
// its two reads - CAUSE: upstream fault. It advances the fast path's own
// unreachability streak, which is the tick-cadence half of the persisted
// SeadexFailures streak (that one advances only inside a reconcile, so without
// this a blind fast path would WARN for two days before reaching the level this
// stack alerts on); it persists whatever the pass had already learned; and it
// emits the degraded completion line the loop deadman counts.
func (s *Scout) tickUnreadableUpstream(ctx context.Context, msg, reason string, err error, st *state.State, mapCache *mapping.Cache, attrs ...any) bool {
	s.unreachableRun++
	s.warnUnreachableUpstream(msg, err)
	return s.closeDegradedTick(ctx, reason, st, mapCache, attrs...)
}

// tickOversizeWindow closes a tick whose window is too large to fetch - CAUSE:
// upstream fault, the second one the fast path has. It advances the oversize
// streak rather than the unreachability one because the two have different
// remedies (wait for the reconcile, or check this container's clock, against
// inspect reachability and egress), it has nothing to persist (this exit is
// reached before the state load), and it emits the degraded completion line.
func (s *Scout) tickOversizeWindow(ctx context.Context, count int) bool {
	s.oversizeRun++
	s.warnOversizeWindow(count)
	return s.closeDegradedTick(ctx, "window-oversize", nil, nil, "window_entries", count)
}

// tickMappingUnusable closes a tick with no usable Fribb index - CAUSE: mapping
// unusable. No upstream streak advances (the streak this condition feeds is the
// mapping loader's own, persisted, and already advanced by the load itself), the
// refreshed cache IS persisted so that streak survives a restart, and the
// degraded completion line carries reason=mapping-unusable.
//
// Nothing can be matched without an index, so a compare would produce an empty
// finding set for entries that may well be misaligned, and reporting that would
// stop reporting conditions that are still true. The feed advance is skipped for
// the same reason rebuildFeed skips it (an unusable map types every entry as
// anime and drops SeaDex movies from Radarr's RSS view), which is why the caller
// applies this gate BEFORE it. mapUsable is the one home of the question, shared
// with the reconcile: a stale-but-usable map (*mapping.StaleMapError, which
// carries a usable cached index) still advances.
func (s *Scout) tickMappingUnusable(ctx context.Context, mapErr error, st *state.State, mapCache *mapping.Cache) bool {
	s.log.Warn("mapping unusable; skipping tick comparison",
		"error", logSafeUpstreamError(mapErr))
	return s.closeDegradedTick(ctx, "mapping-unusable", st, mapCache)
}

// tickComplete closes a tick that did its whole job - CAUSE: healthy, which
// includes a tick whose window was empty (the probe answered, and the answer was
// "nothing"). No streak advances: the arm that reached here already settled the
// counters its own evidence covers. It persists what the pass learned, and it is
// the ONE site that emits the completed-tick line, for the same reason
// logTickDegraded is the only site for its twin - the message is pinned by
// alerts.yaml's loop deadman and its attribute set is what an operator reads on a
// quiet tick, so two emitters are a string and a schema two callers can drift
// apart.
func (s *Scout) tickComplete(ctx context.Context, entries, findings int, st *state.State, mapCache *mapping.Cache) bool {
	s.saveTick(ctx, st, mapCache)
	s.log.Info("tick complete",
		"seadex_entries", entries, "findings", findings,
		"window", changeWindow.String())
	return true
}

// closeDegradedTick is the shared tail of the three degraded CAUSES above, and it
// is reached only through them - never from an exit directly, which is what stops
// a call site from assembling its own set of effects again. It applies the two
// effects every degraded cause shares: persist what the pass learned, then
// re-state the finding set and emit the completion line.
//
// The re-statement is uniform here because a degraded exit compared nothing, so
// nothing was learned that could resolve a standing finding - and silence longer
// than the alert rules' lookback resolves every open row and then re-fires the
// whole set as new (notify.Reemit). The completion line keeps the loop deadman
// satisfied through an upstream outage, so its absence means only "loop wedged",
// which is the only condition its restart runbook fits. Always healthy: a tick
// performs no walk.
func (s *Scout) closeDegradedTick(ctx context.Context, reason string, st *state.State, mapCache *mapping.Cache, attrs ...any) bool {
	s.saveTick(ctx, st, mapCache)
	s.notifier.Reemit()
	s.logTickDegraded(reason, attrs...)
	return true
}

// logTickDegraded is the ONE site that emits the degraded-tick line. It is
// separate from closeDegradedTick because the not-ready arm needs the line
// without the re-statement, and this message is pinned by alerts.yaml's stall
// rule - a second emission site is a string two callers can drift apart, which is
// the same reason the escalation ERRORs each have exactly one.
func (s *Scout) logTickDegraded(reason string, attrs ...any) {
	s.log.Warn("tick degraded", append([]any{"reason", reason}, attrs...)...)
}

// warnUnreachableUpstream is the unreachable-upstream fault's diagnostic: it
// reports a tick that could not read SeaDex and escalates a sustained run to
// ERROR. The streak itself is advanced by tickUnreadableUpstream, the outcome
// that owns this cause - the same way tickOversizeWindow owns the oversize one -
// so no exit picks a counter. See frozenFastPathTolerance for why the tick needs
// a streak of its own at all. Like the oversize latch it re-fires at and above
// the threshold, so a count-based rule keeps firing while the condition holds.
func (s *Scout) warnUnreachableUpstream(msg string, err error) {
	s.escalate(s.unreachableRun, s.latchTicks(frozenFastPathTolerance), msg,
		"SeaDex has been unreadable on every recent tick; the fast path is blind and only the daily reconcile is refreshing - inspect releases.moe reachability and egress",
		attrError, logSafeUpstreamError(err),
		"consecutive_unreachable_ticks", s.unreachableRun)
}

// warnOversizeWindow reports a window too large to fetch, escalating a
// sustained run to ERROR. It escalates because nothing in this stack alerts on
// WARN: while this holds, the fast path is frozen - no new RSS item, no new
// finding - and only the reconcile is still working, which is a real fault with
// a real remedy (wait for the reconcile, or check the clock, since a clock
// running BEHIND widens every window the same way a bulk upstream edit does).
func (s *Scout) warnOversizeWindow(count int) {
	s.escalate(s.oversizeRun, s.latchTicks(frozenFastPathTolerance),
		"SeaDex change window too large to fetch; deferring to the reconcile",
		"SeaDex change window has been too large to fetch repeatedly; the fast path is frozen and only the daily reconcile is refreshing - check this container's clock, then wait for the reconcile",
		"window_entries", count, "max", seadexapi.MaxWindowEntries,
		"consecutive_oversize_ticks", s.oversizeRun, "window", changeWindow.String())
}

// tickChanged runs the tick's work once the probe has said there is something
// to do and that it fits: fetch the window, advance the RSS journal from it,
// and report the findings its entries produce.
//
// It compares against the CACHED library snapshot rather than walking the arrs.
// That is the whole reason a tick is cheap, and it is the source of this
// design's one accepted regression: an upgrade the operator performs is only
// noticed by the next reconcile, so a finding they have already acted on keeps
// being reported for up to reconcileInterval.
//
// The probe's answer is a moment old, not a guarantee: a bulk upstream edit
// landing between the two requests can make the window multi-page after all, and
// the walk pages it (every structural guard still applies and maxPages still
// bounds it). That is the one case where a tick costs more than one page, and it
// is the right direction - the alternative is refusing evidence the upstream
// already sent.
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
		// The mapping load above may have accepted a freshly revalidated Fribb
		// body; the outcome persists it, because discarding it re-downloads
		// ~5.9 MB on the next tick - during a SeaDex outage, every 15 minutes.
		return s.tickUnreadableUpstream(ctx,
			"change window fetch failed; skipping tick", "window-fetch-failed", err,
			&st, &mapCache, "window_entries", count)
	}
	// The fast path READ the upstream, so this is where its unreachability streak
	// ends - the reset the probe's success deliberately no longer performs,
	// because a reset there cancelled the increment this boundary makes within
	// the same tick.
	s.unreachableRun = 0
	if !mapUsable(mapErr) {
		// The gate runs BEFORE the feed advance deliberately; see
		// tickMappingUnusable for the rule and for all three of its effects.
		return s.tickMappingUnusable(ctx, mapErr, &st, &mapCache)
	}
	s.advanceFeed(ctx, entries, idx, &st)
	result := s.matcher.Match(ctx, entries, &st.Library, idx, st.Memo)
	if ctx.Err() != nil {
		// Match returns early on a cancellation, so the match set is truncated
		// and publishing it would resolve entries this tick never finished
		// evaluating. The memo still holds the lookups that DID resolve, and the
		// outcome persists them.
		st.Memo = result.Memo
		return s.tickInterrupted(ctx, "before comparison", &st, &mapCache)
	}
	cleanMatches, failedItems := splitFailedMatches(result.Matches)
	findings := s.comparer.Compare(cleanMatches)
	// The report REPLACES only the rows of the entries this window EVALUATED, and
	// preserves every row whose entry had incomplete evidence. Rows for entries
	// outside the window are untouched, which is what makes a partial pass safe
	// here at all.
	s.notifier.ReportScoped(findings, evaluatedIDs(result.Matches),
		unionIDs(failedItems, result.IncompleteIDs))
	st.Memo = result.Memo
	return s.tickComplete(ctx, len(entries), len(findings), &st, &mapCache)
}

// saveTick persists what a tick legitimately learned - the refreshed mapping
// cache and the AniList memo - and ONLY when it learned something. It never
// writes the library snapshot: a tick performs no walk, so it has no snapshot to
// write and must not overwrite the reconcile's with a stale copy.
//
// A nil st or mapCache means the exit was reached BEFORE the state load (the
// probe's two arms), so there is nothing to persist. Every outcome calls this
// unconditionally rather than each testing for itself whether persistence is one
// of its effects - the point of a cause-owned outcome is that the exit does not
// decide.
//
// The skip matters because state.json is one ~2.5 MB document dominated by that
// library snapshot, so persisting a tick's few KB of memo and validators rewrites
// the whole file. At ~92 productive ticks a day that is ~230 MB/day of writes on
// a flash-backed volume, and on a tick that renewed nothing it buys nothing.
//
// mapCache is taken by pointer for the same reason handlePreCompareGate takes
// its cache that way: mapping.Cache is heavy enough that a by-value parameter
// is a gocritic hugeParam. It is read, never retained.
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
// the persisted one in a way a future load would miss.
//
// FetchedAt is deliberately excluded, and it is the whole reason this function
// exists: the Fribb body changes about weekly, so almost every tick gets a 304
// and the loader returns the cached map with a fresh FetchedAt (mapping.go's
// not-modified arm). Comparing whole Cache values would therefore report a change
// on every single tick and the skip would never fire. Dropping a FetchedAt update
// costs nothing: the refresh interval is zero, so the loader revalidates every
// pass regardless of how old the persisted stamp claims to be.
//
// Validators are the real signal. A 200 that changed the records carries a new
// ETag or Last-Modified, so comparing those plus the record count catches an
// accepted refresh, and RejectedRefreshes catches a refusal - the streak the
// mapping-rejection escalation reads, which must survive a restart to mean
// anything.
func mappingWorthPersisting(prev, next *mapping.Cache) bool {
	return prev.ETag != next.ETag ||
		prev.LastModified != next.LastModified ||
		len(prev.Records) != len(next.Records) ||
		prev.RejectedRefreshes != next.RejectedRefreshes
}

// evaluatedIDs is the tick's deletion-authority set: the AniList IDs this
// window actually EVALUATED, which is not the same as the IDs it fetched.
//
// Deletion by absence is only sound where absence is evidence. An entry the
// window carried can end without a finding for two very different reasons: it
// was compared and is aligned (absence means resolved - authority), or the
// linkage to a library item was lost (absence means the app can no longer tell -
// no authority). The second is reachable without any transient failure: a
// mapping record whose arr id no longer resolves to a cached item, a definitive
// AniList not-found, an unusable AniList record, a title match with no unique
// candidate. Every one of those yields match.SourceUnmapped, which is NOT in
// IncompleteIDs, so granting authority over it would delete the entry's standing
// rows and silently resolve an alert whose condition still holds.
//
// A match linked to an item whose walk failed IS included: it is authorized and
// simultaneously in the incomplete set, where preservation takes precedence -
// the same relationship a full pass has.
//
// Removal from the library is deliberately NOT in a tick's authority. A tick
// compares against the cached snapshot it did not walk, so it cannot distinguish
// a removed item from a mapping that stopped resolving; the reconcile, which
// walks, is what resolves those rows.
func evaluatedIDs(matches []match.Match) map[int]struct{} {
	ids := make(map[int]struct{}, len(matches))
	for i := range matches {
		if m := &matches[i]; m.InLibrary() {
			ids[m.Entry.AniListID] = struct{}{}
		}
	}
	return ids
}
