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

	"github.com/cplieger/seadex-scout/internal/anilist"
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
	Rebuild(ctx context.Context, entries []seadex.Entry, idx *mapping.Index) error
}

// Deps are the assembled components a Scout runs a cycle with.
type Deps struct {
	Logger   *slog.Logger
	Store    *state.Store
	Library  *library.Walker
	Mapping  *mapping.Loader
	SeaDex   *seadex.Client
	Matcher  *match.Matcher
	Comparer *compare.Comparer
	Auditor  *audit.Auditor
	Reporter *report.Reporter
	AniList  *anilist.Client
	// Feed rebuilds and persists the indexer's Torznab feed from each cycle's
	// SeaDex snapshot. Nil when no Torznab feed is configured (the cycle then
	// skips all feed work).
	Feed FeedWriter
}

// Scout runs compare cycles from its assembled dependencies.
type Scout struct {
	deps Deps
	log  *slog.Logger
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
func (s *Scout) Cycle(ctx context.Context) (healthy bool) {
	start := time.Now()
	st := s.loadState(ctx)

	snap, walkErr := s.deps.Library.Walk(ctx)
	if walkErr != nil {
		s.log.Error("library walk failed; cycle unhealthy", "error", walkErr)
		// Alert-only (no Torznab feed): a failed walk is unhealthy and there is
		// nothing else to do, so skip the SeaDex/Fribb fetch (the pre-fold
		// behaviour). With a feed configured, fall through to refresh it - it
		// needs only SeaDex + Fribb, not the arrs - before returning unhealthy.
		if s.deps.Feed == nil {
			return false
		}
	}

	// The shared SeaDex + Fribb snapshot feeds BOTH halves: the Torznab feed
	// (arr-independent) and the compare pass below. Fetching once here is what
	// keeps a notification and what the arrs see in the feed on the same data.
	mapCache, idx, mapErr := s.deps.Mapping.Load(ctx, &st.Mapping)
	if mapErr != nil {
		s.log.Warn("mapping degraded", "error", mapErr, "usable_records", idx.Len())
	}
	entries, seaErr := s.deps.SeaDex.FetchEntries(ctx)

	// Rebuild the Torznab feed from the shared snapshot, independent of the arr
	// walk (see rebuildFeed): a notification and what the arrs see in the feed
	// come from this one fetch.
	s.rebuildFeed(ctx, entries, idx, seaErr)

	// From here the compare pass is gated on the arr walk (the health signal): a
	// failed walk is unhealthy and leaves findings untouched (no save), while the
	// feed above was still refreshed.
	if walkErr != nil {
		return false
	}
	if mapErr != nil && idx.Len() == 0 {
		// No usable map at all (not even a stale cache) means every entry would
		// fail to resolve and the reporter would falsely mark all prior findings
		// resolved. Preserve findings and save only the refreshed library
		// snapshot. A stale-but-usable map (idx.Len() > 0) is still
		// degraded-but-comparable and flows into the normal cycle below.
		s.degradedSave(ctx, &st, snap, &mapCache)
		s.log.Warn("mapping unusable; skipping comparison, findings preserved", "error", mapErr)
		return true
	}
	if seaErr != nil {
		s.degradedSave(ctx, &st, snap, &mapCache)
		s.log.Warn("seadex fetch failed; skipping comparison, findings preserved", "error", seaErr)
		return true
	}

	result := s.deps.Matcher.Match(ctx, entries, &snap, idx, st.Memo)
	if result.Degraded {
		// A transient AniList outage left needed fallback lookups incomplete, so
		// some entries are missing matches they would normally resolve. Comparing
		// now would treat those absent findings as resolved. Save the refreshed
		// library/mapping/memo (the memo keeps the lookups that did succeed) but
		// leave the finding dedupe table untouched.
		st.Library, st.Mapping, st.Memo = snap, mapCache, result.Memo
		s.save(ctx, &st)
		s.log.Warn("anilist degraded; skipping comparison, findings preserved")
		return true
	}
	findings := s.deps.Comparer.Compare(result.Matches)

	// A cold start (a fresh install, or a lost/reset cache) has no dedupe table
	// yet: baseline the current findings silently so the whole pre-existing
	// backlog is not dumped as notifications at once. Steady-state emission
	// resumes next cycle via Report. The len(Findings) guard keeps an upgrade of
	// an already-running instance (state predating the flag but already holding
	// findings) on the normal emit path, so it never silently swallows a
	// finding. The full list is always available on demand via report mode.
	var newFindings map[string]report.Alerted
	if !st.Baselined && len(st.Findings) == 0 {
		newFindings = s.deps.Reporter.Baseline(findings, time.Now())
	} else {
		newFindings = s.deps.Reporter.Report(findings, st.Findings, time.Now())
	}
	st.Baselined = true

	diff := library.DiffSnapshots(&st.Library, &snap)
	aniStats := s.deps.AniList.Stats()
	s.log.Info("cycle complete",
		"seadex_entries", len(entries),
		"library_items", len(snap.Items),
		"findings", len(findings),
		"mapped", sumCounts(result.Coverage.Hits),
		"unmapped", sumCounts(result.Coverage.Unmapped),
		"anilist_calls", aniStats.Calls,
		"anilist_waits", aniStats.RateLimitWaits,
		"added", diff.Added, "removed", diff.Removed, "changed", diff.Changed,
		"duration", time.Since(start).Round(time.Millisecond).String())

	st.Library, st.Mapping, st.Memo, st.Findings = snap, mapCache, result.Memo, newFindings
	s.save(ctx, &st)
	return true
}

// rebuildFeed refreshes the indexer's Torznab feed from the cycle's shared
// SeaDex snapshot, independent of the arr walk (the feed needs only SeaDex +
// Fribb, so an arr outage must not freeze it). It is a no-op when no feed is
// configured or the SeaDex fetch failed - the last-good feed is then kept; a
// nil/empty map degrades only categorization (items fall back to anime).
func (s *Scout) rebuildFeed(ctx context.Context, entries []seadex.Entry, idx *mapping.Index, seaErr error) {
	if s.deps.Feed == nil || seaErr != nil || len(entries) == 0 {
		return
	}
	if err := s.deps.Feed.Rebuild(ctx, entries, idx); err != nil {
		s.log.Warn("indexer feed rebuild failed; keeping previous feed", "error", err)
	}
}

// Report runs a one-shot SeaDex-alignment audit over the current library and
// returns the report. It is read-only on persisted state (it loads the mapping
// cache and AniList memo to avoid needless refetching, but never saves), so it
// is safe to run on demand while the daemon's cycle runs: the shared clients are
// concurrency-safe and each run carries its own state copy. It returns an error
// only when the library walk or SeaDex fetch fails (there is nothing to report).
func (s *Scout) Report(ctx context.Context) (audit.Report, error) {
	start := time.Now()
	st := s.loadState(ctx)

	snap, err := s.deps.Library.Walk(ctx)
	if err != nil {
		return audit.Report{}, fmt.Errorf("library walk: %w", err)
	}

	_, idx, mapErr := s.deps.Mapping.Load(ctx, &st.Mapping)
	if mapErr != nil {
		s.log.Warn("report: mapping degraded", "error", mapErr, "usable_records", idx.Len())
	}

	entries, err := s.deps.SeaDex.FetchEntries(ctx)
	if err != nil {
		return audit.Report{}, fmt.Errorf("seadex fetch: %w", err)
	}

	result := s.deps.Matcher.Match(ctx, entries, &snap, idx, st.Memo)
	rep := s.deps.Auditor.Audit(result.Matches, &snap, idx)
	s.log.Info("report generated",
		"seadex_entries", len(entries),
		"library_items", len(snap.Items),
		"rows", len(rep.Rows),
		"duration", time.Since(start).Round(time.Millisecond).String())
	return rep, nil
}

// loadState loads persisted state, falling back to an empty state on error.
func (s *Scout) loadState(ctx context.Context) state.State {
	st, err := s.deps.Store.Load(ctx)
	if err != nil {
		s.log.Error("state load failed; starting from empty state", "error", err)
		return state.State{}
	}
	return st
}

// degradedSave persists the caches refreshed before a SeaDex failure (library
// snapshot and map), leaving the AniList memo and finding dedupe untouched so a
// transient SeaDex outage does not resolve live findings.
func (s *Scout) degradedSave(ctx context.Context, st *state.State, snap library.Snapshot, mapCache *mapping.Cache) {
	st.Library = snap
	st.Mapping = *mapCache
	s.save(ctx, st)
}

// saveGrace bounds the detached shutdown save. It must stay well inside the
// container's stop grace period (compose stop_grace_period: 20s) so the write
// completes before SIGKILL.
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
