package scout

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"github.com/cplieger/arrapi"
	"github.com/cplieger/seadex-scout/internal/arrwalk"
	"github.com/cplieger/seadex-scout/internal/compare"
	"github.com/cplieger/seadex-scout/internal/match"
	"github.com/cplieger/seadex-scout/internal/notify"
	"github.com/cplieger/seadex-scout/internal/seadex"
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
		if mode == seadex.FetchWindow {
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
		if healthy := s.Cycle(context.Background()); !healthy {
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
				if healthy := s.Cycle(context.Background()); !healthy {
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

	if healthy := s.Cycle(context.Background()); !healthy {
		t.Fatal("reconcile healthy=false, want true")
	}
	if n := recorder.CountExact("reconcile complete"); n != 1 {
		t.Fatalf("'reconcile complete' after the full pass = %d, want 1", n)
	}

	// A PRODUCTIVE tick: it fetches, advances and reports, and still must not
	// claim a reconcile happened.
	if healthy := s.Cycle(context.Background()); !healthy {
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

	if healthy := s.Cycle(context.Background()); !healthy {
		t.Fatal("reconcile healthy=false, want true")
	}
	savesAfterReconcile := store.saves

	if healthy := s.Cycle(context.Background()); !healthy {
		t.Fatal("tick healthy=false, want true (an empty window is a successful tick)")
	}

	if _, window := countWindowModes(sea); window != 0 {
		t.Errorf("window fetches = %d, want 0 (the probe already answered)", window)
	}
	if s.emptyRun != 1 {
		t.Errorf("emptyRun = %d, want 1", s.emptyRun)
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
	// The window's cost bound is a probe, not a fetch, and the tick must not
	// masquerade as a completed pass.
	if n := recorder.CountExact("tick complete"); n != 0 {
		t.Errorf("'tick complete' count = %d, want 0 (nothing was fetched or reported)", n)
	}
}

// TestTickEmptyRunWarnsAtItsLatch pins the wedge diagnostic for a permanently
// empty window.
//
// An empty 48h window already means 48h of upstream silence has elapsed, so the
// latch adds another 48h of empty probes on top - roughly 96h of total silence,
// against a measured worst genuine silence of 86.6h. It stays a WARN because it
// IS usually healthy; the condition it is really looking for is a container
// clock running more than the window AHEAD, which puts every window in the
// upstream's future and looks identical to a quiet upstream.
//
// It must fire ONCE, at the latch, not on every tick after it: a per-tick WARN
// at a 15-minute cadence is 96 identical lines a day, which is how a real signal
// gets filtered out.
func TestTickEmptyRunWarnsAtItsLatch(t *testing.T) {
	logger, recorder := capture.New()
	sea := &fakeSeaDex{
		entries: seadexFrierenEntry(),
		countFn: func(context.Context, time.Time) (int, error) { return 0, nil },
	}
	// A reconcile every 1000 iterations, so the whole run below is ticks.
	s, _ := newTickScout(logger, sea, nil, nil, 1000)
	if healthy := s.Cycle(context.Background()); !healthy {
		t.Fatal("reconcile healthy=false, want true")
	}

	const warn = "no SeaDex change seen for a very long run of ticks; " +
		"if this persists, check this container's clock against the upstream"
	for i := 1; i <= emptyRunLatch+2; i++ {
		if healthy := s.Cycle(context.Background()); !healthy {
			t.Fatalf("tick %d healthy=false, want true", i)
		}
		want := 0
		if i >= emptyRunLatch {
			want = 1
		}
		if got := recorder.CountExact(warn); got != want {
			t.Fatalf("after %d consecutive empty ticks the latch WARN count = %d, want %d", i, got, want)
		}
	}
	if got := recorder.CountLevel(slog.LevelError, "no SeaDex change seen"); got != 0 {
		t.Errorf("empty-run ERROR count = %d, want 0 (a quiet upstream is usually healthy)", got)
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
			return seadex.MaxWindowEntries, nil
		},
	}
	feed := &fakeFeed{}
	s, _ := newTickScout(logger, sea, feed, nil, 1000)
	if healthy := s.Cycle(context.Background()); !healthy {
		t.Fatal("reconcile healthy=false, want true")
	}

	const warnMsg = "SeaDex change window too large to fetch; deferring to the reconcile"
	const errSub = "SeaDex change window has been too large to fetch repeatedly"
	for i := 1; i <= oversizeRunLatch; i++ {
		if healthy := s.Cycle(context.Background()); !healthy {
			t.Fatalf("oversize tick %d healthy=false, want true (it is degraded, not unhealthy)", i)
		}
		if s.oversizeRun != i {
			t.Errorf("after %d oversize ticks oversizeRun = %d, want %d", i, s.oversizeRun, i)
		}
		wantWarns, wantErrors := i, 0
		if i >= oversizeRunLatch {
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
	if s.emptyRun != 0 {
		t.Errorf("emptyRun = %d, want 0 (an oversize tick is not an empty one)", s.emptyRun)
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
		"one below the bound fetches": {seadex.MaxWindowEntries - 1, true, false},
		"exactly the bound defers":    {seadex.MaxWindowEntries, false, true},
		"above the bound defers":      {seadex.MaxWindowEntries + 1, false, true},
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
			if healthy := s.Cycle(context.Background()); !healthy {
				t.Fatal("reconcile healthy=false, want true")
			}
			if healthy := s.Cycle(context.Background()); !healthy {
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

// TestTickProductiveResetsBothRuns pins the reset. Both wedge counters measure a
// CONSECUTIVE run, so a productive tick has to clear them - otherwise a single
// long quiet spell would latch its diagnostic permanently, and the next genuine
// wedge would be indistinguishable from the noise it left behind.
//
// Each counter is driven to just under its latch first, so the assertion is
// about the reset and not about a counter that never moved.
func TestTickProductiveResetsBothRuns(t *testing.T) {
	tests := map[string]struct {
		empty, oversize int
	}{
		"after a long empty run":    {emptyRunLatch - 1, 0},
		"after a long oversize run": {0, oversizeRunLatch - 1},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			logger := scoutTestLogger()
			sea := &fakeSeaDex{
				entries:       seadexFrierenEntry(),
				windowEntries: []seadex.Entry{windowEntry(1001, 501)},
			}
			s, _ := newTickScout(logger, sea, &fakeFeed{}, nil, 96)
			if healthy := s.Cycle(context.Background()); !healthy {
				t.Fatal("reconcile healthy=false, want true")
			}
			s.emptyRun, s.oversizeRun = tc.empty, tc.oversize

			if healthy := s.Cycle(context.Background()); !healthy {
				t.Fatal("productive tick healthy=false, want true")
			}

			if s.emptyRun != 0 {
				t.Errorf("emptyRun = %d, want 0 after a productive tick", s.emptyRun)
			}
			if s.oversizeRun != 0 {
				t.Errorf("oversizeRun = %d, want 0 after a productive tick", s.oversizeRun)
			}
			if _, window := countWindowModes(sea); window != 1 {
				t.Errorf("window fetches = %d, want 1 (the tick was productive)", window)
			}
		})
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

	if healthy := s.Cycle(context.Background()); !healthy {
		t.Fatal("reconcile healthy=false, want true")
	}
	if feed.calls != 1 {
		t.Errorf("Rebuild calls after the reconcile = %d, want 1", feed.calls)
	}
	if feed.advanceCalls != 0 {
		t.Errorf("Advance calls after the reconcile = %d, want 0 (a full pass rebuilds)", feed.advanceCalls)
	}

	if healthy := s.Cycle(context.Background()); !healthy {
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

	if healthy := s.Cycle(context.Background()); !healthy {
		t.Fatal("reconcile healthy=false, want true")
	}
	if healthy := s.Cycle(context.Background()); !healthy {
		t.Fatal("tick healthy=false, want true (a feed failure must not fail the tick)")
	}
	if !recorder.Contains("indexer feed advance failed; keeping previous feed") {
		t.Errorf("feed advance failure not warned; messages: %v", recorder.Messages())
	}
	if n := recorder.CountExact("tick complete"); n != 1 {
		t.Errorf("'tick complete' count = %d, want 1 (the tick still finished its compare and report)", n)
	}
}

// TestTickWithNoFeedConfiguredStillReports pins the nil-feed arm: with no
// Torznab feed configured the tick skips all feed work and still does the half
// the operator always gets (compare and report), rather than short-circuiting.
func TestTickWithNoFeedConfiguredStillReports(t *testing.T) {
	logger, recorder := capture.New()
	sea := &fakeSeaDex{
		entries:       seadexFrierenEntry(),
		windowEntries: []seadex.Entry{windowEntry(1001, 501)},
	}
	s, _ := newTickScout(logger, sea, nil, nil, 96)

	if healthy := s.Cycle(context.Background()); !healthy {
		t.Fatal("reconcile healthy=false, want true")
	}
	if healthy := s.Cycle(context.Background()); !healthy {
		t.Fatal("tick healthy=false, want true")
	}
	if n := recorder.CountExact("tick complete"); n != 1 {
		t.Errorf("'tick complete' count = %d, want 1", n)
	}
}

// TestTickReportsWithExactlyTheWindowsAuthority pins the deletion authority the
// tick hands the notifier, which is the subtlest correctness property of the
// whole fast path.
//
// A tick compared only the window's entries, so it may only delete rows those
// entries own. Every other row must be carried: a row whose entry the tick never
// examined is absent from its finding set for exactly the same reason a resolved
// one is, and deleting it would clear an alert while the condition still holds,
// then re-raise it as new on the next reconcile.
//
// The set is asserted through the notifier's own behaviour rather than by
// intercepting the call: three rows are seeded, the window carries two of their
// owners, and only those two may go.
func TestTickReportsWithExactlyTheWindowsAuthority(t *testing.T) {
	logger, recorder := capture.New()
	notifier := notify.NewNotifier(logger, nil)
	// Seed three standing rows under three different owners.
	notifier.Report([]compare.Finding{
		tickFinding(1001, 501),
		tickFinding(1002, 502),
		tickFinding(1003, 503),
	}, nil)
	if total, _ := lastSummaryCounter(t, recorder, "total"); total != 3 {
		t.Fatalf("seeded finding set = %d rows, want 3", total)
	}

	// The window carries owners 1001 and 1003 only, and neither produces a
	// finding (nothing in the test library maps to them).
	sea := &fakeSeaDex{
		entries:       seadexFrierenEntry(),
		windowEntries: []seadex.Entry{windowEntry(1001, 501), windowEntry(1003, 503)},
	}
	s, _ := newTickScout(logger, sea, nil, notifier, 96)
	if healthy := s.Cycle(context.Background()); !healthy {
		t.Fatal("reconcile healthy=false, want true")
	}
	// The reconcile re-reported with FULL authority, so re-seed the standing
	// rows it deleted by omission before the tick runs.
	notifier.Report([]compare.Finding{
		tickFinding(1001, 501),
		tickFinding(1002, 502),
		tickFinding(1003, 503),
	}, nil)

	if healthy := s.Cycle(context.Background()); !healthy {
		t.Fatal("tick healthy=false, want true")
	}

	total, seen := lastSummaryCounter(t, recorder, "total")
	if !seen {
		t.Fatal("the tick emitted no 'findings reported' summary line")
	}
	if total != 1 {
		t.Errorf("finding set after the tick = %d rows, want 1 (only 1002, whose owner the window never examined)", total)
	}
	carried, _ := lastSummaryCounter(t, recorder, "carried")
	if carried != 1 {
		t.Errorf("carried = %d, want 1 (the row outside the window's authority)", carried)
	}
	// Nothing was preserved for incompleteness: with a definitive-not-found
	// AniList no match is transiently incomplete, so the survival above is
	// attributable to the authority bound alone.
	if preserved, _ := lastSummaryCounter(t, recorder, "preserved"); preserved != 0 {
		t.Errorf("preserved = %d, want 0 (no entry had incomplete evidence)", preserved)
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

	if healthy := s.Cycle(context.Background()); !healthy {
		t.Fatal("reconcile healthy=false, want true")
	}
	if n := len(store.st.Library.Items); n != 1 {
		t.Fatalf("library items after the reconcile = %d, want 1 (the walk's snapshot)", n)
	}
	savesAfterReconcile := store.saves

	if healthy := s.Cycle(context.Background()); !healthy {
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
// establish what changed says NOTHING rather than something wrong.
//
// Reporting an empty set would clear every standing alert (the notifier deletes
// by omission within its authority), and the correct behaviour on an unreachable
// upstream is to let the last report stand until the alert's own lookback window
// expires. Neither failure is unhealthy either - container health follows the
// library ingest, and a restart cannot fix an upstream outage.
//
// The wedge counters must not move on either: they measure upstream STATE, not
// reachability, which the existing SeadexFailures streak already owns. A probe
// failure counted as an empty window would latch the clock-skew WARN during an
// ordinary outage.
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

			if healthy := s.Cycle(context.Background()); !healthy {
				t.Fatal("reconcile healthy=false, want true")
			}
			// The reconcile reported with full authority; re-seed the standing
			// row so the tick's silence is observable.
			notifier.Report([]compare.Finding{tickFinding(1002, 502)}, nil)
			summariesBefore := recorder.CountExact("findings reported")

			if healthy := s.Cycle(context.Background()); !healthy {
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
			if got := recorder.CountExact("findings reported"); got != summariesBefore {
				t.Errorf("the failed tick emitted a report (summary lines %d, want the pre-tick %d); "+
					"an empty report would clear every standing alert", got, summariesBefore)
			}
			if total, _ := lastSummaryCounter(t, recorder, "total"); total != 1 {
				t.Errorf("finding set = %d rows, want the standing 1 left untouched", total)
			}
			if feed.advanceCalls != 0 {
				t.Errorf("Advance calls = %d, want 0 (nothing was fetched)", feed.advanceCalls)
			}
			if s.emptyRun != 0 || s.oversizeRun != 0 {
				t.Errorf("wedge counters = empty %d / oversize %d, want 0/0 (they measure upstream state, not reachability)",
					s.emptyRun, s.oversizeRun)
			}
			if n := recorder.CountExact("tick complete"); n != 0 {
				t.Errorf("'tick complete' count = %d, want 0 (the tick did not complete its work)", n)
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
	if healthy := s.Cycle(context.Background()); !healthy {
		t.Fatal("reconcile healthy=false, want true")
	}

	before := time.Now()
	if healthy := s.Cycle(context.Background()); !healthy {
		t.Fatal("tick healthy=false, want true")
	}
	if len(sea.countSince) != 1 {
		t.Fatalf("probes = %d, want 1", len(sea.countSince))
	}
	age := before.Sub(sea.countSince[0])
	if age < changeWindow-time.Minute || age > changeWindow+time.Minute {
		t.Errorf("probe since = %v before now, want ~%v (changeWindow)", age, changeWindow)
	}
	if changeWindow <= s.deps.PollInterval {
		t.Errorf("changeWindow %v is not wider than the poll interval %v; a missed tick would be unrecoverable",
			changeWindow, s.deps.PollInterval)
	}
}
