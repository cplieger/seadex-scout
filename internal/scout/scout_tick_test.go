package scout

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"github.com/cplieger/arrapi/v2"
	"github.com/cplieger/seadex-scout/internal/arrwalk"
	"github.com/cplieger/seadex-scout/internal/compare"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/match"
	"github.com/cplieger/seadex-scout/internal/notify"
	"github.com/cplieger/seadex-scout/internal/seadex"
	"github.com/cplieger/seadex-scout/internal/seadexapi"
	"github.com/cplieger/seadex-scout/internal/state"
	"github.com/cplieger/slogx/capture"
)

// pollIntervalForEvery is the Deps.PollInterval that makes reconcileEvery()
// report every. Tests state the RATIO they need (how many iterations between
// two reconciles) rather than a duration, because the ratio is what Cycle
// dispatches on; deriving the duration here keeps a change to reconcileInterval
// from silently turning a tick test into a reconcile test.
func pollIntervalForEvery(every int) time.Duration {
	return reconcileInterval / time.Duration(every)
}

// tickDeps assembles the cycle dependencies the tick tests share: a healthy
// one-series arr walk (so the FIRST iteration's reconcile populates the cached
// library snapshot every later tick compares against), a fresh in-memory
// mapping cache, and a definitive-not-found AniList so no match is ever
// transiently incomplete.
//
// feed and notifier are the seams individual tests substitute; a nil feed means
// no Torznab feed is configured.
func tickDeps(logger *slog.Logger, sea *fakeSeaDex, feed FeedWriter, notifier *notify.Notifier, every int) (*Deps, *fakeStore) {
	store := &fakeStore{st: state.State{Mapping: frierenMappingCache()}}
	if notifier == nil {
		notifier = notify.NewNotifier(logger, nil)
	}
	sonarr := &fakeSonarr{series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}}}
	return &Deps{
		Logger:       logger,
		Store:        store,
		Library:      arrwalk.NewWalker(&arrwalk.Config{Sonarr: sonarr, Logger: scoutTestLogger()}),
		Mapping:      fakeMapping{},
		SeaDex:       sea,
		Matcher:      match.NewMatcher(notFoundAniList{}, scoutTestLogger()),
		Comparer:     compare.NewComparer(compare.Config{}),
		Notifier:     notifier,
		Feed:         feed,
		PollInterval: pollIntervalForEvery(every),
	}, store
}

// newTickScout is tickDeps assembled into a Scout.
func newTickScout(logger *slog.Logger, sea *fakeSeaDex, feed FeedWriter, notifier *notify.Notifier, every int) (*Scout, *fakeStore) {
	deps, store := tickDeps(logger, sea, feed, notifier, every)
	return New(deps), store
}

// windowEntry is a SeaDex entry for a window: a curated Nyaa release under an
// AniList id nothing in the test library maps to, so it drives the tick's
// plumbing without producing a finding of its own.
func windowEntry(alID, viewID int) seadex.Entry {
	return seadex.Entry{
		AniListID: alID,
		Torrents: []seadex.Torrent{{
			ReleaseGroup: "SubsPlease",
			Tracker:      "Nyaa",
			InfoHash:     "hash" + strconv.Itoa(viewID),
			URL:          "https://nyaa.si/view/" + strconv.Itoa(viewID),
			IsBest:       true,
			Files:        []seadex.File{{Name: "Show S01E01 1080p.mkv", Length: 1}},
		}},
	}
}

// countWindowModes counts how many FetchEntries calls asked for each mode, so a
// test can say "the reconcile fetched, the tick did not" without indexing into
// the call log.
func countWindowModes(sea *fakeSeaDex) (full, window int) {
	for _, mode := range sea.fetchModes() {
		if mode == seadexapi.FetchWindow {
			window++
			continue
		}
		full++
	}
	return full, window
}

// lastSummaryCounter reads one counter off the LAST "findings reported" line,
// so a test that seeded the notifier with an earlier pass reads the pass it
// just ran rather than the seed.
func lastSummaryCounter(t *testing.T, recorder *capture.Recorder, key string) (int64, bool) {
	t.Helper()
	var value int64
	var seen bool
	for _, rec := range recorder.Records() {
		if rec.Message != "findings reported" {
			continue
		}
		rec.Attrs(func(a slog.Attr) bool {
			if a.Key == key {
				value, seen = a.Value.Int64(), true
			}
			return true
		})
	}
	return value, seen
}

// TestCycleDispatchesReconcileThenTicks pins the dispatcher, which is the whole
// point of the new Cycle: iteration 0 and every reconcileEvery-th iteration run
// the FULL pass, and every other iteration runs a tick.
//
// Iteration 0 must reconcile because everything downstream assumes a complete
// pass has happened - the notifier's finding set is empty until one runs, and a
// tick compares against a cached library only a walk can populate. The periodic
// reconcile is the backstop for everything a window structurally cannot see (a
// deletion, an in-place torrent edit, an outage longer than the window), so a
// dispatch that drifted toward ticking forever would make all of those gaps
// permanent while every iteration still looked healthy.
//
// The two paths are counted through the SeaDex seam rather than through logs: a
// reconcile is exactly one FetchFull, and a tick is exactly one CountWindow.
func TestCycleDispatchesReconcileThenTicks(t *testing.T) {
	logger, recorder := capture.New()
	// An empty window keeps each tick to its probe, so the counts stay
	// unambiguous.
	sea := &fakeSeaDex{entries: seadexFrierenEntry(), countFn: func(context.Context, time.Time) (int, error) {
		return 0, nil
	}}
	const every = 3
	s, _ := newTickScout(logger, sea, nil, nil, every)

	if got := s.reconcileEvery(); got != every {
		t.Fatalf("reconcileEvery() = %d, want %d", got, every)
	}

	const iterations = 7
	for i := range iterations {
		if healthy := s.Cycle(t.Context()); !healthy {
			t.Fatalf("Cycle iteration %d healthy=false, want true", i)
		}
	}

	// Iterations 0, 3 and 6 reconcile; 1, 2, 4 and 5 tick.
	wantReconciles, wantTicks := 3, 4
	full, window := countWindowModes(sea)
	if full != wantReconciles {
		t.Errorf("full fetches = %d, want %d (iterations 0, 3 and 6)", full, wantReconciles)
	}
	if window != 0 {
		t.Errorf("window fetches = %d, want 0 (every probe reported an empty window)", window)
	}
	if got := len(sea.countSince); got != wantTicks {
		t.Errorf("probes = %d, want %d (iterations 1, 2, 4 and 5)", got, wantTicks)
	}
	if n := recorder.CountExact("reconcile complete"); n != wantReconciles {
		t.Errorf("'reconcile complete' count = %d, want %d", n, wantReconciles)
	}
	if n := recorder.CountExact("cycle complete"); n != wantReconciles {
		t.Errorf("'cycle complete' count = %d, want %d (one per full pass)", n, wantReconciles)
	}
}

// TestCycleReconcilesEveryIterationWithoutAPollInterval pins the conservative
// default: an unwired or non-positive PollInterval reconciles every time. That
// is what keeps every pre-existing full-cycle test testing a full cycle, and it
// is the right reading for a caller that has not said how often it runs - a
// wrong guess here would silently turn a rarely-invoked entry point into one
// that only ever ticks and never reconciles.
func TestCycleReconcilesEveryIterationWithoutAPollInterval(t *testing.T) {
	for name, interval := range map[string]time.Duration{
		"unwired":  0,
		"negative": -time.Hour,
	} {
		t.Run(name, func(t *testing.T) {
			logger, recorder := capture.New()
			sea := &fakeSeaDex{entries: seadexFrierenEntry()}
			deps, _ := tickDeps(logger, sea, nil, nil, 1)
			deps.PollInterval = interval
			s := New(deps)

			if got := s.reconcileEvery(); got != 1 {
				t.Fatalf("reconcileEvery() = %d, want 1", got)
			}
			for range 3 {
				if healthy := s.Cycle(t.Context()); !healthy {
					t.Fatal("Cycle healthy=false, want true")
				}
			}
			full, window := countWindowModes(sea)
			if full != 3 || window != 0 {
				t.Errorf("fetches = %d full / %d window, want 3 full and 0 window", full, window)
			}
			if len(sea.countSince) != 0 {
				t.Errorf("probes = %d, want 0 (no iteration ticked)", len(sea.countSince))
			}
			if n := recorder.CountExact("reconcile complete"); n != 3 {
				t.Errorf("'reconcile complete' count = %d, want 3", n)
			}
		})
	}
}

// TestReconcileCompleteIsNotEmittedByATick pins the deadman's signal.
//
// The shipped Loki deadman counts "cycle complete" / "cycle degraded", and once
// most iterations are ticks that line can no longer distinguish "the loop is
// alive" from "the backstop still runs". "reconcile complete" is the second
// line that separates them, so it is only useful if a tick never emits it: a
// tick that did would make a reconcile which silently stopped forever look
// perfectly healthy, and every gap the window cannot see would become permanent.
func TestReconcileCompleteIsNotEmittedByATick(t *testing.T) {
	logger, recorder := capture.New()
	sea := &fakeSeaDex{
		entries:       seadexFrierenEntry(),
		windowEntries: []seadex.Entry{windowEntry(1001, 501)},
	}
	s, _ := newTickScout(logger, sea, nil, nil, 96)

	if healthy := s.Cycle(t.Context()); !healthy {
		t.Fatal("reconcile healthy=false, want true")
	}
	if n := recorder.CountExact("reconcile complete"); n != 1 {
		t.Fatalf("'reconcile complete' after the full pass = %d, want 1", n)
	}

	// A PRODUCTIVE tick: it fetches, advances and reports, and still must not
	// claim a reconcile happened.
	if healthy := s.Cycle(t.Context()); !healthy {
		t.Fatal("tick healthy=false, want true")
	}
	if n := recorder.CountExact("tick complete"); n != 1 {
		t.Errorf("'tick complete' count = %d, want 1 (the tick did run)", n)
	}
	if n := recorder.CountExact("reconcile complete"); n != 1 {
		t.Errorf("'reconcile complete' count = %d, want 1 (the tick must not emit it)", n)
	}
	if n := recorder.CountExact("cycle complete"); n != 1 {
		t.Errorf("'cycle complete' count = %d, want 1 (a tick is not a completed full cycle either)", n)
	}
}

// TestTickEmptyWindowSkipsFetch pins the cheap path, which is the common one:
// the probe said nothing changed, so the tick stops there. It must not fetch
// (that would spend a real page's bytes to learn what the ~88-byte probe already
// said), it is healthy (an empty window is a SUCCESSFUL tick - the marker
// attests the loop is alive, not that anything changed), and it advances the
// empty-run diagnostic.
func TestTickEmptyWindowSkipsFetch(t *testing.T) {
	logger, recorder := capture.New()
	sea := &fakeSeaDex{
		entries: seadexFrierenEntry(),
		// A populated window that must never be fetched: were the tick to fetch
		// anyway, the feed and report assertions below would change.
		windowEntries: []seadex.Entry{windowEntry(1001, 501)},
		countFn:       func(context.Context, time.Time) (int, error) { return 0, nil },
	}
	feed := &fakeFeed{}
	s, store := newTickScout(logger, sea, feed, nil, 96)

	if healthy := s.Cycle(t.Context()); !healthy {
		t.Fatal("reconcile healthy=false, want true")
	}
	savesAfterReconcile := store.saves

	if healthy := s.Cycle(t.Context()); !healthy {
		t.Fatal("tick healthy=false, want true (an empty window is a successful tick)")
	}

	if _, window := countWindowModes(sea); window != 0 {
		t.Errorf("window fetches = %d, want 0 (the probe already answered)", window)
	}
	if s.oversizeRun != 0 {
		t.Errorf("oversizeRun = %d, want 0", s.oversizeRun)
	}
	if feed.advanceCalls != 0 {
		t.Errorf("Advance calls = %d, want 0 (nothing changed)", feed.advanceCalls)
	}
	if store.saves != savesAfterReconcile {
		t.Errorf("saves = %d, want the reconcile's %d (an empty tick learned nothing to persist)",
			store.saves, savesAfterReconcile)
	}
	// The window's cost bound is a probe, not a fetch - but the tick DID
	// complete, so it must say so and it must re-state the finding set. Both are
	// the alerting contract: a silent iteration is indistinguishable from a
	// wedged loop for the scan deadman, and an emission gap longer than the
	// better-release rule's lookback resolves every standing finding and then
	// re-fires the whole set.
	if n := recorder.CountExact("tick complete"); n != 1 {
		t.Errorf("'tick complete' count = %d, want 1 (an empty window is a completed tick, and the deadman counts this line)", n)
	}
	if n := recorder.CountExact("findings reported"); n != 2 {
		t.Errorf("'findings reported' count = %d, want 2 (the reconcile's, then the empty tick's re-statement)", n)
	}
}

// TestTickOversizeWindowSkipsFetchAndEscalates pins the other wedge: a window
// too large to fetch in one request.
//
// The tick must not fetch a prefix of it. The walk sorts on `created`, so page 1
// of an oversized window holds the OLDEST records - precisely not what a
// freshness pass wants - so the correct answer is to defer to the reconcile.
//
// The escalation to ERROR at the latch is deliberate and matters: nothing in
// this stack alerts on WARN, and while this condition holds the fast path is
// FROZEN (no new RSS item, no new finding) with only the daily reconcile still
// working. That is a real fault with a real remedy, so it must page.
func TestTickOversizeWindowSkipsFetchAndEscalates(t *testing.T) {
	logger, recorder := capture.New()
	sea := &fakeSeaDex{
		entries:       seadexFrierenEntry(),
		windowEntries: []seadex.Entry{windowEntry(1001, 501)},
		countFn: func(context.Context, time.Time) (int, error) {
			return seadexapi.MaxWindowEntries, nil
		},
	}
	feed := &fakeFeed{}
	// A 15-minute cadence (the default), where the frozen-fast-path tolerance
	// derives to 8 ticks - well inside one reconcile period, so the whole run
	// below is ticks.
	s, _ := newTickScout(logger, sea, feed, nil, 96)
	if healthy := s.Cycle(t.Context()); !healthy {
		t.Fatal("reconcile healthy=false, want true")
	}

	latch := s.latchTicks()
	const warnMsg = "SeaDex change window too large to fetch; deferring to the reconcile"
	const errSub = "SeaDex change window has been too large to fetch repeatedly"
	for i := 1; i <= latch; i++ {
		if healthy := s.Cycle(t.Context()); !healthy {
			t.Fatalf("oversize tick %d healthy=false, want true (it is degraded, not unhealthy)", i)
		}
		if s.oversizeRun != i {
			t.Errorf("after %d oversize ticks oversizeRun = %d, want %d", i, s.oversizeRun, i)
		}
		wantWarns, wantErrors := i, 0
		if i >= latch {
			// The latch tick escalates INSTEAD of warning, so the WARN count
			// stops one short of the tick count.
			wantWarns, wantErrors = i-1, 1
		}
		if got := recorder.CountExact(warnMsg); got != wantWarns {
			t.Errorf("after %d oversize ticks WARN count = %d, want %d", i, got, wantWarns)
		}
		if got := recorder.CountLevel(slog.LevelError, errSub); got != wantErrors {
			t.Errorf("after %d oversize ticks ERROR count = %d, want %d", i, got, wantErrors)
		}
	}
	if _, window := countWindowModes(sea); window != 0 {
		t.Errorf("window fetches = %d, want 0 (an oversized window defers to the reconcile, it does not fetch a prefix)", window)
	}
	if feed.advanceCalls != 0 {
		t.Errorf("Advance calls = %d, want 0", feed.advanceCalls)
	}
}

// TestTickOversizeBoundIsInclusive pins the boundary of the oversize test,
// which is the one place an off-by-one silently changes behaviour: a window of
// exactly MaxWindowEntries is still ONE request, but the count is what the
// upstream reports for the FILTER, and the walk needs a short terminal chunk to
// know it is done - so a window at the bound would take a second request. The
// bound is therefore inclusive, and one below it fetches.
func TestTickOversizeBoundIsInclusive(t *testing.T) {
	tests := map[string]struct {
		count      int
		wantFetch  bool
		wantWarned bool
	}{
		"one below the bound fetches": {seadexapi.MaxWindowEntries - 1, true, false},
		"exactly the bound defers":    {seadexapi.MaxWindowEntries, false, true},
		"above the bound defers":      {seadexapi.MaxWindowEntries + 1, false, true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			logger, recorder := capture.New()
			sea := &fakeSeaDex{
				entries:       seadexFrierenEntry(),
				windowEntries: []seadex.Entry{windowEntry(1001, 501)},
				countFn: func(context.Context, time.Time) (int, error) {
					return tc.count, nil
				},
			}
			s, _ := newTickScout(logger, sea, nil, nil, 96)
			if healthy := s.Cycle(t.Context()); !healthy {
				t.Fatal("reconcile healthy=false, want true")
			}
			if healthy := s.Cycle(t.Context()); !healthy {
				t.Fatal("tick healthy=false, want true")
			}

			_, window := countWindowModes(sea)
			if gotFetch := window > 0; gotFetch != tc.wantFetch {
				t.Errorf("window fetched = %v, want %v for a reported count of %d", gotFetch, tc.wantFetch, tc.count)
			}
			gotWarn := recorder.Contains("SeaDex change window too large to fetch")
			if gotWarn != tc.wantWarned {
				t.Errorf("oversize warning present = %v, want %v", gotWarn, tc.wantWarned)
			}
		})
	}
}

// TestTickProductiveResetsTheOversizeRun pins the reset. The wedge counter
// measures a CONSECUTIVE run, so a productive tick has to clear it - otherwise a
// single long spell of oversized windows would latch its diagnostic permanently,
// and the next genuine wedge would be indistinguishable from the noise it left
// behind.
//
// The counter is driven to just under its latch first, so the assertion is about
// the reset and not about a counter that never moved.
func TestTickProductiveResetsTheOversizeRun(t *testing.T) {
	logger := scoutTestLogger()
	sea := &fakeSeaDex{
		entries:       seadexFrierenEntry(),
		windowEntries: []seadex.Entry{windowEntry(1001, 501)},
	}
	s, _ := newTickScout(logger, sea, &fakeFeed{}, nil, 96)
	if healthy := s.Cycle(t.Context()); !healthy {
		t.Fatal("reconcile healthy=false, want true")
	}
	// The counter is latched against a wall-clock tolerance converted at this
	// loop's interval, so prime from the derived value rather than a literal
	// count.
	s.oversizeRun = s.latchTicks() - 1

	if healthy := s.Cycle(t.Context()); !healthy {
		t.Fatal("productive tick healthy=false, want true")
	}

	if s.oversizeRun != 0 {
		t.Errorf("oversizeRun = %d, want 0 after a productive tick", s.oversizeRun)
	}
	if _, window := countWindowModes(sea); window != 1 {
		t.Errorf("window fetches = %d, want 1 (the tick was productive)", window)
	}
}

// TestTickAdvancesTheFeedAndNeverRebuilds pins the feed dispatch in both
// directions, because the two calls are not interchangeable and an inverted
// dispatch is silent.
//
// Rebuild REPLACES the search curation index from its argument, so a Rebuild
// from a window would shrink it from the whole catalogue to the window's handful
// and take Prowlarr search down until the next full pass. Advance from a full
// catalogue would be merely wasteful in comparison - it never rewrites the
// index - which is why the tick-must-not-Rebuild half is the load-bearing one.
func TestTickAdvancesTheFeedAndNeverRebuilds(t *testing.T) {
	logger := scoutTestLogger()
	window := []seadex.Entry{windowEntry(1001, 501), windowEntry(1002, 502)}
	sea := &fakeSeaDex{entries: seadexFrierenEntry(), windowEntries: window}
	feed := &fakeFeed{}
	s, _ := newTickScout(logger, sea, feed, nil, 96)

	if healthy := s.Cycle(t.Context()); !healthy {
		t.Fatal("reconcile healthy=false, want true")
	}
	if feed.calls != 1 {
		t.Errorf("Rebuild calls after the reconcile = %d, want 1", feed.calls)
	}
	if feed.advanceCalls != 0 {
		t.Errorf("Advance calls after the reconcile = %d, want 0 (a full pass rebuilds)", feed.advanceCalls)
	}

	if healthy := s.Cycle(t.Context()); !healthy {
		t.Fatal("tick healthy=false, want true")
	}
	if feed.calls != 1 {
		t.Errorf("Rebuild calls after the tick = %d, want the reconcile's 1 (a window must never rebuild the curation index)", feed.calls)
	}
	if feed.advanceCalls != 1 {
		t.Errorf("Advance calls after the tick = %d, want 1", feed.advanceCalls)
	}
	if got := feed.advanceWindows; len(got) != 1 || len(got[0]) != 2 || got[0][0] != 1001 || got[0][1] != 1002 {
		t.Errorf("Advance window = %v, want [[1001 1002]] (exactly the fetched window)", got)
	}
}

// TestTickFeedAdvanceFailureKeepsTheTickHealthy pins the feed's blast radius: it
// is arr-independent and its own concern, so a failed Advance keeps the last
// good feed, warns, and leaves the tick healthy and still reporting - the same
// contract rebuildFeed has on the full pass.
func TestTickFeedAdvanceFailureKeepsTheTickHealthy(t *testing.T) {
	logger, recorder := capture.New()
	sea := &fakeSeaDex{
		entries:       seadexFrierenEntry(),
		windowEntries: []seadex.Entry{windowEntry(1001, 501)},
	}
	feed := &fakeFeed{advanceErr: errors.New("advance boom")}
	s, _ := newTickScout(logger, sea, feed, nil, 96)

	if healthy := s.Cycle(t.Context()); !healthy {
		t.Fatal("reconcile healthy=false, want true")
	}
	if healthy := s.Cycle(t.Context()); !healthy {
		t.Fatal("tick healthy=false, want true (a feed failure must not fail the tick)")
	}
	if !recorder.Contains("indexer feed advance failed; keeping previous feed") {
		t.Errorf("feed advance failure not warned; messages: %v", recorder.Messages())
	}
	if n := recorder.CountExact("tick complete"); n != 1 {
		t.Errorf("'tick complete' count = %d, want 1 (the tick still finished its compare and report)", n)
	}
}

// TestTickDeletesOnlyRowsItEvaluated pins the deletion authority the tick hands
// the notifier, which is the subtlest correctness property of the whole fast
// path.
//
// Authority is the set of entries the tick EVALUATED, not the set it fetched.
// Both halves matter:
//
//   - a row whose entry the tick never carried must be carried forward: it is
//     absent from the finding set for exactly the same reason a resolved one is,
//     and deleting it would clear an alert while the condition still holds, then
//     re-raise it as new on the next reconcile;
//   - a row whose entry the tick DID carry but could not link to a library item
//     must ALSO be carried. An entry whose mapping record no longer resolves, or
//     whose AniList lookup definitively found nothing, produces no finding
//     because the app lost track of the item - not because the condition was
//     fixed. That case is not in IncompleteIDs, so nothing else protects it.
//
// The set is asserted through the notifier's own behaviour rather than by
// intercepting the call: three rows are seeded, the window carries two of their
// owners, and only the one that resolves to a library item may go.
func TestTickDeletesOnlyRowsItEvaluated(t *testing.T) {
	logger, recorder := capture.New()
	notifier := notify.NewNotifier(logger, nil)
	// Seed three standing rows: the mapped in-library owner, an owner the window
	// never carries, and an owner the window carries but cannot resolve.
	seed := func() {
		notifier.Report([]compare.Finding{
			tickFinding(154587, 501),
			tickFinding(1002, 502),
			tickFinding(1003, 503),
		}, nil)
	}
	seed()
	if total, _ := lastSummaryCounter(t, recorder, "total"); total != 3 {
		t.Fatalf("seeded finding set = %d rows, want 3", total)
	}

	// The window carries 154587 (mapped to the walked Frieren series, so the
	// tick evaluates it and it produces no finding) and 1003 (nothing in the
	// test library maps to it, so the tick cannot evaluate it at all).
	sea := &fakeSeaDex{
		entries:       seadexFrierenEntry(),
		windowEntries: append(seadexFrierenEntry(), windowEntry(1003, 503)),
	}
	s, _ := newTickScout(logger, sea, nil, notifier, 96)
	if healthy := s.Cycle(t.Context()); !healthy {
		t.Fatal("reconcile healthy=false, want true")
	}
	// The reconcile re-reported with FULL authority, so re-seed the standing
	// rows it deleted by omission before the tick runs.
	seed()

	if healthy := s.Cycle(t.Context()); !healthy {
		t.Fatal("tick healthy=false, want true")
	}

	total, seen := lastSummaryCounter(t, recorder, "total")
	if !seen {
		t.Fatal("the tick emitted no 'findings reported' summary line")
	}
	if total != 2 {
		t.Errorf("finding set after the tick = %d rows, want 2 (1002, never carried, and 1003, carried but unresolvable)", total)
	}
	carried, _ := lastSummaryCounter(t, recorder, "carried")
	if carried != 2 {
		t.Errorf("carried = %d, want 2 (the row outside the window plus the row the tick could not evaluate)", carried)
	}
	// Nothing was preserved for incompleteness: with a definitive-not-found
	// AniList no match is transiently incomplete, so the survival above is
	// attributable to the authority bound alone.
	if preserved, _ := lastSummaryCounter(t, recorder, "preserved"); preserved != 0 {
		t.Errorf("preserved = %d, want 0 (no entry had incomplete evidence)", preserved)
	}
}

// TestTickInterruptedDuringMatchingPublishesNothingAndPersists pins the tick's
// post-match shutdown OUTCOME, which is the sharpest deletion-authority contract
// on the fast path and the one exit whose three effects differ most from its
// siblings'.
//
// match.Matcher.Match returns early on a cancelled context, so the match set it
// hands back is TRUNCATED, and evaluatedIDs derives ReportScoped's deletion
// authority from exactly that set - so a tick that published it would tell the
// notifier that entries it never finished evaluating are resolved, silently, on
// any redeploy that lands mid-match. It emits no completion line for the same
// reason: an interrupted pass did not complete, and counting it turns a redeploy
// into a pass that ran and failed.
//
// What it MUST still do is persist. The three effects are owned per cause
// (tickInterrupted), and declining the completion line is not a reason to discard
// the revalidated Fribb validators the pass already accepted - a discarded
// revalidation re-downloads ~5.9 MB on the next boot, and the persisted
// refresh-rejection streak the mapping escalation reads only means anything if it
// survives a restart. The reconcile's finishInterruptedMatch draws exactly that
// line one file over.
//
// The reconcile's half of the publication contract is pinned by
// TestCycleShutdownDuringMatchingWarnsShutdownNotAniList; the tick's had no test,
// so nothing failed if the check was removed - or if it kept declining to save.
func TestTickInterruptedDuringMatchingPublishesNothingAndPersists(t *testing.T) {
	logger, recorder := capture.New()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// The window carries an id nothing in the test library maps to, so the matcher
	// takes the AniList title fallback - the seam the cancellation lands in. The
	// reconcile's own entry is id-mapped and never reaches it.
	sea := &fakeSeaDex{
		entries:       seadexFrierenEntry(),
		windowEntries: []seadex.Entry{windowEntry(1003, 503)},
	}
	deps, store := tickDeps(logger, sea, nil, nil, 96)
	deps.Matcher = match.NewMatcher(&ctxCancellingAniList{cancel: cancel}, scoutTestLogger())
	// An accepted mapping refresh on every load, so "did the interrupted tick
	// persist what it learned" is observable at all: with a 304-shaped loader
	// saveTick's own skip fires and the assertion could not tell a declined save
	// from a vacuous one.
	deps.Mapping = &refreshingMapping{}
	s := New(deps)

	if healthy := s.Cycle(ctx); !healthy {
		t.Fatal("reconcile healthy=false, want true")
	}
	s.notifier.Report([]compare.Finding{tickFinding(1003, 503)}, nil)
	summariesAfterSeed := recorder.CountExact("findings reported")
	savesAfterSeed := store.saves

	if healthy := s.Cycle(ctx); !healthy {
		t.Fatal("tick healthy=false, want true (a shutdown mid-matching is not an ingest fault)")
	}

	if n := recorder.CountExact("tick interrupted by shutdown before comparison"); n != 1 {
		t.Errorf("shutdown WARN count = %d, want 1", n)
	}
	if n := recorder.CountExact("tick complete"); n != 0 {
		t.Errorf("'tick complete' count = %d, want 0 (an interrupted tick did not complete)", n)
	}
	if n := recorder.CountExact("tick degraded"); n != 0 {
		t.Errorf("'tick degraded' count = %d, want 0 (an interrupted tick emits no completion line at all)", n)
	}
	if n := recorder.CountExact("findings reported") - summariesAfterSeed; n != 0 {
		t.Errorf("the interrupted tick emitted %d new 'findings reported' summaries, want 0: publishing a truncated match set would resolve rows for entries it never finished evaluating", n)
	}
	if store.saves != savesAfterSeed+1 {
		t.Errorf("saves = %d, want %d: a shutdown declines the completion line, never the persistence - the revalidated Fribb validators and the rejection streak the mapping escalation reads must survive the restart",
			store.saves, savesAfterSeed+1)
	}
	// The one thing a tick must never write, on this exit as on every other.
	if items := store.st.Library.Items; len(items) != 1 || items[0].Title != "Frieren" {
		t.Errorf("library snapshot after the interrupted tick = %+v, want the reconcile's single Frieren item", items)
	}
}

// tickFinding builds a standing finding under alID whose dedupe key is unique
// per url id, for seeding the notifier's current set.
func tickFinding(alID, urlID int) compare.Finding {
	return compare.Finding{
		Status:           compare.StatusBetter,
		Kind:             "encode",
		Arr:              "sonarr",
		Title:            "Show " + strconv.Itoa(alID),
		AniListID:        alID,
		Tracker:          "Nyaa",
		CurrentGroup:     "erai-raws",
		RecommendedGroup: "SubsPlease",
		Resolution:       "1080p",
		ReleaseURL:       "https://nyaa.si/view/" + strconv.Itoa(urlID),
	}
}

// TestTickNeverWritesTheLibrarySnapshot pins saveTick's one prohibition. A tick
// performs no arr walk, so it HAS no library snapshot - writing one would
// overwrite the reconcile's with an empty or stale copy, and every later tick
// compares against exactly that cached snapshot. The failure would be
// invisible: the tick stays healthy and simply stops finding anything.
//
// What a tick legitimately learned - the refreshed mapping cache and the AniList
// memo - must still be persisted, or a restart pays for a cold rebuild.
func TestTickNeverWritesTheLibrarySnapshot(t *testing.T) {
	logger := scoutTestLogger()
	sea := &fakeSeaDex{
		entries:       seadexFrierenEntry(),
		windowEntries: []seadex.Entry{windowEntry(1001, 501)},
	}
	s, store := newTickScout(logger, sea, nil, nil, 96)

	if healthy := s.Cycle(t.Context()); !healthy {
		t.Fatal("reconcile healthy=false, want true")
	}
	if n := len(store.st.Library.Items); n != 1 {
		t.Fatalf("library items after the reconcile = %d, want 1 (the walk's snapshot)", n)
	}
	savesAfterReconcile := store.saves

	if healthy := s.Cycle(t.Context()); !healthy {
		t.Fatal("tick healthy=false, want true")
	}

	if store.saves != savesAfterReconcile+1 {
		t.Errorf("saves = %d, want %d (a productive tick persists the mapping cache and memo)",
			store.saves, savesAfterReconcile+1)
	}
	items := store.st.Library.Items
	if len(items) != 1 || items[0].Title != "Frieren" {
		t.Errorf("library snapshot after the tick = %+v, want the reconcile's single Frieren item", items)
	}
	if len(store.st.Mapping.Records) != 1 {
		t.Errorf("persisted mapping records = %d, want the refreshed cache's 1", len(store.st.Mapping.Records))
	}
}

// TestTickUpstreamFailuresAreHealthyAndReportNothing pins both of the tick's
// failure arms together, because they share one rule: a tick that could not
// establish what changed CHANGES nothing, and says so.
//
// It re-states the finding set unchanged rather than replacing it. Replacing it
// with an empty set would clear every standing alert (the notifier deletes by
// omission within its authority), and staying silent instead would let those
// alerts expire out of their own lookback window and then re-fire as a burst -
// so the correct behaviour is to keep reporting exactly what was already true,
// which costs nothing upstream. It also emits the degraded completion line the
// scan deadman counts, because a silent iteration is indistinguishable from a
// wedged loop and only the loop's death fits that alert's restart runbook.
// Neither failure is unhealthy either - container health follows the library
// ingest, and a restart cannot fix an upstream outage.
//
// The two STATE wedge counters must not move on either: they measure what the
// upstream contains, not whether it answered, and a probe failure counted as an
// empty window would latch the clock-skew WARN during an ordinary outage. The
// unreachability streak is the one that does advance.
func TestTickUpstreamFailuresAreHealthyAndReportNothing(t *testing.T) {
	boom := errors.New("upstream boom")
	tests := map[string]struct {
		sea      func() *fakeSeaDex
		wantWarn string
		wantPrbs int
		wantFtch int
	}{
		"the probe fails": {
			sea: func() *fakeSeaDex {
				return &fakeSeaDex{
					entries: seadexFrierenEntry(),
					countFn: func(context.Context, time.Time) (int, error) { return 0, boom },
				}
			},
			wantWarn: "change probe failed; skipping tick",
			wantPrbs: 1,
			wantFtch: 0,
		},
		"the window fetch fails": {
			sea: func() *fakeSeaDex {
				return &fakeSeaDex{
					entries:   seadexFrierenEntry(),
					windowErr: boom,
					countFn:   func(context.Context, time.Time) (int, error) { return 3, nil },
				}
			},
			wantWarn: "change window fetch failed; skipping tick",
			wantPrbs: 1,
			wantFtch: 1,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			logger, recorder := capture.New()
			notifier := notify.NewNotifier(logger, nil)
			notifier.Report([]compare.Finding{tickFinding(1002, 502)}, nil)
			sea := tc.sea()
			feed := &fakeFeed{}
			s, _ := newTickScout(logger, sea, feed, notifier, 96)

			if healthy := s.Cycle(t.Context()); !healthy {
				t.Fatal("reconcile healthy=false, want true")
			}
			// The reconcile reported with full authority; re-seed the standing
			// row so the tick's silence is observable.
			notifier.Report([]compare.Finding{tickFinding(1002, 502)}, nil)
			summariesBefore := recorder.CountExact("findings reported")

			if healthy := s.Cycle(t.Context()); !healthy {
				t.Fatal("tick healthy=false, want true (an upstream failure is degraded, not unhealthy)")
			}

			if !recorder.Contains(tc.wantWarn) {
				t.Errorf("warning %q not logged; messages: %v", tc.wantWarn, recorder.Messages())
			}
			if got := len(sea.countSince); got != tc.wantPrbs {
				t.Errorf("probes = %d, want %d", got, tc.wantPrbs)
			}
			if _, window := countWindowModes(sea); window != tc.wantFtch {
				t.Errorf("window fetches = %d, want %d", window, tc.wantFtch)
			}
			if got := recorder.CountExact("findings reported"); got != summariesBefore+1 {
				t.Errorf("report summary lines = %d, want the pre-tick %d plus the failed tick's re-statement; "+
					"silence lets every standing alert expire out of its lookback window", got, summariesBefore+1)
			}
			if total, _ := lastSummaryCounter(t, recorder, "total"); total != 1 {
				t.Errorf("finding set = %d rows, want the standing 1 left untouched", total)
			}
			if carried, _ := lastSummaryCounter(t, recorder, "carried"); carried != 1 {
				t.Errorf("carried = %d, want 1 (a re-statement compares nothing, so every row is carried)", carried)
			}
			if feed.advanceCalls != 0 {
				t.Errorf("Advance calls = %d, want 0 (nothing was fetched)", feed.advanceCalls)
			}
			if s.oversizeRun != 0 {
				t.Errorf("wedge counter = oversize %d, want 0 (it measures upstream state, not reachability)",
					s.oversizeRun)
			}
			if s.unreachableRun != 1 {
				t.Errorf("unreachableRun = %d, want 1 (the fast path's own SeaDex-unreachable streak, which the reconcile-only SeadexFailures cannot see)",
					s.unreachableRun)
			}
			if n := recorder.CountExact("tick complete"); n != 0 {
				t.Errorf("'tick complete' count = %d, want 0 (the tick did not complete its work)", n)
			}
			if n := recorder.CountExact("tick degraded"); n != 1 {
				t.Errorf("'tick degraded' count = %d, want 1 (the deadman must see the loop is alive)", n)
			}
		})
	}
}

// TestTickWindowIsWiderThanTheInterval pins the window's own contract, which is
// what makes a missed tick recoverable at all: the tick asks for changeWindow of
// history, not one interval's worth. A window narrowed to the interval would
// lose every change that landed during a restart, a skipped iteration, or a
// clock nudge - silently, since each later tick would still look perfectly
// successful.
func TestTickWindowIsWiderThanTheInterval(t *testing.T) {
	logger := scoutTestLogger()
	sea := &fakeSeaDex{
		entries: seadexFrierenEntry(),
		countFn: func(context.Context, time.Time) (int, error) { return 0, nil },
	}
	s, _ := newTickScout(logger, sea, nil, nil, 96)
	if healthy := s.Cycle(t.Context()); !healthy {
		t.Fatal("reconcile healthy=false, want true")
	}

	before := time.Now()
	if healthy := s.Cycle(t.Context()); !healthy {
		t.Fatal("tick healthy=false, want true")
	}
	if len(sea.countSince) != 1 {
		t.Fatalf("probes = %d, want 1", len(sea.countSince))
	}
	age := before.Sub(sea.countSince[0])
	if age < changeWindow-time.Minute || age > changeWindow+time.Minute {
		t.Errorf("probe since = %v before now, want ~%v (changeWindow)", age, changeWindow)
	}
	if changeWindow <= s.pollInterval {
		t.Errorf("changeWindow %v is not wider than the poll interval %v; a missed tick would be unrecoverable",
			changeWindow, s.pollInterval)
	}
}

// TestEveryTickExitEmitsALineTheDeadmanCounts is the alerting contract, pinned
// as one table over every way a tick can end.
//
// SeadexScoutScanStalled fires when NO `(cycle|tick) (complete|degraded)` line
// appears within its window, and its remedy is "restart the container". So every
// exit a healthy or merely-degraded tick can take must emit one of those lines,
// or the rule pages for a wedged loop that is not wedged. Two of these exits are
// measured-normal behaviour rather than faults: the upstream had 154 consecutive
// empty windows in 90 days of history, and any SeaDex outage longer than the
// rule's window walks the failed-probe arm every 15 minutes.
//
// The same exits must re-state the finding set, because emission is what holds a
// better-release alert firing: a quiet run longer than that rule's lookback
// would otherwise resolve every standing finding and re-fire the whole set as
// new when the upstream next moved.
func TestEveryTickExitEmitsALineTheDeadmanCounts(t *testing.T) {
	boom := errors.New("upstream boom")
	// deadmanVocabulary is the log-message set alerts.yaml's stall rule matches.
	deadmanVocabulary := []string{"tick complete", "tick degraded"}
	tests := map[string]struct {
		sea  func() *fakeSeaDex
		want string
	}{
		"an empty window completes": {
			sea: func() *fakeSeaDex {
				return &fakeSeaDex{
					entries: seadexFrierenEntry(),
					countFn: func(context.Context, time.Time) (int, error) { return 0, nil },
				}
			},
			want: "tick complete",
		},
		"a productive window completes": {
			sea: func() *fakeSeaDex {
				return &fakeSeaDex{
					entries:       seadexFrierenEntry(),
					windowEntries: []seadex.Entry{windowEntry(1001, 501)},
				}
			},
			want: "tick complete",
		},
		"a failed probe degrades": {
			sea: func() *fakeSeaDex {
				return &fakeSeaDex{
					entries: seadexFrierenEntry(),
					countFn: func(context.Context, time.Time) (int, error) { return 0, boom },
				}
			},
			want: "tick degraded",
		},
		"a failed window fetch degrades": {
			sea: func() *fakeSeaDex {
				return &fakeSeaDex{
					entries:   seadexFrierenEntry(),
					windowErr: boom,
					countFn:   func(context.Context, time.Time) (int, error) { return 3, nil },
				}
			},
			want: "tick degraded",
		},
		"an oversized window degrades": {
			sea: func() *fakeSeaDex {
				return &fakeSeaDex{
					entries: seadexFrierenEntry(),
					countFn: func(context.Context, time.Time) (int, error) {
						return seadexapi.MaxWindowEntries, nil
					},
				}
			},
			want: "tick degraded",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			logger, recorder := capture.New()
			notifier := notify.NewNotifier(logger, nil)
			deps, _ := tickDeps(logger, tc.sea(), &fakeFeed{}, notifier, 96)
			s := New(deps)

			if healthy := s.Cycle(t.Context()); !healthy {
				t.Fatal("reconcile healthy=false, want true")
			}
			// Seed a standing row AFTER the reconcile so the tick's own
			// re-statement is the only thing that can keep it alive.
			notifier.Report([]compare.Finding{tickFinding(1002, 502)}, nil)
			summariesBefore := recorder.CountExact("findings reported")

			if healthy := s.Cycle(t.Context()); !healthy {
				t.Fatal("tick healthy=false, want true (no tick exit is unhealthy: health follows the arr walk)")
			}

			emitted := map[string]int{}
			for _, msg := range deadmanVocabulary {
				emitted[msg] = recorder.CountExact(msg)
			}
			if emitted[tc.want] != 1 {
				t.Errorf("%q count = %d, want 1; deadman lines seen = %v. A tick exit with no completion line "+
					"makes SeadexScoutScanStalled fire with a restart runbook for a loop that is running", tc.want, emitted[tc.want], emitted)
			}
			if got := recorder.CountExact("findings reported"); got != summariesBefore+1 {
				t.Errorf("report summary lines = %d, want %d (every tick exit must re-state the finding set, "+
					"or a quiet run resolves every standing alert)", got, summariesBefore+1)
			}
			if total, _ := lastSummaryCounter(t, recorder, "total"); total < 1 {
				t.Errorf("finding set after the tick = %d rows, want the standing row still present", total)
			}
		})
	}
}

// TestCycleRetriesReconcileUntilReadyThenGivesUp pins the readiness gate and its
// bounded retry, which together are the whole mitigation for a false all-clear.
//
// The in-memory finding set is empty until a reconcile fills it, so a tick that
// published before then would emit its handful of window findings as the app's
// entire state - resolving every other standing condition. Two rules follow:
// while no reconcile has succeeded the loop RETRIES the reconcile instead of
// ticking (the dark window is minutes, not a day), and a tick that does run in
// that state publishes nothing.
//
// The retry is bounded because it is a full catalogue fetch plus a full arr
// walk: retrying forever against a condition that will not clear would run a
// full pass every 15 minutes against a community-run upstream, which is the
// traffic this design exists to remove.
func TestCycleRetriesReconcileUntilReadyThenGivesUp(t *testing.T) {
	logger, recorder := capture.New()
	// A SeaDex fetch failure gates the reconcile before it can report: healthy,
	// degraded, and no finding set established.
	sea := &fakeSeaDex{
		err:           errors.New("seadex boom"),
		entries:       seadexFrierenEntry(),
		windowEntries: []seadex.Entry{windowEntry(1001, 501)},
	}
	// An every wide enough that the retries are visibly OUT of cadence (only
	// iteration 0 is due), and narrow enough that the test can then walk to the
	// next scheduled reconcile.
	const every = reconcileRetryLatch + 4
	s, _ := newTickScout(logger, sea, &fakeFeed{}, nil, every)

	for i := 1; i <= reconcileRetryLatch; i++ {
		if healthy := s.Cycle(t.Context()); !healthy {
			t.Fatalf("iteration %d healthy=false, want true (a gated upstream is degraded, not unhealthy)", i)
		}
		if s.ready {
			t.Fatalf("ready=true after %d gated reconciles, want false (none of them reported a finding set)", i)
		}
		if full, _ := countWindowModes(sea); full != i {
			t.Errorf("after %d iterations full fetches = %d, want %d (a reconcile that established nothing must be retried, not deferred a day)", i, full, i)
		}
	}

	// The budget is spent: the loop stops retrying out of cadence and ticks
	// instead, and that tick publishes nothing.
	summariesBefore := recorder.CountExact("findings reported")
	if healthy := s.Cycle(t.Context()); !healthy {
		t.Fatal("post-budget iteration healthy=false, want true")
	}
	if full, _ := countWindowModes(sea); full != reconcileRetryLatch {
		t.Errorf("full fetches = %d, want %d (the retry must be bounded: an unbounded one runs a full pass every interval)", full, reconcileRetryLatch)
	}
	if _, window := countWindowModes(sea); window != 0 {
		t.Errorf("window fetches = %d, want 0 (a tick with no established finding set must not compare, let alone publish)", window)
	}
	if got := recorder.CountExact("findings reported"); got != summariesBefore {
		t.Errorf("report summary lines = %d, want the pre-tick %d (publishing a window's findings as the whole state resolves everything else)", got, summariesBefore)
	}
	if n := recorder.CountExact("tick degraded"); n != 1 {
		t.Errorf("'tick degraded' count = %d, want 1 (the suppressed tick must still tell the deadman the loop is alive)", n)
	}
	if reasons := tickDegradedReasons(recorder); len(reasons) != 1 || reasons[0] != "awaiting-first-reconcile" {
		t.Errorf("tick degraded reasons = %v, want [awaiting-first-reconcile]", reasons)
	}

	// A reconcile that DOES report flips the gate. It arrives on the normal
	// cadence rather than immediately, which is the bound's whole point.
	sea.err = nil
	for range every {
		if s.ready {
			break
		}
		if healthy := s.Cycle(t.Context()); !healthy {
			t.Fatal("recovering iteration healthy=false, want true")
		}
	}
	if !s.ready {
		t.Fatalf("ready=false after %d further iterations, want true (the scheduled reconcile must still run and open the gate)", every)
	}
}

// TestCycleReadyGateOpensOnlyOnAReconcileThatReported pins which reconcile exit
// counts as readiness: only the one that reached the compare and reported a
// whole-catalogue finding set. A gated exit returns healthy and leaves the set
// empty, so treating it as ready is exactly the false all-clear the gate exists
// to prevent.
func TestCycleReadyGateOpensOnlyOnAReconcileThatReported(t *testing.T) {
	logger := scoutTestLogger()
	sea := &fakeSeaDex{entries: seadexFrierenEntry(), windowEntries: []seadex.Entry{windowEntry(1001, 501)}}
	s, _ := newTickScout(logger, sea, nil, nil, 96)
	if s.ready {
		t.Fatal("ready=true before any cycle ran, want false")
	}
	if healthy := s.Cycle(t.Context()); !healthy {
		t.Fatal("reconcile healthy=false, want true")
	}
	if !s.ready {
		t.Error("ready=false after a completed reconcile, want true (its Report established the whole-catalogue set)")
	}
	if s.reconcileRetries != 0 {
		t.Errorf("reconcileRetries = %d, want 0 (a reconcile that reported spends no retry budget)", s.reconcileRetries)
	}
}

// tickDegradedReasons reports the reason attribute of every "tick degraded"
// line, the tick-side twin of degradedReasons.
func tickDegradedReasons(recorder *capture.Recorder) []string {
	var reasons []string
	for _, rec := range recorder.Records() {
		if rec.Message != "tick degraded" {
			continue
		}
		rec.Attrs(func(a slog.Attr) bool {
			if a.Key == "reason" {
				reasons = append(reasons, a.Value.String())
			}
			return true
		})
	}
	return reasons
}

// TestReconcileCompleteIsEmittedByADegradedReconcile pins the other half of that
// signal's contract: the backstop deadman asks whether the full pass RAN, not
// whether it was clean.
//
// A partial arr walk closes the cycle as degraded, and it still fetched the whole
// catalogue, walked the whole library it could reach, and rebuilt the whole feed
// and search index - every gap the 48h window cannot see was covered. Emitting
// the marker only on the clean path would page SeadexScoutReconcileStalled after
// three degraded days for a backstop that ran on all three, while the operator
// already has the `cycle degraded` line telling them about the quality problem.
func TestReconcileCompleteIsEmittedByADegradedReconcile(t *testing.T) {
	logger, recorder := capture.New()
	sonarr := &flakySonarr{
		fakeSonarr: fakeSonarr{
			series: []arrapi.Series{
				{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023},
				{ID: 8, Title: "Second Show", TvdbID: 124, Year: 2024},
			},
			files: map[int][]arrapi.EpisodeFile{
				7: {{SeasonNumber: 1, ReleaseGroup: "Erai-raws"}},
				8: {{SeasonNumber: 1, ReleaseGroup: "Erai-raws"}},
			},
		},
		failEpisodes: map[int]bool{8: true},
	}
	s := New(&Deps{
		Logger:   logger,
		Store:    &fakeStore{st: state.State{Mapping: twoRecordMappingCache()}},
		Library:  arrwalk.NewWalker(&arrwalk.Config{Sonarr: sonarr, Logger: scoutTestLogger()}),
		Mapping:  fakeMapping{},
		SeaDex:   &fakeSeaDex{entries: seadexFrierenEntry()},
		Matcher:  match.NewMatcher(notFoundAniList{}, scoutTestLogger()),
		Comparer: compare.NewComparer(compare.Config{}),
		Notifier: notify.NewNotifier(scoutTestLogger(), nil),
	})

	if healthy := s.Cycle(t.Context()); !healthy {
		t.Fatal("partial-walk reconcile healthy=false, want true (a partial walk is degraded, not unhealthy)")
	}
	if reasons := degradedReasons(recorder); len(reasons) != 1 || reasons[0] != "partial-walk" {
		t.Fatalf("degraded reasons = %v, want [partial-walk] (this test needs a DEGRADED completed reconcile)", reasons)
	}
	if n := recorder.CountExact("cycle complete"); n != 0 {
		t.Errorf("'cycle complete' count = %d, want 0 (the cycle was degraded)", n)
	}
	if n := recorder.CountExact("reconcile complete"); n != 1 {
		t.Errorf("'reconcile complete' count = %d, want 1; the backstop ran end to end, and withholding the marker "+
			"makes SeadexScoutReconcileStalled fire for a reconcile that never stopped", n)
	}
}

// TestReconcilePrunesTheMemoAndTickDoesNot pins which pass may garbage-collect
// the AniList memo, which is the whole reason pruning is an explicit call rather
// than something the match does on its way out.
//
// Pruning keeps an expired entry only when SeaDex still lists its AniList id, so
// the judgement is only sound against a WHOLE catalogue. A tick sees a 48-hour
// window, so if it pruned it would delete nearly every expired entry in the memo
// - including the positives the Torznab feed's stale-title tier reads, which
// silently downgrades those items' RSS titles to the file-name derivation and
// their category to the default, sending a movie to Sonarr instead of Radarr.
//
// The test is also the guard on the failure this shape introduces: pruning now
// depends on a caller remembering to ask for it, and forgetting means the memo
// accumulates entries for shows SeaDex no longer carries, forever.
func TestReconcilePrunesTheMemoAndTickDoesNot(t *testing.T) {
	logger := scoutTestLogger()
	sea := &fakeSeaDex{
		entries:       seadexFrierenEntry(),
		windowEntries: []seadex.Entry{windowEntry(1001, 501)},
	}
	deps, store := tickDeps(logger, sea, nil, nil, 96)
	// An expired entry for an id NEITHER the catalogue nor the window carries:
	// dead cache data, and the reconcile must collect it.
	const deadID = 424242
	store.st.Memo = match.Memo{Entries: map[int]match.MemoEntry{
		deadID: {NotFound: true, Expiry: time.Now().Add(-time.Hour)},
	}}
	s := New(deps)

	if healthy := s.Cycle(t.Context()); !healthy {
		t.Fatal("reconcile healthy=false, want true")
	}
	if _, kept := store.st.Memo.Entries[deadID]; kept {
		t.Error("the reconcile left a dead memo entry behind; nothing else collects it, so it would persist forever")
	}

	// Re-seed and run a TICK: its window cannot speak for the catalogue, so the
	// same entry must survive even though it is expired and unlisted.
	store.st.Memo = match.Memo{Entries: map[int]match.MemoEntry{
		deadID: {NotFound: true, Expiry: time.Now().Add(-time.Hour)},
	}}
	if healthy := s.Cycle(t.Context()); !healthy {
		t.Fatal("tick healthy=false, want true")
	}
	if _, kept := store.st.Memo.Entries[deadID]; !kept {
		t.Error("a tick pruned the memo; a 48-hour window has no standing to decide an id it never fetched is no longer curated")
	}
}

// TestReconcileAnnouncesItselfBeforeDoingWork pins the one line the scan deadman
// needs that is not a completion line.
//
// Every other line that deadman counts is emitted when a pass FINISHES, so
// between a container start and the first finished pass an absence rule has
// nothing to match — and it reads that as a wedged loop. A cold full pass takes
// ~25 minutes and historically up to 2h, which is why the health marker's lease
// is floored at three hours; without a start line the deadman would page fifteen
// minutes into every restart while the app was working normally.
//
// It must be emitted BEFORE the walk, not after, and by a gated pass too: a pass
// that announces itself and then never completes is exactly the wedge the rule
// should catch, and one that announces itself and then degrades is alive.
func TestReconcileAnnouncesItselfBeforeDoingWork(t *testing.T) {
	for name, tc := range map[string]struct {
		sea *fakeSeaDex
	}{
		"a clean reconcile":           {&fakeSeaDex{entries: seadexFrierenEntry()}},
		"a reconcile gated upstream":  {&fakeSeaDex{err: errors.New("seadex down"), entries: seadexFrierenEntry()}},
		"a reconcile with no entries": {&fakeSeaDex{}},
	} {
		t.Run(name, func(t *testing.T) {
			logger, recorder := capture.New()
			s, _ := newTickScout(logger, tc.sea, nil, nil, 96)

			if healthy := s.Cycle(t.Context()); !healthy {
				t.Fatal("reconcile healthy=false, want true (an upstream gate is degraded, not unhealthy)")
			}
			if n := recorder.CountExact("reconcile started"); n != 1 {
				t.Errorf("'reconcile started' count = %d, want 1 whatever the outcome; without it the deadman "+
					"pages 15 minutes into every restart", n)
			}
			// Ordering: the announcement has to precede whatever the pass logs
			// about its own outcome, or it cannot cover a slow pass.
			msgs := recorder.Messages()
			first := -1
			for i, m := range msgs {
				if m == "reconcile started" {
					first = i
					break
				}
			}
			if first != 0 {
				t.Errorf("'reconcile started' was log line %d of %v, want the first thing the pass says", first, msgs)
			}
		})
	}
}

// rejectingAfterFirstMapping loads successfully once - the startup reconcile
// that establishes the library snapshot and the finding set - and then refuses
// every later refresh with a plain error, no index, and an incremented rejection
// streak, which is what the acceptance guard does once the persisted cache stops
// being usable. unreachableMapLoader cannot drive this: it fails from the first
// call, so no reconcile ever opens the ready gate and the loop never reaches a
// tick.
type rejectingAfterFirstMapping struct{ calls int }

func (m *rejectingAfterFirstMapping) Load(_ context.Context, prev *mapping.Cache) (mapping.Cache, *mapping.Index, error) {
	m.calls++
	if m.calls == 1 {
		return *prev, mapping.NewIndex(prev.Records), nil
	}
	c := *prev
	c.RejectedRefreshes = prev.RejectedRefreshes + 1
	return c, nil, errors.New("fribb refresh refused: indexes to no usable records")
}

// TestTickWithAnUnusableMapSkipsComparisonAndKeepsEveryRow pins the tick's
// mapping-unusable OUTCOME, whose three contracts are all silent when they break.
// With no usable Fribb index nothing can be matched, so comparing anyway would
// hand ReportScoped an empty finding set with authority over every entry the
// window carried and resolve conditions that are still true. The scan deadman
// needs the degraded completion line with reason=mapping-unusable, because a
// silent iteration is indistinguishable from a wedged loop. And the
// refresh-rejection streak the mapping escalation reads must still be persisted,
// or a restart resets it.
//
// The reconcile's equivalent gate is pinned twice
// (TestCycleMappingUnusableReportsNothing and
// TestCycleUnusableMapWithSeaDexOutageWarnsFeedKept); the tick's half had no
// test, so a regression that dropped or inverted it kept every tick green.
func TestTickWithAnUnusableMapSkipsComparisonAndKeepsEveryRow(t *testing.T) {
	logger, recorder := capture.New()
	sea := &fakeSeaDex{
		entries:       seadexFrierenEntry(),
		windowEntries: seadexFrierenEntry(),
	}
	deps, store := tickDeps(logger, sea, nil, nil, 96)
	deps.Mapping = &rejectingAfterFirstMapping{}
	s := New(deps)

	if healthy := s.Cycle(t.Context()); !healthy {
		t.Fatal("reconcile healthy=false, want true")
	}
	// Seed a standing row AFTER the reconcile's full-authority pass, so only the
	// tick under test could delete it.
	s.notifier.Report([]compare.Finding{tickFinding(154587, 501)}, nil)
	savesAfterSeed := store.saves

	if healthy := s.Cycle(t.Context()); !healthy {
		t.Fatal("tick healthy=false, want true (a restart cannot fix a Fribb outage)")
	}

	if reasons := tickDegradedReasons(recorder); len(reasons) != 1 || reasons[0] != "mapping-unusable" {
		t.Errorf("tick degraded reasons = %v, want [mapping-unusable] (the scan deadman counts this line)", reasons)
	}
	if n := recorder.CountExact("mapping unusable; skipping tick comparison"); n != 1 {
		t.Errorf("'mapping unusable; skipping tick comparison' emitted %d times, want 1", n)
	}
	total, seen := lastSummaryCounter(t, recorder, "total")
	if !seen {
		t.Fatal("the gated tick emitted no 'findings reported' summary line")
	}
	if total != 1 {
		t.Errorf("finding set after the gated tick = %d rows, want the seeded 1 re-stated unchanged (an unmatched window must not resolve a standing row)", total)
	}
	if store.saves != savesAfterSeed+1 {
		t.Errorf("saves = %d, want %d: a gated tick must still persist the refresh-rejection streak the mapping escalation reads", store.saves, savesAfterSeed+1)
	}
	if got := store.st.Mapping.RejectedRefreshes; got != 1 {
		t.Errorf("persisted RejectedRefreshes = %d, want 1", got)
	}
}

// revalidatingMapping is a mapping loader that behaves like the REAL one on a
// 304: same records, same validators, fresh FetchedAt (mapping.go's
// not-modified arm does exactly this). The default fake returns prev untouched,
// which is not what production looks like on the ~96% of ticks that revalidate
// without a body change - and a test built on it cannot see a comparison that
// wrongly includes the timestamp.
type revalidatingMapping struct{}

func (revalidatingMapping) Load(_ context.Context, prev *mapping.Cache) (mapping.Cache, *mapping.Index, error) {
	c := *prev
	c.FetchedAt = time.Now()
	return c, mapping.NewIndex(prev.Records), nil
}

// refreshingMapping is a mapping loader that reports a NEW validator on every
// call, i.e. an accepted Fribb refresh rather than a 304. It is the real trigger
// for the write the skip below must not suppress.
type refreshingMapping struct{ calls int }

func (r *refreshingMapping) Load(_ context.Context, prev *mapping.Cache) (mapping.Cache, *mapping.Index, error) {
	r.calls++
	c := *prev
	c.ETag = "etag-" + strconv.Itoa(r.calls)
	return c, mapping.NewIndex(prev.Records), nil
}

// TestProductiveTickSkipsTheStateWriteWhenNothingChanged pins the write skip and
// the one thing that makes it non-trivial.
//
// state.json is a single ~2.5 MB document dominated by the library snapshot, so
// persisting a tick's few KB of memo and validators rewrites all of it. At ~92
// productive ticks a day that is ~230 MB/day of writes, and a tick that renewed
// no AniList entry and got a 304 for the mapping learned nothing worth any of it.
//
// The trap: almost every tick DOES get a 304, and the loader returns the cached
// map with a fresh FetchedAt. Comparing whole cache values would therefore see a
// change every time and the skip would never fire, so the comparison excludes
// that timestamp deliberately. This test is what stops a future "compare the
// whole struct, it is simpler" from silently restoring the 230 MB - and the
// second case is what stops the skip from swallowing a real refresh.
func TestProductiveTickSkipsTheStateWriteWhenNothingChanged(t *testing.T) {
	for name, tc := range map[string]struct {
		mapping   MappingSource
		wantSaves bool
	}{
		// The default fake hands back the cached records unchanged, which is what
		// a 304 looks like to the scout, and the window entry needs no AniList
		// lookup - so this tick fetches, advances and reports while learning
		// nothing persistable.
		"a revalidated mapping and no renewal writes nothing": {revalidatingMapping{}, false},
		"an accepted mapping refresh still writes":            {&refreshingMapping{}, true},
	} {
		t.Run(name, func(t *testing.T) {
			// The window carries the MAPPED Frieren entry, so it matches by id
			// and needs no AniList lookup. An unmapped id would take the title
			// fallback and memoize its not-found answer, which is a real thing
			// learned and must be persisted - so it would not exercise the skip.
			sea := &fakeSeaDex{
				entries:       seadexFrierenEntry(),
				windowEntries: seadexFrierenEntry(),
			}
			deps, store := tickDeps(scoutTestLogger(), sea, nil, nil, 96)
			deps.Mapping = tc.mapping
			s := New(deps)

			if healthy := s.Cycle(t.Context()); !healthy {
				t.Fatal("reconcile healthy=false, want true")
			}
			// The reconcile's own save has happened; from here the tick is on its own.
			savesAfterReconcile := store.saves

			if healthy := s.Cycle(t.Context()); !healthy {
				t.Fatal("tick healthy=false, want true")
			}
			if _, window := countWindowModes(sea); window != 1 {
				t.Fatalf("window fetches = %d, want 1 (the tick must have done its work)", window)
			}

			wrote := store.saves > savesAfterReconcile
			if wrote != tc.wantSaves {
				if tc.wantSaves {
					t.Errorf("saves = %d, want more than %d: a tick that learned something must persist it",
						store.saves, savesAfterReconcile)
				} else {
					t.Errorf("saves = %d, want the reconcile's %d: a tick that renewed nothing and got an "+
						"unchanged mapping must not rewrite the whole state document", store.saves, savesAfterReconcile)
				}
			}
		})
	}
}
