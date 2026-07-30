package scout

import (
	"context"
	"time"

	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/match"
	"github.com/cplieger/seadex-scout/internal/seadex"
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

	// emptyRunLatch is the consecutive-empty-tick count that WARNs. An empty
	// 48h window means 48h of upstream silence has already elapsed, so this
	// threshold is (192 * 15m) = 48h of empty probes ON TOP of that, i.e. ~96h
	// of total silence. Measured against 90 days of upstream history the
	// longest genuine silence was 86.6h - which is 154 ticks - so a lower
	// latch would fire on healthy behaviour. It stays a WARN because it IS
	// usually healthy.
	//
	// It fires ONCE, at ==, while the two latches below re-fire at >=. The
	// asymmetry is deliberate in both directions: this one is usually a healthy
	// upstream and per-tick WARN spam would be noise, while those two are faults
	// whose ERROR must keep the count-based Loki rule firing for as long as the
	// condition holds.
	emptyRunLatch = 192

	// oversizeRunLatch is the consecutive-oversize-tick count that ERRORs. This
	// one pages, and at ERROR rather than WARN, because nothing in this stack
	// alerts on WARN: an oversize window means the fast path is frozen (no new
	// RSS items, no new findings) and only the reconcile is still working. 8
	// ticks is 2h at the default interval, matching the fleet's other
	// escalation thresholds.
	oversizeRunLatch = 8

	// unreachableRunLatch is the consecutive-unreadable-upstream-tick count
	// that ERRORs, and it is the tick's half of the SeadexFailures streak.
	// recordSeaDexFetch only runs inside the reconcile, so without this a fast
	// path that can never read SeaDex - a filter the upstream rejects, an
	// envelope larger than maxProbeBytes, an egress rule that blocks the
	// probe's query shape - would WARN every 15 minutes and reach the ERROR
	// this stack alerts on only after two DAYS. 8 ticks is 2h, the same number
	// and the same reasoning as the oversize latch.
	unreachableRunLatch = 8
)

// tick runs one bounded recent-changes pass. It is healthy whenever it
// completed, including when it found nothing: an empty window is a successful
// tick, and the marker it commits attests that the loop is alive, not that
// anything changed.
//
// EVERY exit emits one completion line and re-states the finding set, because
// both are what the alerting contract reads:
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
		// state. Emit the liveness line and nothing else: an empty set must not
		// be published either, since "no findings" is a claim this tick cannot
		// make. The failed reconciles' own degraded lines and their escalation
		// are what tell the operator why.
		s.log.Warn("tick degraded", "reason", "awaiting-first-reconcile",
			"reconcile_attempts", s.reconcileRetries)
		return true
	}
	since := time.Now().Add(-changeWindow)
	count, err := s.deps.SeaDex.CountWindow(ctx, since)
	if err != nil {
		// A failed probe is a failed tick: it advances neither wedge counter
		// that measures upstream STATE (emptyRun/oversizeRun), but it does
		// advance the fast path's own unreachability streak, because
		// recordSeaDexFetch runs on the reconcile only and would otherwise take
		// two days to escalate this.
		s.warnUnreachableUpstream("change probe failed; skipping tick", err)
		return s.tickDegraded("probe-failed")
	}
	switch {
	case count == 0:
		s.emptyRun++
		s.oversizeRun, s.unreachableRun = 0, 0
		if s.emptyRun == emptyRunLatch {
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
		s.deps.Notifier.Reemit()
		s.log.Info("tick complete", "seadex_entries", 0, "findings", 0,
			"window", changeWindow.String())
		return true
	case count >= seadex.MaxWindowEntries:
		s.oversizeRun++
		s.emptyRun, s.unreachableRun = 0, 0
		s.warnOversizeWindow(count)
		return s.tickDegraded("window-oversize", "window_entries", count)
	}
	s.emptyRun, s.oversizeRun, s.unreachableRun = 0, 0, 0
	return s.tickChanged(ctx, since, count)
}

// tickDegraded closes a tick that ran but could not do its job: it re-states
// the finding set (nothing was compared, so nothing resolved) and emits the
// completion line the scan deadman counts. It mirrors cycleDegraded, in the
// same vocabulary, for the same reason - the deadman must stay satisfied
// through an upstream outage and go quiet only when the LOOP stops, which is
// the only condition its restart runbook fits. Always healthy: a tick performs
// no walk, and health follows the library ingest.
func (s *Scout) tickDegraded(reason string, attrs ...any) bool {
	s.deps.Notifier.Reemit()
	s.log.Warn("tick degraded", append([]any{"reason", reason}, attrs...)...)
	return true
}

// warnUnreachableUpstream reports a tick that could not read SeaDex and
// escalates a sustained run to ERROR. It is the tick-cadence half of the
// persisted SeadexFailures streak (see unreachableRunLatch): that streak
// advances only inside the reconcile, so without this the fast path could stay
// blind for two days before reaching the level this stack alerts on. Like the
// oversize latch it re-fires at and above the threshold, so a count-based rule
// keeps firing while the condition holds.
func (s *Scout) warnUnreachableUpstream(msg string, err error) {
	s.unreachableRun++
	attrs := []any{
		"error", logSafeUpstreamError(err),
		"consecutive_unreachable_ticks", s.unreachableRun,
	}
	if s.unreachableRun >= unreachableRunLatch {
		s.log.Error("SeaDex has been unreadable on every recent tick; the fast path is blind and only the daily reconcile is refreshing - inspect releases.moe reachability and egress",
			attrs...)
		return
	}
	s.log.Warn(msg, attrs...)
}

// warnOversizeWindow reports a window too large to fetch, escalating a
// sustained run to ERROR. It escalates because nothing in this stack alerts on
// WARN: while this holds, the fast path is frozen - no new RSS item, no new
// finding - and only the reconcile is still working, which is a real fault with
// a real remedy (wait for the reconcile, or check the clock, since a clock
// running BEHIND widens every window the same way a bulk upstream edit does).
func (s *Scout) warnOversizeWindow(count int) {
	attrs := []any{
		"window_entries", count, "max", seadex.MaxWindowEntries,
		"consecutive_oversize_ticks", s.oversizeRun, "window", changeWindow.String(),
	}
	if s.oversizeRun >= oversizeRunLatch {
		s.log.Error("SeaDex change window has been too large to fetch repeatedly; the fast path is frozen and only the daily reconcile is refreshing - check this container's clock, then wait for the reconcile",
			attrs...)
		return
	}
	s.log.Warn("SeaDex change window too large to fetch; deferring to the reconcile", attrs...)
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
	entries, err := s.deps.SeaDex.FetchEntries(ctx,
		seadex.Options{Mode: seadex.FetchWindow, Since: since})
	if err != nil {
		s.warnUnreachableUpstream("change window fetch failed; skipping tick", err)
		// The mapping load above may have accepted a freshly revalidated Fribb
		// body; discarding it re-downloads ~5.9 MB on the next tick, which
		// during a SeaDex outage is every 15 minutes. Every gated reconcile
		// path persists it for the same reason.
		s.saveTick(ctx, &st, &mapCache)
		return s.tickDegraded("window-fetch-failed", "window_entries", count)
	}
	s.advanceFeed(ctx, entries, idx, &st)
	if mapErr != nil && idx == nil {
		// No usable mapping means nothing can be matched, so a compare would
		// produce an empty finding set for entries that may well be
		// misaligned - and reporting that would stop reporting conditions that
		// are still true. The feed advance above was already attempted; with no
		// index it returned early too, since the title synthesis needs one.
		s.log.Warn("mapping unusable; skipping tick comparison",
			"error", logSafeUpstreamError(mapErr))
		s.saveTick(ctx, &st, &mapCache)
		return s.tickDegraded("mapping-unusable")
	}
	result := s.deps.Matcher.Match(ctx, entries, &st.Library, idx, st.Memo)
	if ctx.Err() != nil {
		s.log.Warn("tick interrupted by shutdown before comparison", "cause", context.Cause(ctx))
		return true
	}
	cleanMatches, failedItems := splitFailedMatches(result.Matches)
	findings := s.deps.Comparer.Compare(cleanMatches)
	// The report REPLACES only the rows of the entries this window EVALUATED, and
	// preserves every row whose entry had incomplete evidence. Rows for entries
	// outside the window are untouched, which is what makes a partial pass safe
	// here at all.
	s.deps.Notifier.ReportScoped(findings, evaluatedIDs(result.Matches),
		unionIDs(failedItems, result.IncompleteIDs))
	st.Memo = result.Memo
	s.saveTick(ctx, &st, &mapCache)
	s.log.Info("tick complete",
		"seadex_entries", len(entries), "findings", len(findings),
		"window", changeWindow.String())
	return true
}

// saveTick persists what a tick legitimately learned: the refreshed mapping
// cache and the AniList memo. It never writes the library snapshot - a tick
// performs no walk, so it has no snapshot to write and must not overwrite the
// reconcile's with a stale copy.
//
// mapCache is taken by pointer for the same reason handlePreCompareGate takes
// its cache that way: mapping.Cache is heavy enough that a by-value parameter
// is a gocritic hugeParam. It is read, never retained.
func (s *Scout) saveTick(ctx context.Context, st *state.State, mapCache *mapping.Cache) {
	st.Mapping = *mapCache
	s.save(ctx, st)
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
