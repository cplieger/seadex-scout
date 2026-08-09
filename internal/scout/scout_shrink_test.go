package scout

import (
	"context"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/cplieger/arrapi"
	"github.com/cplieger/seadex-scout/internal/anilist"
	"github.com/cplieger/seadex-scout/internal/arrwalk"
	"github.com/cplieger/seadex-scout/internal/compare"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/match"
	"github.com/cplieger/seadex-scout/internal/notify"
	"github.com/cplieger/seadex-scout/internal/seadex"
	"github.com/cplieger/seadex-scout/internal/state"
	"github.com/cplieger/slogx/capture"
)

// --- the two-arr fixture the per-arr shrink guard is pinned on ---

// shrinkMappingCache is the ID bridge for the shrink fixture: the Frieren TV
// record (Sonarr side) plus a MOVIE record carrying its TMDB id (Radarr side),
// so each arr owns one comparable, curated item.
func shrinkMappingCache() mapping.Cache {
	return mapping.Cache{FetchedAt: time.Now(), Records: []mapping.Record{
		{AniListID: 154587, Type: "TV", TvdbID: 123, SeasonTvdb: 1},
		{AniListID: 500, Type: "MOVIE", TmdbMovies: []int{900}},
	}}
}

// shrinkSeaDexEntries curates a better release for one item on EACH arr, so a
// cycle that compares both sides emits exactly two findings and a side whose
// findings resolved is visible as its row disappearing.
func shrinkSeaDexEntries() []seadex.Entry {
	return append(seadexFrierenEntry(), seadex.Entry{
		AniListID: 500,
		Torrents: []seadex.Torrent{{
			ReleaseGroup: "SubsPlease",
			Tracker:      "Nyaa",
			InfoHash:     "fed",
			URL:          "https://nyaa.si/view/3",
			IsBest:       true,
			Files:        []seadex.File{{Name: "A Silent Voice 2016 1080p.mkv", Length: 1}},
		}},
	})
}

// twoArrShrinkFixture is the two-arr deployment the per-arr shrink guard's
// tests run on: Sonarr lists four series and Radarr two movies, one curated on
// each side, so both arrs carry a live finding and each side's item count moves
// independently.
//
// The counts are chosen so the AGGREGATE comparison this guard replaced stays
// SILENT on the regression it exists for: prior 4+2 = 6 items, and a walk where
// Radarr answers ZERO while Sonarr grows to five is 5 items - above half of 6 -
// so the whole-library test never fired while an entire arr had vanished, the
// compare ran against a library missing that side, and every finding for it
// silently resolved.
type twoArrShrinkFixture struct {
	sonarr *fakeSonarr
	radarr *fakeRadarr
	store  *fakeStore
}

func newTwoArrShrinkFixture() *twoArrShrinkFixture {
	return &twoArrShrinkFixture{
		sonarr: &fakeSonarr{
			series: []arrapi.Series{
				{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023},
				{ID: 8, Title: "Second Show", TvdbID: 124, Year: 2024},
				{ID: 9, Title: "Third Show", TvdbID: 125, Year: 2024},
				{ID: 10, Title: "Fourth Show", TvdbID: 126, Year: 2024},
			},
			files: map[int][]arrapi.EpisodeFile{
				7:  {{SeasonNumber: 1, ReleaseGroup: "Erai-raws"}},
				8:  {{SeasonNumber: 1, ReleaseGroup: "Erai-raws"}},
				9:  {{SeasonNumber: 1, ReleaseGroup: "Erai-raws"}},
				10: {{SeasonNumber: 1, ReleaseGroup: "Erai-raws"}},
			},
		},
		radarr: &fakeRadarr{movies: []arrapi.Movie{
			{
				ID: 11, Title: "A Silent Voice", TmdbID: 900, Year: 2016, HasFile: true,
				MovieFile: &arrapi.MovieFile{
					RelativePath: "A Silent Voice 2016 1080p BluRay-LostYears.mkv",
					ReleaseGroup: "LostYears",
				},
			},
			{ID: 12, Title: "Your Name", TmdbID: 901, Year: 2016},
		}},
		store: &fakeStore{st: state.State{Mapping: shrinkMappingCache()}},
	}
}

// scout builds a reconciling Scout over the fixture's arrs and shared store. A
// fresh Scout per cycle keeps the iteration counter at zero, so every Cycle is
// a reconcile (the only pass that walks the arrs), and lets each cycle's log
// output be captured on its own recorder while the persisted state carries
// across through the shared store.
func (f *twoArrShrinkFixture) scout(logger *slog.Logger) *Scout {
	return New(&Deps{
		Logger:       logger,
		Store:        f.store,
		Library:      arrwalk.NewWalker(&arrwalk.Config{Sonarr: f.sonarr, Radarr: f.radarr, Logger: scoutTestLogger()}),
		Mapping:      fakeMapping{},
		SeaDex:       &fakeSeaDex{entries: shrinkSeaDexEntries()},
		Matcher:      match.NewMatcher(notFoundAniList{}, scoutTestLogger()),
		Comparer:     compare.NewComparer(compare.Config{}),
		Notifier:     notify.NewNotifier(logger, nil),
		AniListStats: aniStatsFn(anilist.NewClient(noNetworkClient(), "http://unused.invalid/gql", 1, scoutTestLogger())),
	})
}

// seed runs one healthy cycle so the persisted snapshot becomes the baseline
// every later shrink comparison is made against, asserting the per-arr counts it
// established (wantRadarr is 0 for the fixture whose Radarr is deliberately
// empty from the start). A baseline that never landed would make every later
// "the side was carried" assertion vacuous.
func (f *twoArrShrinkFixture) seed(t *testing.T, wantRadarr int) {
	t.Helper()
	if healthy := f.scout(scoutTestLogger()).Cycle(context.Background()); !healthy {
		t.Fatal("seeding Cycle healthy=false, want true")
	}
	if got := countItemsByArr(f.store.st.Library.Items); got[library.ArrSonarr] != 4 || got[library.ArrRadarr] != wantRadarr {
		t.Fatalf("seeded per-arr counts = %v, want 4 sonarr and %d radarr", got, wantRadarr)
	}
}

// emptyRadarr is the regression's shape: Radarr's list call SUCCEEDS and
// returns nothing (a database restored empty, a container recreated without its
// volume, a url repointed at a fresh instance) while Sonarr GROWS, so the
// library total stays above half and only the per-arr view can see it.
func (f *twoArrShrinkFixture) emptyRadarr() {
	f.radarr.movies = nil
	f.sonarr.series = append(f.sonarr.series,
		arrapi.Series{ID: 13, Title: "Fifth Show", TvdbID: 127, Year: 2025})
	f.sonarr.files[13] = []arrapi.EpisodeFile{{SeasonNumber: 1, ReleaseGroup: "Erai-raws"}}
}

// findingArrs returns the arr of every finding row a cycle emitted. Findings
// are STATE - a row present in a pass is emitted and a row absent from an
// authoritative pass resolves - so this IS the observable "did that side's
// findings survive" set.
func findingArrs(recorder *capture.Recorder) []string {
	return recorder.AttrValuesExact("better release available", "arr")
}

// --- the regression this guard exists for ---

// TestCycleOneArrEmptiedKeepsThatSidesFindings is the headline: when ONE arr
// successfully returns zero items while the other keeps the library total above
// half, the emptied side must be judged suspect on its OWN prior count, its
// prior items carried into the merged snapshot, the comparison must still RUN,
// and that side's findings must NOT resolve.
//
// Before the guard was per-arr this was silent: the aggregate item comparison
// never fired (5 fresh items against a prior 6 is above half), the compare ran
// at full authority against a library missing an entire arr, and every finding
// for that arr resolved with only the walker's empty-list WARN to show for it.
func TestCycleOneArrEmptiedKeepsThatSidesFindings(t *testing.T) {
	f := newTwoArrShrinkFixture()
	f.seed(t, 2)
	f.emptyRadarr()

	logger, recorder := capture.New()
	if healthy := f.scout(logger).Cycle(context.Background()); !healthy {
		t.Fatal("Cycle healthy=false, want true (a shrunken side is degraded, not an ingest failure)")
	}

	// The compare RAN and the emptied side's finding is still stated: the
	// suspect side's rows are recomputed from its carried prior items, which is
	// what "carried forward" means once the snapshot is complete again.
	arrs := findingArrs(recorder)
	if !slices.Contains(arrs, library.ArrRadarr) {
		t.Errorf("finding rows by arr = %v, want the emptied radarr side's finding still stated (this is the regression: it used to resolve silently)", arrs)
	}
	if !slices.Contains(arrs, library.ArrSonarr) {
		t.Errorf("finding rows by arr = %v, want the healthy sonarr side's finding stated too (the compare must run, not be skipped)", arrs)
	}

	// The persisted snapshot is the MERGE: the suspect side's prior items plus
	// the healthy side's fresh ones. Keeping the suspect side's prior COUNT is
	// what stops the one-cycle ratchet.
	counts := countItemsByArr(f.store.st.Library.Items)
	if counts[library.ArrRadarr] != 2 {
		t.Errorf("persisted radarr items = %d, want the prior 2 carried forward", counts[library.ArrRadarr])
	}
	if counts[library.ArrSonarr] != 5 {
		t.Errorf("persisted sonarr items = %d, want the healthy side's fresh 5 (it must keep updating while the other is withheld)", counts[library.ArrSonarr])
	}

	// Only the emptied side carries a streak.
	if got := f.store.st.ShrunkWalksByArr; len(got) != 1 || got[library.ArrRadarr] != 1 {
		t.Errorf("persisted ShrunkWalksByArr = %v, want only {radarr: 1}", got)
	}

	// One completion line, degraded, naming the withheld side.
	if n := recorder.CountExact("cycle degraded"); n != 1 {
		t.Errorf("'cycle degraded' count = %d, want exactly 1 completion line", n)
	}
	if n := recorder.CountExact("cycle complete"); n != 0 {
		t.Errorf("'cycle complete' count = %d, want 0 (a withheld arr must not read as fully successful)", n)
	}
	if reasons := degradedReasons(recorder); len(reasons) != 1 || reasons[0] != "library-shrunk" {
		t.Errorf("degraded reasons = %v, want [library-shrunk]", reasons)
	}
	if got, ok := recorder.AttrValueExact("cycle degraded", "shrunk_arrs"); !ok || got != library.ArrRadarr {
		t.Errorf("'cycle degraded' shrunk_arrs = %q (found=%t), want %q", got, ok, library.ArrRadarr)
	}
}

// TestCycleShrunkSideDoesNotRatchetPriorCount pins the reason the guard MERGES
// instead of refusing to persist: because the suspect side's PRIOR items are
// what gets persisted, its prior count stays high, so the shrink test fires
// again on the next cycle and the streak advances. Persisting the shrunken side
// would make the guard a one-cycle ratchet - next cycle the baseline is the
// small one, the test passes, and the mass-resolve happens anyway.
func TestCycleShrunkSideDoesNotRatchetPriorCount(t *testing.T) {
	f := newTwoArrShrinkFixture()
	f.seed(t, 2)
	f.emptyRadarr()

	firstLogger, first := capture.New()
	f.scout(firstLogger).Cycle(context.Background())
	secondLogger, second := capture.New()
	f.scout(secondLogger).Cycle(context.Background())

	for i, recorder := range []*capture.Recorder{first, second} {
		if got, ok := recorder.AttrValue("library walk shrank", "prior_items"); !ok || got != "2" {
			t.Errorf("cycle %d prior_items = %q (found=%t), want 2 on BOTH cycles (a ratcheted baseline would report 0)", i+1, got, ok)
		}
	}
	if got, ok := second.AttrValue("library walk shrank", "consecutive_shrunk_walks"); !ok || got != "2" {
		t.Errorf("second cycle consecutive_shrunk_walks = %q (found=%t), want 2 (the streak must advance, not restart)", got, ok)
	}
	if got := f.store.st.ShrunkWalksByArr[library.ArrRadarr]; got != 2 {
		t.Errorf("persisted radarr streak = %d, want 2 after two consecutive shrunken cycles", got)
	}
	if counts := countItemsByArr(f.store.st.Library.Items); counts[library.ArrRadarr] != 2 {
		t.Errorf("persisted radarr items = %d, want the prior 2 still carried on the second cycle", counts[library.ArrRadarr])
	}
	if !slices.Contains(findingArrs(second), library.ArrRadarr) {
		t.Error("second cycle dropped the withheld side's finding, want it still stated")
	}
}

// --- the streak: escalation, then bounded acceptance ---

// TestCycleShrunkSideEscalatesThenAcceptsAtThreshold pins the whole ladder of
// the single shrink log site, per arr: below shrunkWalkEscalationThreshold a
// shrunken side WARNs, at that threshold the SAME site logs ERROR (firing the
// SeadexScoutCycleError Loki rule) while still withholding the side, and at
// shrunkWalkAcceptThreshold the guard ACCEPTS the smaller library - one loud
// WARN, not an ERROR, because acceptance is a designed outcome rather than a
// condition needing an operator.
//
// Acceptance is the case worth reading closely: the fresh (empty) side is
// persisted, its streak entry is deleted, the compare resolves its stale
// findings normally, and the cycle closes CLEAN. What makes that acceptable is
// that it is deliberate, logged, and time-bounded - a silent mass-resolve after
// six days is exactly what the guard exists to prevent.
func TestCycleShrunkSideEscalatesThenAcceptsAtThreshold(t *testing.T) {
	tests := map[string]struct {
		priorStreak  int
		wantLevel    slog.Level
		wantAccepted bool
	}{
		"below the escalation threshold WARNs and withholds": {
			priorStreak: shrunkWalkEscalationThreshold - 2,
			wantLevel:   slog.LevelWarn,
		},
		"at the escalation threshold ERRORs and still withholds": {
			priorStreak: shrunkWalkEscalationThreshold - 1,
			wantLevel:   slog.LevelError,
		},
		"one pass before the accept threshold still withholds": {
			priorStreak: shrunkWalkAcceptThreshold - 2,
			wantLevel:   slog.LevelError,
		},
		"at the accept threshold the smaller library is accepted": {
			priorStreak:  shrunkWalkAcceptThreshold - 1,
			wantLevel:    slog.LevelWarn,
			wantAccepted: true,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			f := newTwoArrShrinkFixture()
			f.seed(t, 2)
			f.emptyRadarr()
			f.store.st.ShrunkWalksByArr = map[string]int{library.ArrRadarr: tc.priorStreak}

			logger, recorder := capture.New()
			if healthy := f.scout(logger).Cycle(context.Background()); !healthy {
				t.Fatal("Cycle healthy=false, want true (the shrink guard never fails the ingest)")
			}

			// Exactly one shrink line, at the level the streak earned.
			if got := recorder.CountLevel(tc.wantLevel, "library walk"); got != 1 {
				t.Errorf("shrink lines at %v = %d, want exactly 1 (a single log site, escalating); messages = %v",
					tc.wantLevel, got, recorder.Messages())
			}
			if !recorder.HasAttr("library walk", "arr", library.ArrRadarr) {
				t.Error("shrink line does not name the arr, want arr=radarr on every arm")
			}
			for key, want := range map[string]string{"items": "0", "prior_items": "2"} {
				if got, ok := recorder.AttrValue("library walk", key); !ok || got != want {
					t.Errorf("shrink line %s = %q (found=%t), want %q (both counts belong on every arm)", key, got, ok, want)
				}
			}

			counts := countItemsByArr(f.store.st.Library.Items)
			if !tc.wantAccepted {
				if counts[library.ArrRadarr] != 2 {
					t.Errorf("persisted radarr items = %d, want the prior 2 still withheld below the accept threshold", counts[library.ArrRadarr])
				}
				if got := f.store.st.ShrunkWalksByArr[library.ArrRadarr]; got != tc.priorStreak+1 {
					t.Errorf("persisted radarr streak = %d, want %d", got, tc.priorStreak+1)
				}
				if reasons := degradedReasons(recorder); len(reasons) != 1 || reasons[0] != "library-shrunk" {
					t.Errorf("degraded reasons = %v, want [library-shrunk] while the side is withheld", reasons)
				}
				if !slices.Contains(findingArrs(recorder), library.ArrRadarr) {
					t.Error("withheld side's finding was dropped, want it still stated")
				}
				return
			}
			// Acceptance: the fresh shape stands, loudly and exactly once.
			if counts[library.ArrRadarr] != 0 {
				t.Errorf("persisted radarr items = %d, want 0 (the smaller library is now accepted)", counts[library.ArrRadarr])
			}
			if _, still := f.store.st.ShrunkWalksByArr[library.ArrRadarr]; still {
				t.Errorf("radarr streak = %v, want the entry deleted on acceptance", f.store.st.ShrunkWalksByArr)
			}
			if n := recorder.CountLevel(slog.LevelError, "library walk shrank"); n != 0 {
				t.Errorf("acceptance ERROR count = %d, want 0 (a designed outcome is a WARN under this app's self-heal-vs-operator rule)", n)
			}
			if !recorder.AttrContains("accepting the smaller library", "arr", library.ArrRadarr) {
				t.Error("acceptance line does not name the arr, want arr=radarr")
			}
			if got, ok := recorder.AttrValue("accepting the smaller library", "passes_before_accept"); !ok || got != "0" {
				t.Errorf("acceptance passes_before_accept = %q (found=%t), want 0", got, ok)
			}
			if slices.Contains(findingArrs(recorder), library.ArrRadarr) {
				t.Error("accepted side still stated a finding, want its stale findings resolved by the compare")
			}
			if n := recorder.CountExact("cycle complete"); n != 1 {
				t.Errorf("'cycle complete' count = %d, want 1 (nothing is withheld any more); reasons = %v", n, degradedReasons(recorder))
			}
		})
	}
}

// TestCycleRecoveredSideResetsOnlyItsOwnStreak pins the per-arr independence of
// the reset: the arr whose walk passes the guard ends ITS streak and nothing
// else, so one side recovering never clears the evidence standing against the
// other. The whole-library reset this replaced did exactly that.
func TestCycleRecoveredSideResetsOnlyItsOwnStreak(t *testing.T) {
	f := newTwoArrShrinkFixture()
	f.seed(t, 2)
	f.emptyRadarr()
	f.store.st.ShrunkWalksByArr = map[string]int{library.ArrSonarr: 3, library.ArrRadarr: 1}

	if healthy := f.scout(scoutTestLogger()).Cycle(context.Background()); !healthy {
		t.Fatal("Cycle healthy=false, want true")
	}
	got := f.store.st.ShrunkWalksByArr
	if _, still := got[library.ArrSonarr]; still {
		t.Errorf("ShrunkWalksByArr = %v, want the recovered sonarr entry deleted", got)
	}
	if got[library.ArrRadarr] != 2 {
		t.Errorf("radarr streak = %d, want 2 (the still-shrunken side's evidence must survive the other's recovery)", got[library.ArrRadarr])
	}
}

// TestCycleAlwaysEmptySideIsNeverSuspect pins the guard's lower bound: a side
// whose PRIOR count is zero has nothing to have shrunk from, so a legitimately
// empty arr - a Radarr the operator has enabled but not filled yet - must not
// be gated, must not accrue a streak, and must not degrade the cycle.
func TestCycleAlwaysEmptySideIsNeverSuspect(t *testing.T) {
	f := newTwoArrShrinkFixture()
	f.radarr.movies = nil
	f.seed(t, 0)

	logger, recorder := capture.New()
	if healthy := f.scout(logger).Cycle(context.Background()); !healthy {
		t.Fatal("Cycle healthy=false, want true")
	}
	if len(f.store.st.ShrunkWalksByArr) != 0 {
		t.Errorf("ShrunkWalksByArr = %v, want empty (an always-empty side never shrank)", f.store.st.ShrunkWalksByArr)
	}
	if n := recorder.Count("library walk shrank"); n != 0 {
		t.Errorf("shrink lines = %d, want 0 for a legitimately empty arr", n)
	}
	if n := recorder.CountExact("cycle complete"); n != 1 {
		t.Errorf("'cycle complete' count = %d, want 1; reasons = %v", n, degradedReasons(recorder))
	}
}

// TestCycleSingleArrDeploymentShrinksAsBefore pins that the per-arr guard is a
// strict generalization: in a one-arr deployment the arr's own count IS the
// library count, so a below-half walk withholds the whole snapshot, advances
// that arr's streak, and closes the cycle degraded with reason=library-shrunk -
// the behaviour the aggregate guard had, now expressed per arr.
func TestCycleSingleArrDeploymentShrinksAsBefore(t *testing.T) {
	sonarr := &fakeSonarr{
		series: []arrapi.Series{
			{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023},
			{ID: 8, Title: "Second Show", TvdbID: 124, Year: 2024},
			{ID: 9, Title: "Third Show", TvdbID: 125, Year: 2024},
			{ID: 10, Title: "Fourth Show", TvdbID: 126, Year: 2024},
		},
		files: map[int][]arrapi.EpisodeFile{
			7:  {{SeasonNumber: 1, ReleaseGroup: "Erai-raws"}},
			8:  {{SeasonNumber: 1, ReleaseGroup: "Erai-raws"}},
			9:  {{SeasonNumber: 1, ReleaseGroup: "Erai-raws"}},
			10: {{SeasonNumber: 1, ReleaseGroup: "Erai-raws"}},
		},
	}
	store := &fakeStore{st: state.State{Mapping: shrinkMappingCache()}}
	newScout := func(logger *slog.Logger) *Scout {
		return New(&Deps{
			Logger:       logger,
			Store:        store,
			Library:      arrwalk.NewWalker(&arrwalk.Config{Sonarr: sonarr, Logger: scoutTestLogger()}),
			Mapping:      fakeMapping{},
			SeaDex:       &fakeSeaDex{entries: shrinkSeaDexEntries()},
			Matcher:      match.NewMatcher(notFoundAniList{}, scoutTestLogger()),
			Comparer:     compare.NewComparer(compare.Config{}),
			Notifier:     notify.NewNotifier(logger, nil),
			AniListStats: aniStatsFn(anilist.NewClient(noNetworkClient(), "http://unused.invalid/gql", 1, scoutTestLogger())),
		})
	}
	if healthy := newScout(scoutTestLogger()).Cycle(context.Background()); !healthy {
		t.Fatal("seeding Cycle healthy=false, want true")
	}

	// One series left of four: 1*2 < 4 trips the guard.
	sonarr.series = sonarr.series[:1]
	logger, recorder := capture.New()
	if healthy := newScout(logger).Cycle(context.Background()); !healthy {
		t.Fatal("Cycle healthy=false, want true (a shrunken walk is degraded, not unhealthy)")
	}
	if counts := countItemsByArr(store.st.Library.Items); counts[library.ArrSonarr] != 4 {
		t.Errorf("persisted sonarr items = %d, want the prior 4 (the single side is withheld whole)", counts[library.ArrSonarr])
	}
	if got := store.st.ShrunkWalksByArr[library.ArrSonarr]; got != 1 {
		t.Errorf("sonarr streak = %d, want 1", got)
	}
	if reasons := degradedReasons(recorder); len(reasons) != 1 || reasons[0] != "library-shrunk" {
		t.Errorf("degraded reasons = %v, want [library-shrunk]", reasons)
	}
	if got, ok := recorder.AttrValueExact("cycle degraded", "shrunk_arrs"); !ok || got != library.ArrSonarr {
		t.Errorf("'cycle degraded' shrunk_arrs = %q (found=%t), want %q", got, ok, library.ArrSonarr)
	}
}

// TestMergeShrunkSidesDisabledArrIsNeverSuspect pins the enabled-sides rule at
// the unit boundary: an arr the deployment does NOT have wired contributes no
// items, and an arr the operator has just DISABLED legitimately loses all of
// them - neither may be read as a suspicious truncation, or disabling Radarr
// would withhold the whole library for six days.
func TestMergeShrunkSidesDisabledArrIsNeverSuspect(t *testing.T) {
	logger, recorder := capture.New()
	// Sonarr only: the prior snapshot still holds the disabled Radarr's items.
	s := New(&Deps{
		Logger:   logger,
		Store:    &fakeStore{},
		Library:  arrwalk.NewWalker(&arrwalk.Config{Sonarr: &fakeSonarr{}, Logger: scoutTestLogger()}),
		Notifier: notify.NewNotifier(logger, nil),
	})
	st := state.State{Library: library.Snapshot{Items: []library.Item{
		{Arr: library.ArrSonarr, ArrID: 7, Title: "Frieren"},
		{Arr: library.ArrRadarr, ArrID: 11, Title: "A Silent Voice"},
		{Arr: library.ArrRadarr, ArrID: 12, Title: "Your Name"},
	}}}
	snap := library.Snapshot{Items: []library.Item{{Arr: library.ArrSonarr, ArrID: 7, Title: "Frieren"}}}

	if suspect := s.mergeShrunkSides(&st, &snap); len(suspect) != 0 {
		t.Errorf("mergeShrunkSides = %v, want no suspect side (sonarr held; radarr is not enabled)", suspect)
	}
	if len(st.ShrunkWalksByArr) != 0 {
		t.Errorf("ShrunkWalksByArr = %v, want empty", st.ShrunkWalksByArr)
	}
	if len(snap.Items) != 1 {
		t.Errorf("snapshot items = %d, want 1 (a disabled arr's prior items are not carried back in)", len(snap.Items))
	}
	if n := recorder.Count("library walk shrank"); n != 0 {
		t.Errorf("shrink lines = %d, want 0", n)
	}
}

// TestCycleShrunkSidePersistsSeaDexStreakReset pins the reset arm of the
// documented SeadexFailures contract ("resets to 0 on any successful fetch")
// across a shrunken cycle: a shrink no longer stops the pass, so the reset
// rides the cycle's own closing save. Its predecessor pinned the same property
// on the arm that saved and returned early; the property outlives that arm,
// because a recovery during a persistent shrink must not leave a stale streak
// frozen in state.json and falsely escalate the first later blip to ERROR.
func TestCycleShrunkSidePersistsSeaDexStreakReset(t *testing.T) {
	f := newTwoArrShrinkFixture()
	f.seed(t, 2)
	f.emptyRadarr()
	f.store.st.SeadexFailures = 3

	if healthy := f.scout(scoutTestLogger()).Cycle(context.Background()); !healthy {
		t.Fatal("Cycle healthy=false, want true")
	}
	if f.store.st.SeadexFailures != 0 {
		t.Errorf("persisted SeadexFailures = %d, want 0 (the successful fetch's reset must survive a shrunken cycle)", f.store.st.SeadexFailures)
	}
	if f.store.st.ShrunkWalksByArr[library.ArrRadarr] != 1 {
		t.Errorf("persisted radarr streak = %d, want 1 (the shrink streak advances on the same save)", f.store.st.ShrunkWalksByArr[library.ArrRadarr])
	}
}
