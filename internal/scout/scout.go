// Package scout orchestrates one compare cycle: load state, walk the library,
// refresh the ID map, pull SeaDex, match entries to library items, compare, and
// report findings, then persist the caches.
//
// Cycle health follows the library ingest: a failed arr walk is unhealthy (a
// restart or config fix could recover it), while a SeaDex, mapping, or AniList
// failure is degraded but healthy (a restart cannot fix an upstream outage) and
// preserves prior findings rather than falsely resolving them - scoped to the
// affected entries where the failure is scoped (a transient AniList lookup
// failure degrades only the entries it left unresolved; the rest of the cycle
// compares and reports normally).
package scout

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
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
	"github.com/cplieger/seadex-scout/internal/state"
	"github.com/cplieger/seadex-scout/internal/trackerlink"
)

// --- dependency seams + assembly ---

// FeedWriter rebuilds and persists the indexer's Torznab feed from the cycle's
// shared SeaDex snapshot, so the findings and the RSS feed the arrs grab from
// are produced by one data engine from a single fetch. The indexer's feed writer
// implements it; Deps.Feed is nil when no Torznab feed is configured and the
// cycle then does no feed work. Because Rebuild persists the feed, a cycle run
// by the `poll` subcommand refreshes a resident daemon's feed too. info supplies
// the per-show metadata the writer synthesizes RSS titles from (see
// feedEntryInfo); it is built over persisted state only, keeping the rebuild
// arr-independent.
type FeedWriter interface {
	Rebuild(ctx context.Context, entries []seadex.Entry, info indexer.EntryInfoFunc) error
	// Advance folds a bounded window of recently-changed entries into the
	// persisted feed without re-deriving what a window cannot speak for. See
	// indexer.FeedWriter.Advance for why it is not Rebuild with a flag.
	Advance(ctx context.Context, window []seadex.Entry, info indexer.EntryInfoFunc) error
}

// SeaDexSource supplies the SeaDex entries snapshot a cycle compares and
// rebuilds the feed from. It is the consumer-side seam over the concrete
// *seadex.Client (which implements it; build.go injects it), so orchestration
// tests can drive cycle outcomes with a fake instead of standing up the
// PocketBase adapter over an httptest server.
type SeaDexSource interface {
	FetchEntries(ctx context.Context, opts seadex.Options) ([]seadex.Entry, error)
	// CountWindow reports how many records changed since t without downloading
	// them. It is the tick's cost bound (see Scout.tick).
	CountWindow(ctx context.Context, t time.Time) (int, error)
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

// MappingSource supplies the Fribb mapping cache and index a cycle (or a
// one-shot report) loads from the persisted cache. It is the consumer-side
// seam over the concrete *mapping.Loader (which implements it; build.go
// injects it), so orchestration tests can supply mapping outcomes with a fake
// instead of constructing the loader's HTTP client, source URL, and override
// path (the mapping package's own suite covers the real loader's fetch and
// degradation behavior).
type MappingSource interface {
	Load(ctx context.Context, prev *mapping.Cache) (mapping.Cache, *mapping.Index, error)
}

// The concrete Fribb loader must keep satisfying the cycle's seam.
var _ MappingSource = (*mapping.Loader)(nil)

// Deps are the assembled components a Scout runs a compare CYCLE with. Every
// field is one the cycle reaches: it compares, notifies, and rebuilds the feed.
// The read-only one-shot report has its own role struct (ReportDeps, built by
// NewReporter) because the two entry points need DISJOINT sets - Cycle never
// audits, and Report never compares, notifies or writes the feed - so the
// composition root builds only the components the flow it is starting can
// actually call, instead of one bag whose unused half was prose (l-f149). The
// runner-up shape was an exported shared core plus per-role extensions; two role
// structs over one internal union keep each runner reading a single field set.
type Deps struct {
	Logger   *slog.Logger
	Store    StateStore
	Library  *arrwalk.Walker
	Mapping  MappingSource
	SeaDex   SeaDexSource
	Matcher  *match.Matcher
	Comparer *compare.Comparer
	Notifier *notify.Notifier
	// AniListStats reports the AniList client's cumulative request counters
	// (calls, rate-limit waits) for the cycle completion logs. The scout only
	// needs these two counters, so it takes a narrow callback instead of the
	// concrete client (build.go injects a closure over the client's Stats);
	// nil when no AniList client is wired (the early-return degradation paths
	// and unit tests) - the daemon always wires it.
	AniListStats func() AniListStats
	// Feed rebuilds and persists the indexer's Torznab feed from each cycle's
	// SeaDex snapshot. Nil when no Torznab feed is configured (the cycle then
	// skips all feed work).
	Feed FeedWriter
	// PollInterval is the loop's own interval, which decides how many ticks
	// separate two reconciles (see reconcileEvery). Zero or negative means
	// every iteration reconciles - the conservative reading, and what an
	// unwired test gets.
	PollInterval time.Duration
}

// ReportDeps are the assembled components a Scout runs the read-only one-shot
// report with: it walks the library, loads the mapping cache and AniList memo,
// fetches SeaDex, matches, and audits. There is deliberately no Comparer,
// Notifier or Feed field - the report emits no finding, sends no notification
// and never rewrites the indexer snapshot - so the root's report path builds
// neither them nor the Prowlarr HTTP client the feed writer carries. Store is
// still needed (the report READS persisted state; build.go injects the read-only
// store so the flow cannot write or quarantine state.json).
type ReportDeps struct {
	Logger  *slog.Logger
	Store   StateStore
	Library *arrwalk.Walker
	Mapping MappingSource
	SeaDex  SeaDexSource
	Matcher *match.Matcher
	Auditor *audit.Auditor
}

// components is the union both role structs project into, so each runner reads
// one field set and no method needs to know which constructor built the Scout.
// A component the constructing role does not carry stays nil, which is exactly
// what "this flow cannot reach it" means.
type components struct {
	Logger       *slog.Logger
	Store        StateStore
	Library      *arrwalk.Walker
	Mapping      MappingSource
	SeaDex       SeaDexSource
	Matcher      *match.Matcher
	Comparer     *compare.Comparer
	Auditor      *audit.Auditor
	Notifier     *notify.Notifier
	AniListStats func() AniListStats
	Feed         FeedWriter
	PollInterval time.Duration
}

// Every persisted degradation streak escalates its single log site from WARN to
// ERROR (firing the existing SeadexScoutCycleError Loki rule) at the shared
// fleet-wide threshold, whose policy lives in degradation.EscalationThreshold:
// tolerate 8 consecutive degraded cycles - long enough to ride out a transient
// blip, short enough that a condition which never self-heals alerts instead of
// WARNing forever. Each owning site documents when its streak advances or
// resets and what the remedy is: handleLibraryGate (shrunk walk),
// recordSeaDexFetch (fetch failures), recordAniListDegradation,
// recordPartialWalk, and loadMapping (refresh rejections, a streak the mapping
// loader owns and persists - only the log-level policy lives here).
//
// The count is CADENCE-RELATIVE, so the two cadences this loop now runs need
// two thresholds. A streak that advances on the RECONCILE advances once a day,
// so the fleet's 8 would mean 8 days before the ERROR that is the only level
// this stack alerts on; reconcileEscalationThreshold re-expresses the same
// policy for that cadence. A streak that advances on the TICK keeps the fleet
// number, which is ~2h at the default interval.
const (
	// reconcileEscalationThreshold is the fleet's consecutive-failure policy at
	// the reconcile's daily cadence. Two consecutive failed full passes is 48h,
	// the closest whole-run threshold to the ~24h the fleet's 8 used to buy
	// when every cycle was a full pass on a 3h interval - and one full run of
	// tolerance is the minimum that can still distinguish a transient failure
	// from a condition that will not self-heal.
	reconcileEscalationThreshold = 2

	shrunkWalkEscalationThreshold      = reconcileEscalationThreshold
	seadexFailureEscalationThreshold   = reconcileEscalationThreshold
	aniListDegradedEscalationThreshold = reconcileEscalationThreshold
	partialWalkEscalationThreshold     = reconcileEscalationThreshold
	// mappingRejectionEscalationThreshold keeps the fleet number: loadMapping
	// runs on every changed TICK as well as every reconcile, so its streak
	// advances at tick cadence (~2h at the default interval).
	mappingRejectionEscalationThreshold = degradation.EscalationThreshold
)

// Scout runs compare cycles from its assembled dependencies.
//
// The three counters below are per-PROCESS and deliberately not persisted.
// They exist to shape one loop's behaviour, and a restart runs a reconcile
// (iterations counts zero, so the first iteration reconciles), which is exactly
// the state a fresh count wants. Persisting them would make a losable value
// load-bearing for correctness, which is the mistake three earlier revisions of
// this design made.
type Scout struct {
	log  *slog.Logger
	deps components
	// iterations counts loop iterations, so every reconcileEvery-th one
	// reconciles. It lives HERE and not in the loop closure: the scheduler's
	// Cycler is Cycle(ctx) bool, and a counter in the closure would not advance
	// on an iteration the cross-process lock skipped - silently losing a
	// reconcile. Keeping it on the Scout also makes an exec'd `poll` participate
	// without a flag of its own.
	iterations int
	// emptyRun counts consecutive ticks whose window held nothing,
	// oversizeRun counts consecutive ticks whose window was too large to fetch,
	// and unreachableRun counts consecutive ticks that could not read the
	// upstream at all. All three are diagnostics for a wedged fast path (see
	// tick); a productive tick resets all three.
	emptyRun       int
	oversizeRun    int
	unreachableRun int
	// ready reports whether a reconcile has completed and reported a full
	// finding set since this process started. Until it has, the in-memory set
	// is empty or partial, so a tick must not publish it as the app's state:
	// emitting 2 rows where the truth is 190 resolves 188 conditions that are
	// still true, and an operator cannot tell that set from a genuinely quiet
	// library. reconcileRetries bounds how hard the loop tries to get there
	// (see reconcileRetryLatch).
	ready            bool
	reconcileRetries int
}

// The tick's window and its two wedge diagnostics.
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

// reconcileRetryLatch bounds the immediate retry of a reconcile that did not
// establish a finding set. A failed or gated startup reconcile leaves the
// notifier empty, so waiting a full reconcileInterval for the next one means up
// to 24h in which the app knows nothing and says nothing - the dark window.
// Retrying on the very next iteration makes that window minutes instead.
//
// It is BOUNDED because the retry is a full catalogue fetch plus a full arr
// walk. Retrying forever against a condition that will not clear (a library
// whose shrink guard keeps gating, an upstream that stays down) would run a
// full pass every 15 minutes - 1.67 GiB/day against a community-run upstream,
// which is the traffic this whole design exists to avoid. 4 attempts is ~1h at
// the default interval; past that the normal cadence resumes and the failure
// streaks plus the two deadmen own the escalation.
const reconcileRetryLatch = 4

// reconcileInterval is how often a full pass runs. It is a CONSTANT, not a
// config key: the tick interval is an operator tradeoff (how fresh, how much
// upstream load), while this is the backstop's own cadence with no reason to
// tune - and a tunable one admits a 15m full pass, which is 1.67 GiB/day
// against a community-run upstream.
const reconcileInterval = 24 * time.Hour

// reconcileEvery reports how many loop iterations separate two reconciles. A
// zero or negative interval (an unwired test) reconciles every iteration, the
// conservative reading.
func (s *Scout) reconcileEvery() int {
	if s.deps.PollInterval <= 0 {
		return 1
	}
	return max(1, int(reconcileInterval/s.deps.PollInterval))
}

// Cycle runs ONE loop iteration and reports whether it was healthy. It is the
// dispatcher over the two kinds of pass:
//
//   - a RECONCILE (the full pass: whole catalogue, whole arr walk, whole
//     compare, whole feed and curation-index rebuild) runs on the FIRST
//     iteration and every reconcileEvery-th one after it. It is the backstop
//     for everything a window structurally cannot see - a deletion, an
//     in-place torrent edit, a shared torrent's other parents, an outage
//     longer than the window, a clock wrong by more than it - and it is what
//     refills the notifier's in-memory finding set.
//   - a TICK (a bounded recent-changes window) runs on every other iteration.
//     It is what makes the RSS feed and the alerts fresh in minutes rather
//     than hours, at a fraction of the bytes.
//
// The first iteration reconciles because everything downstream assumes a
// complete pass has happened: the notifier's set is empty until one runs, and
// the tick compares against a cached library only a walk can populate. A
// reconcile that did NOT establish that set is retried on the next iteration
// rather than in a day (see reconcileRetryLatch).
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

// reconcileRetryDue reports whether a reconcile should run out of cadence
// because no complete pass has succeeded yet in this process. It is the bounded
// retry the in-memory finding set depends on: see reconcileRetryLatch for why
// it is bounded, and Scout.ready for what it is waiting for.
func (s *Scout) reconcileRetryDue() bool {
	return !s.ready && s.reconcileRetries < reconcileRetryLatch
}

// cycleDegraded emits the degraded-cycle completion line. Every cycle that
// ran to its end without full success closes with this single WARN: the
// degraded-but-healthy early returns (unusable map, failed or empty SeaDex
// fetch, the library shrink guard), the degraded completed-compare paths (a
// partial walk, a transient AniList degradation, a stale-but-usable map), and
// the unhealthy failed-walk arm (whose fault keeps its own ERROR line). The
// cycle-deadman alert counts completion lines, so it stays satisfied during a
// long arr or upstream outage instead of firing as if the daemon died - its
// absence then means only "loop wedged", matching its restart runbook. reason
// distinguishes the gate; the healthy path keeps "cycle complete" as-is, and
// a shutdown-interrupted cycle emits neither (it did not complete).
func (s *Scout) cycleDegraded(reason string, attrs ...any) {
	s.log.Warn("cycle degraded", append([]any{"reason", reason}, attrs...)...)
}

// New builds a Scout for the compare CYCLE from deps. Its Report method is
// reachable but unwired (no Auditor): a flow that reports must be built by
// NewReporter.
func New(deps *Deps) *Scout {
	return newScout(&components{
		Logger:       deps.Logger,
		Store:        deps.Store,
		Library:      deps.Library,
		Mapping:      deps.Mapping,
		SeaDex:       deps.SeaDex,
		Matcher:      deps.Matcher,
		Comparer:     deps.Comparer,
		Notifier:     deps.Notifier,
		AniListStats: deps.AniListStats,
		Feed:         deps.Feed,
		PollInterval: deps.PollInterval,
	})
}

// NewReporter builds a Scout for the read-only one-shot Report from deps. The
// compare-cycle components stay nil, so the report flow provably cannot notify,
// compare or rewrite the indexer feed - and the root never constructs them.
func NewReporter(deps *ReportDeps) *Scout {
	return newScout(&components{
		Logger:  deps.Logger,
		Store:   deps.Store,
		Library: deps.Library,
		Mapping: deps.Mapping,
		SeaDex:  deps.SeaDex,
		Matcher: deps.Matcher,
		Auditor: deps.Auditor,
	})
}

// newScout is the shared assembly both role constructors end in: it resolves
// the logger default once so the two roles cannot drift on it.
func newScout(c *components) *Scout {
	log := c.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Scout{deps: *c, log: log}
}

// --- cycle orchestration ---

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
			// that can NEVER hold anything looks identical, and the way that
			// happens is a container clock running more than changeWindow
			// ahead, which puts every window in the upstream's future.
			s.log.Warn("no SeaDex change seen for a very long run of ticks; if this persists, check this container's clock against the upstream",
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

// reconcile runs one FULL compare pass and reports whether the run was healthy
// (the library ingest succeeded). It never returns an error: a failed ingest
// returns false, and an upstream (SeaDex/mapping/AniList) failure returns true
// but degraded. See Cycle for when it runs rather than a tick.
func (s *Scout) reconcile(ctx context.Context) bool {
	start := time.Now()
	startStats := s.aniStats()
	st := s.loadState(ctx)

	snap, walkErr := s.deps.Library.Walk(ctx)
	if walkErr != nil && ctx.Err() != nil {
		// A shutdown/redeploy cancelled the cycle mid-walk: not an arr fault,
		// so neither the "library walk failed" ERROR (it would trip the
		// SeadexScoutCycleError alert on every redeploy landing mid-cycle)
		// nor an unhealthy verdict - the same "a redeploy is not an ingest
		// fault" rule every LATER interruption arm already applies
		// (finishInterruptedMatch, handleUpstreamGate's ctx arm). A `poll`
		// SIGTERMed mid-walk now exits 0 like one SIGTERMed mid-match, and
		// the daemon's health marker is not flipped by a routine stop.
		s.log.Warn("cycle interrupted by shutdown during library walk", "cause", context.Cause(ctx))
		return true
	}
	if s.stopAfterWalkFailure(walkErr) {
		return false
	}

	// The shared SeaDex + Fribb snapshot feeds BOTH halves: the Torznab feed
	// (arr-independent) and the compare pass below. Fetching once here is what
	// keeps a notification and what the arrs see in the feed on the same data.
	mapCache, idx, mapErr := s.loadMapping(ctx, &st)
	entries, seaErr := s.deps.SeaDex.FetchEntries(ctx, seadex.Options{Mode: seadex.FetchFull})
	s.warnCatalogueLinkQuality(entries)

	// Rebuild the Torznab feed from the shared snapshot, independent of the arr
	// walk (see rebuildFeed): a notification and what the arrs see in the feed
	// come from this one fetch. The persisted state (library titles + AniList
	// memo) feeds only the title synthesis, never a fresh arr walk.
	errs := cycleOutcomes{walk: walkErr, mapping: mapErr, seadex: seaErr}
	s.rebuildFeed(ctx, entries, idx, &st, errs)

	// From here the compare pass is gated on the arr walk (the health signal): a
	// failed walk is unhealthy and leaves findings untouched (only the refreshed
	// mapping cache is persisted), while the feed above was still refreshed. The
	// pre-compare degradation gate (failed walk, unusable map, failed/empty
	// SeaDex fetch) is factored into a helper so Cycle reads as the top-down
	// happy path.
	if handled, healthy := s.handlePreCompareGate(ctx, &st, snap, &mapCache, entries, errs); handled {
		return healthy
	}

	result := s.deps.Matcher.Match(ctx, entries, &snap, idx, st.Memo)
	if ctx.Err() != nil {
		// A shutdown arrived during or right after matching. The match set may
		// be truncated (entries after the cancellation were never attempted),
		// so comparing it would falsely resolve their findings. Keep the
		// whole-cycle skip for this one case; a transient AniList degradation
		// instead carries Result.IncompleteIDs and flows into the compare
		// below with exactly the affected entries' rows carried forward.
		return s.finishInterruptedMatch(ctx, start, startStats, &st, snap, &mapCache, result)
	}
	return s.finishCompletedCycle(ctx, start, startStats, &st, snap, &mapCache, entries, result, mapErr)
}

// warnCatalogueLinkQuality emits the catalogue-wide tracker-link diagnostics
// for one fetched SeaDex snapshot: how many torrents carry a URL the publisher
// refuses (omitted/empty, a foreign host under a trusted label, an unknown
// tracker, a malformed value) as ONE aggregate WARN, so a tracker host
// migration or schema drift that strips every release link is alertable from
// Loki instead of silently emptying every finding's release_url; plus a SECOND
// line for the one cause whose remedy is OURS rather than the record's - a
// tracker this build's table does not know, where "add the tracker and ship a
// release" is not something the operator can fix upstream (l-f127). The second
// line sits BESIDE the aggregate rather than carving a subset out of it, so the
// aggregate's count and meaning stay exactly as deployed alert rules read them.
//
// It lives here, not in internal/seadex where it used to (l-f156): the
// judgment needs the PUBLISH policy (internal/trackerlink, reached through the
// classify adapter that keeps the (tracker, rawURL) pairing in one place), and
// that policy sits a layer above the wire client - which cannot reach the
// adapter at all, since classify imports seadex. The orchestrator holds the
// whole catalogue the moment either fetch path returns, so it has the same
// one-pass view the client had, and both paths (Cycle here, Report below) call
// it with the same logger the client used, keeping message, level and attrs
// unchanged. A failed fetch returns no entries, so the counters stay zero and
// nothing is logged - the client's own success-only gate, preserved.
//
// Definition matters as much as placement: the aggregate is deliberately the
// publisher's own refusal (classify.PublishRefusal), not a weaker
// is-the-url-field-blank test that would miss a wholesale host drift, and it is
// the same rule filter.Obtainable acts on downstream.
func (s *Scout) warnCatalogueLinkQuality(entries []seadex.Entry) {
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
		s.log.Warn("seadex torrent URLs unusable; affected findings and feed items carry no release link",
			"count", unusable, "entries", len(entries))
	}
	if unknownTracker > 0 {
		s.log.Warn("seadex trackers unknown to this build; add them to seadex-scout's tracker table to publish their links",
			"count", unknownTracker, "entries", len(entries))
	}
}

// stopAfterWalkFailure logs a failed library walk and reports whether Cycle
// should stop immediately. A genuine walk failure is unhealthy (a
// shutdown-cancelled walk never reaches this - Cycle attributes it to the
// shutdown and stays healthy); an alert-only deployment (no Torznab feed)
// stops right away since nothing else remains to do - emitting the "cycle
// degraded" completion line beside the ERROR - while a configured feed falls
// through so the arr-independent feed rebuild still runs (the pre-compare
// gate then returns unhealthy and emits the completion line).
func (s *Scout) stopAfterWalkFailure(walkErr error) bool {
	if walkErr == nil {
		return false
	}
	// The arr URL may carry userinfo (config.Validate only warns on that
	// shape), so the error must be reduced before it crosses any log
	// boundary; walkFailureAttrs adds the failed side's identity beside the
	// reduced error.
	attrs := walkFailureAttrs(walkErr)
	s.log.Error("library walk failed; cycle unhealthy", attrs...)
	// Alert-only (no Torznab feed): a failed walk is unhealthy and there is
	// nothing else to do, so skip the SeaDex/Fribb fetch (the pre-fold
	// behaviour) - the cycle ends here, so emit its completion line now (the
	// ERROR above carries the fault; this keeps the cycle deadman fed during
	// an arr outage). With a feed configured, fall through to refresh it - it
	// needs only SeaDex + Fribb, not the arrs - before returning unhealthy
	// (the library gate then emits the completion line).
	if s.deps.Feed == nil {
		s.cycleDegraded("walk-failed", attrs...)
		return true
	}
	return false
}

// attrError is the slog attribute key for an error value, named because the
// attr-slice builders (walkFailureAttrs, mappingDegradedAttrs, the SeaDex
// failure arm) share it as a slice-literal element (goconst); direct log-call
// sites keep the literal "error".
const attrError = "error"

// walkFailureAttrs builds the attribute set shared by the walk-failure log
// boundaries (the ERROR and both walk-failed "cycle degraded" completion
// lines): the LogSafeError-reduced error - a transport failure wraps a
// *url.Error embedding the full request URL, which may carry configured
// userinfo credentials, so it must not reach Loki unreduced - plus a bounded
// `arr` attribute naming the failed side when the walk error carries one
// (arrwalk.WalkErrArr). The side must come from the ORIGINAL error: the
// reduction collapses the chain to the *url.Error's underlying cause,
// discarding arrwalk.Walk's textual "walking sonarr/radarr" wrapper, so with
// both arrs enabled the reduced error alone would not say which dependency
// failed.
func walkFailureAttrs(walkErr error) []any {
	attrs := []any{attrError, httpx.LogSafeError(walkErr)}
	if arr := arrwalk.WalkErrArr(walkErr); arr != "" {
		attrs = append(attrs, "arr", arr)
	}
	return attrs
}

// maxLoggedErrorBytes bounds an upstream error's rendered text before it
// becomes a slog attribute value. It mirrors internal/mapping's constant of
// the same name: the mapping loader already reduces every error it hands back
// at its own boundary, and the SeaDex client does not, so the reduction is
// applied here at the cycle's single SeaDex log site.
const maxLoggedErrorBytes = 8 << 10

// logSafeUpstreamError renders an upstream error as a bounded, single-line,
// rune-sanitized error value. It is the cycle's LOG-BOUNDARY reduction for a
// SeaDex error, applied without asking which of the client's arms produced it:
// a fetch failure can embed raw upstream bytes (a rejected keyset cursor, a
// structural mismatch quoting the offending JSON token, a status error echoing
// a URL), and this site cannot tell those apart from the error value alone.
// The known arms do self-bound today - the keyset-cursor arms at
// seadex.maxLoggedCursorBytes, the page decode at maxLoggedDecodeBytes - so
// this is the outer bound that keeps the guarantee independent of which arm the
// error came from, the same reduction internal/mapping applies to its own
// errors. An honest error passes through byte-identical.
func logSafeUpstreamError(err error) error {
	if err == nil {
		return nil
	}
	return errors.New(runesafe.SanitizeSingleLineBounded(err.Error(), maxLoggedErrorBytes))
}

// loadMapping refreshes the Fribb map from the persisted cache, logging a
// degraded load once. A cancelled load is the shutdown, not a Fribb fault; the
// pre-compare gate logs the interruption instead (same rule as the
// walk/matching paths). The degraded log is WARN, escalating to ERROR (which
// fires the existing SeadexScoutCycleError Loki rule) once the loader's
// acceptance guards have rejected degradation.EscalationThreshold
// consecutive refreshes: that state re-downloads the ~5.9MB body every cycle
// against an aging cache and never self-heals without the operator, so it
// must alert rather than WARN forever. The rejection streak is read off the
// returned Cache (RejectedRefreshes) so this stays the single log site -
// no second log line in the mapping package, no double-logging; a returned
// *StaleMapError contributes the same streak as an attribute via LogAttrs.
func (s *Scout) loadMapping(ctx context.Context, st *state.State) (mapping.Cache, *mapping.Index, error) {
	mapCache, idx, mapErr := s.deps.Mapping.Load(ctx, &st.Mapping)
	if mapErr != nil && ctx.Err() == nil {
		attrs := mappingDegradedAttrs(mapErr, idx.Len(), mapCache.RejectedRefreshes)
		// Escalate on the PERSISTED streak, not on the error type: a guard that
		// keeps refusing a fresh body when there is no usable stale cache to
		// return (a first boot against a poisoned or restructured upstream)
		// degrades with a plain error rather than a *StaleMapError, and that
		// condition never self-heals either - reading the streak off the cache
		// covers both shapes with one rule.
		if mapCache.RejectedRefreshes >= mappingRejectionEscalationThreshold {
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
// mapping-degraded log sites: the existing error and usable_records attributes,
// plus StaleMapError's structured degradation fields (stale_reason,
// stale_age_seconds, stale_records) when the error carries them, so Loki can
// query the rejection class and stale age without parsing the message text.
// rejections is the persisted streak off the returned Cache; it is emitted on
// its own only when no *StaleMapError supplied it (the no-usable-cache path),
// so the attribute is present on every escalation without being duplicated.
func mappingDegradedAttrs(mapErr error, usableRecords, rejections int) []any {
	attrs := []any{attrError, mapErr, "usable_records", usableRecords}
	if stale, ok := errors.AsType[*mapping.StaleMapError](mapErr); ok {
		return append(attrs, stale.LogAttrs()...)
	}
	if rejections > 0 {
		attrs = append(attrs, "stale_consecutive_rejections", rejections)
	}
	return attrs
}

// AniListStats are the AniList request counters the cycle completion line
// logs cumulatively and per cycle. It is the seam's own named type
// (Deps.AniListStats returns it), so the boundary carries the field names
// rather than an anonymous same-typed pair a caller can transpose.
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

// --- cycle completion paths ---

// finishInterruptedMatch closes a cycle whose matching was cut short by a
// shutdown/redeploy: the match set is truncated, so comparing it would treat
// the never-attempted entries' absent findings as resolved. Save the refreshed
// library/mapping/memo (the memo keeps the lookups that did succeed) but leave
// the finding dedupe table untouched, log the interruption as the shutdown
// (matching the library-walk path) rather than an AniList fault, and emit no
// completion line (an interrupted cycle did not complete). Always healthy: a
// redeploy is not an ingest fault. A transient AniList degradation never lands
// here - the completed match carries Result.IncompleteIDs and
// finishCompletedCycle preserves exactly the affected entries' findings.
func (s *Scout) finishInterruptedMatch(ctx context.Context, start time.Time, startStats AniListStats, st *state.State, snap library.Snapshot, mapCache *mapping.Cache, result match.Result) bool {
	st.Library, st.Mapping, st.Memo = snap, *mapCache, result.Memo
	s.save(ctx, st)
	attrs := append(s.aniListCycleAttrs(startStats),
		"duration", time.Since(start).Round(time.Millisecond).String())
	s.log.Warn("cycle interrupted by shutdown during matching",
		append([]any{"cause", context.Cause(ctx)}, attrs...)...)
	return true
}

// finishCompletedCycle runs the compare over the completed match result,
// reports the findings, logs the completion line
// ("cycle complete", or "cycle degraded" for a partial walk, a transient
// AniList degradation, or a stale-but-usable map), and persists the full
// refreshed state. On a partial walk the compare runs on the items that walked
// cleanly only: matches linked to Failed items are excluded (their file state
// is missing, not empty). Finding resolution is scoped so that degraded
// items' prior findings are preserved rather than falsely resolved - both the
// Failed-walk items and the entries whose needed AniList lookup failed
// transiently (match.Result.IncompleteIDs; their entries sit unmapped in the
// match set, so the compare yields no finding for them and only the
// preservation set keeps their prior findings from resolving). Always healthy.
func (s *Scout) finishCompletedCycle(ctx context.Context, start time.Time, startStats AniListStats, st *state.State, snap library.Snapshot, mapCache *mapping.Cache, entries []seadex.Entry, result match.Result, mapErr error) bool {
	cleanMatches, failedItems := splitFailedMatches(result.Matches)
	findings := s.deps.Comparer.Compare(cleanMatches)
	// Findings are reported as STATE: notify re-emits the whole current set and
	// a condition that stops being reported resolves when the alert rule's
	// lookback expires. There is no cold-start baseline and no persisted dedupe
	// table - see notify.Notifier. The preserve set scopes what replacement may
	// DELETE: an entry whose walk failed, or whose needed AniList lookup failed
	// transiently, has incomplete evidence, so its absence from findings is
	// missing data and its prior rows are carried forward. A single permanently
	// failing series holds Snapshot.Partial forever, which is what makes that
	// scoping necessary rather than decorative.
	s.deps.Notifier.Report(findings, unionIDs(failedItems, result.IncompleteIDs))
	// The in-memory set is now authoritative for the whole catalogue, so ticks
	// may publish it (see Scout.ready). This is the ONLY site that sets it:
	// every other reconcile exit either gated before the compare or was
	// interrupted, and neither establishes a set a tick may speak for.
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
	s.recordAniListDegradation(st, &result)
	s.recordPartialWalk(st, &snap)
	s.logCompletedCycle(&snap, &result, mapErr, failedItems, st.AniListDegraded, attrs)
	// A SECOND line, deliberately, carrying nothing but the fact that a full
	// pass finished. The scan deadman counts cycle/tick completion lines, and
	// once most iterations are ticks it can no longer tell "the loop is alive"
	// from "the backstop still runs". Since the reconcile IS the backstop for
	// everything a window structurally cannot see, a reconcile that silently
	// stopped forever would make every one of those gaps permanent. This line
	// is what a deadman at 3x reconcileInterval watches.
	//
	// It is emitted for EVERY reconcile that ran end to end, degraded or not.
	// The question that deadman asks is "did the backstop run", and a degraded
	// reconcile still did the whole catalogue fetch, the whole walk and the
	// whole rebuild; the "cycle degraded" line beside it carries the quality
	// signal. Emitting it only on the clean path would page after three
	// degraded days for a backstop that ran every one of them.
	s.log.Info("reconcile complete", "interval", reconcileInterval.String())

	st.Library, st.Mapping, st.Memo = snap, *mapCache, result.Memo
	s.save(ctx, st)
	return true
}

// recordAniListDegradation advances or resets the persisted AniList
// degradation streak and escalates a sustained outage. The streak
// advances/resets only on COMPLETED cycles (mirroring how SeadexFailures
// resets beside the fetch-success check): a gated or interrupted cycle is
// evidence of neither an outage nor a recovery. It runs before the completion
// line so the WARN and the escalated ERROR both carry the up-to-date streak,
// and the persisted value rides the caller's save. The escalation fires on
// EVERY completed AniList-degraded cycle at the threshold, including one
// whose completion line the partial-walk switch arm wins - otherwise a
// sustained AniList outage that coexists with a persistent partial walk
// advances the streak forever without ever alerting.
func (s *Scout) recordAniListDegradation(st *state.State, result *match.Result) {
	if !result.Degraded {
		st.AniListDegraded = 0
		return
	}
	st.AniListDegraded++
	if st.AniListDegraded >= aniListDegradedEscalationThreshold {
		s.log.Error("anilist lookups degraded repeatedly; matching incomplete and findings frozen for affected entries - inspect graphql.anilist.co reachability and egress",
			"incomplete_lookups", len(result.IncompleteIDs),
			"consecutive_anilist_degraded", st.AniListDegraded)
	}
}

// recordPartialWalk advances or resets the persisted partial-walk streak and
// escalates a sustained partial ingest. It mirrors recordAniListDegradation
// exactly: the streak advances/resets only on COMPLETED cycles (a gated or
// interrupted cycle observed no walk verdict), it runs before the completion
// line so the escalated ERROR precedes the degraded-cycle line it explains,
// and the persisted value rides the caller's save. Unlike the AniList arm the
// streak is NOT threaded into logCompletedCycle, so consecutive_partial_walks
// appears on the escalated ERROR only. Without it a single permanently failing
// series is signalled only by the per-cycle "reason=partial-walk" WARN forever -
// and since those items' findings are carried forward on evidence that never
// refreshes, nothing would ever escalate the silence.
func (s *Scout) recordPartialWalk(st *state.State, snap *library.Snapshot) {
	if !snap.Partial {
		st.PartialWalks = 0
		return
	}
	st.PartialWalks++
	if st.PartialWalks >= partialWalkEscalationThreshold {
		s.log.Error("library walk partial repeatedly; the failing series never compare and the one-shot report refuses a partial snapshot, so those items' findings are carried forward on evidence that never refreshes - inspect the arrs' episode endpoints for the skipped series",
			"consecutive_partial_walks", st.PartialWalks)
	}
}

// logCompletedCycle emits the one completion line the deadman alert counts:
// "cycle complete", or "cycle degraded" with the most severe applicable
// reason (partial walk, then AniList degradation, then a stale-but-usable
// map, then an arr side emptied by its tag filter).
func (s *Scout) logCompletedCycle(snap *library.Snapshot, result *match.Result, mapErr error, failedItems map[int]struct{}, aniListStreak int, attrs []any) {
	switch {
	case snap.Partial:
		// A partial walk compared only the clean items, so the cycle closed
		// degraded: report the degraded coverage on the completion line the
		// deadman alert counts alongside "cycle complete".
		s.cycleDegraded("partial-walk", append([]any{"failed_items", len(failedItems)}, attrs...)...)
	case result.Degraded:
		// A transient AniList failure left some entries' needed lookups
		// incomplete: the compare ran on the unaffected majority with the
		// affected entries' rows carried forward, but the cycle must not
		// read as fully successful. Same reason attr as before the scoped
		// handling, so the deadman and any reason-keyed queries stay stable.
		// The persisted streak's SUSTAINED-degradation ERROR escalation (the
		// SeadexScoutCycleError rule) lives in recordAniListDegradation,
		// beside the streak update, so it fires even when the partial-walk
		// arm wins this completion line.
		s.cycleDegraded("anilist-degraded",
			append([]any{
				"incomplete_lookups", len(result.IncompleteIDs),
				"consecutive_anilist_degraded", aniListStreak,
			}, attrs...)...)
	case mapErr != nil:
		// Only a stale-but-usable mapping error reaches this point; unusable and
		// cancelled loads returned at the pre-compare gate. The compare ran on
		// the cached map, but the cycle is still upstream-degraded, so it must
		// not read as fully successful.
		s.cycleDegraded("mapping-stale", attrs...)
	case snap.FilteredEmpty:
		// arr_tags filtering kept nothing out of a non-empty arr list on at
		// least one enabled side, so the cycle watched a library the operator
		// did not intend: a dead include set (every label renamed, mistyped, or
		// expanded from an unset ${VAR}) admits nothing, and every prior
		// finding on that side resolves. The walk itself succeeded, so this
		// stays the LEAST severe reason and never fails the cycle - the
		// remedy is a config or arr-side fix, and the walker's WARN names
		// which side and how many items it listed. Without this arm the
		// steady state read "cycle complete" forever: the shrink guard fires
		// for at most one cycle (it then persists the empty snapshot as the
		// baseline) and not at all on a first-ever boot.
		s.cycleDegraded("tags-emptied-side", attrs...)
	default:
		s.log.Info("cycle complete", attrs...)
	}
}

// splitFailedMatches partitions the match set around the model's placeholder
// rule (library.Item.Comparable): a match linked to an item whose file data the
// walk could not establish - a series whose episode fetch failed, a movie
// Radarr reports a file for without sending its payload - is excluded from the
// compare (its file state is missing, not empty, so comparing would misread
// every recommendation as unmet), and those items' AniList IDs are returned so
// finding resolution can preserve their prior findings. A walk with no
// placeholders returns the matches untouched and a nil set.
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

// --- feed rebuild ---

// rebuildFeed refreshes the indexer's Torznab feed from the cycle's shared
// SeaDex snapshot, independent of the arr walk (the feed needs only SeaDex +
// Fribb + persisted state, so an arr outage must not freeze it). It is a no-op
// when no feed is configured, the SeaDex fetch failed, or the map is unusable
// (a load error that is NOT a mapping.StaleMapError) - the last-good feed is
// then kept: rebuilding against an unusable map would categorize every entry as
// anime and silently drop all SeaDex movies from Radarr's RSS view. A
// stale-but-usable map (errs.mapping matches mapping.StaleMapError, which carries a
// usable cached index) still rebuilds, exactly like the pre-compare gate's
// discrimination. The per-show metadata closure is built over PERSISTED state
// (st's library snapshot and AniList memo, loaded at cycle start) - never this
// cycle's walk - so the title synthesis inherits the same arr-independence.
func (s *Scout) rebuildFeed(ctx context.Context, entries []seadex.Entry, idx *mapping.Index, st *state.State, errs cycleOutcomes) {
	if s.deps.Feed == nil || errs.seadex != nil || len(entries) == 0 || !mapUsable(errs.mapping) {
		return
	}
	info := feedEntryInfo(idx, &st.Library, st.Memo)
	if err := s.deps.Feed.Rebuild(ctx, entries, info); err != nil && ctx.Err() == nil {
		// A cancelled rebuild is the shutdown, not a feed fault; the pre-compare
		// gate logs the interruption (the last-good feed is kept either way).
		s.log.Warn("indexer feed rebuild failed; keeping previous feed", "error", err)
	}
}

// advanceFeed is the tick's half of rebuildFeed: it folds the window into the
// persisted journal instead of rebuilding it. The gates are the same shape -
// no feed configured, nothing to fold, or no usable mapping means no work - and
// a failure keeps the last-good feed rather than degrading the tick.
func (s *Scout) advanceFeed(ctx context.Context, window []seadex.Entry, idx *mapping.Index, st *state.State) {
	if s.deps.Feed == nil || len(window) == 0 || idx == nil {
		return
	}
	info := feedEntryInfo(idx, &st.Library, st.Memo)
	if err := s.deps.Feed.Advance(ctx, window, info); err != nil && ctx.Err() == nil {
		s.log.Warn("indexer feed advance failed; keeping previous feed", "error", err)
	}
}

// logFeedOutageOnGatedCycle surfaces a concurrent SeaDex zero-entry response
// when an earlier gate (a failed arr walk, a suspicious shrunken walk, or an
// unusable mapping) already closed the cycle but a feed is configured, so a
// multi-dependency outage does not read as the gate's primary failure only.
// A FAILED fetch is not logged here: recordSeaDexFetch already logs it exactly
// once ahead of gate selection (carrying feed_kept), so this stays silent then
// rather than duplicating it. During a shutdown the SeaDex failure is the
// cancellation (the interruption is logged by the gate that owns it), so it
// stays silent then too.
func (s *Scout) logFeedOutageOnGatedCycle(ctx context.Context, entries []seadex.Entry, seaErr error) {
	if s.deps.Feed == nil || ctx.Err() != nil || seaErr != nil {
		return
	}
	if len(entries) == 0 {
		s.log.Warn("seadex returned zero entries; indexer feed kept previous feed")
	}
}

// --- pre-compare gates ---

// cycleOutcomes carries one cycle's three independent ingest/upstream results so
// the gate helpers read them by NAME: three same-typed positional error
// parameters compile in any order, and a transposition silently swaps which gate
// fires.
type cycleOutcomes struct {
	walk    error
	mapping error
	seadex  error
}

// handlePreCompareGate applies the pre-compare degradation gate: it reports
// whether the cycle should stop before the compare pass (handled) and, when it
// should, the health outcome to return. The library gate (failed walk,
// suspicious shrunken walk) runs first, then the upstream gate
// (shutdown cancellation, unusable map, failed/empty SeaDex fetch); see each
// helper for the per-branch policy. A stale-but-usable map (errs.mapping matches
// mapping.StaleMapError) is degraded-but-comparable and flows into the normal
// compare path (handled=false).
func (s *Scout) handlePreCompareGate(ctx context.Context, st *state.State, snap library.Snapshot, mapCache *mapping.Cache, entries []seadex.Entry, errs cycleOutcomes) (handled, healthy bool) {
	// Record the SeaDex fetch outcome (and log a failure) exactly once, before
	// the mutually exclusive gates below pick a winner: gate precedence must
	// not decide whether an observed SeaDex outage exists in persisted state.
	s.recordSeaDexFetch(ctx, st, errs.seadex)
	if handled, healthy := s.handleLibraryGate(ctx, st, snap, mapCache, entries, errs); handled {
		return true, healthy
	}
	return s.handleUpstreamGate(ctx, st, snap, mapCache, entries, errs)
}

// recordSeaDexFetch records the cycle's SeaDex fetch outcome in the persisted
// state and, on a failure, emits its single log line - before the mutually
// exclusive pre-compare gates run, the same way recordAniListDegradation is
// applied ahead of the completion-line precedence. Centralizing it here is
// what makes the streak (state.State.SeadexFailures) independent of which gate
// closes the cycle: the failed-walk, shrunk-walk, and mapping-unusable arms all
// save state, so a double outage still advances the streak and still escalates
// to ERROR at seadexFailureEscalationThreshold instead of WARNing forever
// behind a higher-precedence gate. A successful fetch resets the streak (the
// documented "resets to 0 on any successful fetch" contract); a cancelled fetch
// (a shutdown) is evidence of neither an outage nor a recovery, so it leaves the
// streak untouched and stays silent - the gate that owns the interruption logs
// it. feed_kept records whether a configured Torznab feed kept its previous
// snapshot through this outage.
func (s *Scout) recordSeaDexFetch(ctx context.Context, st *state.State, seaErr error) {
	if seaErr == nil {
		st.SeadexFailures = 0
		return
	}
	if ctx.Err() != nil {
		return
	}
	// The persisted streak escalates this single log site to ERROR (the
	// SeadexScoutCycleError rule) once the outage has spanned
	// seadexFailureEscalationThreshold consecutive cycles; below it the WARN
	// keeps an upstream blip off the alert. Both levels carry the streak so
	// Loki can see how long the outage has run.
	st.SeadexFailures++
	attrs := []any{attrError, logSafeUpstreamError(seaErr), "consecutive_seadex_failures", st.SeadexFailures, "feed_kept", s.deps.Feed != nil}
	if st.SeadexFailures >= seadexFailureEscalationThreshold {
		s.log.Error("seadex fetch failed repeatedly; skipping comparison, findings not re-reported this cycle - inspect SeaDex (releases.moe) reachability and egress", attrs...)
	} else {
		s.log.Warn("seadex fetch failed; skipping comparison, findings not re-reported this cycle", attrs...)
	}
}

// handleLibraryGate gates the compare pass on the library ingest. A failed arr
// walk is unhealthy and persists only the refreshed mapping cache (findings,
// memo, and the prior library snapshot ride along untouched). A non-failed
// walk (partial included - Failed placeholders keep the item count, so a
// shrink means the arr's series list itself shrank) that shrank below half
// the prior snapshot's items (degradation.Shrunk, the shared below-half
// policy home; zero items is the extreme case) is degraded but healthy: it
// persists ONLY the refreshed mapping cache plus the consecutive shrunk-walk
// streak, so a shrunken snapshot can never replace st.Library and
// mass-resolve findings (now or a cycle later), and never auto-accepts. A
// partial snapshot (per-series episode-fetch failures) is NOT gated here: the
// compare proceeds on the items that walked cleanly, with the Failed items'
// rows carried forward by replacement scoping (see finishCompletedCycle).
func (s *Scout) handleLibraryGate(ctx context.Context, st *state.State, snap library.Snapshot, mapCache *mapping.Cache, entries []seadex.Entry, errs cycleOutcomes) (handled, healthy bool) {
	if errs.walk != nil {
		// With a feed configured, Cycle fell through the walk failure so the
		// arr-independent feed could still refresh. If SeaDex ALSO failed (or
		// returned nothing), rebuildFeed silently kept the previous feed and this
		// early return would swallow that second outage - surface it here so a
		// multi-dependency outage does not read as arr-only. Single SeaDex
		// failures (walk healthy) keep their own WARNs in the upstream gate, so
		// no duplicates.
		s.logFeedOutageOnGatedCycle(ctx, entries, errs.seadex)
		// Persist only the refreshed mapping cache, like the shrunk-walk arm
		// below: discarding it re-downloads an updated Fribb body next cycle.
		// Findings, memo, and the prior library snapshot ride along untouched
		// (an unusable-map load returns the prior cache, making this persist a
		// no-op then).
		st.Mapping = *mapCache
		s.save(ctx, st)
		// The cycle ran to its degraded end (the feed refresh above was the
		// remaining work), so emit the completion line the cycle deadman
		// counts; the walk's ERROR already carries the fault, so the deadman
		// stays quiet during an arr outage and fires only on a wedged loop.
		// A shutdown that landed after the walk failure (cancelling the
		// SeaDex fetch or mapping load) keeps the no-completion-line rule:
		// an interrupted cycle did not complete, degraded or not.
		if ctx.Err() == nil {
			// Same reduction + failed-side attribution as
			// stopAfterWalkFailure's log sites: the walk error may embed a
			// credential-bearing request URL, and the reduced error alone
			// does not name the failed arr.
			s.cycleDegraded("walk-failed", walkFailureAttrs(errs.walk)...)
		}
		return true, false
	}
	if len(st.Library.Items) > 0 && degradation.Shrunk(len(snap.Items), len(st.Library.Items)) {
		// Like the walk-failed arm above, this gate skips the compare after
		// rebuildFeed already ran: if SeaDex ALSO failed (or returned
		// nothing), the previous feed was silently kept - surface it so a
		// shrink + SeaDex double outage does not read as shrink-only.
		s.logFeedOutageOnGatedCycle(ctx, entries, errs.seadex)
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
			s.log.Error("library walk shrank repeatedly; skipping comparison, findings not re-reported this cycle - inspect the arrs and arr_tags, or remove state.json to accept the smaller library", attrs...)
		} else {
			s.log.Warn("library walk shrank below half the prior snapshot; skipping comparison, findings not re-reported this cycle", attrs...)
		}
		// A shutdown that landed after the shrunken walk (cancelling the
		// SeaDex fetch or mapping load) keeps the no-completion-line rule,
		// mirroring the walk-failed arm above: an interrupted cycle did not
		// complete, degraded or not. The shrink WARN and streak stay - the
		// shrink evidence comes from the completed walk.
		if ctx.Err() == nil {
			s.cycleDegraded("library-shrunk", attrs...)
		}
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
// outage does not falsely resolve live findings. The failed-fetch arm's
// persisted consecutive-failure streak (state.State.SeadexFailures) and its
// single WARN/ERROR log site live in recordSeaDexFetch, applied ahead of gate
// selection so a higher-precedence gate cannot hide the outage from the streak;
// this arm only saves, emits the completion line, and returns. A shutdown
// cancellation during the load or fetch is attributed to the shutdown, not
// the upstream.
func (s *Scout) handleUpstreamGate(ctx context.Context, st *state.State, snap library.Snapshot, mapCache *mapping.Cache, entries []seadex.Entry, errs cycleOutcomes) (handled, healthy bool) {
	if ctx.Err() != nil && (errs.mapping != nil || errs.seadex != nil) {
		// A shutdown/redeploy cancelled the cycle during the mapping load or
		// SeaDex fetch: the errors are the cancellation, not an upstream fault.
		// Preserve findings exactly like an upstream outage (degradedSave) but
		// attribute the interruption to the shutdown instead of blaming Fribb
		// or SeaDex (matching the library-walk and matching paths). The
		// SeaDex-failure streak is untouched: a cancelled fetch is evidence of
		// neither an outage nor a recovery.
		s.degradedSave(ctx, st, snap, mapCache)
		s.log.Warn("cycle interrupted by shutdown before comparison; findings not re-reported this cycle",
			"cause", context.Cause(ctx))
		return true, true
	}
	if !mapUsable(errs.mapping) {
		// Like the walk-failed and shrunk-walk arms, this gate closes the
		// cycle before the errs.seadex arm below: if SeaDex ALSO failed (or
		// returned nothing), rebuildFeed silently kept the previous feed -
		// surface it so a mapping + SeaDex double outage does not read as
		// mapping-only.
		s.logFeedOutageOnGatedCycle(ctx, entries, errs.seadex)
		s.degradedSave(ctx, st, snap, mapCache)
		// Same feed_kept signal recordSeaDexFetch and the zero-entries arm
		// attach: an unusable map skips the feed rebuild too (see rebuildFeed,
		// which will not categorize every entry as anime), so a configured feed
		// is serving its previous snapshot. This was the one rebuild-skip cause
		// with no feed-attributed signal anywhere in the cycle's output.
		s.log.Warn("mapping unusable; skipping comparison, findings not re-reported this cycle",
			"error", errs.mapping, "feed_kept", s.deps.Feed != nil)
		s.cycleDegraded("mapping-unusable", "error", errs.mapping)
		return true, true
	}
	if errs.seadex != nil {
		// The failure was already recorded and logged once by
		// recordSeaDexFetch (ahead of gate selection), so this arm only owns
		// the degraded save, the completion line, and the verdict.
		s.degradedSave(ctx, st, snap, mapCache)
		s.cycleDegraded("seadex-fetch-failed", "error", logSafeUpstreamError(errs.seadex))
		return true, true
	}
	if len(entries) == 0 {
		s.degradedSave(ctx, st, snap, mapCache)
		// Same feed_kept signal recordSeaDexFetch attaches to a failed fetch: a
		// zero-entries response skips the rebuild too (see rebuildFeed), so a
		// configured feed is serving its previous snapshot.
		s.log.Warn("seadex returned zero entries; skipping comparison, findings not re-reported this cycle",
			"feed_kept", s.deps.Feed != nil)
		// A shutdown that landed after a nil-error zero-entry fetch keeps the
		// no-completion-line rule, mirroring the two library-gate arms: the
		// ctx arm above pre-empts only an ERRORED load/fetch, so a cancelled
		// cycle can still reach this defensive arm.
		if ctx.Err() == nil {
			s.cycleDegraded("seadex-zero-entries")
		}
		return true, true
	}
	return false, true
}

// --- one-shot report ---

// reportSnapshot walks the library for a one-shot report, failing on a walk
// error or a partial snapshot: auditing an incomplete snapshot would publish a
// successful, timestamped report that silently omits the skipped series,
// contradicting the whole-library audit contract.
func (s *Scout) reportSnapshot(ctx context.Context) (library.Snapshot, error) {
	snap, err := s.deps.Library.Walk(ctx)
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
func (s *Scout) reportMapping(ctx context.Context, st *state.State) (*mapping.Index, error) {
	mapCache, idx, mapErr := s.deps.Mapping.Load(ctx, &st.Mapping)
	if mapErr == nil || ctx.Err() != nil {
		return idx, nil
	}
	if !mapUsable(mapErr) {
		return nil, fmt.Errorf("mapping unusable: %w", mapErr)
	}
	// The report never escalates: it is a read-only one-shot, and the operator
	// is watching its exit. The streak is still reported for parity with the
	// cycle's attribute set.
	s.log.Warn("report: mapping degraded", mappingDegradedAttrs(mapErr, idx.Len(), mapCache.RejectedRefreshes)...)
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
func (s *Scout) Report(ctx context.Context) (audit.Report, error) {
	start := time.Now()
	st := s.loadState(ctx)

	snap, err := s.reportSnapshot(ctx)
	if err != nil {
		return audit.Report{}, err
	}
	if len(st.Library.Items) > 0 && degradation.Shrunk(len(snap.Items), len(st.Library.Items)) {
		// The daemon gates its whole compare on exactly this shape
		// (handleLibraryGate's shrink guard): a non-failed walk retaining under
		// half the last persisted snapshot is a suspicious truncation, not a
		// real change. The report still renders - it is read-only, cannot clear
		// the daemon's gate, and is the operator's fallback view when the cycle
		// is stuck - but it must SAY so, or the timestamped artifact silently
		// omits every missing series, exactly the incompleteness reportSnapshot
		// refuses a partial snapshot over. No prior snapshot (report-only
		// deployments never persist one) means no baseline, so the check
		// no-ops there rather than guessing.
		s.log.Warn("report: library walk shrank below half the last persisted snapshot; the audit covers the smaller library - inspect the arrs and arr_tags",
			"items", len(snap.Items), "prior_items", len(st.Library.Items))
	}

	idx, err := s.reportMapping(ctx, &st)
	if err != nil {
		return audit.Report{}, err
	}

	entries, err := s.deps.SeaDex.FetchEntries(ctx, seadex.Options{Mode: seadex.FetchFull})
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
	s.warnCatalogueLinkQuality(entries)
	if len(entries) == 0 {
		// Defense in depth: FetchEntries errors on an empty completed
		// catalogue, but a future client regression returning (nil, nil) would
		// otherwise publish a successful report marking every library item
		// not_on_seadex - refuse instead, mirroring Cycle's zero-entries
		// degradation gate.
		return audit.Report{}, errors.New("seadex fetch: returned zero entries")
	}

	result := s.deps.Matcher.Match(ctx, entries, &snap, idx, st.Memo)
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
		s.log.Warn("report: anilist degraded; affected entries listed in the incomplete section",
			"incomplete_lookups", len(result.IncompleteIDs))
	}
	rep := s.deps.Auditor.Audit(result.Matches, &snap, idx, result.IncompleteIDs)
	s.log.Info("report generated",
		"seadex_entries", len(entries),
		"library_items", len(snap.Items),
		"rows", len(rep.Rows),
		"incomplete_mappings", len(rep.Incomplete),
		"duration", time.Since(start).Round(time.Millisecond).String())
	return rep, nil
}

// --- state + stats helpers ---

// aniStats returns the AniList client's cumulative stats via the injected
// callback, or zero stats when none is wired (the early-return degradation
// paths and unit tests build Deps without one; the daemon always wires it).
func (s *Scout) aniStats() AniListStats {
	if s.deps.AniListStats == nil {
		return AniListStats{}
	}
	return s.deps.AniListStats()
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

// saveGrace bounds the detached shutdown retry, measured from the cancellation
// that triggered it — the SIGTERM at which the container stop grace itself
// starts. It stays inside Docker's default 10s stop grace (the public compose
// example sets no stop_grace_period), so the write completes before SIGKILL.
// atomicfile's temp+rename means a SIGKILL mid-write cannot corrupt state - the
// only cost of a missed save is losing the AniList memo, which self-heals over
// one cold cycle.
const saveGrace = 5 * time.Second

// save persists state, tolerating a shutdown mid-cycle. When the run context is
// cancelled (SIGTERM during a redeploy), the atomic write fails with
// context.Canceled and the caches are lost — so a cancellation is retried once
// with a detached, briefly-bounded context (context.WithoutCancel keeps the
// values, drops the cancellation), letting the write finish so the expensive
// AniList memo survives the restart. The retry always runs and gets a full
// saveGrace measured from the CANCELLATION rather than from entry into save,
// because the first attempt never spends the container stop grace either way:
// on the paths that reach save already cancelled, the store refuses at its
// context check before encoding anything (state.prepareSave), and when the
// cancellation instead lands mid-write, the elapsed encode (library snapshot,
// the ~5.9 MB mapping cache, the memo) began before the SIGTERM. Subtracting
// it would starve the retry to zero in exactly the slow-volume case the retry
// exists for. A cancellation is not a fault (a redeploy is routine), so only a
// genuine write failure is logged at ERROR — which keeps it off the cycle-error
// alert. A deliberate preservation refusal (state.ErrSavePreserved) is likewise
// not a fault and logs at WARN: the redeploy SIGTERM that cancels the cycle can
// land in Load's read window, which blocks the save by design, and alerting on
// that would page the operator on every redeploy.
func (s *Scout) save(ctx context.Context, st *state.State) {
	err := s.deps.Store.Save(ctx, st)
	if err != nil && (errors.Is(err, context.Canceled) || ctx.Err() != nil) {
		// The container stop grace starts at the SIGTERM that cancelled ctx, so
		// the retry budget is anchored HERE rather than shortened by the first
		// attempt (the doc above says why); saveGrace < Docker's 10s default
		// keeps the post-SIGTERM total inside the grace.
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), saveGrace)
		err = s.deps.Store.Save(dctx, st)
		cancel()
	}
	if err != nil {
		// A deliberate preservation refusal is not a write fault: nothing is
		// broken and nothing was lost, the store is protecting bytes it could
		// not classify. Reporting it at ERROR fires the cycle-error alert on a
		// routine redeploy, because a SIGTERM landing in Load's read window is
		// exactly what sets that block. Report it as the degradation it is.
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
