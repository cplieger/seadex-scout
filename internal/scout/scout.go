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

	"github.com/cplieger/httpx/v3"
	"github.com/cplieger/seadex-scout/internal/audit"
	"github.com/cplieger/seadex-scout/internal/compare"
	"github.com/cplieger/seadex-scout/internal/degradation"
	"github.com/cplieger/seadex-scout/internal/indexer"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/match"
	"github.com/cplieger/seadex-scout/internal/notify"
	"github.com/cplieger/seadex-scout/internal/seadex"
	"github.com/cplieger/seadex-scout/internal/state"
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
	Rebuild(ctx context.Context, entries []seadex.Entry, info func(alID int) indexer.EntryInfo) error
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

// Deps are the assembled components a Scout runs a cycle with.
type Deps struct {
	Logger   *slog.Logger
	Store    StateStore
	Library  *library.Walker
	Mapping  MappingSource
	SeaDex   SeaDexSource
	Matcher  *match.Matcher
	Comparer *compare.Comparer
	Auditor  *audit.Auditor
	Notifier *notify.Notifier
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
// fewer than 1/libraryShrinkFactor of the prior snapshot's items (below half
// at the default 2) is treated as suspicious (a misconfigured arr_tags
// filter, an emptied or fresh arr) rather than a real library change. The
// zero-items case is the extreme of the same shrink. It references
// degradation.ShrinkGuardFactor - the single home of the below-half policy
// this guard shares with the mapping loader's refresh shrink guard - rather
// than re-declaring the fraction.
const libraryShrinkFactor = degradation.ShrinkGuardFactor

// shrunkWalkEscalationThreshold is the consecutive-shrunk-walk streak
// (state.State.ShrunkWalks) at which the scout escalates its shrunk-walk log
// from WARN to ERROR (firing the existing SeadexScoutCycleError Loki rule).
// It references degradation.EscalationThreshold - the single home of the
// shared escalation policy: tolerate 8 consecutive degraded cycles, about a
// day at the default 3h cadence, before escalating - long enough to ride out
// a transient arr oddity, short enough that a persistent misconfiguration
// (arr_tags leaving one item) alerts instead of silently skipping the compare
// forever. The remedy is operator-driven: fix the arr/tags, or remove
// state.json to accept the smaller library - the guard never auto-accepts a
// shrunken walk.
const shrunkWalkEscalationThreshold = degradation.EscalationThreshold

// seadexFailureEscalationThreshold is the consecutive-failed-fetch streak
// (state.State.SeadexFailures) at which the scout escalates its single
// seadex-fetch-failed log site from WARN to ERROR (firing the existing
// SeadexScoutCycleError rule). It references
// degradation.EscalationThreshold - the single home of the shared
// escalation policy the shrunk-walk and mapping-rejection streaks already
// ride: tolerate 8 consecutive degraded cycles, about a day at the default
// 3h cadence, before escalating. A SeaDex blip must stay a WARN (findings
// are preserved and a restart cannot fix an upstream outage), but an outage
// that persists for a day is a lasting upstream fault or an egress
// misconfiguration and must alert instead of WARNing forever. Recovery is
// operator-free: the first successful fetch resets the streak.
const seadexFailureEscalationThreshold = degradation.EscalationThreshold

// aniListDegradedEscalationThreshold is the consecutive anilist-degraded
// completed-cycle streak (state.State.AniListDegraded) at which the scout
// escalates its single anilist-degraded log site from WARN to ERROR (firing
// the existing SeadexScoutCycleError rule). It references
// degradation.EscalationThreshold - the single home of the shared escalation
// policy its three sibling streaks already ride. A transient AniList blip
// must stay a WARN (the compare ran on the unaffected majority with the
// affected findings preserved), but a degradation that persists for a day is
// a lasting egress or upstream fault - and under a cold start it keeps the
// baseline permanently incomplete, silently suppressing every notification -
// so it must alert instead of WARNing forever. Recovery is operator-free:
// the first undegraded completed cycle resets the streak.
const aniListDegradedEscalationThreshold = degradation.EscalationThreshold

// Scout runs compare cycles from its assembled dependencies.
type Scout struct {
	deps Deps
	log  *slog.Logger
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

// New builds a Scout from deps.
func New(deps *Deps) *Scout {
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Scout{deps: *deps, log: log}
}

// --- cycle orchestration ---

// Cycle runs one full compare cycle and reports whether the run was healthy
// (the library ingest succeeded). It never returns an error: a failed ingest
// returns false, and an upstream (SeaDex/mapping/AniList) failure returns true
// but degraded.
func (s *Scout) Cycle(ctx context.Context) bool {
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
	entries, seaErr := s.deps.SeaDex.FetchEntries(ctx)

	// Rebuild the Torznab feed from the shared snapshot, independent of the arr
	// walk (see rebuildFeed): a notification and what the arrs see in the feed
	// come from this one fetch. The persisted state (library titles + AniList
	// memo) feeds only the title synthesis, never a fresh arr walk.
	s.rebuildFeed(ctx, entries, idx, &st, mapErr, seaErr)

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
	if result.Degraded && ctx.Err() != nil {
		// A shutdown/redeploy cancelled the cycle mid-matching: the match set
		// is truncated (entries after the cancellation were never attempted),
		// so comparing it would falsely resolve their findings. Keep the
		// whole-cycle skip for this one case; a transient AniList degradation
		// instead carries Result.IncompleteIDs and flows into the compare
		// below with exactly the affected entries' findings preserved.
		return s.finishInterruptedMatch(ctx, start, startStats, &st, snap, &mapCache, result)
	}
	return s.finishCompletedCycle(ctx, start, startStats, &st, snap, &mapCache, entries, result, mapErr)
}

// stopAfterWalkFailure logs a failed library walk and reports whether Cycle
// should stop immediately. A genuine walk
// failure is unhealthy (a shutdown-cancelled walk never reaches this - Cycle
// attributes it to the shutdown and stays healthy); an alert-only deployment
// (no Torznab feed) stops right
// away since nothing else remains to do - emitting the "cycle degraded"
// completion line beside the ERROR - while a configured feed falls through
// so the arr-independent feed rebuild still runs (the pre-compare gate then
// returns unhealthy and emits the completion line).
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
// (library.WalkErrArr). The side must come from the ORIGINAL error: the
// reduction collapses the chain to the *url.Error's underlying cause,
// discarding library.Walk's textual "walking sonarr/radarr" wrapper, so with
// both arrs enabled the reduced error alone would not say which dependency
// failed.
func walkFailureAttrs(walkErr error) []any {
	attrs := []any{attrError, httpx.LogSafeError(walkErr)}
	if arr := library.WalkErrArr(walkErr); arr != "" {
		attrs = append(attrs, "arr", arr)
	}
	return attrs
}

// loadMapping refreshes the Fribb map from the persisted cache, logging a
// degraded load once. A cancelled load is the shutdown, not a Fribb fault; the
// pre-compare gate logs the interruption instead (same rule as the
// walk/matching paths). The degraded log is WARN, escalating to ERROR (which
// fires the existing SeadexScoutCycleError Loki rule) once the loader's
// acceptance guards have rejected degradation.EscalationThreshold
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
		if ok && stale.ConsecutiveRejections() >= degradation.EscalationThreshold {
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
	attrs := []any{attrError, mapErr, "usable_records", usableRecords}
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
func (s *Scout) finishInterruptedMatch(ctx context.Context, start time.Time, startStats aniListStats, st *state.State, snap library.Snapshot, mapCache *mapping.Cache, result match.Result) bool {
	st.Library, st.Mapping, st.Memo = snap, *mapCache, result.Memo
	s.save(ctx, st)
	attrs := append(s.aniListCycleAttrs(startStats),
		"duration", time.Since(start).Round(time.Millisecond).String())
	s.log.Warn("cycle interrupted by shutdown during matching",
		append([]any{"cause", context.Cause(ctx)}, attrs...)...)
	return true
}

// finishCompletedCycle runs the compare over the completed match result,
// emits (or cold-start baselines) the findings, logs the completion line
// ("cycle complete", or "cycle degraded" for a partial walk, a transient
// AniList degradation, or a stale-but-usable map), and persists the full
// refreshed state. On a partial walk the compare runs on the items that walked
// cleanly only: matches linked to Failed items are excluded (their file state
// is missing, not empty). Finding resolution is scoped so that degraded
// items' prior findings are preserved rather than falsely resolved - both the
// Failed-walk items and the entries whose needed AniList lookup failed
// transiently (match.Result.IncompleteIDs; their entries sit unmapped in the
// match set, so the compare yields no finding for them and only the
// preservation set keeps their prior findings from resolving). During the
// cold-start window a partial or AniList-degraded cycle seeds an incomplete
// baseline instead (see the gate below). Always healthy.
func (s *Scout) finishCompletedCycle(ctx context.Context, start time.Time, startStats aniListStats, st *state.State, snap library.Snapshot, mapCache *mapping.Cache, entries []seadex.Entry, result match.Result, mapErr error) bool {
	cleanMatches, failedItems := splitFailedMatches(result.Matches)
	findings := s.deps.Comparer.Compare(cleanMatches)
	newFindings := s.reconcileFindings(st, findings,
		unionIDs(failedItems, result.IncompleteIDs), snap.Partial || result.Degraded, time.Now())

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
	s.logCompletedCycle(&snap, &result, mapErr, failedItems, st.AniListDegraded, attrs)

	st.Library, st.Mapping, st.Memo, st.Findings = snap, *mapCache, result.Memo, newFindings
	s.save(ctx, st)
	return true
}

// reconcileFindings emits (or cold-start baselines) this cycle's findings
// against the persisted dedupe table, returning the refreshed table. A cold
// start (a fresh install, or a lost/reset cache) has no dedupe table yet:
// baseline the current findings silently so the whole pre-existing backlog is
// not dumped as notifications at once. A partial first walk (or an
// AniList-degraded first match) seeds the same way but records the baseline
// as incomplete (state.BaselineIncomplete): the seed covers only the items
// that walked cleanly and mapped completely, so every following successful
// cycle keeps seeding silently - the affected items' pre-existing backlog
// must not burst as fresh notifications when they recover - until the first
// complete cycle seeds the whole library and clears the flag. Steady-state
// emission then resumes via Notify. The len(Findings) guard keeps an upgrade
// of an already-running instance (state predating the flags but already
// holding findings) on the normal emit path. One cell stays conservative: a
// state with no findings and no flags set (an upgraded fully-aligned
// instance, or an install whose first cycles were all degraded) is
// indistinguishable from a cold start and baselines, preferring a one-cycle
// silent seed over bursting a whole backlog - a finding first appearing in
// exactly that cycle is seeded, not emitted. The full list is always
// available on demand via report mode.
func (s *Scout) reconcileFindings(st *state.State, findings []compare.Finding, preserve map[int]struct{}, incomplete bool, now time.Time) map[string]notify.Alerted {
	if st.BaselineIncomplete || (!st.Baselined && len(st.Findings) == 0) {
		current := s.deps.Notifier.Baseline(findings, now)
		st.Baselined = true
		st.BaselineIncomplete = incomplete
		return current
	}
	// Resolution scoping: a prior finding whose entry sits in the preserve
	// set - a Failed-walk item's AniList id, or an id whose needed AniList
	// lookup failed transiently this cycle - is carried forward unresolved
	// (its absence from findings is missing data, not alignment), while
	// the unaffected majority emits and resolves normally.
	st.Baselined = true
	return s.deps.Notifier.Notify(findings, st.Findings, preserve, now)
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

// logCompletedCycle emits the one completion line the deadman alert counts:
// "cycle complete", or "cycle degraded" with the most severe applicable
// reason (partial walk, then AniList degradation, then a stale-but-usable
// map).
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
		// affected entries' prior findings preserved, but the cycle must not
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
	default:
		s.log.Info("cycle complete", attrs...)
	}
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
// stale-but-usable map (mapErr matches mapping.StaleMapError, which carries a
// usable cached index) still rebuilds, exactly like the pre-compare gate's
// discrimination. The per-show metadata closure is built over PERSISTED state
// (st's library snapshot and AniList memo, loaded at cycle start) - never this
// cycle's walk - so the title synthesis inherits the same arr-independence.
func (s *Scout) rebuildFeed(ctx context.Context, entries []seadex.Entry, idx *mapping.Index, st *state.State, mapErr, seaErr error) {
	if s.deps.Feed == nil || seaErr != nil || len(entries) == 0 || !mapUsable(mapErr) {
		return
	}
	info := feedEntryInfo(idx, &st.Library, st.Memo)
	if err := s.deps.Feed.Rebuild(ctx, entries, info); err != nil && ctx.Err() == nil {
		// A cancelled rebuild is the shutdown, not a feed fault; the pre-compare
		// gate logs the interruption (the last-good feed is kept either way).
		s.log.Warn("indexer feed rebuild failed; keeping previous feed", "error", err)
	}
}

// logFeedOutageOnGatedCycle surfaces a concurrent SeaDex outage when an
// earlier gate (a failed arr walk, a suspicious shrunken walk, or an
// unusable mapping) already closed the cycle but a feed is configured, so a
// multi-dependency outage does not read as the gate's primary failure only.
// During a shutdown the SeaDex failure is the
// cancellation (the interruption is logged by the gate that owns it), so it
// stays silent then.
func (s *Scout) logFeedOutageOnGatedCycle(ctx context.Context, entries []seadex.Entry, seaErr error) {
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

// --- pre-compare gates ---

// handlePreCompareGate applies the pre-compare degradation gate: it reports
// whether the cycle should stop before the compare pass (handled) and, when it
// should, the health outcome to return. The library gate (failed walk,
// suspicious shrunken walk) runs first, then the upstream gate
// (shutdown cancellation, unusable map, failed/empty SeaDex fetch); see each
// helper for the per-branch policy. A stale-but-usable map (mapErr matches
// mapping.StaleMapError) is degraded-but-comparable and flows into the normal
// compare path (handled=false).
func (s *Scout) handlePreCompareGate(ctx context.Context, st *state.State, snap library.Snapshot, mapCache *mapping.Cache, entries []seadex.Entry, walkErr, mapErr, seaErr error) (handled, healthy bool) {
	if seaErr == nil {
		// The SeaDex fetch genuinely succeeded, so the consecutive-failure
		// streak ends here regardless of which gate closes the cycle - the
		// walk-failed and shrunk-walk arms save state too, and the documented
		// contract (state.State.SeadexFailures) is "resets to 0 on any
		// successful fetch". A cancelled fetch (seaErr != nil) is evidence of
		// neither an outage nor a recovery and leaves the streak untouched.
		st.SeadexFailures = 0
	}
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
// findings preserved by resolution scoping (see finishCompletedCycle).
func (s *Scout) handleLibraryGate(ctx context.Context, st *state.State, snap library.Snapshot, mapCache *mapping.Cache, entries []seadex.Entry, walkErr, seaErr error) (handled, healthy bool) {
	if walkErr != nil {
		// With a feed configured, Cycle fell through the walk failure so the
		// arr-independent feed could still refresh. If SeaDex ALSO failed (or
		// returned nothing), rebuildFeed silently kept the previous feed and this
		// early return would swallow that second outage - surface it here so a
		// multi-dependency outage does not read as arr-only. Single SeaDex
		// failures (walk healthy) keep their own WARNs in the upstream gate, so
		// no duplicates.
		s.logFeedOutageOnGatedCycle(ctx, entries, seaErr)
		// Persist only the refreshed mapping cache, like the shrunk-walk arm
		// below: discarding it re-downloads an updated Fribb
		// body next cycle. Findings, memo, and the prior library snapshot
		// ride along untouched (an unusable-map load returns the prior cache,
		// making this persist a no-op then).
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
			s.cycleDegraded("walk-failed", walkFailureAttrs(walkErr)...)
		}
		return true, false
	}
	if len(st.Library.Items) > 0 && len(snap.Items)*libraryShrinkFactor < len(st.Library.Items) {
		// Like the walk-failed arm above, this gate skips the compare after
		// rebuildFeed already ran: if SeaDex ALSO failed (or returned
		// nothing), the previous feed was silently kept - surface it so a
		// shrink + SeaDex double outage does not read as shrink-only.
		s.logFeedOutageOnGatedCycle(ctx, entries, seaErr)
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
// outage does not falsely resolve live findings. The failed-fetch arm carries
// a persisted consecutive-failure streak (state.State.SeadexFailures) that
// escalates its single log site from WARN to ERROR at
// seadexFailureEscalationThreshold, mirroring the shrunk-walk and
// mapping-rejection guards; a successful fetch resets it in
// handlePreCompareGate (beside the fetch-success check), so the library-gate
// early returns see the reset too. A shutdown
// cancellation during the load or fetch is attributed to the shutdown, not
// the upstream.
func (s *Scout) handleUpstreamGate(ctx context.Context, st *state.State, snap library.Snapshot, mapCache *mapping.Cache, entries []seadex.Entry, mapErr, seaErr error) (handled, healthy bool) {
	if ctx.Err() != nil && (mapErr != nil || seaErr != nil) {
		// A shutdown/redeploy cancelled the cycle during the mapping load or
		// SeaDex fetch: the errors are the cancellation, not an upstream fault.
		// Preserve findings exactly like an upstream outage (degradedSave) but
		// attribute the interruption to the shutdown instead of blaming Fribb
		// or SeaDex (matching the library-walk and matching paths). The
		// SeaDex-failure streak is untouched: a cancelled fetch is evidence of
		// neither an outage nor a recovery.
		s.degradedSave(ctx, st, snap, mapCache)
		s.log.Warn("cycle interrupted by shutdown before comparison; findings preserved",
			"cause", context.Cause(ctx))
		return true, true
	}
	if !mapUsable(mapErr) {
		// Like the walk-failed and shrunk-walk arms, this gate closes the
		// cycle before the seaErr arm below: if SeaDex ALSO failed (or
		// returned nothing), rebuildFeed silently kept the previous feed -
		// surface it so a mapping + SeaDex double outage does not read as
		// mapping-only.
		s.logFeedOutageOnGatedCycle(ctx, entries, seaErr)
		s.degradedSave(ctx, st, snap, mapCache)
		s.log.Warn("mapping unusable; skipping comparison, findings preserved", "error", mapErr)
		s.cycleDegraded("mapping-unusable", "error", mapErr)
		return true, true
	}
	if seaErr != nil {
		// The persisted streak escalates this single log site to ERROR (the
		// SeadexScoutCycleError rule) once the outage has spanned
		// seadexFailureEscalationThreshold consecutive cycles; below it the
		// WARN keeps an upstream blip off the alert. Both levels carry the
		// streak so Loki can see how long the outage has run.
		st.SeadexFailures++
		s.degradedSave(ctx, st, snap, mapCache)
		attrs := []any{attrError, seaErr, "consecutive_seadex_failures", st.SeadexFailures}
		if st.SeadexFailures >= seadexFailureEscalationThreshold {
			s.log.Error("seadex fetch failed repeatedly; skipping comparison, findings preserved - inspect SeaDex (releases.moe) reachability and egress", attrs...)
		} else {
			s.log.Warn("seadex fetch failed; skipping comparison, findings preserved", attrs...)
		}
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
		// may carry configured userinfo credentials.
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
