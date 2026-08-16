// Package scout orchestrates one compare cycle: load state, walk the library,
// refresh the ID map, pull SeaDex, match entries to library items, compare, and
// report findings, then persist the caches.
//
// Cycle health follows the library ingest: a failed arr walk is unhealthy (a
// restart or config fix could recover it), while a SeaDex, mapping or AniList
// failure is degraded but healthy and preserves prior findings rather than
// falsely resolving them - scoped to the affected entries where it is scoped.
package scout

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/cplieger/httpx/v4"
	"github.com/cplieger/runesafe"
	"github.com/cplieger/seadex-scout/internal/arrwalk"
	"github.com/cplieger/seadex-scout/internal/audit"
	"github.com/cplieger/seadex-scout/internal/classify"
	"github.com/cplieger/seadex-scout/internal/compare"
	"github.com/cplieger/seadex-scout/internal/degradation"
	"github.com/cplieger/seadex-scout/internal/indexer"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/match"
	"github.com/cplieger/seadex-scout/internal/notify"
	"github.com/cplieger/seadex-scout/internal/seadex"
	"github.com/cplieger/seadex-scout/internal/seadexapi"
	"github.com/cplieger/seadex-scout/internal/state"
	"github.com/cplieger/seadex-scout/internal/trackerlink"
)

// FeedWriter rebuilds and persists the indexer's Torznab feed from the cycle's
// shared SeaDex snapshot, so the findings and the RSS feed the arrs grab from are
// produced by one data engine. Deps.Feed is nil when no Torznab feed is
// configured. info supplies the per-show metadata the writer synthesizes RSS
// titles from, built over persisted state only, keeping the rebuild
// arr-independent.
type FeedWriter interface {
	Rebuild(ctx context.Context, entries []seadex.Entry, info indexer.EntryInfoFunc) error
	// Advance folds a bounded window of recently-changed entries into the
	// persisted feed without re-deriving what a window cannot speak for.
	Advance(ctx context.Context, window []seadex.Entry, info indexer.EntryInfoFunc) error
}

// SeaDexSource supplies the SeaDex entries snapshot a cycle compares and rebuilds
// the feed from. It is the consumer-side seam over *seadexapi.Client, so
// orchestration tests can drive cycle outcomes with a fake.
type SeaDexSource interface {
	FetchEntries(ctx context.Context, opts seadexapi.Options) ([]seadex.Entry, error)
	// CountWindow reports how many records changed since t without downloading
	// them. It is the tick's cost bound (see Scout.tick).
	CountWindow(ctx context.Context, t time.Time) (int, error)
}

// The concrete PocketBase client must keep satisfying the cycle's seam.
var _ SeaDexSource = (*seadexapi.Client)(nil)

// StateStore loads and saves the persisted cross-cycle state a cycle reads and
// writes. It is the consumer-side seam over *state.Store, so orchestration tests
// can drive state transitions with an in-memory fake.
type StateStore interface {
	Load(ctx context.Context) (state.State, error)
	Save(ctx context.Context, st *state.State) error
}

// The concrete file-backed store must keep satisfying the cycle's seam.
var _ StateStore = (*state.Store)(nil)

// MappingSource supplies the Fribb mapping cache and index a cycle (or a one-shot
// report) loads from the persisted cache. It is the consumer-side seam over
// *mapping.Loader, so orchestration tests can supply mapping outcomes with a fake.
type MappingSource interface {
	Load(ctx context.Context, prev *mapping.Cache) (mapping.Cache, *mapping.Index, error)
}

// The concrete Fribb loader must keep satisfying the cycle's seam.
var _ MappingSource = (*mapping.Loader)(nil)

// Deps are the assembled components a Scout runs a compare CYCLE with. Every
// field is one the cycle reaches: it compares, notifies, and rebuilds the feed.
// The read-only one-shot report has its own role struct (ReportDeps) because the
// two entry points need DISJOINT sets, so the composition root builds only the
// components the flow it is starting can call - and a mis-wired flow does not
// compile, where it used to be a nil-pointer panic.
type Deps struct {
	Logger   *slog.Logger
	Store    StateStore
	Library  *arrwalk.Walker
	Mapping  MappingSource
	SeaDex   SeaDexSource
	Matcher  *match.Matcher
	Comparer *compare.Comparer
	Notifier *notify.Notifier
	// AniListStats reports the AniList client's cumulative request counters for
	// the cycle completion logs. A narrow callback rather than the concrete
	// client; nil when no AniList client is wired (the daemon always wires it).
	AniListStats func() AniListStats
	// Feed rebuilds and persists the indexer's Torznab feed. Nil when no Torznab
	// feed is configured (the cycle then skips all feed work).
	Feed FeedWriter
	// PollInterval is the loop's own interval, which decides how many ticks
	// separate two reconciles (see reconcileEvery). Zero or negative means every
	// iteration reconciles - the conservative reading.
	PollInterval time.Duration
}

// ReportDeps are the assembled components a Reporter runs the read-only one-shot
// report with: it walks the library, loads the mapping cache and AniList memo,
// fetches SeaDex, matches, and audits. There is deliberately no Comparer,
// Notifier or Feed field. Store is still needed - the report READS persisted
// state, and the root injects the read-only store so it cannot write state.json.
type ReportDeps struct {
	Logger  *slog.Logger
	Store   StateStore
	Library *arrwalk.Walker
	Mapping MappingSource
	SeaDex  SeaDexSource
	Matcher *match.Matcher
	Auditor *audit.Auditor
}

// core is what BOTH flows need, and nothing else: every field here is one each
// role genuinely reaches, so a nil value in it is a wiring bug in every role
// rather than a documented no-op for one of them.
type core struct {
	log     *slog.Logger
	store   StateStore
	library *arrwalk.Walker
	mapping MappingSource
	seadex  SeaDexSource
	matcher *match.Matcher
}

// newCore assembles the field set both role constructors project into,
// resolving the logger default once so the two roles cannot drift on it.
func newCore(log *slog.Logger, store StateStore, lib *arrwalk.Walker, mapSrc MappingSource, sea SeaDexSource, matcher *match.Matcher) core {
	if log == nil {
		log = slog.Default()
	}
	return core{log: log, store: store, library: lib, mapping: mapSrc, seadex: sea, matcher: matcher}
}

// Every persisted degradation streak escalates its single log site from WARN to
// ERROR at the shared threshold in internal/degradation, where the count lives
// because it is CADENCE-RELATIVE: ~2h on the tick, 8 days on the reconcile.

// Scout runs compare cycles from its assembled dependencies, carrying the
// compare-cycle components only: the one-shot audit is *Reporter's. The three
// counters below are per-PROCESS and deliberately not persisted - a restart runs
// a reconcile, which is exactly the state a fresh count wants, and persisting
// them would make a losable value load-bearing for correctness.
type Scout struct {
	core
	comparer *compare.Comparer
	notifier *notify.Notifier
	// aniListStats reports the AniList client's cumulative request counters; nil
	// when no AniList client is wired (see Deps.AniListStats).
	aniListStats func() AniListStats
	// feed rebuilds and persists the indexer's Torznab feed. Nil when no
	// Torznab feed is configured (the cycle then skips all feed work).
	feed FeedWriter
	// pollInterval is the loop's own interval, which decides how many ticks
	// separate two reconciles (see reconcileEvery).
	pollInterval time.Duration
	// iterations counts the loop iterations that actually RAN a cycle, so every
	// reconcileEvery-th one reconciles. Held here rather than in the loop's timer
	// closure, which also runs on a tick the cross-process lock skipped.
	iterations int
	// oversizeRun and unreachableRun count consecutive ticks that were too large
	// to fetch or could not read the upstream at all - diagnostics for a wedged
	// fast path; a productive tick resets both.
	oversizeRun    int
	unreachableRun int
	// reconcileRetries bounds how hard the loop tries to establish the finding
	// set (see reconcileRetryLatch and ready).
	reconcileRetries int
	// ready reports whether a reconcile has reported a full finding set since this
	// process started. Until it has, a tick must not publish the partial set:
	// emitting 2 rows where the truth is 190 resolves 188 live conditions.
	ready bool
}

// reconcileRetryLatch bounds the immediate retry of a reconcile that did not
// establish a finding set. A failed or gated startup reconcile leaves the
// notifier empty, so waiting a full reconcileInterval means up to 24h in which
// the app knows nothing and says nothing. It is BOUNDED because the retry is a
// full catalogue fetch plus a full arr walk: retrying forever against a
// condition that will not clear is 1.67 GiB/day against a community-run
// upstream. 4 attempts is ~1h at the default interval.
const reconcileRetryLatch = 4

// reconcileInterval is how often a full pass runs. It is a CONSTANT, not a config
// key: the tick interval is the operator's tradeoff, while the backstop's own
// cadence has no reason to tune - and a tunable one admits a 15m full pass, which
// is 1.67 GiB/day against a community-run upstream.
const reconcileInterval = 24 * time.Hour

// reconcileEvery reports how many loop iterations separate two reconciles. A zero
// or negative interval reconciles every iteration, the conservative reading.
func (s *Scout) reconcileEvery() int {
	if s.pollInterval <= 0 {
		return 1
	}
	return max(1, int(reconcileInterval/s.pollInterval))
}

// reconcileCadenceAttr is the value the reconcile's start/complete lines carry
// for `interval`: how often a full pass ACTUALLY runs. reconcileInterval is the
// TARGET and reconcileEvery quantizes it to whole loop iterations, so at a 5h
// interval the real cadence is 20h. An operator sizes the reconcile deadman's
// absence window off this attribute, so it must name the cadence they will
// actually see; external mode reports "external".
func (s *Scout) reconcileCadenceAttr() string {
	if s.pollInterval <= 0 {
		return "external"
	}
	return (time.Duration(s.reconcileEvery()) * s.pollInterval).String()
}

// Cycle runs ONE loop iteration and reports whether it was healthy. It dispatches
// between the two kinds of pass: a RECONCILE (the full pass - whole catalogue,
// whole arr walk, whole compare, whole feed and curation-index rebuild) on the
// FIRST iteration and every reconcileEvery-th one after it, and a TICK (a bounded
// recent-changes window) on every other. The first iteration reconciles because
// everything downstream assumes a complete pass has happened: the notifier's set
// is empty until one runs, and the tick compares against a cached library only a
// walk can populate. A reconcile that established no set is retried next pass.
func (s *Scout) Cycle(ctx context.Context) bool {
	due := s.iterations%s.reconcileEvery() == 0 || s.reconcileRetryDue()
	s.iterations++
	if !due {
		return s.tick(ctx)
	}
	healthy := s.reconcile(ctx)
	if !s.ready {
		// The reconcile did not reach finishCompletedCycle, so the finding set
		// is still empty or stale: charge the attempt against the retry budget.
		s.reconcileRetries++
	}
	return healthy
}

// reconcileRetryDue reports whether a reconcile should run out of cadence because
// no complete pass has succeeded yet in this process. See reconcileRetryLatch.
func (s *Scout) reconcileRetryDue() bool {
	return !s.ready && s.reconcileRetries < reconcileRetryLatch
}

// cycleDegraded emits the degraded-cycle completion line. Every cycle that ran to
// its end without full success closes with this single WARN. The cycle deadman
// counts completion lines, so it stays satisfied during a long arr or upstream
// outage instead of firing as if the daemon died - its absence then means only
// "loop wedged", matching its restart runbook. reason distinguishes the gate; a
// shutdown-interrupted cycle emits neither, because it did not complete.
func (s *Scout) cycleDegraded(reason string, attrs ...any) {
	s.log.Warn("cycle degraded", append([]any{"reason", reason}, attrs...)...)
}

// cycleGateDegraded closes a reconcile that was GATED before the compare: it
// re-states the finding set and then emits the degraded completion line. A pass
// that compared nothing learned nothing that could resolve a standing finding, so
// staying silent lets the rules' lookback expire every open row and then re-fire
// the whole set as new. Only the pre-compare exits use it: the completed-compare
// paths reached Notifier.Report already. The re-statement is gated on readiness,
// because an empty "findings reported" summary reads as "no findings".
func (s *Scout) cycleGateDegraded(reason string, attrs ...any) {
	if s.ready {
		s.notifier.Reemit()
	}
	s.cycleDegraded(reason, attrs...)
}

// New builds a Scout for the compare CYCLE from deps. The one-shot audit is not
// reachable on it at all: Report belongs to *Reporter, which NewReporter builds.
func New(deps *Deps) *Scout {
	return &Scout{
		core:         newCore(deps.Logger, deps.Store, deps.Library, deps.Mapping, deps.SeaDex, deps.Matcher),
		comparer:     deps.Comparer,
		notifier:     deps.Notifier,
		aniListStats: deps.AniListStats,
		feed:         deps.Feed,
		pollInterval: deps.PollInterval,
	}
}

// reconcile runs one FULL compare pass and reports whether the run was healthy
// (the library ingest succeeded). It never returns an error: a failed ingest
// returns false, and an upstream failure returns true but degraded.
func (s *Scout) reconcile(ctx context.Context) bool {
	start := time.Now()
	// The one line a pass emits BEFORE doing any work, and the reason the scan
	// deadman can be tight: every other line it counts is a COMPLETION line, and a
	// cold reconcile takes ~25 minutes.
	s.log.Info("reconcile started", "interval", s.reconcileCadenceAttr())
	startStats := s.aniStats()
	st := s.loadState(ctx)

	snap, walkErr := s.library.Walk(ctx)
	if walkErr != nil && ctx.Err() != nil {
		// A shutdown cancelled the cycle mid-walk: not an arr fault, so neither the
		// walk-failed ERROR (it would trip the cycle-error alert on every redeploy)
		// nor an unhealthy verdict - the rule every later interruption arm applies.
		s.log.Warn("cycle interrupted by shutdown during library walk", "cause", context.Cause(ctx))
		return true
	}
	if s.stopAfterWalkFailure(walkErr) {
		return false
	}

	// The shared SeaDex + Fribb snapshot feeds BOTH halves: the Torznab feed and
	// the compare pass, so a notification and what the arrs see stay on one fetch.
	mapCache, idx, mapErr := s.loadMapping(ctx, &st)
	entries, seaErr := s.seadex.FetchEntries(ctx, seadexapi.Options{Mode: seadexapi.FetchFull})
	s.warnCatalogueLinkQuality(entries)

	errs := cycleOutcomes{walk: walkErr, mapping: mapErr, seadex: seaErr}
	s.rebuildFeed(ctx, entries, idx, &st, errs)

	// From here the compare pass is gated on the arr walk (the health signal), and
	// the gate also applies the per-arr shrink guard, which MERGES a suspect arr's
	// prior items into snap rather than skipping the compare.
	handled, healthy, shrunkArrs := s.handlePreCompareGate(ctx, &st, &snap, &mapCache, entries, errs)
	if handled {
		return healthy
	}

	result := s.matcher.Match(ctx, entries, &snap, idx, st.Memo)
	// The reconcile is the ONE pass holding a whole catalogue, so it is the one that
	// may garbage-collect the memo: the tick's 48 hours would delete nearly every
	// expired entry, including the ones the feed's stale-title tier reads.
	s.matcher.PruneMemo(&result, entries)
	if ctx.Err() != nil {
		// A shutdown arrived during or right after matching. The match set may be
		// truncated, so comparing it would falsely resolve those entries' findings.
		// A transient AniList degradation instead flows into the compare below.
		return s.finishInterruptedMatch(ctx, start, startStats, &st, snap, &mapCache, &result)
	}
	return s.finishCompletedCycle(ctx, start, startStats, &st, snap, &mapCache, entries, &result, mapErr, shrunkArrs)
}

// warnCatalogueLinkQuality emits the catalogue-wide tracker-link diagnostics for
// one fetched SeaDex snapshot: how many torrents carry a URL the publisher
// refuses, as ONE aggregate WARN, so a tracker host migration that strips every
// release link is alertable instead of silently emptying every release_url; plus
// a SECOND line for the one cause whose remedy is OURS - a tracker this build's
// table does not know. The aggregate is deliberately the publisher's own refusal
// (classify.PublishRefusal), not a weaker is-the-url-blank test that would miss
// a wholesale host drift. A failed fetch logs nothing: its counters stay zero.
func (c *core) warnCatalogueLinkQuality(entries []seadex.Entry) {
	var unusable, unknownTracker int
	for i := range entries {
		for j := range entries[i].Torrents {
			link, refusal := classify.PublishRefusal(&entries[i].Torrents[j])
			if link == "" {
				unusable++
			}
			if refusal == trackerlink.RefusalUnknownTracker {
				unknownTracker++
			}
		}
	}
	if unusable > 0 {
		c.log.Warn("seadex torrent URLs unusable; affected findings and feed items carry no release link",
			"count", unusable, "entries", len(entries))
	}
	if unknownTracker > 0 {
		c.log.Warn("seadex trackers unknown to this build; add them to seadex-scout's tracker table to publish their links",
			"count", unknownTracker, "entries", len(entries))
	}
}

// stopAfterWalkFailure logs a failed library walk and reports whether Cycle
// should stop immediately. A genuine walk failure is unhealthy (a
// shutdown-cancelled walk never reaches here); an alert-only deployment stops
// right away, while a configured feed falls through so the arr-independent feed
// rebuild still runs.
func (s *Scout) stopAfterWalkFailure(walkErr error) bool {
	if walkErr == nil {
		return false
	}
	// The arr URL may carry userinfo, so the error must be reduced before it
	// crosses a log boundary; walkFailureAttrs adds the failed side's identity.
	attrs := walkFailureAttrs(walkErr)
	s.log.Error("library walk failed; cycle unhealthy", attrs...)
	// Alert-only (no Torznab feed): a failed walk is unhealthy and nothing else
	// remains, so emit the completion line here - the ERROR above carries the
	// fault. With a feed configured, fall through to refresh it from SeaDex + Fribb.
	if s.feed == nil {
		s.cycleGateDegraded("walk-failed", attrs...)
		return true
	}
	return false
}

// attrError is the slog attribute key for an error value, named because the
// attr-slice builders share it as a slice-literal element (goconst); direct log
// sites keep the literal.
const attrError = "error"

// walkFailureAttrs builds the attribute set shared by the walk-failure log
// boundaries: the LogSafeError-reduced error - a transport failure wraps a
// *url.Error embedding the request URL, which may carry configured userinfo
// credentials - plus a bounded `arr` attribute naming the failed side. The side
// must come from the ORIGINAL error: the reduction discards arrwalk's wrapper.
func walkFailureAttrs(walkErr error) []any {
	attrs := []any{attrError, httpx.LogSafeError(walkErr)}
	if arr := arrwalk.WalkErrArr(walkErr); arr != "" {
		attrs = append(attrs, "arr", arr)
	}
	return attrs
}

// maxLoggedErrorBytes bounds an upstream error's rendered text before it becomes
// a slog attribute value. The mapping loader already reduces every error it hands
// back and the SeaDex client does not, so the reduction is applied at this site.
const maxLoggedErrorBytes = 8 << 10

// logSafeUpstreamError renders an upstream error as a bounded, single-line,
// rune-sanitized error value. It is the cycle's LOG-BOUNDARY reduction for a
// SeaDex error, applied without asking which client arm produced it: a fetch
// failure can embed raw upstream bytes, and this site cannot tell those apart
// from the error value alone. An honest error passes through byte-identical.
func logSafeUpstreamError(err error) error {
	if err == nil {
		return nil
	}
	return errors.New(runesafe.SanitizeSingleLineBounded(err.Error(), maxLoggedErrorBytes))
}

// loadMapping refreshes the Fribb map from the persisted cache, logging a
// degraded load once. A cancelled load is the shutdown, not a Fribb fault. The
// degraded log is WARN, escalating to ERROR once the loader's acceptance guards
// have rejected degradation.TickEscalationThreshold consecutive refreshes: that
// state re-downloads the ~5.9MB body every cycle and never self-heals. The streak
// is read off the returned Cache, so this stays the single log site.
func (s *Scout) loadMapping(ctx context.Context, st *state.State) (mapping.Cache, *mapping.Index, error) {
	mapCache, idx, mapErr := s.mapping.Load(ctx, &st.Mapping)
	if mapErr != nil && ctx.Err() == nil {
		attrs := mappingDegradedAttrs(mapErr, idx.Len(), mapCache.RejectedRefreshes)
		// Escalate on the PERSISTED streak, not on the error type: a guard that keeps
		// refusing a fresh body with no usable stale cache to return degrades with a
		// plain error rather than a *StaleMapError, and never self-heals either.
		if mapCache.RejectedRefreshes >= degradation.TickEscalationThreshold {
			// The attrs carry the streak (stale_consecutive_rejections) and,
			// when a stale map was returned, the rejecting guard (stale_reason).
			s.log.Error("mapping degraded: refresh rejected repeatedly; inspect upstream, or remove state.json to cold-start if the change is legitimate", attrs...)
		} else {
			s.log.Warn("mapping degraded", attrs...)
		}
	}
	return mapCache, idx, mapErr
}

// mappingDegradedAttrs builds the attribute set shared by the cycle and report
// mapping-degraded log sites: the error and usable_records attributes, plus
// StaleMapError's structured fields when the error carries them, so Loki can
// query the rejection class without parsing the message text. rejections is the
// persisted streak off the returned Cache and is appended UNCONDITIONALLY,
// because that is the value escalation reads two lines up.
func mappingDegradedAttrs(mapErr error, usableRecords, rejections int) []any {
	attrs := []any{attrError, mapErr, "usable_records", usableRecords}
	if stale, ok := errors.AsType[*mapping.StaleMapError](mapErr); ok {
		attrs = append(attrs, stale.LogAttrs()...)
	}
	if rejections > 0 {
		attrs = append(attrs, "stale_consecutive_rejections", rejections)
	}
	return attrs
}

// AniListStats are the AniList request counters the cycle completion line logs
// cumulatively and per cycle. It is the seam's own named type, so the boundary
// carries field names rather than a transposable same-typed pair.
type AniListStats struct {
	Calls          int64
	RateLimitWaits int64
}

// aniListCycleAttrs returns the cumulative and per-cycle AniList counters both
// cycle completion paths log.
func (s *Scout) aniListCycleAttrs(startStats AniListStats) []any {
	cur := s.aniStats()
	return []any{
		"anilist_calls", cur.Calls,
		"anilist_calls_cycle", cur.Calls - startStats.Calls,
		"anilist_waits", cur.RateLimitWaits,
		"anilist_waits_cycle", cur.RateLimitWaits - startStats.RateLimitWaits,
	}
}

// finishInterruptedMatch closes a cycle whose matching was cut short by a
// shutdown: the match set is truncated, so comparing it would treat the
// never-attempted entries' absent findings as resolved. Save the refreshed
// library/mapping/memo but leave the finding table untouched, log the
// interruption as the shutdown rather than an AniList fault, and emit no
// completion line. Always healthy: a redeploy is not an ingest fault.
func (s *Scout) finishInterruptedMatch(ctx context.Context, start time.Time, startStats AniListStats, st *state.State, snap library.Snapshot, mapCache *mapping.Cache, result *match.Result) bool {
	st.Library, st.Mapping, st.Memo = snap, *mapCache, result.Memo
	s.save(ctx, st)
	attrs := append(s.aniListCycleAttrs(startStats),
		"duration", time.Since(start).Round(time.Millisecond).String())
	s.log.Warn("cycle interrupted by shutdown during matching",
		append([]any{"cause", context.Cause(ctx)}, attrs...)...)
	return true
}

// finishCompletedCycle runs the compare over the completed match result, reports
// the findings, logs the completion line ("cycle complete", or "cycle degraded"
// for a shrink-guarded arr, a partial walk, an AniList degradation or a
// stale-but-usable map), and persists the refreshed state. On a partial walk the
// compare runs on the items that walked cleanly only, and finding resolution is
// scoped so degraded items' prior findings are preserved rather than resolved.
// shrunkArrs are the arrs whose PRIOR items snap already carries; that needs no
// authority narrowing and only decides the completion line's severity.
func (s *Scout) finishCompletedCycle(ctx context.Context, start time.Time, startStats AniListStats, st *state.State, snap library.Snapshot, mapCache *mapping.Cache, entries []seadex.Entry, result *match.Result, mapErr error, shrunkArrs []string) bool {
	cleanMatches, failedItems := splitFailedMatches(result.Matches)
	findings := s.comparer.Compare(cleanMatches)
	// Findings are reported as STATE: the whole set is re-emitted and a condition
	// resolves by absence. The preserve set scopes what replacement may DELETE, so
	// an entry with incomplete evidence keeps its prior rows.
	s.notifier.Report(findings, unionIDs(failedItems, result.IncompleteIDs))
	// The in-memory set is now authoritative for the whole catalogue, so ticks may
	// publish it (see Scout.ready). This is the ONLY site that sets it: every other
	// reconcile exit gated before the compare or was interrupted.
	s.ready = true

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
	s.recordAniListDegradation(st, result)
	s.recordPartialWalk(st, &snap)
	s.logCompletedCycle(&snap, result, mapErr, failedItems, st.AniListDegraded, shrunkArrs, attrs)
	// A SECOND line, carrying nothing but the fact that a full pass finished: once
	// most iterations are ticks, the deadman cannot tell "the loop is alive" from
	// "the backstop still runs". Emitted for every reconcile that ran end to end.
	s.log.Info("reconcile complete", "interval", s.reconcileCadenceAttr())

	st.Library, st.Mapping, st.Memo = snap, *mapCache, result.Memo
	s.save(ctx, st)
	return true
}

// escalate emits a latched degradation at the level its streak has earned: ERROR
// once the streak has reached threshold, WARN before that, with the SAME attrs
// either way so a Loki query need not know which level it landed at. Four
// conditions across both kinds of pass share the app's alert contract: WARN is a
// transient failure or a designed outcome, ERROR is reserved for a condition that
// will not clear without an operator, and the streak tells those apart. Both
// messages are passed in, because the WARN and the ERROR say different things.
func (s *Scout) escalate(streak, threshold int, warnMsg, errMsg string, attrs ...any) {
	if streak >= threshold {
		s.log.Error(errMsg, attrs...)
		return
	}
	s.log.Warn(warnMsg, attrs...)
}

// recordAniListDegradation advances or resets the persisted AniList degradation
// streak and escalates a sustained outage (degradation.Advance is the rule it
// shares with recordPartialWalk). It runs before the completion line so both
// levels carry the up-to-date streak, and the escalation fires on EVERY completed
// AniList-degraded cycle at the threshold - including one whose completion line
// the partial-walk arm wins.
func (s *Scout) recordAniListDegradation(st *state.State, result *match.Result) {
	if degradation.Advance(&st.AniListDegraded, result.Degraded, degradation.ReconcileEscalationThreshold) {
		s.log.Error("anilist lookups degraded repeatedly; matching incomplete and findings frozen for affected entries - inspect graphql.anilist.co reachability and egress",
			"incomplete_lookups", len(result.IncompleteIDs),
			"consecutive_anilist_degraded", st.AniListDegraded)
	}
}

// recordPartialWalk advances or resets the persisted partial-walk streak and
// escalates a sustained partial ingest, sharing degradation.Advance with
// recordAniListDegradation. The streak is NOT threaded into logCompletedCycle, so
// consecutive_partial_walks appears on the escalated ERROR only. Without it a
// single permanently failing series would only ever WARN, while its items'
// findings are carried forward on evidence that never refreshes.
func (s *Scout) recordPartialWalk(st *state.State, snap *library.Snapshot) {
	if degradation.Advance(&st.PartialWalks, snap.Partial, degradation.ReconcileEscalationThreshold) {
		s.log.Error("library walk partial repeatedly; the failing series never compare and the one-shot report refuses a partial snapshot, so those items' findings are carried forward on evidence that never refreshes - inspect the arrs' episode endpoints for the skipped series",
			"consecutive_partial_walks", st.PartialWalks)
	}
}

// logCompletedCycle emits the one completion line the deadman alert counts:
// "cycle complete", or "cycle degraded" with the most severe applicable
// reason (a shrink-guarded arr, then a partial walk, then AniList degradation,
// then a stale-but-usable map, then an arr side emptied by its tag filter).
func (s *Scout) logCompletedCycle(snap *library.Snapshot, result *match.Result, mapErr error, failedItems map[int]struct{}, aniListStreak int, shrunkArrs []string, attrs []any) {
	switch {
	case len(shrunkArrs) > 0:
		// An arr's walk shrank suspiciously, so the compare ran against that side's
		// PRIOR items: part of the library model is one reconcile stale. The MOST
		// severe reason - every reason below degrades a subset of entries.
		s.cycleDegraded("library-shrunk",
			append([]any{"shrunk_arrs", strings.Join(shrunkArrs, ",")}, attrs...)...)
	case snap.Partial:
		// A partial walk compared only the clean items, so report the degraded
		// coverage on the completion line the deadman counts.
		s.cycleDegraded("partial-walk", append([]any{"failed_items", len(failedItems)}, attrs...)...)
	case result.Degraded:
		// A transient AniList failure left some entries' lookups incomplete: the
		// compare ran on the unaffected majority with those rows carried forward.
		// The sustained-degradation ERROR lives in recordAniListDegradation.
		s.cycleDegraded("anilist-degraded",
			append([]any{
				"incomplete_lookups", len(result.IncompleteIDs),
				"consecutive_anilist_degraded", aniListStreak,
			}, attrs...)...)
	case mapErr != nil:
		// Only a stale-but-usable mapping error reaches this point; unusable and
		// cancelled loads returned at the pre-compare gate.
		s.cycleDegraded("mapping-stale", attrs...)
	case snap.FilteredEmpty:
		// arr_tags filtering kept nothing out of a non-empty arr list on an enabled
		// side, so the cycle watched a library the operator did not intend. The walk
		// succeeded, so this is the LEAST severe reason.
		s.cycleDegraded("tags-emptied-side", attrs...)
	default:
		s.log.Info("cycle complete", attrs...)
	}
}

// splitFailedMatches partitions the match set around the model's placeholder rule
// (library.Item.Comparable): a match linked to an item whose file data the walk
// could not establish is excluded from the compare (its file state is missing,
// not empty, so comparing would misread every recommendation as unmet), and those
// items' AniList IDs are returned so resolution can preserve their prior findings.
func splitFailedMatches(matches []match.Match) (clean []match.Match, failedItems map[int]struct{}) {
	clean = make([]match.Match, 0, len(matches))
	for i := range matches {
		if m := &matches[i]; m.InLibrary() && !m.Item.Comparable() {
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

// unionIDs returns the union of two AniList-id sets for the finding
// preservation scope, reusing one side unchanged when the other is empty (the
// common cases: a clean walk, or a non-degraded match) and nil when both are.
func unionIDs(a, b map[int]struct{}) map[int]struct{} {
	if len(b) == 0 {
		return a
	}
	if len(a) == 0 {
		return b
	}
	u := make(map[int]struct{}, len(a)+len(b))
	maps.Copy(u, a)
	maps.Copy(u, b)
	return u
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

// rebuildFeed refreshes the indexer's Torznab feed from the cycle's shared SeaDex
// snapshot, independent of the arr walk (the feed needs only SeaDex + Fribb +
// persisted state, so an arr outage must not freeze it). It is a no-op when no
// feed is configured, the SeaDex fetch failed, or the map is unusable - the
// last-good feed is then kept, because rebuilding against an unusable map would
// categorize every entry as anime and drop all SeaDex movies from Radarr's view.
func (s *Scout) rebuildFeed(ctx context.Context, entries []seadex.Entry, idx *mapping.Index, st *state.State, errs cycleOutcomes) {
	if s.feed == nil || errs.seadex != nil || len(entries) == 0 || !mapUsable(errs.mapping) {
		return
	}
	info := feedEntryInfo(idx, &st.Library, st.Memo)
	if err := s.feed.Rebuild(ctx, entries, info); err != nil && ctx.Err() == nil {
		// A cancelled rebuild is the shutdown, not a feed fault; the pre-compare
		// gate logs the interruption (the last-good feed is kept either way).
		s.log.Warn("indexer feed rebuild failed; keeping previous feed", "error", err)
	}
}

// advanceFeed is the tick's half of rebuildFeed: it folds the window into the
// persisted journal instead of rebuilding it. No feed or nothing to fold means no
// work, the caller owns the mapping-usability gate, and a failure keeps the
// last-good feed rather than degrading the tick.
func (s *Scout) advanceFeed(ctx context.Context, window []seadex.Entry, idx *mapping.Index, st *state.State) {
	if s.feed == nil || len(window) == 0 {
		return
	}
	info := feedEntryInfo(idx, &st.Library, st.Memo)
	if err := s.feed.Advance(ctx, window, info); err != nil && ctx.Err() == nil {
		s.log.Warn("indexer feed advance failed; keeping previous feed", "error", err)
	}
}

// cycleOutcomes carries one cycle's three independent ingest/upstream results so
// the gate helpers read them by NAME: three same-typed positional error
// parameters compile in any order, and a transposition swaps which gate fires.
type cycleOutcomes struct {
	walk    error
	mapping error
	seadex  error
}

// handlePreCompareGate applies the pre-compare degradation gate: it reports
// whether the cycle should stop before the compare pass, the health outcome to
// return when it should, and the arrs the per-arr shrink guard left SUSPECT
// (whose prior items the snapshot now carries), which is what makes the
// completion line read degraded. The library gate runs first, then the shrink
// guard, then the upstream gate. snap is taken by POINTER because the shrink
// guard MERGES into it, so every later reader sees one complete library.
func (s *Scout) handlePreCompareGate(ctx context.Context, st *state.State, snap *library.Snapshot, mapCache *mapping.Cache, entries []seadex.Entry, errs cycleOutcomes) (handled, healthy bool, shrunkArrs []string) {
	// Record the SeaDex fetch outcome exactly once, before the mutually exclusive
	// gates below pick a winner: gate precedence must not decide whether an
	// observed SeaDex outage exists in persisted state.
	s.recordSeaDexFetch(ctx, st, errs.seadex)
	if walkHandled, walkHealthy := s.handleLibraryGate(ctx, st, mapCache, errs); walkHandled {
		return true, walkHealthy, nil
	}
	// The shrink guard runs BEFORE the upstream gate, and that order is
	// load-bearing: the upstream arms persist this snapshot, so merging afterwards
	// would write a shrunken side through as the new baseline.
	shrunkArrs = s.mergeShrunkSides(st, snap)
	// Every upstream arm is degraded but HEALTHY (a restart cannot fix an
	// upstream outage), so only the library gate above can report unhealthy.
	return s.handleUpstreamGate(ctx, st, snap, mapCache, entries, errs), true, shrunkArrs
}

// recordSeaDexFetch records the cycle's SeaDex fetch outcome in the persisted
// state and, on a failure, emits its single log line - before the mutually
// exclusive pre-compare gates run. Centralizing it here is what makes the streak
// independent of which gate closes the cycle, so a double outage still escalates
// instead of WARNing forever behind a higher-precedence gate. A successful fetch
// resets the streak; a cancelled fetch is evidence of neither an outage nor a
// recovery, so it leaves the streak untouched and stays silent.
func (s *Scout) recordSeaDexFetch(ctx context.Context, st *state.State, seaErr error) {
	if seaErr == nil {
		st.SeadexFailures = 0
		return
	}
	if ctx.Err() != nil {
		return
	}
	// The persisted streak escalates this single log site to ERROR once the outage
	// has spanned degradation.ReconcileEscalationThreshold consecutive cycles;
	// below it the WARN keeps a blip off the alert. Both levels carry the streak.
	st.SeadexFailures++
	attrs := []any{attrError, logSafeUpstreamError(seaErr), "consecutive_seadex_failures", st.SeadexFailures, "feed_kept", s.feed != nil}
	s.escalate(st.SeadexFailures, degradation.ReconcileEscalationThreshold,
		"seadex fetch failed; skipping comparison, findings re-stated unchanged this cycle",
		"seadex fetch failed repeatedly; skipping comparison, findings re-stated unchanged this cycle - inspect SeaDex (releases.moe) reachability and egress",
		attrs...)
}

// handleLibraryGate gates the compare pass on the library ingest. A failed arr
// walk is unhealthy and persists only the refreshed mapping cache. It is the ONLY
// arm that stops the cycle here: a walk that shrank suspiciously no longer skips
// the comparison (mergeShrunkSides carries that side's prior items instead), and
// a partial snapshot is not gated either - the compare proceeds on the clean
// items with the Failed items' rows carried forward.
func (s *Scout) handleLibraryGate(ctx context.Context, st *state.State, mapCache *mapping.Cache, errs cycleOutcomes) (handled, healthy bool) {
	if errs.walk != nil {
		// Persist only the refreshed mapping cache: discarding it re-downloads an
		// updated Fribb body next cycle. Findings, memo and the prior snapshot stay.
		st.Mapping = *mapCache
		s.save(ctx, st)
		// The cycle ran to its degraded end, so emit the completion line the deadman
		// counts; the walk's ERROR already carries the fault. A shutdown that landed
		// after the walk failure keeps the no-completion-line rule.
		if ctx.Err() == nil {
			// Same reduction and failed-side attribution as stopAfterWalkFailure: the
			// walk error may embed a credential-bearing request URL.
			s.cycleGateDegraded("walk-failed", walkFailureAttrs(errs.walk)...)
		}
		return true, false
	}
	return false, true
}

// countItemsByArr tallies a snapshot's items per arr. The per-arr counts the
// shrink guard compares need no new persisted field: library.Item already carries
// the arr that produced it.
func countItemsByArr(items []library.Item) map[string]int {
	counts := make(map[string]int, 2)
	for i := range items {
		counts[items[i].Arr]++
	}
	return counts
}

// mergeShrunkSides applies the library shrink guard PER ARR and returns the arrs
// it left suspect: for every ENABLED arr whose fresh item count is a suspicious
// truncation of its OWN prior count (degradation.Shrunk), it substitutes that
// side's PRIOR items in snap, advances that side's streak, and emits the
// escalating diagnostic. Merging keeps snap a COMPLETE library model, so the
// compare runs at full authority and the shrink test still fires next cycle; a
// streak at the accept threshold takes the smaller library, loudly. A side whose
// prior count is ZERO is never suspect.
func (s *Scout) mergeShrunkSides(st *state.State, snap *library.Snapshot) []string {
	prior, current := countItemsByArr(st.Library.Items), countItemsByArr(snap.Items)
	var suspect []string
	for _, arr := range s.library.EnabledArrs() {
		if prior[arr] == 0 || !degradation.Shrunk(current[arr], prior[arr]) {
			// A recovered (or never-tripped) side ends its OWN streak; deleting rather
			// than zeroing keeps the map to the sides with evidence against them.
			delete(st.ShrunkWalksByArr, arr)
			continue
		}
		streak := st.ShrunkWalksByArr[arr] + 1
		attrs := shrunkWalkAttrs(arr, current[arr], prior[arr], streak)
		if streak >= degradation.ShrunkWalkAcceptThreshold {
			// The guard has withheld this side for its whole tolerance, so it accepts.
			// WARN, not ERROR: this is a DESIGNED outcome. But it can never be SILENT
			// - a mass-resolve nobody was told about is what the guard prevents.
			delete(st.ShrunkWalksByArr, arr)
			s.log.Warn("library walk stayed shrunken for the whole tolerated streak; accepting the smaller library for this arr as the new shape, so its stale findings resolve this cycle - if that is not intended, fix that arr and arr_tags and the next walk re-establishes the larger library", attrs...)
			continue
		}
		if st.ShrunkWalksByArr == nil {
			st.ShrunkWalksByArr = make(map[string]int, 2)
		}
		st.ShrunkWalksByArr[arr] = streak
		suspect = append(suspect, arr)
		// One log site, escalating: a shrink that persists for a day is a
		// misconfiguration rather than a blip. Both arms name the arr, both counts,
		// the streak, the passes left before acceptance, and the remedy.
		s.escalate(streak, degradation.ReconcileEscalationThreshold,
			"library walk shrank below half this arr's prior snapshot; carrying that arr's prior items forward, so its findings are recomputed from them and not resolved - inspect that arr and arr_tags, or remove state.json to accept the smaller library immediately; passes_before_accept more consecutive shrunken reconciles and the app accepts it on its own",
			"library walk shrank repeatedly for this arr; still carrying that arr's prior items forward, so its findings are recomputed from them and not resolved - inspect that arr and arr_tags, or remove state.json to accept the smaller library immediately; passes_before_accept more consecutive shrunken reconciles and the app accepts it on its own",
			attrs...)
	}
	if len(suspect) > 0 {
		snap.Items = carryPriorItems(snap.Items, st.Library.Items, suspect)
	}
	return suspect
}

// shrunkWalkAttrs is the attribute set every shrink-guard log site carries, so a
// Loki query reads the same fields whichever arm it landed on: the arr, both
// counts, the streak, and how many further shrunken reconciles remain before the
// guard accepts the smaller library.
func shrunkWalkAttrs(arr string, items, priorItems, streak int) []any {
	return []any{
		"arr", arr,
		"items", items,
		"prior_items", priorItems,
		"consecutive_shrunk_walks", streak,
		"passes_before_accept", max(degradation.ShrunkWalkAcceptThreshold-streak, 0),
	}
}

// carryPriorItems merges one walk's items with the prior snapshot's: every
// healthy side's FRESH items, then every suspect side's PRIOR ones. The result is
// a complete library model, which is what lets the compare run at full authority.
func carryPriorItems(fresh, prior []library.Item, suspect []string) []library.Item {
	merged := make([]library.Item, 0, len(fresh)+len(prior))
	for i := range fresh {
		if !slices.Contains(suspect, fresh[i].Arr) {
			merged = append(merged, fresh[i])
		}
	}
	for i := range prior {
		if slices.Contains(suspect, prior[i].Arr) {
			merged = append(merged, prior[i])
		}
	}
	return merged
}

// handleUpstreamGate gates the compare pass on the map's usability and the SeaDex
// fetch, and reports whether the cycle stopped here. An unusable map (the loader
// owns that discrimination, so a handful of operator overrides on an empty index
// cannot defeat the gate), a failed fetch, or a successful-but-empty fetch are
// each degraded but healthy - which is why the health verdict is the CALLER's
// constant. They preserve prior findings and save only the refreshed snapshot and
// map (degradedSave). A shutdown during the load or fetch is the shutdown's.
func (s *Scout) handleUpstreamGate(ctx context.Context, st *state.State, snap *library.Snapshot, mapCache *mapping.Cache, entries []seadex.Entry, errs cycleOutcomes) (handled bool) {
	if ctx.Err() != nil && (errs.mapping != nil || errs.seadex != nil) {
		// A shutdown cancelled the cycle during the mapping load or SeaDex fetch: the
		// errors are the cancellation, not an upstream fault. Preserve findings like
		// an outage but attribute the interruption to the shutdown; no streak moves.
		s.degradedSave(ctx, st, snap, mapCache)
		s.log.Warn("cycle interrupted by shutdown before comparison; findings not re-reported this cycle",
			"cause", context.Cause(ctx))
		return true
	}
	if !mapUsable(errs.mapping) {
		s.degradedSave(ctx, st, snap, mapCache)
		// Same feed_kept signal the other arms attach: an unusable map skips the feed
		// rebuild too, so a configured feed is serving its previous snapshot.
		s.log.Warn("mapping unusable; skipping comparison, findings re-stated unchanged this cycle",
			"error", errs.mapping, "feed_kept", s.feed != nil)
		s.cycleGateDegraded("mapping-unusable", "error", errs.mapping)
		return true
	}
	if errs.seadex != nil {
		// The failure was already recorded and logged once by recordSeaDexFetch, so
		// this arm only owns the degraded save, the completion line and the verdict.
		s.degradedSave(ctx, st, snap, mapCache)
		s.cycleGateDegraded("seadex-fetch-failed", "error", logSafeUpstreamError(errs.seadex))
		return true
	}
	if len(entries) == 0 {
		s.degradedSave(ctx, st, snap, mapCache)
		// Same feed_kept signal: a zero-entries response skips the rebuild too, so a
		// configured feed is serving its previous snapshot.
		s.log.Warn("seadex returned zero entries; skipping comparison, findings re-stated unchanged this cycle",
			"feed_kept", s.feed != nil)
		// A shutdown that landed after a nil-error zero-entry fetch keeps the
		// no-completion-line rule; the ctx arm above pre-empts only an errored fetch.
		if ctx.Err() == nil {
			s.cycleGateDegraded("seadex-zero-entries")
		}
		return true
	}
	return false
}

// aniStats returns the AniList client's cumulative stats via the injected
// callback, or zero stats when none is wired (the daemon always wires it).
func (s *Scout) aniStats() AniListStats {
	if s.aniListStats == nil {
		return AniListStats{}
	}
	return s.aniListStats()
}

// loadState loads persisted state, falling back to an empty state on error. A
// load cut short by shutdown cancellation is not a state fault: it returns empty
// silently, and the next context-aware stage reports the shutdown once at WARN.
func (c *core) loadState(ctx context.Context) state.State {
	st, err := c.store.Load(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			return state.State{}
		}
		c.log.Error("state load failed; starting from empty state", "error", err)
		return state.State{}
	}
	return st
}

// degradedSave persists the caches refreshed before the compare pass was skipped
// (library snapshot and map), leaving the AniList memo and finding table
// untouched, so a degraded upstream or a shutdown cannot falsely resolve findings.
func (s *Scout) degradedSave(ctx context.Context, st *state.State, snap *library.Snapshot, mapCache *mapping.Cache) {
	st.Library = *snap
	st.Mapping = *mapCache
	s.save(ctx, st)
}

// saveGrace bounds the detached shutdown retry, measured from the cancellation
// that triggered it - the SIGTERM at which the container stop grace itself
// starts. It stays inside Docker's default 10s stop grace. atomicfile's
// temp+rename means a SIGKILL mid-write cannot corrupt state; the only cost of a
// missed save is the AniList memo, which self-heals over one cold cycle.
const saveGrace = 5 * time.Second

// save persists state, tolerating a shutdown mid-cycle. When the run context is
// cancelled (SIGTERM during a redeploy) the atomic write fails with
// context.Canceled and the caches are lost, so a cancellation is retried once
// with a detached, briefly-bounded context, letting the expensive AniList memo
// survive the restart. The retry gets a full saveGrace measured from the
// CANCELLATION, because the first attempt never spends the stop grace either way.
// A cancellation is not a fault, so only a genuine write failure logs at ERROR;
// a deliberate preservation refusal (state.ErrSavePreserved) logs at WARN.
func (s *Scout) save(ctx context.Context, st *state.State) {
	err := s.store.Save(ctx, st)
	if err != nil && (errors.Is(err, context.Canceled) || ctx.Err() != nil) {
		// The container stop grace starts at the SIGTERM that cancelled ctx, so the
		// retry budget is anchored HERE rather than shortened by the first attempt.
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), saveGrace)
		err = s.store.Save(dctx, st)
		cancel()
	}
	if err != nil {
		// A deliberate preservation refusal is not a write fault: nothing is broken
		// and nothing was lost. Reporting it at ERROR would fire the cycle-error
		// alert on a routine redeploy, since a SIGTERM in Load's read window sets it.
		if errors.Is(err, state.ErrSavePreserved) {
			s.log.Warn("state save skipped; on-disk state preserved", "error", err)
			return
		}
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
