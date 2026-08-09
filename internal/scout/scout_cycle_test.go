package scout

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/cplieger/arrapi"
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

// Exact-message log-contract assertions use capture.Recorder.CountExact: the
// pinned msg values here back Loki alert rules, where a substring match would
// false-pass on a superstring message.

type degradedMatcherAniList struct{}

func (degradedMatcherAniList) Fetch(context.Context, int) (anilist.Media, error) {
	return anilist.Media{}, context.DeadlineExceeded
}

func (degradedMatcherAniList) FetchMany(context.Context, []int) (anilist.BatchResult, error) {
	return anilist.BatchResult{}, context.DeadlineExceeded
}

type notFoundAniList struct{}

func (notFoundAniList) Fetch(context.Context, int) (anilist.Media, error) {
	return anilist.Media{}, anilist.ErrNotFound
}

func (notFoundAniList) FetchMany(_ context.Context, ids []int) (anilist.BatchResult, error) {
	verdicts := make(map[int]anilist.Verdict, len(ids))
	for _, id := range ids {
		verdicts[id] = anilist.VerdictAbsent
	}
	return anilist.BatchResult{Media: map[int]anilist.Media{}, Verdicts: verdicts}, nil
}

// TestCycleMappingUnusableReportsNothing pins the unusable-map degrade
// branch: when the mapping refresh yields zero usable records (idx.Len()==0)
// the cycle is degraded-but-healthy, saves only the refreshed library snapshot,
// and never reaches Report - so the notifier's current set is left standing
// rather than replaced by an empty one, which is what would stop reporting
// every condition that is still true.
func TestCycleMappingUnusableReportsNothing(t *testing.T) {
	logger, recorder := capture.New()
	store := &fakeStore{}
	sonarr := &fakeSonarr{series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}}}
	s := New(&Deps{
		Logger:   logger,
		Store:    store,
		Library:  arrwalk.NewWalker(&arrwalk.Config{Sonarr: sonarr, Logger: scoutTestLogger()}),
		Mapping:  emptyRecordsMapLoader(t, scoutTestLogger()),
		SeaDex:   &fakeSeaDex{entries: []seadex.Entry{{AniListID: 154587}}},
		Notifier: notify.NewNotifier(logger, nil),
	})

	if healthy := s.Cycle(context.Background()); !healthy {
		t.Fatal("Cycle healthy=false, want true when the map is unusable (degraded, not unhealthy)")
	}
	loaded := store.st
	// The gate compares nothing, so no finding ROW may be emitted - and at
	// STARTUP no summary either: no reconcile has established the set, so it is
	// empty and non-authoritative, and an empty "findings reported" line reads
	// as "no findings" (same rule as the tick's not-ready arm). The re-statement
	// half of the alerting contract starts once a reconcile has reported - see
	// TestCycleGateReemitsAStandingSetAfterReadiness.
	if n := recorder.CountExact("findings reported"); n != 0 {
		t.Errorf("unusable-map startup cycle emitted the findings summary %d times, want 0 (the set is not established yet)", n)
	}
	if n := recorder.Contains("better release available"); n {
		t.Error("unusable-map cycle emitted a finding row, want none (the gate runs no compare)")
	}
	if len(loaded.Library.Items) != 1 || loaded.Library.Items[0].Title != "Frieren" {
		t.Errorf("library snapshot not refreshed: %+v", loaded.Library)
	}
}

// TestCycleDegradedSavePersistsSanitizedArrURL pins the persistence trust
// boundary on the degraded path: a degraded cycle (unusable map here) still
// saves the refreshed library snapshot through the real state.Store.Save -
// which owns the sanitize-on-persist invariant - so a credentialed
// public_url-derived ArrURL never lands raw in state.json, while the rest of
// the item survives intact.
func TestCycleDegradedSavePersistsSanitizedArrURL(t *testing.T) {
	logger := scoutTestLogger()
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"), logger)
	if err := store.Save(context.Background(), &state.State{ShrunkWalksByArr: map[string]int{library.ArrSonarr: 1}}); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	sonarr := &fakeSonarr{series: []arrapi.Series{{ID: 7, Title: "Frieren", TitleSlug: "frieren", TvdbID: 123, Year: 2023}}}
	s := New(&Deps{
		Logger: logger,
		Store:  store,
		Library: arrwalk.NewWalker(&arrwalk.Config{
			Sonarr: sonarr, Logger: logger, SonarrURL: "https://user:pass@sonarr.example",
		}),
		Mapping: emptyRecordsMapLoader(t, logger),
		SeaDex:  &fakeSeaDex{entries: []seadex.Entry{{AniListID: 154587}}},
		// A gated reconcile re-states the finding set (cycleGateDegraded ->
		// Notifier.Reemit), so Cycle needs the notifier every deployment wires
		// even on a path that compares nothing.
		Notifier: notify.NewNotifier(logger, nil),
	})

	if healthy := s.Cycle(context.Background()); !healthy {
		t.Fatal("Cycle healthy=false, want true when the map is unusable (degraded, not unhealthy)")
	}
	saved, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load after degraded cycle: %v", err)
	}
	if len(saved.Library.Items) != 1 {
		t.Fatalf("saved library items = %d, want 1", len(saved.Library.Items))
	}
	it := saved.Library.Items[0]
	if it.ArrURL != "https://sonarr.example/series/frieren" {
		t.Errorf("saved ArrURL = %q, want the credential stripped (Store.Save must sanitize the degraded save like the cycle-completion saves)", it.ArrURL)
	}
	if it.Title != "Frieren" || it.Arr != library.ArrSonarr || it.ArrID != 7 {
		t.Errorf("saved item = %+v, want Title/Arr/ArrID untouched by sanitization", it)
	}
}

// TestCycleAniListDegradedComparesMajorityAndPreservesAffected pins the
// scoped AniList degradation contract (test a of mc-degradation-scoping): one
// transient lookup failure among N entries no longer suppresses the whole
// cycle's findings. The ID-resolved majority (which needs no lookup) compares
// and emits normally, and the cycle closes healthy with the "cycle degraded"
// completion line (reason anilist-degraded) the deployed deadman counts - never
// "cycle complete". The carry-forward of the affected entry's own row is pinned
// by TestCycleReportCarriesForwardIncompleteEvidence.
func TestCycleAniListDegradedComparesMajority(t *testing.T) {
	logger, recorder := capture.New()
	store := &fakeStore{st: state.State{
		Mapping: mapping.Cache{FetchedAt: time.Now(), Records: []mapping.Record{
			{AniListID: 154587, Type: "TV", TvdbID: 123, SeasonTvdb: 1},
			// Id-less record (a split AniList<->arr mapping): the entry NEEDS
			// the AniList title lookup, which fails transiently this cycle.
			{AniListID: 222, Type: "TV"},
		}},
	}}
	sonarr := &fakeSonarr{
		series: []arrapi.Series{
			{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023},
			{ID: 8, Title: "Idless Show", TvdbID: 124, Year: 2024},
		},
		files: map[int][]arrapi.EpisodeFile{
			7: {{SeasonNumber: 1, ReleaseGroup: "Erai-raws"}},
			8: {{SeasonNumber: 1, ReleaseGroup: "Erai-raws"}},
		},
	}
	entries := append(seadexFrierenEntry(), seadex.Entry{
		AniListID: 222,
		Torrents: []seadex.Torrent{{
			ReleaseGroup: "SubsPlease",
			Tracker:      "Nyaa",
			InfoHash:     "def",
			URL:          "https://nyaa.si/view/2",
			IsBest:       true,
			Files:        []seadex.File{{Name: "Idless Show S01E01 1080p.mkv", Length: 1}},
		}},
	})
	s := New(&Deps{
		Logger:       logger,
		Store:        store,
		Library:      arrwalk.NewWalker(&arrwalk.Config{Sonarr: sonarr, Logger: scoutTestLogger()}),
		Mapping:      fakeMapping{},
		SeaDex:       &fakeSeaDex{entries: entries},
		Matcher:      match.NewMatcher(degradedMatcherAniList{}, scoutTestLogger()),
		Comparer:     compare.NewComparer(compare.Config{}),
		Notifier:     notify.NewNotifier(logger, nil),
		AniListStats: aniStatsFn(anilist.NewClient(noNetworkClient(), "http://unused.invalid/gql", 1, scoutTestLogger())),
	})

	if healthy := s.Cycle(context.Background()); !healthy {
		t.Fatal("Cycle healthy=false, want true when AniList is transiently degraded")
	}
	// The unaffected majority's finding (Frieren, resolved by ID with no
	// AniList lookup) must emit normally instead of being suppressed.
	if n := recorder.CountExact("better release available"); n != 1 {
		t.Errorf("majority finding notification count = %d, want 1 (the compare must run on the unaffected entries)", n)
	}
	// Completion-line contract: the deployed deadman counts "cycle degraded"
	// with its reason attr; the vocabulary must not change.
	if n := recorder.CountExact("cycle degraded"); n != 1 {
		t.Errorf("'cycle degraded' count = %d, want 1", n)
	}
	if reasons := degradedReasons(recorder); len(reasons) != 1 || reasons[0] != "anilist-degraded" {
		t.Errorf("degraded reasons = %v, want [anilist-degraded]", reasons)
	}
	if n := recorder.CountExact("cycle complete"); n != 0 {
		t.Errorf("'cycle complete' count = %d, want 0 on a degraded cycle", n)
	}
}

// TestCycleEmptySeaDexEntriesReportsNothing pins the anomalous empty-but-non-
// error SeaDex response path: a successful fetch with totalPages=1 and an empty
// items array must NOT reach Report - reporting an empty set would replace the
// notifier's whole current set and stop reporting every condition that is still
// true - while still refreshing the library snapshot and staying healthy.
func TestCycleEmptySeaDexEntriesReportsNothing(t *testing.T) {
	captureLogger, recorder := capture.New()
	reporter := notify.NewNotifier(captureLogger, nil)
	logger := scoutTestLogger()
	store := &fakeStore{st: state.State{
		Mapping: frierenMappingCache(),
	}}
	sonarr := &fakeSonarr{series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}}}
	s := New(&Deps{
		Logger:       logger,
		Store:        store,
		Library:      arrwalk.NewWalker(&arrwalk.Config{Sonarr: sonarr, Logger: logger}),
		Mapping:      fakeMapping{},
		SeaDex:       &fakeSeaDex{},
		Matcher:      match.NewMatcher(notFoundAniList{}, logger),
		Comparer:     compare.NewComparer(compare.Config{}),
		Notifier:     reporter,
		AniListStats: aniStatsFn(anilist.NewClient(noNetworkClient(), "http://unused.invalid/gql", 1, logger)),
	})

	if healthy := s.Cycle(context.Background()); !healthy {
		t.Fatal("Cycle healthy=false, want true when SeaDex returns an anomalous empty result")
	}
	loaded := store.st
	if len(loaded.Library.Items) != 1 || loaded.Library.Items[0].Title != "Frieren" {
		t.Errorf("library snapshot not refreshed: %+v", loaded.Library)
	}
	if n := recorder.CountExact("better release available"); n != 0 {
		t.Errorf("empty-SeaDex cycle emitted %d finding lines, want 0", n)
	}
	if n := recorder.CountExact("findings reported"); n != 0 {
		t.Errorf("empty-SeaDex startup cycle emitted the findings summary %d times, want 0 (no reconcile has established the set, so an empty summary would claim \"no findings\")", n)
	}
}

// TestHandlePreCompareGateEmptyArrCarriesPriorItemsInsteadOfSkipping pins the
// gate boundary of the per-arr shrink guard: an enabled arr whose walk returns
// zero items while its prior count was non-zero must NOT stop the cycle (the
// comparison runs on a merged snapshot) and must NOT let the empty snapshot
// through - the gate hands back a snapshot carrying that side's prior items and
// names it suspect, and it saves nothing of its own (the streak rides whichever
// save closes the cycle).
//
// This is the shape the guard's whole design turns on, so it is pinned at the
// gate as well as end-to-end (see scout_shrink_test.go): the arm it replaced
// returned handled=true and skipped the comparison entirely.
func TestHandlePreCompareGateEmptyArrCarriesPriorItemsInsteadOfSkipping(t *testing.T) {
	logger := scoutTestLogger()
	st := state.State{Library: library.Snapshot{Items: []library.Item{
		{Arr: library.ArrSonarr, ArrID: 7, Title: "Frieren"},
	}}}
	store := &fakeStore{st: st}
	s := New(&Deps{
		Logger:   logger,
		Store:    store,
		Library:  arrwalk.NewWalker(&arrwalk.Config{Sonarr: &fakeSonarr{}, Logger: logger}),
		Notifier: notify.NewNotifier(logger, nil),
	})
	snap := library.Snapshot{}

	handled, healthy, shrunkArrs := s.handlePreCompareGate(context.Background(), &st, &snap, &mapping.Cache{}, []seadex.Entry{{AniListID: 1}}, cycleOutcomes{})
	if handled || !healthy {
		t.Errorf("handlePreCompareGate = (%v, %v), want (false, true) (the compare runs on the merged snapshot)", handled, healthy)
	}
	if len(shrunkArrs) != 1 || shrunkArrs[0] != library.ArrSonarr {
		t.Errorf("shrunk arrs = %v, want [sonarr]", shrunkArrs)
	}
	if len(snap.Items) != 1 || snap.Items[0].Title != "Frieren" {
		t.Errorf("merged snapshot = %+v, want the prior item carried forward", snap.Items)
	}
	if st.ShrunkWalksByArr[library.ArrSonarr] != 1 {
		t.Errorf("ShrunkWalksByArr = %v, want {sonarr: 1}", st.ShrunkWalksByArr)
	}
	if store.saves != 0 {
		t.Errorf("saves = %d, want 0 (the shrink guard saves nothing itself; the streak rides the cycle's closing save)", store.saves)
	}
}

// TestCyclePartialWalkComparesCleanSubset pins the
// per-item Partial-aware compare: a walk where one series' episode fetch
// failed (a Failed placeholder item) and one walked cleanly must still run the
// compare on the clean item (its finding is emitted) and close the cycle with
// the "cycle degraded" completion line (reason partial-walk) instead of "cycle
// complete". The failed item's own carry-forward is pinned by
// TestCycleReportCarriesForwardIncompleteEvidence.
func TestCyclePartialWalkComparesCleanSubset(t *testing.T) {
	logger, recorder := capture.New()
	store := &fakeStore{st: state.State{
		Mapping: mapping.Cache{FetchedAt: time.Now(), Records: []mapping.Record{
			{AniListID: 154587, Type: "TV", TvdbID: 123, SeasonTvdb: 1},
			{AniListID: 222, Type: "TV", TvdbID: 124, SeasonTvdb: 1},
		}},
	}}
	sonarr := &flakySonarr{
		fakeSonarr: fakeSonarr{
			series: []arrapi.Series{
				{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023},
				{ID: 8, Title: "Broken Series", TvdbID: 124, Year: 2024},
			},
			files: map[int][]arrapi.EpisodeFile{
				7: {{SeasonNumber: 1, ReleaseGroup: "Erai-raws"}},
			},
		},
		failEpisodes: map[int]bool{8: true},
	}
	entries := append(seadexFrierenEntry(), seadex.Entry{
		AniListID: 222,
		Torrents: []seadex.Torrent{{
			ReleaseGroup: "SubsPlease",
			Tracker:      "Nyaa",
			InfoHash:     "def",
			URL:          "https://nyaa.si/view/2",
			IsBest:       true,
			Files:        []seadex.File{{Name: "Broken Series S01E01 1080p.mkv", Length: 1}},
		}},
	})
	s := New(&Deps{
		Logger:       logger,
		Store:        store,
		Library:      arrwalk.NewWalker(&arrwalk.Config{Sonarr: sonarr, Logger: scoutTestLogger()}),
		Mapping:      fakeMapping{},
		SeaDex:       &fakeSeaDex{entries: entries},
		Matcher:      match.NewMatcher(notFoundAniList{}, scoutTestLogger()),
		Comparer:     compare.NewComparer(compare.Config{}),
		Notifier:     notify.NewNotifier(logger, nil),
		AniListStats: aniStatsFn(anilist.NewClient(noNetworkClient(), "http://unused.invalid/gql", 1, scoutTestLogger())),
	})

	if healthy := s.Cycle(context.Background()); !healthy {
		t.Fatal("Cycle healthy=false, want true (a partial walk is degraded, not unhealthy)")
	}
	if n := recorder.CountExact("better release available"); n != 1 {
		t.Errorf("clean item's finding notification count = %d, want 1 (the compare must run on the clean subset)", n)
	}
	if n := recorder.CountExact("cycle degraded"); n != 1 {
		t.Errorf("'cycle degraded' count = %d, want 1 (the partial walk's completion line)", n)
	}
	if n := recorder.CountExact("cycle complete"); n != 0 {
		t.Errorf("'cycle complete' count = %d, want 0 on a partial walk", n)
	}
	loaded := store.st
	failedPersisted := false
	for _, it := range loaded.Library.Items {
		if it.ArrID == 8 && it.Failed {
			failedPersisted = true
		}
	}
	if len(loaded.Library.Items) != 2 || !failedPersisted {
		t.Errorf("persisted library = %+v, want both items with the failed one marked", loaded.Library.Items)
	}
}

// TestHandlePreCompareGateShrunkWalkWithSeaDexOutageWarnsFeedKept pins the
// shrink guard's feed-outage contract: a library-shrink + SeaDex double outage
// with a feed configured must still surface the SeaDex failure (its standard log
// line, recorded ahead of gate selection, carrying feed_kept) and still advance
// the persisted per-arr shrink streak, so the outage does not read as
// shrink-only in Loki and cannot escape escalation behind a winning gate.
//
// The completion line is the SeaDex gate's now, not the shrink's: the shrink no
// longer stops the cycle (it merges and lets the compare run), so the pass that
// closes early here closes because SeaDex is down. The shrink evidence stands
// beside it in its own escalating line and in the persisted streak.
func TestHandlePreCompareGateShrunkWalkWithSeaDexOutageWarnsFeedKept(t *testing.T) {
	logger, recorder := capture.New()
	feed := &fakeFeed{}
	st := state.State{
		Library: library.Snapshot{Items: []library.Item{
			{Arr: library.ArrSonarr, ArrID: 1, Title: "A"},
			{Arr: library.ArrSonarr, ArrID: 2, Title: "B"},
			{Arr: library.ArrSonarr, ArrID: 3, Title: "C"},
			{Arr: library.ArrSonarr, ArrID: 4, Title: "D"},
		}},
	}
	store := &fakeStore{st: st}
	s := New(&Deps{
		Logger:   logger,
		Store:    store,
		Feed:     feed,
		Library:  arrwalk.NewWalker(&arrwalk.Config{Sonarr: &fakeSonarr{}, Logger: scoutTestLogger()}),
		Notifier: notify.NewNotifier(logger, nil),
	})
	snap := library.Snapshot{Items: []library.Item{{Arr: library.ArrSonarr, ArrID: 1, Title: "A"}}}
	mapCache := mapping.Cache{}

	handled, healthy, shrunkArrs := s.handlePreCompareGate(context.Background(), &st, &snap, &mapCache, nil, cycleOutcomes{seadex: errors.New("seadex down")})
	if !handled || !healthy {
		t.Errorf("handlePreCompareGate = (%v, %v), want (true, true) (the SeaDex outage closes the cycle)", handled, healthy)
	}
	if len(shrunkArrs) != 1 || shrunkArrs[0] != library.ArrSonarr {
		t.Errorf("shrunk arrs = %v, want [sonarr] (the shrink is still observed under the outage)", shrunkArrs)
	}
	if n := recorder.CountExact("seadex fetch failed; skipping comparison, findings re-stated unchanged this cycle"); n != 1 {
		t.Errorf("seadex failure WARN count = %d, want 1 (a shrink + SeaDex double outage must not read as shrink-only)", n)
	}
	if store.st.SeadexFailures != 1 {
		t.Errorf("persisted SeadexFailures = %d, want 1 (the streak advances, and persists, whichever gate closes the cycle)", store.st.SeadexFailures)
	}
	if store.st.ShrunkWalksByArr[library.ArrSonarr] != 1 {
		t.Errorf("persisted ShrunkWalksByArr = %v, want {sonarr: 1} (the shrink streak rides the degraded save)", store.st.ShrunkWalksByArr)
	}
	if counts := countItemsByArr(store.st.Library.Items); counts[library.ArrSonarr] != 4 {
		t.Errorf("persisted sonarr items = %d, want the prior 4 (the degraded save must persist the MERGED snapshot, or the shrink ratchets)", counts[library.ArrSonarr])
	}
	if reasons := degradedReasons(recorder); len(reasons) != 1 || reasons[0] != "seadex-fetch-failed" {
		t.Errorf("degraded reasons = %v, want [seadex-fetch-failed] (the gate that stopped the cycle owns the completion line)", reasons)
	}
	if kept, ok := recordAttr(recorder, "seadex fetch failed; skipping comparison, findings re-stated unchanged this cycle", "feed_kept"); !ok || kept != "true" {
		t.Errorf("seadex-failure WARN feed_kept attr = %q (found=%t), want \"true\" (the configured feed kept its previous snapshot through the outage)", kept, ok)
	}
}

// TestCycleRecoveredWalkResetsShrunkStreak pins the shrink guard's recovery
// rule: a walk that passes the guard for an arr ends THAT arr's persisted
// consecutive-shrunk-walk streak (the entry is deleted, so a passing side costs
// no persisted bytes), so normal resolution resumes and a later shrink starts a
// fresh streak.
func TestCycleRecoveredWalkResetsShrunkStreak(t *testing.T) {
	logger := scoutTestLogger()
	store := &fakeStore{st: state.State{
		Mapping:          frierenMappingCache(),
		Library:          library.Snapshot{Items: []library.Item{{Arr: library.ArrSonarr, ArrID: 7, Title: "Frieren", TvdbID: 123}}},
		ShrunkWalksByArr: map[string]int{library.ArrSonarr: 3},
	}}
	sonarr := &fakeSonarr{
		series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}},
		files: map[int][]arrapi.EpisodeFile{
			7: {{SeasonNumber: 1, ReleaseGroup: "Erai-raws"}},
		},
	}
	s := New(&Deps{
		Logger:       logger,
		Store:        store,
		Library:      arrwalk.NewWalker(&arrwalk.Config{Sonarr: sonarr, Logger: logger}),
		Mapping:      fakeMapping{},
		SeaDex:       &fakeSeaDex{entries: seadexFrierenEntry()},
		Matcher:      match.NewMatcher(notFoundAniList{}, logger),
		Comparer:     compare.NewComparer(compare.Config{}),
		Notifier:     notify.NewNotifier(logger, nil),
		AniListStats: aniStatsFn(anilist.NewClient(noNetworkClient(), "http://unused.invalid/gql", 1, logger)),
	})

	if healthy := s.Cycle(context.Background()); !healthy {
		t.Fatal("Cycle healthy=false, want true on a recovered walk")
	}
	if len(store.st.ShrunkWalksByArr) != 0 {
		t.Errorf("persisted ShrunkWalksByArr = %v, want empty after a walk that passes the guard", store.st.ShrunkWalksByArr)
	}
}

// TestCycleSeaDexFailureEscalatesAfterRepeatedFailures pins the WARN-to-ERROR
// escalation of the single seadex-fetch-failed log site (mirroring the
// shrunk-walk and mapping guards'): below the threshold a failed SeaDex fetch
// logs at WARN; on the 8th consecutive failure (the persisted streak reaching
// degradation.ReconcileEscalationThreshold) the same site logs at ERROR (firing the
// existing SeadexScoutCycleError Loki rule) - exactly one line either way,
// with the streak persisted, prior findings preserved, and the "cycle
// degraded" completion line unchanged.
func TestCycleSeaDexFailureEscalatesAfterRepeatedFailures(t *testing.T) {
	tests := []struct {
		name        string
		priorStreak int
		wantError   bool
	}{
		{name: "below threshold stays WARN", priorStreak: degradation.ReconcileEscalationThreshold - 2, wantError: false},
		{name: "8th consecutive failure escalates to ERROR", priorStreak: degradation.ReconcileEscalationThreshold - 1, wantError: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logger, recorder := capture.New()
			store := &fakeStore{st: state.State{
				Mapping:        frierenMappingCache(),
				SeadexFailures: tc.priorStreak,
			}}
			sonarr := &fakeSonarr{series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}}}
			s := New(&Deps{
				Notifier: notify.NewNotifier(logger, nil),
				Logger:   logger,
				Store:    store,
				Library:  arrwalk.NewWalker(&arrwalk.Config{Sonarr: sonarr, Logger: scoutTestLogger()}),
				Mapping:  fakeMapping{},
				SeaDex:   &fakeSeaDex{err: errors.New("seadex down")},
			})

			if healthy := s.Cycle(context.Background()); !healthy {
				t.Fatal("Cycle healthy=false, want true (a SeaDex outage is degraded, not unhealthy)")
			}
			if got := store.st.SeadexFailures; got != tc.priorStreak+1 {
				t.Errorf("persisted SeadexFailures = %d, want %d (the streak must increment and persist)", got, tc.priorStreak+1)
			}
			warns := recorder.CountLevel(slog.LevelWarn, "seadex fetch failed")
			errs := recorder.CountLevel(slog.LevelError, "seadex fetch failed")
			if tc.wantError {
				if errs != 1 || warns != 0 {
					t.Errorf("escalated log counts: ERROR=%d WARN=%d, want exactly one ERROR and no WARN (single log site)", errs, warns)
				}
			} else if warns != 1 || errs != 0 {
				t.Errorf("below-threshold log counts: WARN=%d ERROR=%d, want exactly one WARN and no ERROR", warns, errs)
			}
			if n := recorder.CountExact("cycle degraded"); n != 1 {
				t.Errorf("'cycle degraded' count = %d, want 1 (the failed-fetch completion line)", n)
			}
			if reasons := degradedReasons(recorder); len(reasons) != 1 || reasons[0] != "seadex-fetch-failed" {
				t.Errorf("degraded reasons = %v, want [seadex-fetch-failed]", reasons)
			}
		})
	}
}

// TestHandlePreCompareGateSeaDexEscalatesBehindWinningMappingGate pins that
// gate precedence cannot hide an observed SeaDex outage: when an unusable
// mapping wins the pre-compare gate on the same cycle the SeaDex fetch failed,
// the streak still advances and the single seadex-fetch-failed site still
// escalates to ERROR at the threshold (firing SeadexScoutCycleError). Before the
// fetch outcome was recorded ahead of gate selection, a coinciding
// mapping or walk gate left the streak frozen, so a first boot with both
// upstreams down could WARN forever and never alert.
func TestHandlePreCompareGateSeaDexEscalatesBehindWinningMappingGate(t *testing.T) {
	logger, recorder := capture.New()
	st := state.State{SeadexFailures: degradation.ReconcileEscalationThreshold - 1}
	store := &fakeStore{st: st}
	s := New(&Deps{
		Logger:   logger,
		Store:    store,
		Feed:     &fakeFeed{},
		Library:  arrwalk.NewWalker(&arrwalk.Config{Sonarr: &fakeSonarr{}, Logger: scoutTestLogger()}),
		Notifier: notify.NewNotifier(logger, nil),
	})
	mapCache := mapping.Cache{}
	snap := library.Snapshot{}

	handled, healthy, shrunkArrs := s.handlePreCompareGate(context.Background(), &st, &snap, &mapCache, nil,
		cycleOutcomes{mapping: errors.New("fribb down"), seadex: errors.New("seadex down")})
	if !handled || !healthy {
		t.Errorf("handlePreCompareGate = (%v, %v), want (true, true)", handled, healthy)
	}
	if len(shrunkArrs) != 0 {
		t.Errorf("shrunk arrs = %v, want none (there is no prior snapshot to have shrunk from)", shrunkArrs)
	}
	if store.st.SeadexFailures != degradation.ReconcileEscalationThreshold {
		t.Errorf("persisted SeadexFailures = %d, want %d (a winning gate must not freeze the streak)", store.st.SeadexFailures, degradation.ReconcileEscalationThreshold)
	}
	if errs := recorder.CountLevel(slog.LevelError, "seadex fetch failed"); errs != 1 {
		t.Errorf("seadex ERROR count = %d, want 1 (the threshold must fire behind the mapping gate)", errs)
	}
	if reasons := degradedReasons(recorder); len(reasons) != 1 || reasons[0] != "mapping-unusable" {
		t.Errorf("degraded reasons = %v, want [mapping-unusable] (the winning gate still owns the completion line)", reasons)
	}
}

// TestCycleSuccessfulSeaDexFetchResetsFailureStreak pins the SeaDex failure
// streak's recovery rule: a cycle whose fetch succeeds resets the persisted
// consecutive-failure streak to zero (persisted by the cycle's closing save,
// no operator action), so a later outage starts a fresh streak instead of
// escalating on its first failed fetch.
func TestCycleSuccessfulSeaDexFetchResetsFailureStreak(t *testing.T) {
	logger := scoutTestLogger()
	store := &fakeStore{st: state.State{
		Mapping:        frierenMappingCache(),
		SeadexFailures: 3,
	}}
	sonarr := &fakeSonarr{
		series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}},
		files: map[int][]arrapi.EpisodeFile{
			7: {{SeasonNumber: 1, ReleaseGroup: "Erai-raws"}},
		},
	}
	s := New(&Deps{
		Logger:       logger,
		Store:        store,
		Library:      arrwalk.NewWalker(&arrwalk.Config{Sonarr: sonarr, Logger: logger}),
		Mapping:      fakeMapping{},
		SeaDex:       &fakeSeaDex{entries: seadexFrierenEntry()},
		Matcher:      match.NewMatcher(notFoundAniList{}, logger),
		Comparer:     compare.NewComparer(compare.Config{}),
		Notifier:     notify.NewNotifier(logger, nil),
		AniListStats: aniStatsFn(anilist.NewClient(noNetworkClient(), "http://unused.invalid/gql", 1, logger)),
	})

	if healthy := s.Cycle(context.Background()); !healthy {
		t.Fatal("Cycle healthy=false, want true on a successful cycle")
	}
	if store.st.SeadexFailures != 0 {
		t.Errorf("persisted SeadexFailures = %d, want 0 after a successful SeaDex fetch", store.st.SeadexFailures)
	}
}

// TestCycleZeroEntriesFetchResetsSeaDexFailureStreak pins the reset arm of
// the documented SeadexFailures contract ("resets to 0 on any successful
// fetch") for a successful-but-EMPTY fetch: zero entries is anomalous (the
// cycle degrades and skips the compare) but the fetch itself succeeded, so
// the persisted streak must end - the zero-entries degradedSave carries the
// reset, and a later real outage starts a fresh streak instead of escalating
// early against a stale count.
func TestCycleZeroEntriesFetchResetsSeaDexFailureStreak(t *testing.T) {
	logger := scoutTestLogger()
	store := &fakeStore{st: state.State{
		Mapping:        frierenMappingCache(),
		SeadexFailures: 3,
	}}
	sonarr := &fakeSonarr{series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}}}
	s := New(&Deps{
		Notifier: notify.NewNotifier(logger, nil),
		Logger:   logger,
		Store:    store,
		Library:  arrwalk.NewWalker(&arrwalk.Config{Sonarr: sonarr, Logger: logger}),
		Mapping:  fakeMapping{},
		SeaDex:   &fakeSeaDex{},
	})

	if healthy := s.Cycle(context.Background()); !healthy {
		t.Fatal("Cycle healthy=false, want true (a zero-entries fetch is degraded, not unhealthy)")
	}
	if store.st.SeadexFailures != 0 {
		t.Errorf("persisted SeadexFailures = %d, want 0 (a zero-entries fetch is still a successful fetch; the documented contract resets the streak)", store.st.SeadexFailures)
	}
}

// TestCycleSteadyStateReportsAndSaves pins the daemon's steady-state operating
// mode end to end: a completed cycle reports its whole finding set through
// Report (one summary line, one emitted row), closes with one "cycle complete"
// line, and persists the refreshed caches.
func TestCycleSteadyStateReportsAndSaves(t *testing.T) {
	logger, recorder := capture.New()
	store := &fakeStore{st: state.State{Mapping: frierenMappingCache()}}
	sonarr := &fakeSonarr{
		series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}},
		files: map[int][]arrapi.EpisodeFile{
			7: {{SeasonNumber: 1, ReleaseGroup: "Erai-raws"}},
		},
	}
	s := New(&Deps{
		Logger:       logger,
		Store:        store,
		Library:      arrwalk.NewWalker(&arrwalk.Config{Sonarr: sonarr, Logger: scoutTestLogger()}),
		Mapping:      fakeMapping{},
		SeaDex:       &fakeSeaDex{entries: seadexFrierenEntry()},
		Matcher:      match.NewMatcher(notFoundAniList{}, scoutTestLogger()),
		Comparer:     compare.NewComparer(compare.Config{}),
		Notifier:     notify.NewNotifier(logger, nil),
		AniListStats: aniStatsFn(anilist.NewClient(noNetworkClient(), "http://unused.invalid/gql", 1, scoutTestLogger())),
	})

	if healthy := s.Cycle(context.Background()); !healthy {
		t.Fatal("Cycle healthy=false, want true on a successful steady-state cycle")
	}
	if n := recorder.CountExact("findings reported"); n != 1 {
		t.Errorf("'findings reported' count = %d, want 1 (the Report path)", n)
	}
	if n := recorder.CountExact("better release available"); n != 1 {
		t.Errorf("new finding notification count = %d, want 1", n)
	}
	if n := recorder.CountExact("cycle complete"); n != 1 {
		t.Errorf("'cycle complete' count = %d, want 1", n)
	}
	if got := store.st.Library.Items; len(got) != 1 || got[0].Title != "Frieren" {
		t.Errorf("persisted library = %+v, want the refreshed Frieren snapshot", got)
	}
}

// TestCycleCompletedCyclePersistsAniListMemo pins the AniList memo half of the
// cycle-completion save: the memo is what makes a cold rebuild a rare one-time
// event (a cold cycle costs ~9 batched requests against ~1704 unmemoized), so
// a completed cycle must persist the lookups it resolved. The entry with no
// Fribb record is the one that consults AniList, and its definitive answer
// must be in the persisted memo afterwards.
func TestCycleCompletedCyclePersistsAniListMemo(t *testing.T) {
	logger := scoutTestLogger()
	store := &fakeStore{st: state.State{Mapping: frierenMappingCache()}}
	sonarr := &fakeSonarr{
		series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}},
		files: map[int][]arrapi.EpisodeFile{
			7: {{SeasonNumber: 1, ReleaseGroup: "Erai-raws"}},
		},
	}
	entries := append(seadexFrierenEntry(), seadex.Entry{AniListID: 999})
	s := New(&Deps{
		Logger:       logger,
		Store:        store,
		Library:      arrwalk.NewWalker(&arrwalk.Config{Sonarr: sonarr, Logger: logger}),
		Mapping:      fakeMapping{},
		SeaDex:       &fakeSeaDex{entries: entries},
		Matcher:      match.NewMatcher(notFoundAniList{}, logger),
		Comparer:     compare.NewComparer(compare.Config{}),
		Notifier:     notify.NewNotifier(logger, nil),
		AniListStats: aniStatsFn(anilist.NewClient(noNetworkClient(), "http://unused.invalid/gql", 1, logger)),
	})

	if healthy := s.Cycle(context.Background()); !healthy {
		t.Fatal("Cycle healthy=false, want true on a successful steady-state cycle")
	}
	if _, ok := store.st.Memo.Entries[999]; !ok {
		t.Errorf("persisted memo = %+v, want the cycle's AniList lookup for 999 memoized (a lost memo forces a slow cold rebuild)", store.st.Memo.Entries)
	}
}

// TestLoadStateCorruptFileStartsCold pins loadState's fallback: a failing
// state load (the corrupt-file decode error the state suite pins on the real
// adapter) must log the failure and start from an empty state instead of
// crashing the cycle or carrying poisoned data forward.
func TestLoadStateCorruptFileStartsCold(t *testing.T) {
	logger := scoutTestLogger()
	s := New(&Deps{Logger: logger, Store: &fakeStore{loadErr: errors.New("state: decode state.json: unexpected end of JSON input")}})

	st := s.loadState(context.Background())

	if len(st.Library.Items) != 0 || len(st.Mapping.Records) != 0 || len(st.Memo.Entries) != 0 {
		t.Errorf("loadState on corrupt file = %+v, want empty state", st)
	}
}

// cancellingSonarr cancels the shared cycle context from inside the walk and
// then fails it, modelling a SIGTERM/redeploy landing mid-walk.
type cancellingSonarr struct{ cancel context.CancelFunc }

func (c *cancellingSonarr) GetSeries(context.Context) ([]arrapi.Series, error) {
	c.cancel()
	return nil, context.Canceled
}

func (c *cancellingSonarr) GetEpisodeFiles(context.Context, int) ([]arrapi.EpisodeFile, error) {
	return nil, nil
}

func (c *cancellingSonarr) GetTags(context.Context) ([]arrapi.Tag, error) {
	return nil, nil
}

// TestCycleShutdownDuringWalkWarnsNotErrors pins the redeploy contract for
// the walk phase: a cycle cancelled mid-walk must log the shutdown WARN,
// never the "library walk failed" ERROR that trips the SeadexScoutCycleError
// Loki alert on a routine redeploy - and it is HEALTHY, the same "a redeploy
// is not an ingest fault" verdict every later interruption arm applies, so a
// `poll` SIGTERMed mid-walk exits 0 like one SIGTERMed mid-match and the
// daemon's health marker is not flipped by a routine stop.
func TestCycleShutdownDuringWalkWarnsNotErrors(t *testing.T) {
	logger, recorder := capture.New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &fakeStore{}
	s := New(&Deps{
		Logger:  logger,
		Store:   store,
		Library: arrwalk.NewWalker(&arrwalk.Config{Sonarr: &cancellingSonarr{cancel: cancel}, Logger: scoutTestLogger()}),
	})

	if healthy := s.Cycle(ctx); !healthy {
		t.Fatal("Cycle healthy=false, want true (a shutdown mid-walk is a redeploy, not an ingest fault)")
	}
	if n := recorder.CountExact("cycle interrupted by shutdown during library walk"); n != 1 {
		t.Errorf("shutdown WARN count = %d, want 1", n)
	}
	if n := recorder.CountExact("library walk failed; cycle unhealthy"); n != 0 {
		t.Errorf("walk-failure ERROR logged %d times on a shutdown, want 0 (it trips the cycle-error alert)", n)
	}
	if n := recorder.CountExact("cycle degraded"); n != 0 {
		t.Errorf("'cycle degraded' count = %d, want 0 (an interrupted cycle did not complete, degraded or not)", n)
	}
}

// ctxCancellingAniList cancels the shared cycle context on its first use and
// returns context.Canceled, modelling a SIGTERM landing while the matcher is
// running its AniList lookups.
type ctxCancellingAniList struct{ cancel context.CancelFunc }

func (c *ctxCancellingAniList) Fetch(context.Context, int) (anilist.Media, error) {
	c.cancel()
	return anilist.Media{}, context.Canceled
}

func (c *ctxCancellingAniList) FetchMany(context.Context, []int) (anilist.BatchResult, error) {
	c.cancel()
	return anilist.BatchResult{}, context.Canceled
}

// TestCycleShutdownDuringMatchingWarnsShutdownNotAniList pins the mid-matching
// shutdown log contract: when the degradation is caused by the cycle context
// being cancelled (a redeploy), the cycle must keep the whole-cycle skip (the
// truncated match set has nothing safe to compare) and log "cycle interrupted
// by shutdown during matching" - never the "cycle degraded" anilist-degraded
// completion line, which would blame a healthy upstream and count an
// interrupted cycle as completed - stay healthy, and preserve prior findings.
func TestCycleShutdownDuringMatchingWarnsShutdownNotAniList(t *testing.T) {
	logger, recorder := capture.New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &fakeStore{st: state.State{
		Mapping: seasonlessMappingCache(),
	}}
	sonarr := &fakeSonarr{series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}}}
	s := New(&Deps{
		Logger:  logger,
		Store:   store,
		Library: arrwalk.NewWalker(&arrwalk.Config{Sonarr: sonarr, Logger: scoutTestLogger()}),
		Mapping: fakeMapping{},
		SeaDex:  &fakeSeaDex{entries: []seadex.Entry{{AniListID: 999}}},
		Matcher: match.NewMatcher(&ctxCancellingAniList{cancel: cancel}, scoutTestLogger()),
	})

	if healthy := s.Cycle(ctx); !healthy {
		t.Fatal("Cycle healthy=false, want true (a shutdown mid-matching is not an arr failure)")
	}
	if n := recorder.CountExact("cycle interrupted by shutdown during matching"); n != 1 {
		t.Errorf("shutdown WARN count = %d, want 1", n)
	}
	if n := recorder.CountExact("cycle degraded"); n != 0 {
		t.Errorf("'cycle degraded' count = %d, want 0 (an interrupted cycle did not complete; emitting it would misattribute the shutdown to AniList)", n)
	}
	if store.saves != 1 {
		t.Errorf("saves = %d, want 1 (the interrupted-match close must persist the refreshed caches via the detached retry, or the AniList memo is lost on every redeploy)", store.saves)
	}
	if len(store.st.Library.Items) != 1 {
		t.Errorf("persisted library items = %d, want 1 (the refreshed walk snapshot must be saved)", len(store.st.Library.Items))
	}
}

// cancellingSeaDex cancels the shared cycle context from inside the fetch and
// then fails it, modelling a SIGTERM/redeploy landing while the SeaDex fetch
// is in flight.
type cancellingSeaDex struct{ cancel context.CancelFunc }

func (c *cancellingSeaDex) FetchEntries(context.Context, seadexapi.Options) ([]seadex.Entry, error) {
	c.cancel()
	return nil, context.Canceled
}

// CountWindow is unreachable here: every test using this fake reconciles, and
// only a tick probes. It cancels and fails the same way so a misrouted
// dispatch surfaces as the fetch failure the test already asserts rather than
// as a silent success.
func (c *cancellingSeaDex) CountWindow(context.Context, time.Time) (int, error) {
	c.cancel()
	return 0, context.Canceled
}

// cancellingEmptySeaDex cancels the shared cycle context from inside the fetch
// and then returns a nil-error EMPTY snapshot: the one pre-compare arm whose
// mapping AND fetch errors are both nil, so handleUpstreamGate's shutdown
// pre-emption (which requires one of them to be non-nil) cannot cover it.
type cancellingEmptySeaDex struct{ cancel context.CancelFunc }

func (c *cancellingEmptySeaDex) FetchEntries(context.Context, seadexapi.Options) ([]seadex.Entry, error) {
	c.cancel()
	return nil, nil
}

func (c *cancellingEmptySeaDex) CountWindow(context.Context, time.Time) (int, error) {
	c.cancel()
	return 0, nil
}

// TestCycleShutdownDuringZeroEntryFetchEmitsNoCompletionLine pins the
// zero-entries arm's shutdown silence: a cancellation landing after a
// nil-error empty fetch must emit NO completion line (neither "cycle complete"
// nor "cycle degraded"), the same no-completion-line rule the walk-failed and
// shrunk-walk arms guard with their own ctx.Err() checks. The zero-entries WARN
// itself stays, like the shrink WARN.
func TestCycleShutdownDuringZeroEntryFetchEmitsNoCompletionLine(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger, recorder := capture.New()
	store := &fakeStore{st: state.State{Mapping: frierenMappingCache()}}
	sonarr := &fakeSonarr{series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}}}
	s := New(&Deps{
		Logger:  logger,
		Store:   store,
		Library: arrwalk.NewWalker(&arrwalk.Config{Sonarr: sonarr, Logger: scoutTestLogger()}),
		Mapping: fakeMapping{},
		SeaDex:  &cancellingEmptySeaDex{cancel: cancel},
	})

	if healthy := s.Cycle(ctx); !healthy {
		t.Fatal("Cycle healthy=false, want true (a shutdown is not an ingest failure)")
	}
	if n := recorder.CountExact("seadex returned zero entries; skipping comparison, findings re-stated unchanged this cycle"); n != 1 {
		t.Errorf("zero-entries WARN count = %d, want 1 (the outage evidence stays)", n)
	}
	if n := recorder.CountExact("cycle degraded"); n != 0 {
		t.Errorf("'cycle degraded' count = %d, want 0 on a shutdown-interrupted cycle", n)
	}
	if n := recorder.CountExact("cycle complete"); n != 0 {
		t.Errorf("'cycle complete' count = %d, want 0 on a shutdown-interrupted cycle", n)
	}
}

// TestCycleShutdownDuringSeaDexFetchWarnsShutdownNotSeaDex pins the
// pre-compare shutdown log contract: when the cycle context is cancelled while
// the SeaDex fetch is in flight (a redeploy), the cycle must log the shutdown
// interruption instead of "seadex fetch failed" (which would blame a healthy
// upstream), stay healthy, and preserve prior findings.
func TestCycleShutdownDuringSeaDexFetchWarnsShutdownNotSeaDex(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger, recorder := capture.New()
	store := &fakeStore{st: state.State{
		Mapping: seasonlessMappingCache(),
	}}
	sonarr := &fakeSonarr{series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}}}
	s := New(&Deps{
		Logger:  logger,
		Store:   store,
		Library: arrwalk.NewWalker(&arrwalk.Config{Sonarr: sonarr, Logger: scoutTestLogger()}),
		Mapping: fakeMapping{},
		SeaDex:  &cancellingSeaDex{cancel: cancel},
	})

	if healthy := s.Cycle(ctx); !healthy {
		t.Fatal("Cycle healthy=false, want true (a shutdown during the SeaDex fetch is not an arr failure)")
	}
	if n := recorder.CountExact("cycle interrupted by shutdown before comparison; findings not re-reported this cycle"); n != 1 {
		t.Errorf("shutdown WARN count = %d, want 1", n)
	}
	if n := recorder.CountExact("seadex fetch failed; skipping comparison, findings re-stated unchanged this cycle"); n != 0 {
		t.Errorf("shutdown misattributed to a SeaDex outage %d times, want 0", n)
	}
	if n := recorder.CountExact("cycle degraded"); n != 0 {
		t.Errorf("'cycle degraded' count = %d, want 0 (an interrupted cycle did not complete, degraded or not)", n)
	}
	if store.saves != 1 {
		t.Errorf("saves = %d, want 1 (degradedSave must persist the refreshed caches via the detached retry on a shutdown)", store.saves)
	}
	if len(store.st.Library.Items) != 1 {
		t.Errorf("persisted library items = %d, want 1 (the refreshed walk snapshot must be saved)", len(store.st.Library.Items))
	}
}

// TestCycleCancelledSeaDexFetchLeavesFailureStreakUntouched pins the
// no-evidence arm of the SeadexFailures contract: a fetch that failed because
// the cycle context was cancelled (a redeploy SIGTERM mid-fetch) is evidence
// of neither an outage nor a recovery, so the persisted streak must survive
// the shutdown's degradedSave untouched - incrementing would let routine
// redeploys walk a healthy deployment up to the ERROR escalation, and
// resetting would mask a real ongoing outage across a redeploy.
func TestCycleCancelledSeaDexFetchLeavesFailureStreakUntouched(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := scoutTestLogger()
	store := &fakeStore{st: state.State{
		Mapping:        seasonlessMappingCache(),
		SeadexFailures: 5,
	}}
	sonarr := &fakeSonarr{series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}}}
	s := New(&Deps{
		Logger:  logger,
		Store:   store,
		Library: arrwalk.NewWalker(&arrwalk.Config{Sonarr: sonarr, Logger: logger}),
		Mapping: fakeMapping{},
		SeaDex:  &cancellingSeaDex{cancel: cancel},
	})

	if healthy := s.Cycle(ctx); !healthy {
		t.Fatal("Cycle healthy=false, want true (a shutdown during the SeaDex fetch is not an arr failure)")
	}
	if store.st.SeadexFailures != 5 {
		t.Errorf("persisted SeadexFailures = %d, want the seeded 5 untouched (a cancelled fetch is evidence of neither an outage nor a recovery)", store.st.SeadexFailures)
	}
}

// TestCycleStaleMapStillComparesAndRebuildsFeed pins the stale-but-usable map
// arm: a mapping refresh failure that falls back to the cached records (a
// *mapping.StaleMapError) is degraded-but-comparable, so the cycle must still
// rebuild the Torznab feed AND run the compare (emitting findings), and the
// "mapping degraded" WARN must carry the structured stale_reason attribute
// (StaleMapError.LogAttrs) so Loki can query the degradation class.
func TestCycleStaleMapStillComparesAndRebuildsFeed(t *testing.T) {
	logger, recorder := capture.New()
	feed := &fakeFeed{}
	store := &fakeStore{st: state.State{
		// Records present but fetched beyond the 1h refresh window, with the
		// Fribb URL unreachable: Load returns the cached index wrapped in a
		// *mapping.StaleMapError.
		Mapping: mapping.Cache{FetchedAt: time.Now().Add(-2 * time.Hour), Records: []mapping.Record{{AniListID: 154587, Type: "TV", TvdbID: 123, SeasonTvdb: 1}}},
	}}
	sonarr := &fakeSonarr{
		series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}},
		files: map[int][]arrapi.EpisodeFile{
			7: {{SeasonNumber: 1, ReleaseGroup: "Erai-raws"}},
		},
	}
	s := New(&Deps{
		Logger:       logger,
		Store:        store,
		Library:      arrwalk.NewWalker(&arrwalk.Config{Sonarr: sonarr, Logger: scoutTestLogger()}),
		Mapping:      unreachableMapLoader(t, scoutTestLogger()),
		SeaDex:       &fakeSeaDex{entries: seadexFrierenEntry()},
		Matcher:      match.NewMatcher(notFoundAniList{}, scoutTestLogger()),
		Comparer:     compare.NewComparer(compare.Config{}),
		Notifier:     notify.NewNotifier(logger, nil),
		AniListStats: aniStatsFn(anilist.NewClient(noNetworkClient(), "http://unused.invalid/gql", 1, scoutTestLogger())),
		Feed:         feed,
	})

	if healthy := s.Cycle(context.Background()); !healthy {
		t.Fatal("Cycle healthy=false, want true (a stale-but-usable map is degraded, not unhealthy)")
	}
	if feed.calls != 1 {
		t.Errorf("feed Rebuild calls = %d, want 1 (a stale-but-usable map still rebuilds the feed)", feed.calls)
	}
	if n := recorder.CountExact("findings reported"); n != 1 {
		t.Errorf("'findings reported' count = %d, want 1 (a stale map must still compare)", n)
	}
	if n := recorder.CountExact("better release available"); n != 1 {
		t.Errorf("finding notification count = %d, want 1", n)
	}
	if n := recorder.CountExact("cycle degraded"); n != 1 {
		t.Errorf("'cycle degraded' count = %d, want 1 (a stale-map cycle completes degraded)", n)
	}
	if n := recorder.CountExact("cycle complete"); n != 0 {
		t.Errorf("'cycle complete' count = %d, want 0 (a stale-map cycle must not read as fully successful)", n)
	}
	if reasons := degradedReasons(recorder); len(reasons) != 1 || reasons[0] != "mapping-stale" {
		t.Errorf("degraded reasons = %v, want [mapping-stale]", reasons)
	}
	if _, ok := recordAttr(recorder, "mapping degraded", "stale_reason"); !ok {
		t.Error("\"mapping degraded\" WARN carries no stale_reason attribute; StaleMapError.LogAttrs was not appended")
	}
}

// TestSaveGenuineFailureLogsError pins save's fault contract: a save failure
// that is NOT a shutdown cancellation is a genuine write fault and must log
// "state save failed" at ERROR exactly once (the signal the
// SeadexScoutCycleError Loki alert fires on) - on a cancelled context whose
// detached retry also fails (the live-context single-attempt case is
// TestSaveGenuineFailureOnLiveContextIsNotRetried's).
func TestSaveGenuineFailureLogsError(t *testing.T) {
	logger, recorder := capture.New()
	store := &fakeStore{saveErr: errors.New("disk full")}
	s := New(&Deps{Logger: logger, Store: store, Notifier: notify.NewNotifier(logger, nil)})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	s.save(ctx, &state.State{ShrunkWalksByArr: map[string]int{library.ArrSonarr: 1}})

	if store.saves != 0 {
		t.Errorf("saves = %d, want 0 (every attempt failed)", store.saves)
	}
	errCount := recorder.CountLevel(slog.LevelError, "state save failed")
	if errCount != 1 {
		t.Errorf("\"state save failed\" ERROR count = %d, want exactly 1 (the failed detached retry must not double-log)", errCount)
	}
}

// TestLoadMappingEscalatesAfterRepeatedRejections pins the WARN-to-ERROR
// escalation of the single degraded-mapping log site: below the threshold a
// guard-rejected refresh logs "mapping degraded" at WARN; once the persisted
// streak reaches degradation.TickEscalationThreshold the same site logs at
// ERROR (firing the existing SeadexScoutCycleError Loki rule) with the remedy
// in the message and the streak/guard in the structured attrs - exactly one
// line either way (no double-logging), still returning the stale cache.
func TestLoadMappingEscalatesAfterRepeatedRejections(t *testing.T) {
	tests := []struct {
		name        string
		priorStreak int
		wantError   bool
	}{
		{name: "below threshold stays WARN", priorStreak: degradation.TickEscalationThreshold - 2, wantError: false},
		{name: "at threshold escalates to ERROR", priorStreak: degradation.TickEscalationThreshold - 1, wantError: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// One record replacing four trips the below-half-size acceptance
			// guard on every refresh.
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`[{"anilist_id":9,"type":"tv","tvdb_id":900}]`))
			}))
			defer ts.Close()
			logger, recorder := capture.New()
			st := state.State{Mapping: mapping.Cache{
				FetchedAt: time.Now().Add(-2 * time.Hour),
				Records: []mapping.Record{
					{AniListID: 1, Type: "TV", TvdbID: 100},
					{AniListID: 2, Type: "TV", TvdbID: 200},
					{AniListID: 3, Type: "TV", TvdbID: 300},
					{AniListID: 4, Type: "TV", TvdbID: 400},
				},
				RejectedRefreshes: tc.priorStreak,
			}}
			s := New(&Deps{
				Logger:  logger,
				Mapping: mapping.NewLoader(ts.Client(), ts.URL, "", time.Hour, scoutTestLogger()),
			})

			mapCache, _, mapErr := s.loadMapping(context.Background(), &st)
			if mapErr == nil {
				t.Fatal("loadMapping with a guard-rejected refresh returned nil error, want *StaleMapError")
			}
			if len(mapCache.Records) != 4 {
				t.Fatalf("loadMapping kept %d records, want the 4 stale records", len(mapCache.Records))
			}
			if mapCache.RejectedRefreshes != tc.priorStreak+1 {
				t.Errorf("RejectedRefreshes = %d, want %d", mapCache.RejectedRefreshes, tc.priorStreak+1)
			}
			warns := recorder.CountLevel(slog.LevelWarn, "mapping degraded")
			errs := recorder.CountLevel(slog.LevelError, "mapping degraded")
			if tc.wantError {
				if errs != 1 || warns != 0 {
					t.Errorf("escalated log counts: ERROR=%d WARN=%d, want exactly one ERROR and no WARN (single log site)", errs, warns)
				}
			} else if warns != 1 || errs != 0 {
				t.Errorf("below-threshold log counts: WARN=%d ERROR=%d, want exactly one WARN and no ERROR", warns, errs)
			}
		})
	}
}

// degradedReasons collects the reason attr of every "cycle degraded" record,
// so a test can pin both the completion line and which gate emitted it.
func degradedReasons(recorder *capture.Recorder) []string {
	var reasons []string
	for _, r := range recorder.Records() {
		if r.Message != "cycle degraded" {
			continue
		}
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "reason" {
				reasons = append(reasons, a.Value.String())
				return false
			}
			return true
		})
	}
	return reasons
}

// TestCycleDegradedEarlyReturnsEmitCycleDegraded pins the degraded completion
// line: every degraded-but-healthy gate (unusable map, failed SeaDex fetch,
// empty SeaDex result, and the scoped AniList degradation, which now compares
// the unaffected majority instead of returning early) must emit exactly one
// "cycle degraded" WARN with a reason attr naming the gate, and never "cycle
// complete" - so the cycle-deadman alert (which counts completion lines) does
// not fire as if the daemon died during a long upstream outage.
func TestCycleDegradedEarlyReturnsEmitCycleDegraded(t *testing.T) {
	sonarrOK := func() *fakeSonarr {
		return &fakeSonarr{series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}}}
	}
	tests := []struct {
		name       string
		wantReason string
		deps       func(t *testing.T, logger *slog.Logger) *Deps
	}{
		{
			name:       "mapping unusable",
			wantReason: "mapping-unusable",
			deps: func(t *testing.T, logger *slog.Logger) *Deps {
				t.Helper()
				return &Deps{
					Store:    &fakeStore{},
					Library:  arrwalk.NewWalker(&arrwalk.Config{Sonarr: sonarrOK(), Logger: scoutTestLogger()}),
					Mapping:  emptyRecordsMapLoader(t, scoutTestLogger()),
					SeaDex:   &fakeSeaDex{entries: []seadex.Entry{{AniListID: 154587}}},
					Logger:   logger,
					Notifier: notify.NewNotifier(logger, nil),
				}
			},
		},
		{
			name:       "seadex fetch failed",
			wantReason: "seadex-fetch-failed",
			deps: func(t *testing.T, logger *slog.Logger) *Deps {
				t.Helper()
				return &Deps{
					Store:    &fakeStore{st: state.State{Mapping: seasonlessMappingCache()}},
					Library:  arrwalk.NewWalker(&arrwalk.Config{Sonarr: sonarrOK(), Logger: scoutTestLogger()}),
					Mapping:  fakeMapping{},
					SeaDex:   &fakeSeaDex{err: errors.New("seadex down")},
					Logger:   logger,
					Notifier: notify.NewNotifier(logger, nil),
				}
			},
		},
		{
			name:       "seadex zero entries",
			wantReason: "seadex-zero-entries",
			deps: func(t *testing.T, logger *slog.Logger) *Deps {
				t.Helper()
				return &Deps{
					Store:    &fakeStore{st: state.State{Mapping: seasonlessMappingCache()}},
					Library:  arrwalk.NewWalker(&arrwalk.Config{Sonarr: sonarrOK(), Logger: scoutTestLogger()}),
					Mapping:  fakeMapping{},
					SeaDex:   &fakeSeaDex{},
					Logger:   logger,
					Notifier: notify.NewNotifier(logger, nil),
				}
			},
		},
		{
			name:       "anilist degraded",
			wantReason: "anilist-degraded",
			deps: func(t *testing.T, logger *slog.Logger) *Deps {
				t.Helper()
				// The scoped degradation runs the compare (on the unaffected
				// majority), so the compare/report deps are wired here unlike
				// the true early-return gates above.
				return &Deps{
					Store:    &fakeStore{st: state.State{Mapping: seasonlessMappingCache()}},
					Library:  arrwalk.NewWalker(&arrwalk.Config{Sonarr: sonarrOK(), Logger: scoutTestLogger()}),
					Mapping:  fakeMapping{},
					SeaDex:   &fakeSeaDex{entries: []seadex.Entry{{AniListID: 999}}},
					Matcher:  match.NewMatcher(degradedMatcherAniList{}, scoutTestLogger()),
					Comparer: compare.NewComparer(compare.Config{}),
					Notifier: notify.NewNotifier(scoutTestLogger(), nil),
					Logger:   logger,
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logger, recorder := capture.New()
			s := New(tc.deps(t, logger))

			if healthy := s.Cycle(context.Background()); !healthy {
				t.Fatal("Cycle healthy=false, want true (a degraded upstream is not an ingest failure)")
			}
			if n := recorder.CountExact("cycle degraded"); n != 1 {
				t.Errorf("'cycle degraded' count = %d, want exactly 1", n)
			}
			if n := recorder.CountExact("cycle complete"); n != 0 {
				t.Errorf("'cycle complete' count = %d, want 0 on a degraded cycle", n)
			}
			if reasons := degradedReasons(recorder); len(reasons) != 1 || reasons[0] != tc.wantReason {
				t.Errorf("degraded reasons = %v, want [%s]", reasons, tc.wantReason)
			}
		})
	}
}

// titledAniList answers every lookup with the id-less show's own title, year
// and format, so a healthy cycle can MATCH that entry through the title
// fallback and produce a finding for it - the row the degraded cycle then has
// something to carry forward.
type titledAniList struct{}

func (titledAniList) Fetch(context.Context, int) (anilist.Media, error) {
	return idlessShowMedia(), nil
}

func (titledAniList) FetchMany(_ context.Context, ids []int) (anilist.BatchResult, error) {
	media := make(map[int]anilist.Media, len(ids))
	verdicts := make(map[int]anilist.Verdict, len(ids))
	for _, id := range ids {
		media[id] = idlessShowMedia()
		verdicts[id] = anilist.VerdictFound
	}
	return anilist.BatchResult{Media: media, Verdicts: verdicts}, nil
}

func idlessShowMedia() anilist.Media {
	return anilist.Media{Format: "TV", Titles: []string{"Idless Show"}, Year: 2024}
}

// TestCycleReportCarriesForwardIncompleteEvidence pins the ONE argument
// finishCompletedCycle computes for Report beyond the findings themselves: the
// preserve set is the UNION of the failed-walk item ids and the
// AniList-incomplete lookup ids, so neither degradation can mask the other's
// carry-forward.
//
// It is asserted end to end rather than by inspecting the call, because the
// union only matters through its effect: a healthy first cycle reports all
// three rows, then a cycle where ONE series' episode fetch fails (a Failed
// placeholder, partial walk) and a DIFFERENT id-less entry's AniList lookup
// fails transiently must still emit both affected rows - their absence from the
// compare is missing data, not alignment - alongside the unaffected majority,
// with the summary line's preserved counter naming both. If the union dropped
// either half, that half's row would vanish from the log and the operator's
// alert would silently stop firing for a condition that is still true.
func TestCycleReportCarriesForwardIncompleteEvidence(t *testing.T) {
	logger, recorder := capture.New()
	store := &fakeStore{st: state.State{
		Mapping: mapping.Cache{FetchedAt: time.Now(), Records: []mapping.Record{
			{AniListID: 154587, Type: "TV", TvdbID: 123, SeasonTvdb: 1},
			{AniListID: 222, Type: "TV", TvdbID: 124, SeasonTvdb: 1},
			// Id-less record (a split AniList<->arr mapping): the entry NEEDS
			// the AniList title lookup on every cycle whose memo is cold.
			{AniListID: 333, Type: "TV"},
		}},
	}}
	sonarr := &flakySonarr{
		fakeSonarr: fakeSonarr{
			series: []arrapi.Series{
				{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023},
				{ID: 8, Title: "Broken Series", TvdbID: 124, Year: 2024},
				{ID: 9, Title: "Idless Show", TvdbID: 125, Year: 2024},
			},
			files: map[int][]arrapi.EpisodeFile{
				7: {{SeasonNumber: 1, ReleaseGroup: "Erai-raws"}},
				8: {{SeasonNumber: 1, ReleaseGroup: "Erai-raws"}},
				9: {{SeasonNumber: 1, ReleaseGroup: "Erai-raws"}},
			},
		},
	}
	entries := append(seadexFrierenEntry(),
		seadex.Entry{
			AniListID: 222,
			Torrents: []seadex.Torrent{{
				ReleaseGroup: "SubsPlease",
				Tracker:      "Nyaa",
				InfoHash:     "def",
				URL:          "https://nyaa.si/view/2",
				IsBest:       true,
				Files:        []seadex.File{{Name: "Broken Series S01E01 1080p.mkv", Length: 1}},
			}},
		},
		seadex.Entry{
			AniListID: 333,
			Torrents: []seadex.Torrent{{
				ReleaseGroup: "SubsPlease",
				Tracker:      "Nyaa",
				InfoHash:     "ghi",
				URL:          "https://nyaa.si/view/3",
				IsBest:       true,
				Files:        []seadex.File{{Name: "Idless Show S01E01 1080p.mkv", Length: 1}},
			}},
		})
	// One notifier across both cycles: the current set is in-memory state, so
	// the carry-forward can only be observed through the same instance.
	notifier := notify.NewNotifier(logger, nil)
	scout := func(anilistClient match.AniListClient) *Scout {
		return New(&Deps{
			Logger:   logger,
			Store:    store,
			Library:  arrwalk.NewWalker(&arrwalk.Config{Sonarr: sonarr, Logger: scoutTestLogger()}),
			Mapping:  fakeMapping{},
			SeaDex:   &fakeSeaDex{entries: entries},
			Matcher:  match.NewMatcher(anilistClient, scoutTestLogger()),
			Comparer: compare.NewComparer(compare.Config{}),
			Notifier: notifier,
		})
	}

	// Cycle one: everything walks and resolves, so all three rows report.
	if healthy := scout(titledAniList{}).Cycle(context.Background()); !healthy {
		t.Fatal("healthy cycle returned healthy=false, want true")
	}
	if n := recorder.CountExact("better release available"); n != 3 {
		t.Fatalf("healthy cycle emitted %d rows, want 3 (the fixture must report all three before anything can be carried forward)", n)
	}

	// Cycle two: series 8's episode fetch fails (partial walk) and entry 333's
	// AniList lookup fails transiently. The memo entry cycle one wrote is
	// cleared, so the lookup is genuinely attempted again.
	sonarr.failEpisodes = map[int]bool{8: true}
	store.st.Memo = match.Memo{}
	if healthy := scout(degradedMatcherAniList{}).Cycle(context.Background()); !healthy {
		t.Fatal("Cycle healthy=false, want true (partial walk + transient AniList degradation is degraded, not unhealthy)")
	}

	// Both affected rows must be emitted AGAIN, beside the unaffected majority.
	for _, title := range []string{"Frieren", "Broken Series", "Idless Show"} {
		if n := cycleTitleCount(recorder, title); n != 2 {
			t.Errorf("row %q emitted %d times across the two cycles, want 2 (the degraded cycle must still report it)", title, n)
		}
	}
	if got, seen := reportSummaryCounter(recorder, "preserved"); !seen || got != 2 {
		t.Errorf("summary preserved = %d (present %v), want 2 (the failed-walk item AND the incomplete lookup)", got, seen)
	}
	if got, _ := reportSummaryCounter(recorder, "total"); got != 3 {
		t.Errorf("summary total = %d, want 3 (the majority plus both carried rows)", got)
	}
	if reasons := degradedReasons(recorder); len(reasons) != 1 || reasons[0] != "partial-walk" {
		t.Errorf("degraded reasons = %v, want [partial-walk] (the switch's first arm wins the combined degradation)", reasons)
	}
	if n := recorder.CountExact("cycle complete"); n != 1 {
		t.Errorf("'cycle complete' count = %d, want 1 (the healthy cycle only)", n)
	}
}

// cycleTitleCount counts the emitted better-release lines carrying title, so a
// two-cycle test can assert which rows each pass reported.
func cycleTitleCount(recorder *capture.Recorder, title string) int {
	n := 0
	for _, rec := range recorder.Records() {
		if rec.Message != "better release available" {
			continue
		}
		rec.Attrs(func(a slog.Attr) bool {
			if a.Key == "title" && a.Value.String() == title {
				n++
			}
			return true
		})
	}
	return n
}

// reportSummaryCounter reads one counter off the LAST "findings reported"
// summary line, i.e. the most recent pass's accounting.
func reportSummaryCounter(recorder *capture.Recorder, key string) (int64, bool) {
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

// cancellingMappingTransport cancels the shared cycle context from inside the
// mapping loader's refresh request and fails it, modelling a SIGTERM/redeploy
// landing while the Fribb conditional GET is in flight.
type cancellingMappingTransport struct{ cancel context.CancelFunc }

func (c cancellingMappingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	c.cancel()
	return nil, context.Canceled
}

// TestCycleShutdownDuringMappingLoadWarnsShutdownNotFribb pins the mapping arm
// of the misattribution contract: when the cycle context is cancelled while
// the Fribb refresh is in flight (a redeploy), the cycle must log the shutdown
// interruption instead of "mapping degraded" (which would blame a healthy
// upstream), stay healthy, emit no completion line, and preserve findings.
func TestCycleShutdownDuringMappingLoadWarnsShutdownNotFribb(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger, recorder := capture.New()
	store := &fakeStore{st: state.State{
		// Records fetched beyond the 1h refresh window force a refresh
		// attempt, whose transport cancels the cycle context mid-flight.
		Mapping: mapping.Cache{FetchedAt: time.Now().Add(-2 * time.Hour), Records: []mapping.Record{{AniListID: 111, Type: "TV", TvdbID: 123}}},
	}}
	sonarr := &fakeSonarr{series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}}}
	s := New(&Deps{
		Logger:  logger,
		Store:   store,
		Library: arrwalk.NewWalker(&arrwalk.Config{Sonarr: sonarr, Logger: scoutTestLogger()}),
		Mapping: mapping.NewLoader(&http.Client{Transport: cancellingMappingTransport{cancel: cancel}}, "http://unused.invalid/f.json", filepath.Join(t.TempDir(), "ov.json"), time.Hour, scoutTestLogger()),
		SeaDex:  &fakeSeaDex{entries: []seadex.Entry{{AniListID: 999}}},
	})

	if healthy := s.Cycle(ctx); !healthy {
		t.Fatal("Cycle healthy=false, want true (a shutdown during the mapping load is not an arr failure)")
	}
	if n := recorder.CountExact("mapping degraded"); n != 0 {
		t.Errorf("'mapping degraded' fired %d times during a shutdown, want 0 (a cancelled load is the shutdown, not a Fribb fault)", n)
	}
	if n := recorder.CountExact("mapping unusable; skipping comparison, findings re-stated unchanged this cycle"); n != 0 {
		t.Errorf("shutdown misattributed to an unusable map %d times, want 0", n)
	}
	if n := recorder.CountExact("cycle interrupted by shutdown before comparison; findings not re-reported this cycle"); n != 1 {
		t.Errorf("shutdown WARN count = %d, want 1", n)
	}
	if n := recorder.CountExact("cycle degraded"); n != 0 {
		t.Errorf("'cycle degraded' count = %d, want 0 (an interrupted cycle did not complete)", n)
	}
}

// TestCycleCompletionLineCarriesAniListCycleDeltas pins the per-cycle AniList
// counter arithmetic on the completion line: anilist_calls/anilist_waits are
// the client's cumulative counters, and their _cycle twins must be the delta
// against the cycle-start snapshot - the pair the documented "cycle complete"
// Loki line carries. A scripted AniListStats closure (the same seam build.go
// wires the real client's Stats into) returns different values on the
// cycle-start and completion snapshots, so a broken subtraction, a swapped
// operand, or a completion line reading the start snapshot is directly
// observable.
func TestCycleCompletionLineCarriesAniListCycleDeltas(t *testing.T) {
	logger, recorder := capture.New()
	store := &fakeStore{st: state.State{
		Mapping: frierenMappingCache(),
	}}
	sonarr := &fakeSonarr{
		series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}},
		files: map[int][]arrapi.EpisodeFile{
			7: {{SeasonNumber: 1, ReleaseGroup: "Erai-raws"}},
		},
	}
	var statsCalls int
	stats := func() AniListStats {
		statsCalls++
		if statsCalls == 1 {
			return AniListStats{Calls: 10, RateLimitWaits: 1} // the cycle-start snapshot
		}
		return AniListStats{Calls: 60, RateLimitWaits: 3} // the completion-line snapshot
	}
	s := New(&Deps{
		Logger:       logger,
		Store:        store,
		Library:      arrwalk.NewWalker(&arrwalk.Config{Sonarr: sonarr, Logger: scoutTestLogger()}),
		Mapping:      fakeMapping{},
		SeaDex:       &fakeSeaDex{entries: seadexFrierenEntry()},
		Matcher:      match.NewMatcher(notFoundAniList{}, scoutTestLogger()),
		Comparer:     compare.NewComparer(compare.Config{}),
		Notifier:     notify.NewNotifier(scoutTestLogger(), nil),
		AniListStats: stats,
	})

	if healthy := s.Cycle(context.Background()); !healthy {
		t.Fatal("Cycle healthy=false, want true on a successful steady-state cycle")
	}
	wantAttrs := map[string]string{
		"anilist_calls":       "60",
		"anilist_calls_cycle": "50",
		"anilist_waits":       "3",
		"anilist_waits_cycle": "2",
	}
	for key, want := range wantAttrs {
		if got, ok := recordAttr(recorder, "cycle complete", key); !ok || got != want {
			t.Errorf("'cycle complete' %s = %q (found=%t), want %q", key, got, ok, want)
		}
	}
}

// TestCycleCompletionLineCarriesCountsAndCoverage pins the count half of the
// documented "cycle complete" line: seadex_entries, library_items, findings,
// the ID-bridge coverage totals (mapped/unmapped, summed across arrs by
// sumCounts), and the snapshot diff counters. The scenario separates the pairs
// a swap could hide - two coverage hits under DIFFERENT arrs (a Sonarr series
// record plus a Radarr movie record) against one unmapped id-less record, and
// zero added against one removed - so a per-arr total that reports one bucket
// instead of their sum, a swapped mapped/unmapped pair, or a swapped
// added/removed pair is observable. The remaining 1-valued attrs
// (library_items, findings, unmapped, removed, changed) are not mutually
// distinguishable, so a swap purely among them is not covered here.
func TestCycleCompletionLineCarriesCountsAndCoverage(t *testing.T) {
	logger, recorder := capture.New()
	store := &fakeStore{st: state.State{
		Mapping: mapping.Cache{FetchedAt: time.Now(), Records: []mapping.Record{
			{AniListID: 154587, Type: "TV", TvdbID: 123, SeasonTvdb: 1},
			// A MOVIE record carrying its TMDB id is a second ID-bridge hit,
			// counted in the radarr bucket: "mapped" must be the SUM.
			{AniListID: 500, Type: "MOVIE", TmdbMovies: []int{900}},
			// An id-less record is the unmapped bucket (the ID bridge could
			// not resolve an arr id).
			{AniListID: 700, Type: "TV"},
		}},
		Library: library.Snapshot{Items: []library.Item{
			// Same key as the walked Frieren item but different file state:
			// one changed. The second item is gone from the walk: one removed.
			{Arr: library.ArrSonarr, ArrID: 7, Title: "Frieren"},
			{Arr: library.ArrSonarr, ArrID: 99, Title: "Gone"},
		}},
	}}
	sonarr := &fakeSonarr{
		series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}},
		files: map[int][]arrapi.EpisodeFile{
			7: {{SeasonNumber: 1, ReleaseGroup: "Erai-raws"}},
		},
	}
	entries := append(seadexFrierenEntry(),
		seadex.Entry{AniListID: 500},
		seadex.Entry{AniListID: 700},
	)
	s := New(&Deps{
		Logger:       logger,
		Store:        store,
		Library:      arrwalk.NewWalker(&arrwalk.Config{Sonarr: sonarr, Logger: scoutTestLogger()}),
		Mapping:      fakeMapping{},
		SeaDex:       &fakeSeaDex{entries: entries},
		Matcher:      match.NewMatcher(notFoundAniList{}, scoutTestLogger()),
		Comparer:     compare.NewComparer(compare.Config{}),
		Notifier:     notify.NewNotifier(scoutTestLogger(), nil),
		AniListStats: aniStatsFn(anilist.NewClient(noNetworkClient(), "http://unused.invalid/gql", 1, scoutTestLogger())),
	})

	if healthy := s.Cycle(context.Background()); !healthy {
		t.Fatal("Cycle healthy=false, want true on a successful steady-state cycle")
	}
	wantAttrs := map[string]string{
		"seadex_entries": "3",
		"library_items":  "1",
		"findings":       "1",
		"mapped":         "2",
		"unmapped":       "1",
		"added":          "0",
		"removed":        "1",
		"changed":        "1",
	}
	for key, want := range wantAttrs {
		if got, ok := recordAttr(recorder, "cycle complete", key); !ok || got != want {
			t.Errorf("'cycle complete' %s = %q (found=%t), want %q", key, got, ok, want)
		}
	}
}

// TestCycleAniListDegradedStreakEscalatesToError pins the fourth escalation
// class: a persistent AniList degradation (result.Degraded on consecutive
// completed cycles) must escalate its log site to ERROR (firing the
// SeadexScoutCycleError rule) at the shared threshold, exactly like the
// shrunk-walk, SeaDex-failure, and mapping-rejection streaks - a permanently
// broken egress to graphql.anilist.co previously WARNed "cycle degraded"
// forever while findings stayed frozen. Below the threshold no ERROR fires,
// the streak persists in state, and an undegraded completed cycle resets it.
func TestCycleAniListDegradedStreakEscalatesToError(t *testing.T) {
	newScout := func(store *fakeStore) (*Scout, *capture.Recorder) {
		logger, recorder := capture.New()
		sonarr := &fakeSonarr{
			series: []arrapi.Series{{ID: 8, Title: "Idless Show", TvdbID: 124, Year: 2024}},
			files:  map[int][]arrapi.EpisodeFile{8: {{SeasonNumber: 1, ReleaseGroup: "Erai-raws"}}},
		}
		entries := []seadex.Entry{{
			AniListID: 222,
			Torrents: []seadex.Torrent{{
				ReleaseGroup: "SubsPlease", Tracker: "Nyaa", InfoHash: "def",
				URL: "https://nyaa.si/view/2", IsBest: true,
				Files: []seadex.File{{Name: "Idless Show S01E01 1080p.mkv", Length: 1}},
			}},
		}}
		return New(&Deps{
			Logger:       logger,
			Store:        store,
			Library:      arrwalk.NewWalker(&arrwalk.Config{Sonarr: sonarr, Logger: scoutTestLogger()}),
			Mapping:      fakeMapping{},
			SeaDex:       &fakeSeaDex{entries: entries},
			Matcher:      match.NewMatcher(degradedMatcherAniList{}, scoutTestLogger()),
			Comparer:     compare.NewComparer(compare.Config{}),
			Notifier:     notify.NewNotifier(logger, nil),
			AniListStats: aniStatsFn(anilist.NewClient(noNetworkClient(), "http://unused.invalid/gql", 1, scoutTestLogger())),
		}), recorder
	}

	// One cycle below the threshold: streak advances, no ERROR.
	store := &fakeStore{st: state.State{
		AniListDegraded: degradation.ReconcileEscalationThreshold - 2,
		Mapping:         mapping.Cache{FetchedAt: time.Now(), Records: []mapping.Record{{AniListID: 222, Type: "TV"}}},
	}}
	s, recorder := newScout(store)
	if healthy := s.Cycle(context.Background()); !healthy {
		t.Fatal("Cycle healthy=false, want true (degraded, not failed)")
	}
	if got := store.st.AniListDegraded; got != degradation.ReconcileEscalationThreshold-1 {
		t.Errorf("persisted streak = %d, want %d", got, degradation.ReconcileEscalationThreshold-1)
	}
	if n := recorder.CountExact("anilist lookups degraded repeatedly; matching incomplete and findings frozen for affected entries - inspect graphql.anilist.co reachability and egress"); n != 0 {
		t.Errorf("escalation ERROR count below threshold = %d, want 0", n)
	}

	// The threshold cycle: the ERROR fires beside the unchanged completion line.
	s, recorder = newScout(store)
	if healthy := s.Cycle(context.Background()); !healthy {
		t.Fatal("threshold Cycle healthy=false, want true")
	}
	if got := store.st.AniListDegraded; got != degradation.ReconcileEscalationThreshold {
		t.Errorf("persisted streak = %d, want %d", got, degradation.ReconcileEscalationThreshold)
	}
	if n := recorder.CountExact("anilist lookups degraded repeatedly; matching incomplete and findings frozen for affected entries - inspect graphql.anilist.co reachability and egress"); n != 1 {
		t.Errorf("escalation ERROR count at threshold = %d, want 1", n)
	}
	if n := recorder.CountExact("cycle degraded"); n != 1 {
		t.Errorf("'cycle degraded' completion line count = %d, want 1 (the deadman vocabulary must not change)", n)
	}
}

// TestCycleExactlyHalfWalkPassesShrinkGuard pins the shrink guard's exact
// boundary through the public cycle: the policy
// (degradation.Shrunk) is "fewer than 1/factor of the prior items for that arr"
// - strictly BELOW half at the default 2 - so a walk returning exactly half of
// the arr's prior items must pass the guard. The externally meaningful
// consequences are asserted, not the orchestration decomposition: the halved
// walk is persisted as the new snapshot, the shrunk-walk streak resets, the
// cycle stays healthy, and it closes with the completion (not the degraded)
// line. A 1-of-4 walk (1*2 < 4) is the tripping case the escalation test pins.
func TestCycleExactlyHalfWalkPassesShrinkGuard(t *testing.T) {
	logger, recorder := capture.New()
	store := &fakeStore{st: state.State{
		Mapping: mapping.Cache{FetchedAt: time.Now(), Records: []mapping.Record{
			{AniListID: 1, Type: "TV", TvdbID: 101, SeasonTvdb: 1},
		}},
		Library: library.Snapshot{Items: []library.Item{
			{Arr: library.ArrSonarr, ArrID: 1, Title: "A"},
			{Arr: library.ArrSonarr, ArrID: 2, Title: "B"},
			{Arr: library.ArrSonarr, ArrID: 3, Title: "C"},
			{Arr: library.ArrSonarr, ArrID: 4, Title: "D"},
		}},
		ShrunkWalksByArr: map[string]int{library.ArrSonarr: 3},
	}}
	sonarr := &fakeSonarr{series: []arrapi.Series{
		{ID: 1, Title: "A", TvdbID: 101},
		{ID: 2, Title: "B", TvdbID: 102},
	}}
	s := New(&Deps{
		Logger:   logger,
		Store:    store,
		Library:  arrwalk.NewWalker(&arrwalk.Config{Sonarr: sonarr, Logger: scoutTestLogger()}),
		Mapping:  fakeMapping{},
		SeaDex:   &fakeSeaDex{entries: []seadex.Entry{{AniListID: 1}}},
		Matcher:  match.NewMatcher(notFoundAniList{}, scoutTestLogger()),
		Comparer: compare.NewComparer(compare.Config{}),
		Notifier: notify.NewNotifier(scoutTestLogger(), nil),
	})

	if healthy := s.Cycle(context.Background()); !healthy {
		t.Fatal("Cycle healthy=false, want true at exactly half the prior library")
	}
	if got := len(store.st.Library.Items); got != 2 {
		t.Errorf("persisted library items = %d, want 2 (exactly half must pass the shrink guard)", got)
	}
	if len(store.st.ShrunkWalksByArr) != 0 {
		t.Errorf("persisted ShrunkWalksByArr = %v, want empty after the boundary walk completed", store.st.ShrunkWalksByArr)
	}
	if n := recorder.CountExact("cycle degraded"); n != 0 {
		t.Errorf("cycle degraded count = %d, want 0 at the non-shrinking boundary", n)
	}
	if n := recorder.CountExact("cycle complete"); n != 1 {
		t.Errorf("cycle complete count = %d, want 1", n)
	}
}

// TestCycleUndegradedCycleResetsAniListDegradedStreak pins the AniList
// degradation streak's recovery rule (documented on
// degradation.ReconcileEscalationThreshold: "the first undegraded completed cycle
// resets the streak"): a completed cycle whose matching needed no degraded
// lookups must reset the persisted streak to zero, exactly like its
// shrunk-walk and SeaDex-failure siblings, so a later transient blip starts
// a fresh streak instead of escalating to ERROR early against a stale count.
func TestCycleUndegradedCycleResetsAniListDegradedStreak(t *testing.T) {
	logger := scoutTestLogger()
	store := &fakeStore{st: state.State{
		Mapping:         frierenMappingCache(),
		AniListDegraded: 5,
	}}
	sonarr := &fakeSonarr{
		series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}},
		files: map[int][]arrapi.EpisodeFile{
			7: {{SeasonNumber: 1, ReleaseGroup: "Erai-raws"}},
		},
	}
	s := New(&Deps{
		Logger:       logger,
		Store:        store,
		Library:      arrwalk.NewWalker(&arrwalk.Config{Sonarr: sonarr, Logger: logger}),
		Mapping:      fakeMapping{},
		SeaDex:       &fakeSeaDex{entries: seadexFrierenEntry()},
		Matcher:      match.NewMatcher(notFoundAniList{}, logger),
		Comparer:     compare.NewComparer(compare.Config{}),
		Notifier:     notify.NewNotifier(logger, nil),
		AniListStats: aniStatsFn(anilist.NewClient(noNetworkClient(), "http://unused.invalid/gql", 1, logger)),
	})

	if healthy := s.Cycle(context.Background()); !healthy {
		t.Fatal("Cycle healthy=false, want true on an undegraded completed cycle")
	}
	if store.st.AniListDegraded != 0 {
		t.Errorf("persisted AniListDegraded = %d, want 0 after an undegraded completed cycle (the streak's documented recovery rule)", store.st.AniListDegraded)
	}
}

// TestCycleAniListDegradedWinsMappingStaleCompletionLine pins the untested
// pair of logCompletedCycle's precedence order (partial walk, then AniList
// degradation, then a stale map): a cycle that is BOTH AniList-degraded and
// running on a stale-but-usable map must close with reason anilist-degraded,
// never mapping-stale, and its completion line must carry the POST-update
// streak (consecutive_anilist_degraded = 1 on the first degraded cycle) -
// pinning that recordAniListDegradation runs before the completion line.
func TestCycleAniListDegradedWinsMappingStaleCompletionLine(t *testing.T) {
	logger, recorder := capture.New()
	store := &fakeStore{st: state.State{
		// Records present but fetched beyond the 1h refresh window, with the
		// Fribb URL unreachable: the map loads stale-but-usable (a
		// *mapping.StaleMapError), while the unmapped SeaDex entry's needed
		// AniList lookup fails transiently in the same cycle.
		Mapping: mapping.Cache{FetchedAt: time.Now().Add(-2 * time.Hour), Records: []mapping.Record{{AniListID: 111, Type: "TV", TvdbID: 123}}},
	}}
	sonarr := &fakeSonarr{series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}}}
	s := New(&Deps{
		Logger:   logger,
		Store:    store,
		Library:  arrwalk.NewWalker(&arrwalk.Config{Sonarr: sonarr, Logger: scoutTestLogger()}),
		Mapping:  unreachableMapLoader(t, scoutTestLogger()),
		SeaDex:   &fakeSeaDex{entries: []seadex.Entry{{AniListID: 999}}},
		Matcher:  match.NewMatcher(degradedMatcherAniList{}, scoutTestLogger()),
		Comparer: compare.NewComparer(compare.Config{}),
		Notifier: notify.NewNotifier(scoutTestLogger(), nil),
	})

	if healthy := s.Cycle(context.Background()); !healthy {
		t.Fatal("Cycle healthy=false, want true (stale map + AniList degradation is degraded, not unhealthy)")
	}
	if n := recorder.CountExact("cycle degraded"); n != 1 {
		t.Errorf("'cycle degraded' count = %d, want exactly 1", n)
	}
	if reasons := degradedReasons(recorder); len(reasons) != 1 || reasons[0] != "anilist-degraded" {
		t.Errorf("degraded reasons = %v, want [anilist-degraded] (the AniList arm outranks mapping-stale)", reasons)
	}
	if got, ok := recordAttr(recorder, "cycle degraded", "consecutive_anilist_degraded"); !ok || got != "1" {
		t.Errorf("completion-line consecutive_anilist_degraded = %q (found=%t), want \"1\" (the post-update streak)", got, ok)
	}
}

// TestCycleShutdownAfterShrunkenWalkKeepsWarnOmitsCompletionLine pins the
// shrink arm's shutdown guard: a SIGTERM landing after a completed shrunken
// walk (cancelling the SeaDex fetch) keeps the shrink WARN and the persisted
// streak - the shrink evidence comes from the completed walk - but must NOT
// emit the "cycle degraded" completion line (an interrupted cycle did not
// complete), mirroring the walk-failed arm's no-completion-line rule.
func TestCycleShutdownAfterShrunkenWalkKeepsWarnOmitsCompletionLine(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger, recorder := capture.New()
	store := &fakeStore{st: state.State{
		Mapping: frierenMappingCache(),
		Library: library.Snapshot{Items: []library.Item{
			{Arr: library.ArrSonarr, ArrID: 1, Title: "A"},
			{Arr: library.ArrSonarr, ArrID: 2, Title: "B"},
			{Arr: library.ArrSonarr, ArrID: 3, Title: "C"},
			{Arr: library.ArrSonarr, ArrID: 4, Title: "D"},
		}},
	}}
	// 1 series against a prior of 4 trips the shrink guard; the SeaDex fetch
	// then cancels the cycle context, modelling the SIGTERM landing between
	// the completed walk and the fetch.
	sonarr := &fakeSonarr{series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}}}
	s := New(&Deps{
		Logger:  logger,
		Store:   store,
		Library: arrwalk.NewWalker(&arrwalk.Config{Sonarr: sonarr, Logger: scoutTestLogger()}),
		Mapping: fakeMapping{},
		SeaDex:  &cancellingSeaDex{cancel: cancel},
	})

	if healthy := s.Cycle(ctx); !healthy {
		t.Fatal("Cycle healthy=false, want true (a shrunken walk is degraded, not unhealthy)")
	}
	if n := recorder.CountLevel(slog.LevelWarn, "library walk shrank below half this arr's prior snapshot"); n != 1 {
		t.Errorf("shrink WARN count = %d, want 1 (the shrink evidence comes from the completed walk)", n)
	}
	if n := recorder.CountExact("cycle degraded"); n != 0 {
		t.Errorf("'cycle degraded' count = %d, want 0 (a shutdown after the shrunken walk interrupted the cycle; no completion line)", n)
	}
	if store.st.ShrunkWalksByArr[library.ArrSonarr] != 1 {
		t.Errorf("persisted ShrunkWalksByArr = %v, want {sonarr: 1} (the streak persists across the shutdown)", store.st.ShrunkWalksByArr)
	}
}

// TestCycleAniListEscalationFiresWhenPartialWalkWinsCompletionLine pins the
// documented interaction cell of recordAniListDegradation: the escalation
// fires on EVERY completed AniList-degraded cycle at the threshold, INCLUDING
// one whose completion line the partial-walk switch arm wins - otherwise a
// sustained AniList outage coexisting with a persistent partial walk would
// advance the streak forever without ever alerting. The streak must advance
// and persist, the ERROR must fire, and the completion line stays
// partial-walk (the switch's first arm).
func TestCycleAniListEscalationFiresWhenPartialWalkWinsCompletionLine(t *testing.T) {
	logger, recorder := capture.New()
	store := &fakeStore{st: state.State{
		Mapping: mapping.Cache{FetchedAt: time.Now(), Records: []mapping.Record{
			{AniListID: 154587, Type: "TV", TvdbID: 123, SeasonTvdb: 1},
			// Id-less record: the entry NEEDS the AniList title lookup, which
			// fails transiently this cycle, so result.Degraded is true.
			{AniListID: 333, Type: "TV"},
		}},
		AniListDegraded: degradation.ReconcileEscalationThreshold - 1,
	}}
	sonarr := &flakySonarr{
		fakeSonarr: fakeSonarr{
			series: []arrapi.Series{
				{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023},
				{ID: 8, Title: "Broken Series", TvdbID: 124, Year: 2024},
			},
			files: map[int][]arrapi.EpisodeFile{
				7: {{SeasonNumber: 1, ReleaseGroup: "Erai-raws"}},
			},
		},
		failEpisodes: map[int]bool{8: true},
	}
	s := New(&Deps{
		Logger:   logger,
		Store:    store,
		Library:  arrwalk.NewWalker(&arrwalk.Config{Sonarr: sonarr, Logger: scoutTestLogger()}),
		Mapping:  fakeMapping{},
		SeaDex:   &fakeSeaDex{entries: []seadex.Entry{{AniListID: 333}}},
		Matcher:  match.NewMatcher(degradedMatcherAniList{}, scoutTestLogger()),
		Comparer: compare.NewComparer(compare.Config{}),
		Notifier: notify.NewNotifier(scoutTestLogger(), nil),
	})

	if healthy := s.Cycle(context.Background()); !healthy {
		t.Fatal("Cycle healthy=false, want true (partial walk + AniList degradation is degraded, not unhealthy)")
	}
	if reasons := degradedReasons(recorder); len(reasons) != 1 || reasons[0] != "partial-walk" {
		t.Errorf("degraded reasons = %v, want [partial-walk] (the switch's first arm wins the completion line)", reasons)
	}
	const escalationMsg = "anilist lookups degraded repeatedly; matching incomplete and findings frozen for affected entries - inspect graphql.anilist.co reachability and egress"
	if n := recorder.CountExact(escalationMsg); n != 1 {
		t.Errorf("escalation count = %d, want 1 (the escalation must fire even when the partial-walk arm wins the completion line)", n)
	}
	if n := recorder.CountLevel(slog.LevelError, escalationMsg); n != 1 {
		t.Errorf("escalation ERROR count = %d, want 1 (the operator-alert contract requires ERROR, not a same-message downgrade)", n)
	}
	if got := store.st.AniListDegraded; got != degradation.ReconcileEscalationThreshold {
		t.Errorf("persisted AniListDegraded = %d, want %d (the streak must advance and persist under the combined degradation)", got, degradation.ReconcileEscalationThreshold)
	}
}

// TestLoadMappingEscalatesOnTerminalNon2xxStreak pins l-f100 at the operator
// boundary: a terminal non-2xx on the fixed Fribb URL now advances the
// persisted rejection streak, so a permanently 404ing (or 410/500ing) upstream
// escalates the scout's mapping log from WARN to ERROR once the streak reaches
// degradation.TickEscalationThreshold consecutive cycles. Before this it warned
// forever from a frozen zero streak. The below-threshold row is the transient
// filter the user's deliberate no-status-allowlist decision relies on: a
// temporary status incident cannot survive that many cycles, so it never
// reaches ERROR.
func TestLoadMappingEscalatesOnTerminalNon2xxStreak(t *testing.T) {
	for name, tc := range map[string]struct {
		status      int
		priorStreak int
		wantError   bool
	}{
		"404 below threshold stays WARN": {http.StatusNotFound, degradation.TickEscalationThreshold - 2, false},
		"404 at threshold escalates":     {http.StatusNotFound, degradation.TickEscalationThreshold - 1, true},
		"410 at threshold escalates":     {http.StatusGone, degradation.TickEscalationThreshold - 1, true},
		"500 at threshold escalates":     {http.StatusInternalServerError, degradation.TickEscalationThreshold - 1, true},
	} {
		t.Run(name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "boom", tc.status)
			}))
			defer ts.Close()
			logger, recorder := capture.New()
			st := state.State{Mapping: mapping.Cache{
				FetchedAt:         time.Now().Add(-2 * time.Hour),
				Records:           []mapping.Record{{AniListID: 1, Type: "TV", TvdbID: 100}},
				RejectedRefreshes: tc.priorStreak,
			}}
			s := New(&Deps{
				Logger:  logger,
				Mapping: mapping.NewLoader(ts.Client(), ts.URL, "", time.Hour, scoutTestLogger()),
			})

			mapCache, _, mapErr := s.loadMapping(context.Background(), &st)
			if mapErr == nil {
				t.Fatal("loadMapping with a terminal non-2xx returned nil error, want the degraded stale map")
			}
			if mapCache.RejectedRefreshes != tc.priorStreak+1 {
				t.Errorf("RejectedRefreshes = %d, want %d (a terminal non-2xx advances the streak)", mapCache.RejectedRefreshes, tc.priorStreak+1)
			}
			warns := recorder.CountLevel(slog.LevelWarn, "mapping degraded")
			errs := recorder.CountLevel(slog.LevelError, "mapping degraded")
			if tc.wantError {
				if errs != 1 || warns != 0 {
					t.Errorf("escalated log counts: ERROR=%d WARN=%d, want exactly one ERROR and no WARN", errs, warns)
				}
			} else if warns != 1 || errs != 0 {
				t.Errorf("below-threshold log counts: WARN=%d ERROR=%d, want exactly one WARN and no ERROR", warns, errs)
			}
		})
	}
}
