package scout

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/cplieger/arrapi/v2"
	"github.com/cplieger/seadex-scout/internal/anilist"
	"github.com/cplieger/seadex-scout/internal/arrwalk"
	"github.com/cplieger/seadex-scout/internal/compare"
	"github.com/cplieger/seadex-scout/internal/degradation"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/match"
	"github.com/cplieger/seadex-scout/internal/notify"
	"github.com/cplieger/seadex-scout/internal/seadex"
	"github.com/cplieger/seadex-scout/internal/seadexapi"
	"github.com/cplieger/seadex-scout/internal/state"
	"github.com/cplieger/slogx/capture"
)

// The standing-condition contract the alert rules read: every ERROR naming a
// lasting degradation carries a `condition` attribute, and a pass that does not
// observe a standing condition re-states it under one fixed message, so a short
// alert window stays firing for a condition only the daily reconcile observes.
const msgStillStandingContract = "degraded condition still standing"

// conditionLine is one ERROR record carrying a `condition` attribute.
type conditionLine struct {
	msg, condition, arr string
}

// conditionLines returns every ERROR record carrying a `condition` attribute,
// in emission order, starting at record index from.
func conditionLines(recorder *capture.Recorder, from int) []conditionLine {
	var lines []conditionLine
	for i, rec := range recorder.Records() {
		if i < from || rec.Level != slog.LevelError {
			continue
		}
		line := conditionLine{msg: rec.Message}
		rec.Attrs(func(a slog.Attr) bool {
			switch a.Key {
			case "condition":
				line.condition = a.Value.String()
			case "arr":
				line.arr = a.Value.String()
			}
			return true
		})
		if line.condition != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// cycleLines runs one Cycle and returns the condition lines that pass emitted.
func cycleLines(ctx context.Context, t *testing.T, s *Scout, recorder *capture.Recorder) []conditionLine {
	t.Helper()
	from := recorder.Len()
	if healthy := s.Cycle(ctx); !healthy {
		t.Fatalf("Cycle healthy=false, want true (every pass here is degraded at worst)")
	}
	return conditionLines(recorder, from)
}

// emptyProbe is a tick probe that reports an empty window, so a tick re-states
// the finding set and observes no upstream condition.
func emptyProbe(context.Context, time.Time) (int, error) { return 0, nil }

// partialWalkScout is a Scout reconciling every second iteration over a Sonarr
// whose second series fails its episode fetch until failEpisodes is cleared, so
// every reconcile observes a partial walk and every tick observes nothing.
func partialWalkScout(logger *slog.Logger) (*Scout, *flakySonarr) {
	sonarr := &flakySonarr{
		series: []arrapi.Series{
			{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023},
			{ID: 8, Title: "Second Show", TvdbID: 124, Year: 2024},
		},
		files: map[int][]arrapi.EpisodeFile{
			7: {{SeasonNumber: 1, ReleaseGroup: "Erai-raws"}},
			8: {{SeasonNumber: 1, ReleaseGroup: "Erai-raws"}},
		},
		failEpisodes: map[int]bool{8: true},
	}
	s := New(&Deps{
		Logger:       logger,
		Store:        &fakeStore{st: state.State{Mapping: twoRecordMappingCache()}},
		Library:      arrwalk.NewWalker(&arrwalk.Config{Sonarr: sonarr, Logger: scoutTestLogger()}),
		Mapping:      fakeMapping{},
		SeaDex:       &fakeSeaDex{entries: append(seadexFrierenEntry(), secondSeaDexEntry()), countFn: emptyProbe},
		Matcher:      match.New(notFoundAniList{}, scoutTestLogger()),
		Comparer:     compare.New(compare.Config{}),
		Notifier:     notify.NewNotifier(scoutTestLogger(), nil),
		AniListStats: aniStatsFn(anilist.NewClient(noNetworkClient(), "http://unused.invalid/gql", anilist.WithRate(1), anilist.WithLogger(scoutTestLogger()))),
		PollInterval: pollIntervalForEvery(2),
	})
	return s, sonarr
}

// TestStandingConditionIsRestatedOnPassesThatDoNotObserveIt pins the re-statement:
// a partial walk escalates on the second reconcile, and the tick after it, which
// walks nothing, still emits exactly one ERROR naming the condition. Without it
// the alert window would have to span the ~24h between two reconciles. The
// escalating pass itself carries the escalation and no re-statement, so a
// condition is stated exactly once per pass.
func TestStandingConditionIsRestatedOnPassesThatDoNotObserveIt(t *testing.T) {
	logger, recorder := capture.New()
	s, _ := partialWalkScout(logger)
	ctx := t.Context()

	if lines := cycleLines(ctx, t, s, recorder); len(lines) != 0 {
		t.Fatalf("first partial reconcile emitted %v, want nothing below the escalation threshold", lines)
	}
	if lines := cycleLines(ctx, t, s, recorder); len(lines) != 0 {
		t.Fatalf("tick before escalation emitted %v, want nothing", lines)
	}
	escalation := cycleLines(ctx, t, s, recorder)
	if len(escalation) != 1 || escalation[0].condition != "library-walk-partial" || escalation[0].msg == msgStillStandingContract {
		t.Fatalf("escalating reconcile emitted %+v, want exactly the escalated partial-walk ERROR with condition library-walk-partial and no re-statement", escalation)
	}
	restated := cycleLines(ctx, t, s, recorder)
	want := []conditionLine{{msg: msgStillStandingContract, condition: "library-walk-partial"}}
	if len(restated) != 1 || restated[0] != want[0] {
		t.Errorf("tick after escalation emitted %+v, want %+v", restated, want)
	}
}

// TestStandingConditionClearsWhenItsObserverRecovers pins the resolve half: once
// the reconcile walks cleanly, neither that pass nor the next tick names the
// condition, so the alert resolves within one window of the recovery.
func TestStandingConditionClearsWhenItsObserverRecovers(t *testing.T) {
	logger, recorder := capture.New()
	s, sonarr := partialWalkScout(logger)
	ctx := t.Context()
	for range 4 {
		cycleLines(ctx, t, s, recorder)
	}
	if lines := conditionLines(recorder, 0); len(lines) < 2 {
		t.Fatalf("setup emitted %v, want the escalation and a re-statement before recovery", lines)
	}

	sonarr.failEpisodes = nil
	if lines := cycleLines(ctx, t, s, recorder); len(lines) != 0 {
		t.Errorf("clean reconcile emitted %+v, want nothing (the condition ended)", lines)
	}
	if lines := cycleLines(ctx, t, s, recorder); len(lines) != 0 {
		t.Errorf("tick after the clean reconcile emitted %+v, want nothing (a cleared condition is not re-stated)", lines)
	}
}

// TestStandingShrinkConditionIsKeyedPerArr pins that the shrink condition stands
// per arr: with both sides shrunken both are re-stated, and when one recovers only
// the other keeps its alert, because each arr is its own alert series.
func TestStandingShrinkConditionIsKeyedPerArr(t *testing.T) {
	f := newTwoArrShrinkFixture()
	logger, recorder := capture.New()
	s := New(&Deps{
		Logger:       logger,
		Store:        f.store,
		Library:      arrwalk.NewWalker(&arrwalk.Config{Sonarr: f.sonarr, Radarr: f.radarr, Logger: scoutTestLogger()}),
		Mapping:      fakeMapping{},
		SeaDex:       &fakeSeaDex{entries: shrinkSeaDexEntries(), countFn: emptyProbe},
		Matcher:      match.New(notFoundAniList{}, scoutTestLogger()),
		Comparer:     compare.New(compare.Config{}),
		Notifier:     notify.NewNotifier(scoutTestLogger(), nil),
		PollInterval: pollIntervalForEvery(2),
	})
	ctx := t.Context()
	cycleLines(ctx, t, s, recorder)
	cycleLines(ctx, t, s, recorder)
	if got := countItemsByArr(f.store.st.Library.Items); got[library.ArrSonarr] != 4 || got[library.ArrRadarr] != 2 {
		t.Fatalf("seeded per-arr counts = %v, want 4 sonarr and 2 radarr", got)
	}

	movies := f.radarr.movies
	f.radarr.movies = nil
	f.sonarr.series = f.sonarr.series[:1]
	f.store.st.ShrunkWalksByArr = map[string]int{library.ArrSonarr: 1, library.ArrRadarr: 1}
	if lines := cycleLines(ctx, t, s, recorder); len(lines) != 2 {
		t.Fatalf("reconcile with both sides shrunken emitted %+v, want one escalation per arr", lines)
	}
	restated := cycleLines(ctx, t, s, recorder)
	want := []conditionLine{
		{msg: msgStillStandingContract, condition: "library-walk-shrunk", arr: library.ArrRadarr},
		{msg: msgStillStandingContract, condition: "library-walk-shrunk", arr: library.ArrSonarr},
	}
	if len(restated) != 2 || restated[0] != want[0] || restated[1] != want[1] {
		t.Errorf("tick after both escalated emitted %+v, want %+v in this order", restated, want)
	}

	f.radarr.movies = movies
	recovered := cycleLines(ctx, t, s, recorder)
	if len(recovered) != 1 || recovered[0].arr != library.ArrSonarr || recovered[0].msg == msgStillStandingContract {
		t.Errorf("reconcile with radarr recovered emitted %+v, want only sonarr's escalation", recovered)
	}
	restated = cycleLines(ctx, t, s, recorder)
	if len(restated) != 1 || restated[0] != want[1] {
		t.Errorf("tick after radarr recovered emitted %+v, want only %+v", restated, want[1])
	}
}

// TestShrinkConditionClearsWhenTheSmallerLibraryIsAccepted pins that accepting
// the smaller library ends the condition: the accepted shape is no longer a
// fault, so the alert resolves rather than standing until a restart.
func TestShrinkConditionClearsWhenTheSmallerLibraryIsAccepted(t *testing.T) {
	f := newTwoArrShrinkFixture()
	logger, recorder := capture.New()
	s := New(&Deps{
		Logger:       logger,
		Store:        f.store,
		Library:      arrwalk.NewWalker(&arrwalk.Config{Sonarr: f.sonarr, Radarr: f.radarr, Logger: scoutTestLogger()}),
		Mapping:      fakeMapping{},
		SeaDex:       &fakeSeaDex{entries: shrinkSeaDexEntries(), countFn: emptyProbe},
		Matcher:      match.New(notFoundAniList{}, scoutTestLogger()),
		Comparer:     compare.New(compare.Config{}),
		Notifier:     notify.NewNotifier(scoutTestLogger(), nil),
		PollInterval: pollIntervalForEvery(2),
	})
	ctx := t.Context()
	cycleLines(ctx, t, s, recorder)
	cycleLines(ctx, t, s, recorder)
	f.emptyRadarr()
	f.store.st.ShrunkWalksByArr = map[string]int{library.ArrRadarr: degradation.ShrunkWalkAcceptThreshold - 2}
	cycleLines(ctx, t, s, recorder)
	cycleLines(ctx, t, s, recorder)
	if lines := conditionLines(recorder, 0); len(lines) != 2 || lines[1].msg != msgStillStandingContract {
		t.Fatalf("setup emitted %+v, want the shrink escalation and one re-statement", lines)
	}

	if lines := cycleLines(ctx, t, s, recorder); len(lines) != 0 {
		t.Errorf("accepting reconcile emitted %+v, want nothing (acceptance is a WARN)", lines)
	}
	if lines := cycleLines(ctx, t, s, recorder); len(lines) != 0 {
		t.Errorf("tick after the acceptance emitted %+v, want nothing", lines)
	}
}

// failingProbeScout is a Scout at the default 15-minute cadence whose tick probe
// always fails, so the fast path's unreachability streak escalates after eight
// ticks; a cancelled context reports the cancellation instead.
func failingProbeScout(logger *slog.Logger) (*Scout, *fakeSeaDex) {
	sea := &fakeSeaDex{entries: seadexFrierenEntry()}
	sea.countFn = func(ctx context.Context, _ time.Time) (int, error) {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		return 0, errors.New("releases.moe unreachable")
	}
	s, _ := newTickScout(logger, sea, nil, nil, 96)
	return s, sea
}

// TestEscalatedSeaDexUnreachableNamesItsCondition pins the tick escalation's
// condition and that a pass which observes the condition states it once: the
// eighth failing tick escalates, and every later failing tick re-escalates
// rather than adding a re-statement beside it.
func TestEscalatedSeaDexUnreachableNamesItsCondition(t *testing.T) {
	logger, recorder := capture.New()
	s, _ := failingProbeScout(logger)
	ctx := t.Context()
	cycleLines(ctx, t, s, recorder)
	for i := 1; i < oversizeLatchAtDefaultCadence; i++ {
		if lines := cycleLines(ctx, t, s, recorder); len(lines) != 0 {
			t.Fatalf("failing tick %d emitted %+v, want nothing below the latch", i, lines)
		}
	}
	for i := range 2 {
		lines := cycleLines(ctx, t, s, recorder)
		if len(lines) != 1 || lines[0].condition != "seadex-unreachable" || lines[0].msg == msgStillStandingContract {
			t.Errorf("failing tick %d past the latch emitted %+v, want exactly one escalated ERROR with condition seadex-unreachable", oversizeLatchAtDefaultCadence+i, lines)
		}
	}
}

// runToReconcile drives a Scout at every=96 through every tick up to the next
// reconcile, leaving the reconcile itself to the caller.
func runToReconcile(ctx context.Context, t *testing.T, s *Scout, recorder *capture.Recorder) {
	t.Helper()
	cycleLines(ctx, t, s, recorder)
	for range 95 {
		cycleLines(ctx, t, s, recorder)
	}
	if lines := conditionLines(recorder, 0); len(lines) == 0 || lines[len(lines)-1].condition != "seadex-unreachable" {
		t.Fatalf("setup emitted %+v, want the fast path's unreachability escalated", lines)
	}
}

// TestSeaDexUnreachableStaysStandingAcrossTheReconcile pins that the reconcile,
// which runs no probe, re-states the fast path's unreachability while its own
// catalogue fetch fails too: otherwise the alert would see a ~20-minute hole at
// every daily pass of a long outage.
func TestSeaDexUnreachableStaysStandingAcrossTheReconcile(t *testing.T) {
	logger, recorder := capture.New()
	s, sea := failingProbeScout(logger)
	ctx := t.Context()
	runToReconcile(ctx, t, s, recorder)

	sea.err = errors.New("releases.moe unreachable")
	lines := cycleLines(ctx, t, s, recorder)
	want := conditionLine{msg: msgStillStandingContract, condition: "seadex-unreachable"}
	if len(lines) != 1 || lines[0] != want {
		t.Errorf("reconcile during the outage emitted %+v, want exactly %+v", lines, want)
	}
}

// TestSuccessfulCatalogueFetchClearsSeaDexUnreachable pins that a reconcile which
// READ SeaDex ends the fast path's unreachability: a successful catalogue fetch is
// a successful read, so a probe failing afterwards starts a fresh streak at WARN
// instead of re-escalating a condition the reconcile disproved.
func TestSuccessfulCatalogueFetchClearsSeaDexUnreachable(t *testing.T) {
	logger, recorder := capture.New()
	s, _ := failingProbeScout(logger)
	ctx := t.Context()
	runToReconcile(ctx, t, s, recorder)

	if lines := cycleLines(ctx, t, s, recorder); len(lines) != 0 {
		t.Errorf("reconcile that read SeaDex emitted %+v, want nothing", lines)
	}
	if s.unreachableRun != 0 {
		t.Errorf("unreachableRun after a successful catalogue fetch = %d, want 0", s.unreachableRun)
	}
	if lines := cycleLines(ctx, t, s, recorder); len(lines) != 0 {
		t.Errorf("first failing tick after the reconcile emitted %+v, want nothing (a fresh streak starts at WARN)", lines)
	}
}

// TestShutdownInterruptedPassRestatesNothing pins that a pass a shutdown cut
// short emits no re-statement: it completed nothing, and the next process states
// the persisted set itself.
func TestShutdownInterruptedPassRestatesNothing(t *testing.T) {
	logger, recorder := capture.New()
	s, _ := failingProbeScout(logger)
	for range oversizeLatchAtDefaultCadence + 1 {
		cycleLines(t.Context(), t, s, recorder)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if lines := cycleLines(ctx, t, s, recorder); len(lines) != 0 {
		t.Errorf("shutdown-interrupted tick emitted %+v, want nothing", lines)
	}
}

// staleRejectingMapping returns a stale-but-usable map whose persisted
// refresh-rejection streak is rejections, or accepts the refresh (resetting the
// streak) when rejections is zero.
type staleRejectingMapping struct{ rejections int }

func (m *staleRejectingMapping) Load(_ context.Context, prev *mapping.Cache) (mapping.Cache, *mapping.Index, error) {
	c := *prev
	c.RejectedRefreshes = m.rejections
	if m.rejections == 0 {
		return c, mapping.NewIndex(mapping.Source{Records: c.Records}), nil
	}
	return c, mapping.NewIndex(mapping.Source{Records: c.Records}), &mapping.StaleMapError{}
}

// mappingRejectionScout is a Scout reconciling every second iteration over m,
// with an empty tick probe, so ticks load no mapping at all.
func mappingRejectionScout(logger *slog.Logger, m *staleRejectingMapping) *Scout {
	deps, _ := tickDeps(logger, &fakeSeaDex{entries: seadexFrierenEntry(), countFn: emptyProbe}, nil, nil, 2)
	deps.Mapping = m
	return New(deps)
}

// TestMappingRefreshRejectionStandsUntilARefreshIsAccepted pins the mapping
// condition's lifecycle: a probe-only tick loads no mapping and re-states it, and
// an accepted refresh clears it.
func TestMappingRefreshRejectionStandsUntilARefreshIsAccepted(t *testing.T) {
	logger, recorder := capture.New()
	m := &staleRejectingMapping{rejections: 8}
	s := mappingRejectionScout(logger, m)
	ctx := t.Context()

	escalation := cycleLines(ctx, t, s, recorder)
	if len(escalation) != 1 || escalation[0].condition != "mapping-refresh-rejected" || escalation[0].msg == msgStillStandingContract {
		t.Fatalf("rejecting reconcile emitted %+v, want exactly the escalated mapping ERROR with condition mapping-refresh-rejected", escalation)
	}
	want := conditionLine{msg: msgStillStandingContract, condition: "mapping-refresh-rejected"}
	if lines := cycleLines(ctx, t, s, recorder); len(lines) != 1 || lines[0] != want {
		t.Errorf("probe-only tick emitted %+v, want exactly %+v", lines, want)
	}

	m.rejections = 0
	if lines := cycleLines(ctx, t, s, recorder); len(lines) != 0 {
		t.Errorf("reconcile with an accepted refresh emitted %+v, want nothing", lines)
	}
	if lines := cycleLines(ctx, t, s, recorder); len(lines) != 0 {
		t.Errorf("tick after the accepted refresh emitted %+v, want nothing", lines)
	}
}

// TestStandingConditionFollowsTheStreakItObserves pins that a condition stands
// exactly while its observer's streak is at the threshold: a streak found back
// below it (state.json replaced, or written by a separate poll process) is a
// WARN, and the condition stops being re-stated with it.
func TestStandingConditionFollowsTheStreakItObserves(t *testing.T) {
	t.Run("mapping", func(t *testing.T) {
		logger, recorder := capture.New()
		m := &staleRejectingMapping{rejections: 8}
		s := mappingRejectionScout(logger, m)
		ctx := t.Context()
		cycleLines(ctx, t, s, recorder)
		cycleLines(ctx, t, s, recorder)
		if lines := conditionLines(recorder, 0); len(lines) != 2 {
			t.Fatalf("setup emitted %+v, want the escalation and one re-statement", lines)
		}
		m.rejections = 1
		if lines := cycleLines(ctx, t, s, recorder); len(lines) != 0 {
			t.Errorf("reconcile with the streak below the threshold emitted %+v, want nothing", lines)
		}
		if lines := cycleLines(ctx, t, s, recorder); len(lines) != 0 {
			t.Errorf("tick after it emitted %+v, want nothing", lines)
		}
	})
	t.Run("seadex catalogue", func(t *testing.T) {
		logger, recorder := capture.New()
		sea := &fakeSeaDex{entries: seadexFrierenEntry(), countFn: emptyProbe}
		s, store := newTickScout(logger, sea, nil, nil, 2)
		ctx := t.Context()
		cycleLines(ctx, t, s, recorder)
		cycleLines(ctx, t, s, recorder)
		sea.err = errors.New("releases.moe unreachable")
		store.st.SeadexFailures = degradation.ReconcileEscalationThreshold - 1
		cycleLines(ctx, t, s, recorder)
		cycleLines(ctx, t, s, recorder)
		if lines := conditionLines(recorder, 0); len(lines) != 2 || lines[1].condition != "seadex-catalogue-fetch-failing" {
			t.Fatalf("setup emitted %+v, want the catalogue escalation and one re-statement", lines)
		}
		store.st.SeadexFailures = 0
		if lines := cycleLines(ctx, t, s, recorder); len(lines) != 0 {
			t.Errorf("failing reconcile with the streak restarted emitted %+v, want nothing (it is a WARN)", lines)
		}
		if lines := cycleLines(ctx, t, s, recorder); len(lines) != 0 {
			t.Errorf("tick after it emitted %+v, want nothing", lines)
		}
	})
}

// TestCatalogueConditionClearsOnASuccessfulFetch pins the catalogue condition's
// resolve: the first reconcile that downloads the catalogue ends it.
func TestCatalogueConditionClearsOnASuccessfulFetch(t *testing.T) {
	logger, recorder := capture.New()
	sea := &fakeSeaDex{entries: seadexFrierenEntry(), countFn: emptyProbe}
	s, store := newTickScout(logger, sea, nil, nil, 2)
	ctx := t.Context()
	cycleLines(ctx, t, s, recorder)
	cycleLines(ctx, t, s, recorder)
	sea.err = errors.New("releases.moe unreachable")
	store.st.SeadexFailures = degradation.ReconcileEscalationThreshold - 1
	cycleLines(ctx, t, s, recorder)
	cycleLines(ctx, t, s, recorder)
	if lines := conditionLines(recorder, 0); len(lines) != 2 || lines[1].condition != "seadex-catalogue-fetch-failing" {
		t.Fatalf("setup emitted %+v, want the catalogue escalation and one re-statement", lines)
	}

	sea.err = nil
	if lines := cycleLines(ctx, t, s, recorder); len(lines) != 0 {
		t.Errorf("reconcile that downloaded the catalogue emitted %+v, want nothing", lines)
	}
	if lines := cycleLines(ctx, t, s, recorder); len(lines) != 0 {
		t.Errorf("tick after it emitted %+v, want nothing", lines)
	}
}

// switchableAniList answers like degradedMatcherAniList while degraded is set and
// like notFoundAniList once it is cleared.
type switchableAniList struct{ degraded bool }

func (a *switchableAniList) Fetch(ctx context.Context, id int) (anilist.Media, error) {
	if a.degraded {
		return degradedMatcherAniList{}.Fetch(ctx, id)
	}
	return notFoundAniList{}.Fetch(ctx, id)
}

func (a *switchableAniList) FetchMany(ctx context.Context, ids []int) (anilist.BatchResult, error) {
	if a.degraded {
		return degradedMatcherAniList{}.FetchMany(ctx, ids)
	}
	return notFoundAniList{}.FetchMany(ctx, ids)
}

// TestAniListConditionClearsOnAnUndegradedReconcile pins the AniList condition's
// resolve: a reconcile whose lookups all answer ends it.
func TestAniListConditionClearsOnAnUndegradedReconcile(t *testing.T) {
	logger, recorder := capture.New()
	ani := &switchableAniList{degraded: true}
	sonarr := &fakeSonarr{
		series: []arrapi.Series{{ID: 8, Title: "Idless Show", TvdbID: 124, Year: 2024}},
		files:  map[int][]arrapi.EpisodeFile{8: {{SeasonNumber: 1, ReleaseGroup: "Erai-raws"}}},
	}
	s := New(&Deps{
		Logger: logger,
		Store: &fakeStore{st: state.State{
			Mapping: mapping.Cache{FetchedAt: time.Now(), Records: []mapping.Record{{AniListID: 222, Type: "TV"}}},
		}},
		Library:      arrwalk.NewWalker(&arrwalk.Config{Sonarr: sonarr, Logger: scoutTestLogger()}),
		Mapping:      fakeMapping{},
		SeaDex:       &fakeSeaDex{entries: []seadex.Entry{secondSeaDexEntry()}, countFn: emptyProbe},
		Matcher:      match.New(ani, scoutTestLogger()),
		Comparer:     compare.New(compare.Config{}),
		Notifier:     notify.NewNotifier(scoutTestLogger(), nil),
		PollInterval: pollIntervalForEvery(2),
	})
	ctx := t.Context()
	for range 4 {
		cycleLines(ctx, t, s, recorder)
	}
	lines := conditionLines(recorder, 0)
	if len(lines) != 2 || lines[0].condition != "anilist-lookups-failing" || lines[1].msg != msgStillStandingContract {
		t.Fatalf("setup emitted %+v, want the AniList escalation then one re-statement", lines)
	}

	ani.degraded = false
	if lines := cycleLines(ctx, t, s, recorder); len(lines) != 0 {
		t.Errorf("undegraded reconcile emitted %+v, want nothing", lines)
	}
	if lines := cycleLines(ctx, t, s, recorder); len(lines) != 0 {
		t.Errorf("tick after it emitted %+v, want nothing", lines)
	}
}

// TestOversizeConditionClearsOnAnEmptyWindow pins the oversize condition's
// resolve: the first tick whose window is small enough ends it.
func TestOversizeConditionClearsOnAnEmptyWindow(t *testing.T) {
	logger, recorder := capture.New()
	count := seadexapi.MaxWindowEntries
	sea := &fakeSeaDex{entries: seadexFrierenEntry(), countFn: func(context.Context, time.Time) (int, error) {
		return count, nil
	}}
	s, _ := newTickScout(logger, sea, nil, nil, 96)
	ctx := t.Context()
	for range oversizeLatchAtDefaultCadence + 1 {
		cycleLines(ctx, t, s, recorder)
	}
	if lines := conditionLines(recorder, 0); len(lines) != 1 || lines[0].condition != "seadex-window-oversize" {
		t.Fatalf("setup emitted %+v, want the oversize escalation", lines)
	}

	count = 0
	if lines := cycleLines(ctx, t, s, recorder); len(lines) != 0 {
		t.Errorf("empty-window tick emitted %+v, want nothing", lines)
	}
}

// restartedScout builds a second Scout over deps, the way a new process starts:
// the persisted store carries over and the in-memory finding set does not.
func restartedScout(deps *Deps) *Scout {
	deps.Notifier = notify.NewNotifier(scoutTestLogger(), nil)
	return New(deps)
}

// namesCondition reports whether lines carry cond under any message.
func namesCondition(lines []conditionLine, cond string) bool {
	for _, l := range lines {
		if l.condition == cond {
			return true
		}
	}
	return false
}

// TestSeaDexUnreachableSurvivesARestart pins that a new process keeps stating an
// outage the old one escalated, on every pass until a reconcile reads SeaDex.
// Without it a restart during an outage resolves the alert and re-fires it a
// streak later. The recovery is persisted too, so no later process resurrects it.
func TestSeaDexUnreachableSurvivesARestart(t *testing.T) {
	logger, recorder := capture.New()
	deps, store := tickDeps(logger, &fakeSeaDex{entries: seadexFrierenEntry()}, nil, nil, 96)
	sea := deps.SeaDex.(*fakeSeaDex)
	sea.countFn = func(context.Context, time.Time) (int, error) {
		return 0, errors.New("releases.moe unreachable")
	}
	ctx := t.Context()
	old := New(deps)
	for range oversizeLatchAtDefaultCadence + 1 {
		cycleLines(ctx, t, old, recorder)
	}
	want := state.StandingCondition{Condition: degradation.SeaDexUnreachable}
	if got := store.st.Standing; len(got) != 1 || got[0] != want {
		t.Fatalf("persisted standing after the escalation = %+v, want [%+v]", got, want)
	}

	restarted := restartedScout(deps)
	sea.err = errors.New("releases.moe unreachable")
	restated := conditionLine{msg: msgStillStandingContract, condition: "seadex-unreachable"}
	if lines := cycleLines(ctx, t, restarted, recorder); len(lines) != 1 || lines[0] != restated {
		t.Errorf("first pass of the new process emitted %+v, want exactly %+v", lines, restated)
	}
	if lines := cycleLines(ctx, t, restarted, recorder); !namesCondition(lines, "seadex-unreachable") {
		t.Errorf("second failing pass of the new process emitted %+v, want seadex-unreachable still named", lines)
	}

	sea.err = nil
	if lines := cycleLines(ctx, t, restarted, recorder); len(lines) != 0 {
		t.Errorf("reconcile that read SeaDex emitted %+v, want nothing", lines)
	}
	if got := store.st.Standing; len(got) != 0 {
		t.Errorf("persisted standing after the recovery = %+v, want none", got)
	}
}

// TestRestoredSeaDexUnreachableClearsWhenAProbeAnswers pins that a restored
// outage clears once SeaDex answers, even while no reconcile has succeeded:
// after the startup retries are spent, the next reconcile is up to a day away,
// and the alert would keep saying SeaDex is down for all of it. The catalogue
// condition, which a reconcile alone can clear, stays standing.
func TestRestoredSeaDexUnreachableClearsWhenAProbeAnswers(t *testing.T) {
	logger, recorder := capture.New()
	probeErr := errors.New("releases.moe unreachable")
	deps, store := tickDeps(logger, &fakeSeaDex{entries: seadexFrierenEntry()}, nil, nil, 96)
	sea := deps.SeaDex.(*fakeSeaDex)
	sea.countFn = func(context.Context, time.Time) (int, error) { return 0, probeErr }
	ctx := t.Context()
	old := New(deps)
	for range oversizeLatchAtDefaultCadence + 1 {
		cycleLines(ctx, t, old, recorder)
	}

	restarted := restartedScout(deps)
	sea.err = probeErr
	for range reconcileRetryLatch + 1 {
		cycleLines(ctx, t, restarted, recorder)
	}
	if restarted.ready {
		t.Fatalf("setup: the new process became ready, want every reconcile to have failed")
	}

	sea.countFn = emptyProbe
	lines := cycleLines(ctx, t, restarted, recorder)
	if namesCondition(lines, "seadex-unreachable") {
		t.Errorf("not-ready tick whose probe answered emitted %+v, want seadex-unreachable cleared", lines)
	}
	if !namesCondition(lines, "seadex-catalogue-fetch-failing") {
		t.Errorf("not-ready tick whose probe answered emitted %+v, want seadex-catalogue-fetch-failing still named", lines)
	}
	want := []state.StandingCondition{{Condition: degradation.SeaDexCatalogueFetchFailing}}
	if got := store.st.Standing; !slices.Equal(got, want) {
		t.Errorf("persisted standing after the probe answered = %+v, want %+v", got, want)
	}
}

// TestRestoredOversizeSettlesOnTheNotReadyProbe pins the oversize half of the
// not-ready probe: a window that fits again clears the restored condition, while
// a window still too large or a probe that fails leaves it standing.
func TestRestoredOversizeSettlesOnTheNotReadyProbe(t *testing.T) {
	tests := []struct {
		name     string
		probe    func(context.Context, time.Time) (int, error)
		standing bool
	}{
		{name: "window fits", probe: emptyProbe},
		{name: "window still too large", standing: true, probe: func(context.Context, time.Time) (int, error) {
			return seadexapi.MaxWindowEntries, nil
		}},
		{name: "probe fails", standing: true, probe: func(context.Context, time.Time) (int, error) {
			return 0, errors.New("releases.moe unreachable")
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logger, recorder := capture.New()
			sea := &fakeSeaDex{entries: seadexFrierenEntry(), err: errors.New("releases.moe unreachable"), countFn: tc.probe}
			deps, store := tickDeps(logger, sea, nil, nil, 96)
			store.st.Standing = []state.StandingCondition{{Condition: degradation.SeaDexWindowOversize}}
			s := New(deps)
			ctx := t.Context()
			for range reconcileRetryLatch {
				cycleLines(ctx, t, s, recorder)
			}
			lines := cycleLines(ctx, t, s, recorder)
			if s.ready {
				t.Fatalf("setup: the process became ready, want every reconcile to have failed")
			}
			if got := namesCondition(lines, "seadex-window-oversize"); got != tc.standing {
				t.Errorf("not-ready tick emitted %+v, seadex-window-oversize named = %v, want %v", lines, got, tc.standing)
			}
		})
	}
}

// TestOversizeSurvivesARestartAtItsStreak pins that a restored oversize
// condition resumes its streak: the new process's first oversize tick
// re-escalates instead of logging a WARN, which would clear the condition and
// resolve the alert while the window is still too large.
func TestOversizeSurvivesARestartAtItsStreak(t *testing.T) {
	logger, recorder := capture.New()
	count := seadexapi.MaxWindowEntries
	sea := &fakeSeaDex{entries: seadexFrierenEntry(), countFn: func(context.Context, time.Time) (int, error) {
		return count, nil
	}}
	deps, store := tickDeps(logger, sea, nil, nil, 96)
	ctx := t.Context()
	old := New(deps)
	for range oversizeLatchAtDefaultCadence + 1 {
		cycleLines(ctx, t, old, recorder)
	}

	restarted := restartedScout(deps)
	restated := conditionLine{msg: msgStillStandingContract, condition: "seadex-window-oversize"}
	if lines := cycleLines(ctx, t, restarted, recorder); len(lines) != 1 || lines[0] != restated {
		t.Errorf("first pass of the new process emitted %+v, want exactly %+v", lines, restated)
	}
	lines := cycleLines(ctx, t, restarted, recorder)
	if len(lines) != 1 || lines[0].condition != "seadex-window-oversize" || lines[0].msg == msgStillStandingContract {
		t.Errorf("first oversize tick of the new process emitted %+v, want the escalated seadex-window-oversize ERROR", lines)
	}

	count = 0
	if lines := cycleLines(ctx, t, restarted, recorder); len(lines) != 0 {
		t.Errorf("empty-window tick emitted %+v, want nothing", lines)
	}
	if got := store.st.Standing; len(got) != 0 {
		t.Errorf("persisted standing after the recovery = %+v, want none", got)
	}
}

// TestRestoredStandingKeepsOnlyConditionsThisProcessObserves pins the restore
// filter: a persisted condition this process runs no observer for would be
// re-stated forever, so it is dropped, while one it does observe is stated at
// once.
func TestRestoredStandingKeepsOnlyConditionsThisProcessObserves(t *testing.T) {
	tests := []struct {
		name      string
		external  bool
		persisted state.StandingCondition
		want      []conditionLine
	}{
		{
			name:      "tick condition in a daemon",
			persisted: state.StandingCondition{Condition: degradation.SeaDexUnreachable},
			want:      []conditionLine{{msg: msgStillStandingContract, condition: "seadex-unreachable"}},
		},
		{
			name:      "tick condition in external mode",
			external:  true,
			persisted: state.StandingCondition{Condition: degradation.SeaDexUnreachable},
		},
		{
			name:      "shrink on an enabled arr",
			persisted: state.StandingCondition{Condition: degradation.LibraryWalkShrunk, Arr: library.ArrSonarr},
			want:      []conditionLine{{msg: msgStillStandingContract, condition: "library-walk-shrunk", arr: library.ArrSonarr}},
		},
		{
			name:      "shrink on an arr no longer enabled",
			persisted: state.StandingCondition{Condition: degradation.LibraryWalkShrunk, Arr: library.ArrRadarr},
		},
		{
			name:      "condition this build does not know",
			persisted: state.StandingCondition{Condition: "retired-condition"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logger, recorder := capture.New()
			sea := &fakeSeaDex{entries: seadexFrierenEntry(), countFn: emptyProbe, err: errors.New("releases.moe unreachable")}
			deps, store := tickDeps(logger, sea, nil, nil, 96)
			if tc.external {
				deps.PollInterval = 0
			}
			store.st.Standing = []state.StandingCondition{tc.persisted}
			lines := cycleLines(t.Context(), t, New(deps), recorder)
			if len(lines) != len(tc.want) || (len(lines) == 1 && lines[0] != tc.want[0]) {
				t.Errorf("first pass with %+v persisted emitted %+v, want %+v", tc.persisted, lines, tc.want)
			}
		})
	}
}
