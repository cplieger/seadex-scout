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
	"github.com/cplieger/seadex-scout/internal/state"
)

// The one-shot REPORT: the read-only audit flow, the entry point that is not a
// compare cycle. It is a separate role TYPE rather than a second method on
// *Scout (scout.go), mirroring the Deps/ReportDeps split one level up: the two
// flows need disjoint components, so each runner carries only its own.

// Reporter runs the read-only one-shot audit over the shared core. It provably
// cannot compare, notify or rewrite the indexer feed: it has no field for any of
// them, so a mis-wired flow is a compile error rather than a nil-pointer panic
// at the first component the role never carried.
type Reporter struct {
	core
	auditor *audit.Auditor
}

// NewReporter builds a Reporter for the read-only one-shot Report from deps. The
// compare-cycle components are not merely unset but absent from the type, and
// the root never constructs them.
func NewReporter(deps *ReportDeps) *Reporter {
	return &Reporter{
		core:    newCore(deps.Logger, deps.Store, deps.Library, deps.Mapping, deps.SeaDex, deps.Matcher),
		auditor: deps.Auditor,
	}
}

// reportSnapshot walks the library for a one-shot report, failing on a walk
// error or a partial snapshot: auditing an incomplete snapshot would publish a
// successful, timestamped report that silently omits the skipped series,
// contradicting the whole-library audit contract.
func (r *Reporter) reportSnapshot(ctx context.Context) (library.Snapshot, error) {
	snap, err := r.library.Walk(ctx)
	if err != nil {
		// Reduce a transport *url.Error before it crosses the returned-report
		// boundary (main logs this error at ERROR): the request URL inside it
		// may carry configured userinfo credentials. The reduction also drops
		// arrwalk.Walk's "walking sonarr/radarr" wrapper, so name the failed
		// side from the typed walk-side error - the same recovery
		// walkFailureAttrs performs for the cycle's log boundaries.
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

// reportMapping loads the Fribb map for a one-shot report. An unusable map
// (no stale cache either) fails the report: ID matching, season scoping, and
// the not_on_seadex catalogue all depend on it, so publishing would contradict
// the whole-library audit contract (the daemon gate refuses to compare on this
// too). A stale-but-usable map proceeds with a single degraded WARN. A
// cancelled load is the shutdown, not a Fribb fault (the SeaDex fetch after
// this then fails with the cancellation and Report returns it).
func (r *Reporter) reportMapping(ctx context.Context, st *state.State) (*mapping.Index, error) {
	mapCache, idx, mapErr := r.mapping.Load(ctx, &st.Mapping)
	if mapErr == nil || ctx.Err() != nil {
		return idx, nil
	}
	if !mapUsable(mapErr) {
		return nil, fmt.Errorf("mapping unusable: %w", mapErr)
	}
	// The report never escalates: it is a read-only one-shot, and the operator
	// is watching its exit. The streak is still reported for parity with the
	// cycle's attribute set.
	r.log.Warn("report: mapping degraded", mappingDegradedAttrs(mapErr, idx.Len(), mapCache.RejectedRefreshes)...)
	return idx, nil
}

// Report runs a one-shot SeaDex-alignment audit over the current library and
// returns the report. It is read-only on persisted state (it loads the mapping
// cache and AniList memo to avoid needless refetching, but never saves), so it
// is safe to run on demand while the daemon's cycle runs: the shared clients are
// concurrency-safe and each run carries its own state copy. It returns an error
// when the library walk, mapping load, or SeaDex fetch cannot produce a
// complete audit, or when a shutdown interrupts matching. A transient AniList
// failure no longer aborts: the report renders with the affected entries in
// its explicit incomplete-mapping section and the completeness caveat in its
// header, so the unaffected majority is not withheld over a few unresolved ids.
func (r *Reporter) Report(ctx context.Context) (audit.Report, error) {
	start := time.Now()
	st := r.loadState(ctx)

	snap, err := r.reportSnapshot(ctx)
	if err != nil {
		return audit.Report{}, err
	}
	if len(st.Library.Items) > 0 && degradation.Shrunk(len(snap.Items), len(st.Library.Items)) {
		// The daemon applies the same below-half test per ARR, on that arr's own
		// prior count (mergeShrunkSides): a non-failed walk retaining under
		// half the last persisted snapshot is a suspicious truncation, not a
		// real change. There the suspect side's prior items are carried into
		// the compare until the guard's tolerance expires; here the report
		// still renders - it is read-only, cannot influence the daemon's guard,
		// and is the operator's fallback view when a side is withheld - but it
		// must SAY so, or the timestamped artifact silently omits every missing
		// series, exactly the incompleteness reportSnapshot refuses a partial
		// snapshot over. The aggregate reading is deliberate for this WARN: the
		// report covers whatever this walk returned, whole-library, and a
		// read-only path carries no per-arr streak and withholds nothing. No
		// prior snapshot (report-only deployments never persist one) means no
		// baseline, so the check no-ops there rather than guessing.
		r.log.Warn("report: library walk shrank below half the last persisted snapshot; the audit covers the smaller library - inspect the arrs and arr_tags",
			"items", len(snap.Items), "prior_items", len(st.Library.Items))
	}

	idx, err := r.reportMapping(ctx, &st)
	if err != nil {
		return audit.Report{}, err
	}

	entries, err := r.seadex.FetchEntries(ctx, seadexapi.Options{Mode: seadexapi.FetchFull})
	if err != nil {
		// Every returned error is reduced first: the error text can embed raw
		// upstream bytes bounded only by the page wire cap, and that guarantee
		// must not become conditional when a shutdown races a fetch failure.
		// A cancelled fetch additionally keeps the shutdown token so main's
		// dispatchOutcome still matches context.Canceled (WARN, not the
		// cycle-error ERROR); the bounded text rides along as the cause.
		safeErr := logSafeUpstreamError(err)
		if ctx.Err() != nil && (errors.Is(err, ctx.Err()) || errors.Is(err, context.Cause(ctx))) {
			return audit.Report{}, fmt.Errorf("seadex fetch: %w (cause: %v)", ctx.Err(), safeErr)
		}
		return audit.Report{}, fmt.Errorf("seadex fetch: %w", safeErr)
	}
	r.warnCatalogueLinkQuality(entries)
	if len(entries) == 0 {
		// Defense in depth: FetchEntries errors on an empty completed
		// catalogue, but a future client regression returning (nil, nil) would
		// otherwise publish a successful report marking every library item
		// not_on_seadex - refuse instead, mirroring Cycle's zero-entries
		// degradation gate.
		return audit.Report{}, errors.New("seadex fetch: returned zero entries")
	}

	result := r.matcher.Match(ctx, entries, &snap, idx, st.Memo)
	if ctx.Err() != nil {
		// A shutdown arrived during or right after matching. The match set may
		// be truncated (entries after the cancellation were never attempted),
		// and even a complete one should not spend the shutdown grace period
		// building, logging, and persisting a full audit: the signal context
		// is one report-wide budget, so stop here. The wrap carries ctx.Err()
		// for main's shutdown classification (errors.Is context.Canceled,
		// keeping a routine SIGTERM off the ERROR alert) plus the signal
		// cause for display.
		return audit.Report{}, fmt.Errorf("report interrupted: %w (cause: %w)", ctx.Err(), context.Cause(ctx))
	}
	if result.Degraded {
		// A transient AniList failure left some entries' library mapping
		// unresolved. Render the audit anyway - the unaffected majority is
		// complete - with the affected entries listed in the report's
		// incomplete-mapping section and the caveat in its header, instead of
		// aborting the whole run over a handful of unresolved ids.
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
