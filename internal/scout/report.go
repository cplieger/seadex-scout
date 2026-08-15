package scout

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cplieger/httpx/v4"
	"github.com/cplieger/seadex-scout/internal/arrwalk"
	"github.com/cplieger/seadex-scout/internal/audit"
	"github.com/cplieger/seadex-scout/internal/degradation"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/seadexapi"
	"github.com/cplieger/seadex-scout/internal/shutdown"
	"github.com/cplieger/seadex-scout/internal/state"
)

// The one-shot REPORT: the read-only audit flow. It is a separate role TYPE
// rather than a second method on *Scout, mirroring the Deps/ReportDeps split:
// the two flows need disjoint components, so each runner carries only its own.

// Reporter runs the read-only one-shot audit over the shared core. It provably
// cannot compare, notify or rewrite the indexer feed: it has no field for any of
// them, so a mis-wired flow is a compile error rather than a nil-pointer panic.
type Reporter struct {
	core
	auditor *audit.Auditor
}

// NewReporter builds a Reporter for the read-only one-shot Report from deps.
func NewReporter(deps *ReportDeps) *Reporter {
	return &Reporter{
		core:    newCore(deps.Logger, deps.Store, deps.Library, deps.Mapping, deps.SeaDex, deps.Matcher),
		auditor: deps.Auditor,
	}
}

// reportSnapshot walks the library for a one-shot report, failing on a walk
// error or a partial snapshot: auditing an incomplete snapshot would publish a
// timestamped report that silently omits the skipped series.
func (r *Reporter) reportSnapshot(ctx context.Context) (library.Snapshot, error) {
	snap, err := r.library.Walk(ctx)
	if err != nil {
		// Reduce a transport *url.Error before it crosses the returned-report
		// boundary (main logs this at ERROR): the request URL may carry configured
		// userinfo credentials. The reduction also drops arrwalk.Walk's wrapper, so
		// name the failed side from the typed walk-side error.
		if arr := arrwalk.WalkErrArr(err); arr != "" {
			return library.Snapshot{}, fmt.Errorf("library walk (%s): %w", arr, httpx.LogSafeError(err))
		}
		return library.Snapshot{}, fmt.Errorf("library walk: %w", httpx.LogSafeError(err))
	}
	if snap.Partial {
		// The walk skipped series after episode-fetch failures - fail instead,
		// like a failed walk.
		return library.Snapshot{}, errors.New("library walk: partial snapshot after episode-fetch failures")
	}
	return snap, nil
}

// reportMapping loads the Fribb map for a one-shot report. An unusable map (no
// stale cache either) fails the report: ID matching, season scoping and the
// not_on_seadex catalogue all depend on it. A stale-but-usable map proceeds with
// a single degraded WARN, and a cancelled load is the shutdown, not a fault.
func (r *Reporter) reportMapping(ctx context.Context, st *state.State) (*mapping.Index, error) {
	mapCache, idx, mapErr := r.mapping.Load(ctx, &st.Mapping)
	if mapErr == nil || ctx.Err() != nil {
		return idx, nil
	}
	if !mapUsable(mapErr) {
		return nil, fmt.Errorf("mapping unusable: %w", mapErr)
	}
	// The report never escalates: it is a read-only one-shot and the operator is
	// watching its exit. The streak is still reported, for parity with the cycle.
	r.log.Warn("report: mapping degraded", mappingDegradedAttrs(mapErr, idx.Len(), mapCache.RejectedRefreshes)...)
	return idx, nil
}

// Report runs a one-shot SeaDex-alignment audit over the current library and
// returns the report. It is read-only on persisted state, so it is safe to run
// on demand while the daemon's cycle runs: the shared clients are
// concurrency-safe and each run carries its own state copy. It errors when the
// library walk, mapping load or SeaDex fetch cannot produce a complete audit, or
// when a shutdown interrupts matching. A transient AniList failure does not
// abort: the affected entries ride the report's incomplete-mapping section.
func (r *Reporter) Report(ctx context.Context) (audit.Report, error) {
	start := time.Now()
	st := r.loadState(ctx)

	snap, err := r.reportSnapshot(ctx)
	if err != nil {
		return audit.Report{}, err
	}
	if len(st.Library.Items) > 0 && degradation.Shrunk(len(snap.Items), len(st.Library.Items)) {
		// The daemon applies the same below-half test per ARR, on that arr's own
		// prior count. Here the report still renders - it is read-only and is the
		// operator's fallback view when a side is withheld - but it must SAY so, or
		// the timestamped artifact silently omits every missing series. The
		// aggregate reading is deliberate: a read-only path carries no per-arr
		// streak. No prior snapshot means no baseline, so the check no-ops.
		r.log.Warn("report: library walk shrank below half the last persisted snapshot; the audit covers the smaller library - inspect the arrs and arr_tags",
			"items", len(snap.Items), "prior_items", len(st.Library.Items))
	}

	idx, err := r.reportMapping(ctx, &st)
	if err != nil {
		return audit.Report{}, err
	}

	entries, err := r.seadex.FetchEntries(ctx, seadexapi.Options{Mode: seadexapi.FetchFull})
	if err != nil {
		// Every returned error is reduced first: the text can embed raw upstream
		// bytes bounded only by the page wire cap. A cancelled fetch additionally
		// keeps the shutdown token, so main still matches context.Canceled.
		safeErr := logSafeUpstreamError(err)
		if ctx.Err() != nil && (errors.Is(err, ctx.Err()) || errors.Is(err, context.Cause(ctx))) {
			return audit.Report{}, fmt.Errorf("seadex fetch: %w (cause: %v)", ctx.Err(), safeErr)
		}
		return audit.Report{}, fmt.Errorf("seadex fetch: %w", safeErr)
	}
	r.warnCatalogueLinkQuality(entries)
	if len(entries) == 0 {
		// Defense in depth: FetchEntries errors on an empty completed catalogue, but
		// a future regression returning (nil, nil) would publish a report marking
		// every library item not_on_seadex. Refuse instead, mirroring Cycle.
		return audit.Report{}, errors.New("seadex fetch: returned zero entries")
	}

	result := r.matcher.Match(ctx, entries, &snap, idx, st.Memo)
	if ctx.Err() != nil {
		// A shutdown arrived during or right after matching. The match set may be
		// truncated, and even a complete one should not spend the shutdown grace
		// period building and persisting a full audit. The wrap carries ctx.Err()
		// for main's shutdown classification plus the signal cause for display.
		return audit.Report{}, shutdown.InterruptedAs(ctx, "report interrupted")
	}
	if result.Degraded {
		// A transient AniList failure left some entries' mapping unresolved. Render
		// the audit anyway, with those entries in the incomplete-mapping section,
		// instead of aborting the run over a handful of unresolved ids.
		r.log.Warn("report: anilist degraded; affected entries listed in the incomplete section",
			"incomplete_lookups", len(result.IncompleteIDs))
	}
	rep := r.auditor.Audit(result.Matches, &snap, idx, result.IncompleteIDs)
	r.log.Info("report generated",
		"seadex_entries", len(entries),
		"library_items", len(snap.Items),
		"rows", len(rep.Rows),
		"incomplete_mappings", len(rep.Incomplete),
		"duration", time.Since(start).Round(time.Millisecond).String())
	return rep, nil
}
