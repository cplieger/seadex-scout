package scout

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/arrapi"
	"github.com/cplieger/seadex-scout/internal/anilist"
	"github.com/cplieger/seadex-scout/internal/compare"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/match"
	"github.com/cplieger/seadex-scout/internal/report"
	"github.com/cplieger/seadex-scout/internal/seadex"
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

func (degradedMatcherAniList) FetchMany(context.Context, []int) (map[int]anilist.Media, error) {
	return nil, context.DeadlineExceeded
}

type notFoundAniList struct{}

func (notFoundAniList) Fetch(context.Context, int) (anilist.Media, error) {
	return anilist.Media{}, anilist.ErrNotFound
}

func (notFoundAniList) FetchMany(context.Context, []int) (map[int]anilist.Media, error) {
	return map[int]anilist.Media{}, nil
}

// TestCycleMappingUnusablePreservesFindings pins the unusable-map degrade
// branch: when the mapping refresh yields zero usable records (idx.Len()==0)
// the cycle is degraded-but-healthy, saves only the refreshed library snapshot,
// and leaves prior findings untouched (never falsely resolved).
func TestCycleMappingUnusablePreservesFindings(t *testing.T) {
	mapSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("[]"))
	}))
	defer mapSrv.Close()

	logger := scoutTestLogger()
	prior := report.Alerted{
		AlertedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Finding:   report.StoredFinding{Title: "Existing", Status: compare.StatusBetter, AniListID: 154587},
	}
	store := &fakeStore{st: state.State{
		Findings:  map[string]report.Alerted{"prior": prior},
		Baselined: true,
	}}
	sonarr := &fakeSonarr{series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}}}
	s := New(&Deps{
		Logger:  logger,
		Store:   store,
		Library: library.NewWalker(&library.Config{Sonarr: sonarr, Logger: logger}),
		Mapping: mapping.NewLoader(mapSrv.Client(), mapSrv.URL, filepath.Join(t.TempDir(), "ov.json"), time.Hour, logger),
		SeaDex:  &fakeSeaDex{entries: []seadex.Entry{{AniListID: 154587}}},
	})

	if healthy := s.Cycle(context.Background()); !healthy {
		t.Fatal("Cycle healthy=false, want true when the map is unusable (degraded, not unhealthy)")
	}
	loaded := store.st
	if _, ok := loaded.Findings["prior"]; !ok {
		t.Errorf("prior finding not preserved on unusable-map cycle: %+v", loaded.Findings)
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
	mapSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("[]"))
	}))
	defer mapSrv.Close()

	logger := scoutTestLogger()
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"), logger)
	if err := store.Save(context.Background(), &state.State{Baselined: true}); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	sonarr := &fakeSonarr{series: []arrapi.Series{{ID: 7, Title: "Frieren", TitleSlug: "frieren", TvdbID: 123, Year: 2023}}}
	s := New(&Deps{
		Logger: logger,
		Store:  store,
		Library: library.NewWalker(&library.Config{
			Sonarr: sonarr, Logger: logger, SonarrURL: "https://user:pass@sonarr.example",
		}),
		Mapping: mapping.NewLoader(mapSrv.Client(), mapSrv.URL, filepath.Join(t.TempDir(), "ov.json"), time.Hour, logger),
		SeaDex:  &fakeSeaDex{entries: []seadex.Entry{{AniListID: 154587}}},
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
// and emits normally, the affected entry's prior finding is carried forward
// un-resolved with its original alert time (its absence from the compare is
// missing data, not alignment), and the cycle closes healthy with the "cycle
// degraded" completion line (reason anilist-degraded) the deployed deadman
// counts - never "cycle complete".
func TestCycleAniListDegradedComparesMajorityAndPreservesAffected(t *testing.T) {
	logger, recorder := capture.New()
	oldTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	prior := report.Alerted{
		AlertedAt: oldTime,
		Finding:   report.StoredFinding{Title: "Idless Show", Status: compare.StatusBetter, AniListID: 222},
	}
	store := &fakeStore{st: state.State{
		Mapping: mapping.Cache{FetchedAt: time.Now(), Records: []mapping.Record{
			{AniListID: 154587, Type: "TV", TvdbID: 123, SeasonTvdb: 1},
			// Id-less record (a split AniList<->arr mapping): the entry NEEDS
			// the AniList title lookup, which fails transiently this cycle.
			{AniListID: 222, Type: "TV"},
		}},
		Findings:  map[string]report.Alerted{"prior-idless": prior},
		Baselined: true,
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
		Library:      library.NewWalker(&library.Config{Sonarr: sonarr, Logger: scoutTestLogger()}),
		Mapping:      fakeMapping{},
		SeaDex:       &fakeSeaDex{entries: entries},
		Matcher:      match.NewMatcher(degradedMatcherAniList{}, scoutTestLogger()),
		Comparer:     compare.NewComparer(compare.Config{}),
		Reporter:     report.NewReporter(logger),
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
	// The affected entry's prior finding must be preserved, never resolved.
	if n := recorder.CountExact("finding resolved"); n != 0 {
		t.Errorf("resolved count = %d, want 0 (the affected entry's absence is missing data, not alignment)", n)
	}
	preserved, ok := store.st.Findings["prior-idless"]
	if !ok {
		t.Fatalf("affected entry's prior finding was dropped: %+v", store.st.Findings)
	}
	if !preserved.AlertedAt.Equal(oldTime) {
		t.Errorf("preserved AlertedAt = %s, want the original %s", preserved.AlertedAt, oldTime)
	}
	if len(store.st.Findings) != 2 {
		t.Errorf("persisted findings = %d, want 2 (the majority's new finding plus the preserved one)", len(store.st.Findings))
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

// TestCycleAniListDegradedColdStartSeedsIncompleteBaseline pins the baseline
// completeness flag for the scoped AniList degradation: a cold start whose
// first match is AniList-degraded seeds silently and records the baseline as
// incomplete (the affected entries' would-be findings are missing from the
// seed and must not burst as fresh notifications when the upstream recovers)
// - the same window a partial first walk opens - and the first complete
// cycle closes it.
func TestCycleAniListDegradedColdStartSeedsIncompleteBaseline(t *testing.T) {
	logger := scoutTestLogger()
	store := &fakeStore{st: state.State{
		Mapping: mapping.Cache{FetchedAt: time.Now(), Records: []mapping.Record{{AniListID: 111, Type: "TV", TvdbID: 123}}},
	}}
	sonarr := &fakeSonarr{
		series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}},
		files: map[int][]arrapi.EpisodeFile{
			7: {{SeasonNumber: 1, ReleaseGroup: "Erai-raws"}},
		},
	}
	deps := func(matcher *match.Matcher) *Deps {
		return &Deps{
			Logger:       logger,
			Store:        store,
			Library:      library.NewWalker(&library.Config{Sonarr: sonarr, Logger: logger}),
			Mapping:      fakeMapping{},
			SeaDex:       &fakeSeaDex{entries: []seadex.Entry{{AniListID: 999}}},
			Matcher:      matcher,
			Comparer:     compare.NewComparer(compare.Config{}),
			Reporter:     report.NewReporter(logger),
			AniListStats: aniStatsFn(anilist.NewClient(noNetworkClient(), "http://unused.invalid/gql", 1, logger)),
		}
	}

	// Cycle one: the entry's needed lookup fails transiently, so the seed is
	// incomplete and the window opens.
	if healthy := New(deps(match.NewMatcher(degradedMatcherAniList{}, logger))).Cycle(context.Background()); !healthy {
		t.Fatal("degraded cold-start cycle healthy=false, want true")
	}
	if !store.st.Baselined || !store.st.BaselineIncomplete {
		t.Errorf("state after AniList-degraded cold start: Baselined=%v BaselineIncomplete=%v, want true/true",
			store.st.Baselined, store.st.BaselineIncomplete)
	}

	// Cycle two: AniList answers definitively (not-found), the cycle
	// completes, and the window closes.
	if healthy := New(deps(match.NewMatcher(notFoundAniList{}, logger))).Cycle(context.Background()); !healthy {
		t.Fatal("recovered cycle healthy=false, want true")
	}
	if !store.st.Baselined || store.st.BaselineIncomplete {
		t.Errorf("state after the recovered cycle: Baselined=%v BaselineIncomplete=%v, want true/false",
			store.st.Baselined, store.st.BaselineIncomplete)
	}
}

// TestCycleColdStartBaselinesSilently pins the cold-start Baseline branch: a
// fresh instance (no baselined findings yet) must seed the dedupe table WITHOUT
// emitting any per-finding notification, so a pre-existing backlog is not dumped
// as a burst of alerts. The captured log distinguishes the Baseline path from
// the steady-state Report path.
func TestCycleColdStartBaselinesSilently(t *testing.T) {
	captureLogger, recorder := capture.New()
	reporter := report.NewReporter(captureLogger)
	logger := scoutTestLogger()
	store := &fakeStore{st: state.State{
		Mapping: mapping.Cache{FetchedAt: time.Now(), Records: []mapping.Record{{AniListID: 154587, Type: "TV", TvdbID: 123, SeasonTvdb: 1}}},
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
		Library:      library.NewWalker(&library.Config{Sonarr: sonarr, Logger: logger}),
		Mapping:      fakeMapping{},
		SeaDex:       &fakeSeaDex{entries: seadexFrierenEntry()},
		Matcher:      match.NewMatcher(notFoundAniList{}, logger),
		Comparer:     compare.NewComparer(compare.Config{}),
		Reporter:     reporter,
		AniListStats: aniStatsFn(anilist.NewClient(noNetworkClient(), "http://unused.invalid/gql", 1, logger)),
	})

	if healthy := s.Cycle(context.Background()); !healthy {
		t.Fatal("Cycle healthy=false, want true on a successful cold-start cycle")
	}
	if n := recorder.CountExact("better release available"); n != 0 {
		t.Errorf("cold start emitted %d finding notifications, want 0 (backlog must be baselined silently)", n)
	}
	if n := recorder.CountExact("findings reported"); n != 0 {
		t.Errorf("cold start took the Report path (%d 'findings reported'), want the Baseline path", n)
	}
	if n := recorder.CountExact("cold start: findings baselined without notifying"); n != 1 {
		t.Errorf("cold-start baseline summary count = %d, want 1", n)
	}
	loaded := store.st
	if !loaded.Baselined {
		t.Error("state Baselined=false after cold start, want true")
	}
	if loaded.BaselineIncomplete {
		t.Error("state BaselineIncomplete=true after a complete cold-start walk, want false")
	}
	if len(loaded.Findings) == 0 {
		t.Error("cold start did not baseline the current finding into the dedupe table")
	}
}

// TestCycleEmptySeaDexEntriesPreservesFindings pins the anomalous empty-but-non-
// error SeaDex response path: a successful fetch with totalPages=1 and an empty
// items array must preserve prior findings and NOT run Reporter.Report (which
// would emit a "finding resolved" line for the prior finding), while still
// refreshing the library snapshot and staying healthy.
func TestCycleEmptySeaDexEntriesPreservesFindings(t *testing.T) {
	captureLogger, recorder := capture.New()
	reporter := report.NewReporter(captureLogger)
	logger := scoutTestLogger()
	prior := report.Alerted{
		AlertedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Finding:   report.StoredFinding{Title: "Existing", Status: compare.StatusBetter, AniListID: 154587},
	}
	store := &fakeStore{st: state.State{
		Mapping: mapping.Cache{
			FetchedAt: time.Now(),
			Records:   []mapping.Record{{AniListID: 154587, Type: "TV", TvdbID: 123, SeasonTvdb: 1}},
		},
		Findings:  map[string]report.Alerted{"prior": prior},
		Baselined: true,
	}}
	sonarr := &fakeSonarr{series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}}}
	s := New(&Deps{
		Logger:       logger,
		Store:        store,
		Library:      library.NewWalker(&library.Config{Sonarr: sonarr, Logger: logger}),
		Mapping:      fakeMapping{},
		SeaDex:       &fakeSeaDex{},
		Matcher:      match.NewMatcher(notFoundAniList{}, logger),
		Comparer:     compare.NewComparer(compare.Config{}),
		Reporter:     reporter,
		AniListStats: aniStatsFn(anilist.NewClient(noNetworkClient(), "http://unused.invalid/gql", 1, logger)),
	})

	if healthy := s.Cycle(context.Background()); !healthy {
		t.Fatal("Cycle healthy=false, want true when SeaDex returns an anomalous empty result")
	}
	loaded := store.st
	if _, ok := loaded.Findings["prior"]; !ok {
		t.Errorf("prior finding not preserved on empty-SeaDex cycle: %+v", loaded.Findings)
	}
	if len(loaded.Library.Items) != 1 || loaded.Library.Items[0].Title != "Frieren" {
		t.Errorf("library snapshot not refreshed: %+v", loaded.Library)
	}
	if n := recorder.CountExact("finding resolved"); n != 0 {
		t.Errorf("empty-SeaDex cycle emitted %d resolved finding logs, want 0", n)
	}
	if n := recorder.CountExact("findings reported"); n != 0 {
		t.Errorf("empty-SeaDex cycle ran Reporter.Report %d times, want 0", n)
	}
}

// TestHandlePreCompareGateEmptyWalkPreservesPriorSnapshot pins the successful-
// but-empty library-walk gate: a walk returning zero items while the prior
// snapshot had items must stop the compare (mass-resolve guard) without
// persisting the empty snapshot (the one-cycle ratchet).
func TestHandlePreCompareGateEmptyWalkPreservesPriorSnapshot(t *testing.T) {
	logger := scoutTestLogger()
	prior := report.Alerted{AlertedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Finding: report.StoredFinding{Title: "Existing"}}
	st := state.State{Library: library.Snapshot{Items: []library.Item{{ArrID: 7, Title: "Frieren"}}}, Findings: map[string]report.Alerted{"prior": prior}, Baselined: true}
	store := &fakeStore{st: st}
	s := New(&Deps{Logger: logger, Store: store})
	handled, healthy := s.handlePreCompareGate(context.Background(), &st, library.Snapshot{}, &mapping.Cache{}, []seadex.Entry{{AniListID: 1}}, nil, nil, nil)
	if !handled || !healthy {
		t.Errorf("handlePreCompareGate = (%v, %v), want (true, true)", handled, healthy)
	}
	if store.saves != 1 {
		t.Fatalf("saves = %d, want 1 (the gate persists the refreshed mapping cache)", store.saves)
	}
	loaded := store.st
	if len(loaded.Library.Items) != 1 || loaded.Library.Items[0].Title != "Frieren" {
		t.Errorf("persisted library = %+v, want prior non-empty snapshot", loaded.Library)
	}
	if _, ok := loaded.Findings["prior"]; !ok {
		t.Errorf("persisted findings = %+v, want prior finding preserved", loaded.Findings)
	}
}

// TestCyclePartialWalkComparesCleanAndPreservesFailedItemsFindings pins the
// per-item Partial-aware compare: a walk where one series' episode fetch
// failed (a Failed placeholder item) and one walked cleanly must still run the
// compare on the clean item (its finding is emitted), preserve the failed
// item's prior finding unresolved (its absence from the compare result is
// missing data, not alignment), and close the cycle with the "cycle degraded"
// completion line (reason partial-walk) instead of "cycle complete".
func TestCyclePartialWalkComparesCleanAndPreservesFailedItemsFindings(t *testing.T) {
	logger, recorder := capture.New()
	oldTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	prior := report.Alerted{
		AlertedAt: oldTime,
		Finding:   report.StoredFinding{Title: "Broken Series", Status: compare.StatusBetter, AniListID: 222},
	}
	store := &fakeStore{st: state.State{
		Mapping: mapping.Cache{FetchedAt: time.Now(), Records: []mapping.Record{
			{AniListID: 154587, Type: "TV", TvdbID: 123, SeasonTvdb: 1},
			{AniListID: 222, Type: "TV", TvdbID: 124, SeasonTvdb: 1},
		}},
		Findings:  map[string]report.Alerted{"prior-failed": prior},
		Baselined: true,
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
		Library:      library.NewWalker(&library.Config{Sonarr: sonarr, Logger: scoutTestLogger()}),
		Mapping:      fakeMapping{},
		SeaDex:       &fakeSeaDex{entries: entries},
		Matcher:      match.NewMatcher(notFoundAniList{}, scoutTestLogger()),
		Comparer:     compare.NewComparer(compare.Config{}),
		Reporter:     report.NewReporter(logger),
		AniListStats: aniStatsFn(anilist.NewClient(noNetworkClient(), "http://unused.invalid/gql", 1, scoutTestLogger())),
	})

	if healthy := s.Cycle(context.Background()); !healthy {
		t.Fatal("Cycle healthy=false, want true (a partial walk is degraded, not unhealthy)")
	}
	if n := recorder.CountExact("better release available"); n != 1 {
		t.Errorf("clean item's finding notification count = %d, want 1 (the compare must run on the clean subset)", n)
	}
	if n := recorder.CountExact("finding resolved"); n != 0 {
		t.Errorf("resolved notification count = %d, want 0 (the failed item's finding must not resolve)", n)
	}
	if n := recorder.CountExact("cycle degraded"); n != 1 {
		t.Errorf("'cycle degraded' count = %d, want 1 (the partial walk's completion line)", n)
	}
	if n := recorder.CountExact("cycle complete"); n != 0 {
		t.Errorf("'cycle complete' count = %d, want 0 on a partial walk", n)
	}
	loaded := store.st
	preserved, ok := loaded.Findings["prior-failed"]
	if !ok {
		t.Fatalf("failed item's prior finding was resolved, want it preserved: %+v", loaded.Findings)
	}
	if !preserved.AlertedAt.Equal(oldTime) {
		t.Errorf("preserved finding AlertedAt = %s, want original %s", preserved.AlertedAt, oldTime)
	}
	if len(loaded.Findings) != 2 {
		t.Errorf("persisted findings = %d, want 2 (the new clean finding plus the preserved one)", len(loaded.Findings))
	}
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

// TestCyclePartialColdStartSeedsIncompleteBaseline pins the cold-start
// baseline's completeness contract across the incomplete-baseline window: a
// fresh install whose FIRST completed walk is partial seeds the clean subset
// silently and records the baseline as incomplete (state.BaselineIncomplete),
// so the next complete walk seeds the previously-failed series' pre-existing
// backlog silently too (no notification burst) and clears the flag - and only
// then does normal reporting resume, with a genuinely new finding notifying.
func TestCyclePartialColdStartSeedsIncompleteBaseline(t *testing.T) {
	logger, recorder := capture.New()
	store := &fakeStore{st: state.State{
		Mapping: mapping.Cache{FetchedAt: time.Now(), Records: []mapping.Record{
			{AniListID: 154587, Type: "TV", TvdbID: 123, SeasonTvdb: 1},
			{AniListID: 222, Type: "TV", TvdbID: 124, SeasonTvdb: 1},
			{AniListID: 333, Type: "TV", TvdbID: 125, SeasonTvdb: 1},
		}},
	}}
	sonarr := &flakySonarr{
		fakeSonarr: fakeSonarr{
			series: []arrapi.Series{
				{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023},
				{ID: 8, Title: "Broken Series", TvdbID: 124, Year: 2024},
				{ID: 9, Title: "Third Show", TvdbID: 125, Year: 2025},
			},
			files: map[int][]arrapi.EpisodeFile{
				7: {{SeasonNumber: 1, ReleaseGroup: "Erai-raws"}},
				8: {{SeasonNumber: 1, ReleaseGroup: "Erai-raws"}},
				9: {{SeasonNumber: 1, ReleaseGroup: "Erai-raws"}},
			},
		},
		failEpisodes: map[int]bool{8: true},
	}
	seaDex := &fakeSeaDex{entries: append(seadexFrierenEntry(), seadex.Entry{
		AniListID: 222,
		Torrents: []seadex.Torrent{{
			ReleaseGroup: "SubsPlease",
			Tracker:      "Nyaa",
			InfoHash:     "def",
			URL:          "https://nyaa.si/view/2",
			IsBest:       true,
			Files:        []seadex.File{{Name: "Broken Series S01E01 1080p.mkv", Length: 1}},
		}},
	})}
	s := New(&Deps{
		Logger:       scoutTestLogger(),
		Store:        store,
		Library:      library.NewWalker(&library.Config{Sonarr: sonarr, Logger: scoutTestLogger()}),
		Mapping:      fakeMapping{},
		SeaDex:       seaDex,
		Matcher:      match.NewMatcher(notFoundAniList{}, scoutTestLogger()),
		Comparer:     compare.NewComparer(compare.Config{}),
		Reporter:     report.NewReporter(logger),
		AniListStats: aniStatsFn(anilist.NewClient(noNetworkClient(), "http://unused.invalid/gql", 1, scoutTestLogger())),
	})

	// Cycle one: partial first walk (series 8's episode fetch fails). The
	// clean subset seeds silently and the baseline is recorded incomplete.
	if healthy := s.Cycle(context.Background()); !healthy {
		t.Fatal("cycle one healthy=false, want true (a partial walk is degraded, not unhealthy)")
	}
	if !store.st.Baselined || !store.st.BaselineIncomplete {
		t.Errorf("state after partial cold start: Baselined=%v BaselineIncomplete=%v, want true/true (seeded, recorded incomplete)",
			store.st.Baselined, store.st.BaselineIncomplete)
	}
	if len(store.st.Findings) != 1 {
		t.Errorf("seeded findings after partial cold start = %d, want 1 (the clean item)", len(store.st.Findings))
	}
	if n := recorder.CountExact("better release available"); n != 0 {
		t.Errorf("partial cold-start cycle emitted %d finding notifications, want 0", n)
	}

	// Cycle two: the failed series recovers, the walk is complete. Its
	// pre-existing backlog seeds silently (NOT a notification burst) and the
	// completing walk clears the incomplete flag.
	sonarr.failEpisodes = nil
	if healthy := s.Cycle(context.Background()); !healthy {
		t.Fatal("cycle two healthy=false, want true on a complete walk")
	}
	if !store.st.Baselined || store.st.BaselineIncomplete {
		t.Errorf("state after the completing walk: Baselined=%v BaselineIncomplete=%v, want true/false (baseline complete)",
			store.st.Baselined, store.st.BaselineIncomplete)
	}
	if len(store.st.Findings) != 2 {
		t.Errorf("baselined findings after the completing walk = %d, want 2 (the recovered backlog seeded)", len(store.st.Findings))
	}
	if n := recorder.CountExact("better release available"); n != 0 {
		t.Errorf("completing walk emitted %d finding notifications, want 0 (the recovered series' backlog must seed silently)", n)
	}
	if n := recorder.CountExact("finding resolved"); n != 0 {
		t.Errorf("baseline window emitted %d resolved lines, want 0 (nothing was ever emitted to resolve)", n)
	}
	if n := recorder.CountExact("findings reported"); n != 0 {
		t.Errorf("baseline window took the Report path %d times, want 0", n)
	}
	if n := recorder.CountExact("cold start: findings baselined without notifying"); n != 2 {
		t.Errorf("baseline summary count = %d, want 2 (the partial seed and the completing seed)", n)
	}

	// Cycle three: steady state. A genuinely new finding (a new SeaDex entry
	// for an item already in the library) must now notify via Report.
	seaDex.entries = append(seaDex.entries, seadex.Entry{
		AniListID: 333,
		Torrents: []seadex.Torrent{{
			ReleaseGroup: "SubsPlease",
			Tracker:      "Nyaa",
			InfoHash:     "ghi",
			URL:          "https://nyaa.si/view/3",
			IsBest:       true,
			Files:        []seadex.File{{Name: "Third Show S01E01 1080p.mkv", Length: 1}},
		}},
	})
	if healthy := s.Cycle(context.Background()); !healthy {
		t.Fatal("cycle three healthy=false, want true on a steady-state cycle")
	}
	if n := recorder.CountExact("findings reported"); n != 1 {
		t.Errorf("'findings reported' count = %d, want 1 (normal reporting resumes after the baseline completes)", n)
	}
	if n := recorder.CountExact("better release available"); n != 1 {
		t.Errorf("new finding notification count = %d, want 1 (only the genuinely new finding emits)", n)
	}
	if len(store.st.Findings) != 3 {
		t.Errorf("persisted findings after the steady-state cycle = %d, want 3", len(store.st.Findings))
	}
}

// TestHandlePreCompareGateShrunkWalkEscalatesAfterRepeatedShrinks pins the
// WARN-to-ERROR escalation of the single shrunk-walk log site (mirroring the
// mapping guard's): below the threshold a below-half walk logs at WARN; once
// the persisted streak reaches shrunkWalkEscalationThreshold the same site
// logs at ERROR (firing the existing SeadexScoutCycleError Loki rule) -
// exactly one line either way, never auto-accepting, with the prior snapshot
// and findings preserved and the streak persisted.
func TestHandlePreCompareGateShrunkWalkEscalatesAfterRepeatedShrinks(t *testing.T) {
	tests := []struct {
		name        string
		priorStreak int
		wantError   bool
	}{
		{name: "below threshold stays WARN", priorStreak: shrunkWalkEscalationThreshold - 2, wantError: false},
		{name: "at threshold escalates to ERROR", priorStreak: shrunkWalkEscalationThreshold - 1, wantError: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logger, recorder := capture.New()
			prior := report.Alerted{AlertedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Finding: report.StoredFinding{Title: "Existing"}}
			st := state.State{
				Library: library.Snapshot{Items: []library.Item{
					{ArrID: 1, Title: "A"}, {ArrID: 2, Title: "B"}, {ArrID: 3, Title: "C"}, {ArrID: 4, Title: "D"},
				}},
				Findings:    map[string]report.Alerted{"prior": prior},
				ShrunkWalks: tc.priorStreak,
				Baselined:   true,
			}
			store := &fakeStore{st: st}
			s := New(&Deps{Logger: logger, Store: store})
			// 1 item against a prior of 4: 1*2 < 4 trips the shrink guard.
			snap := library.Snapshot{Items: []library.Item{{ArrID: 1, Title: "A"}}}
			mapCache := mapping.Cache{Records: []mapping.Record{{AniListID: 154587, TvdbID: 123}}}

			handled, healthy := s.handlePreCompareGate(context.Background(), &st, snap, &mapCache, []seadex.Entry{{AniListID: 154587}}, nil, nil, nil)
			if !handled || !healthy {
				t.Errorf("handlePreCompareGate = (%v, %v), want (true, true)", handled, healthy)
			}
			if store.saves != 1 {
				t.Fatalf("saves = %d, want 1 (the guard persists the mapping cache and the streak)", store.saves)
			}
			loaded := store.st
			if loaded.ShrunkWalks != tc.priorStreak+1 {
				t.Errorf("persisted ShrunkWalks = %d, want %d", loaded.ShrunkWalks, tc.priorStreak+1)
			}
			if len(loaded.Library.Items) != 4 {
				t.Errorf("persisted library = %d items, want the prior 4 (a shrunken walk is never auto-accepted)", len(loaded.Library.Items))
			}
			if _, ok := loaded.Findings["prior"]; !ok {
				t.Errorf("persisted findings = %+v, want prior finding preserved", loaded.Findings)
			}
			var warns, errs int
			for _, r := range recorder.Records() {
				switch {
				case r.Level == slog.LevelError && strings.HasPrefix(r.Message, "library walk shrank"):
					errs++
				case r.Level == slog.LevelWarn && strings.HasPrefix(r.Message, "library walk shrank"):
					warns++
				}
			}
			if tc.wantError {
				if errs != 1 || warns != 0 {
					t.Errorf("escalated log counts: ERROR=%d WARN=%d, want exactly one ERROR and no WARN (single log site)", errs, warns)
				}
			} else if warns != 1 || errs != 0 {
				t.Errorf("below-threshold log counts: WARN=%d ERROR=%d, want exactly one WARN and no ERROR", warns, errs)
			}
			if n := recorder.CountExact("cycle degraded"); n != 1 {
				t.Errorf("'cycle degraded' count = %d, want 1 (the shrink guard's completion line)", n)
			}
			if reasons := degradedReasons(recorder); len(reasons) != 1 || reasons[0] != "library-shrunk" {
				t.Errorf("degraded reasons = %v, want [library-shrunk]", reasons)
			}
		})
	}
}

// TestHandlePreCompareGateShrunkWalkWithSeaDexOutageWarnsFeedKept pins the
// shrink-guard arm's feed-outage contract: a library-shrink + SeaDex double
// outage with a feed configured must still emit the feed-kept WARN so it does
// not read as shrink-only in Loki, while the shrink guard's own degraded
// completion line and no-rebuild behavior are unchanged.
func TestHandlePreCompareGateShrunkWalkWithSeaDexOutageWarnsFeedKept(t *testing.T) {
	logger, recorder := capture.New()
	feed := &fakeFeed{}
	st := state.State{
		Library: library.Snapshot{Items: []library.Item{
			{ArrID: 1, Title: "A"}, {ArrID: 2, Title: "B"}, {ArrID: 3, Title: "C"}, {ArrID: 4, Title: "D"},
		}},
		Baselined: true,
	}
	store := &fakeStore{st: st}
	s := New(&Deps{Logger: logger, Store: store, Feed: feed})
	snap := library.Snapshot{Items: []library.Item{{ArrID: 1, Title: "A"}}}
	mapCache := mapping.Cache{}

	handled, healthy := s.handlePreCompareGate(context.Background(), &st, snap, &mapCache, nil, nil, nil, errors.New("seadex down"))
	if !handled || !healthy {
		t.Errorf("handlePreCompareGate = (%v, %v), want (true, true)", handled, healthy)
	}
	if n := recorder.CountExact("seadex fetch failed; indexer feed kept previous feed"); n != 1 {
		t.Errorf("feed-kept WARN count = %d, want 1 (a shrink + SeaDex double outage must not read as shrink-only)", n)
	}
	if reasons := degradedReasons(recorder); len(reasons) != 1 || reasons[0] != "library-shrunk" {
		t.Errorf("degraded reasons = %v, want [library-shrunk]", reasons)
	}
	if feed.calls != 0 {
		t.Errorf("feed Rebuild calls = %d, want 0 (nothing to rebuild from)", feed.calls)
	}
}

// TestCycleRecoveredWalkResetsShrunkStreak pins the shrink guard's recovery
// rule: a fully-successful walk that passes the guard resets the persisted
// consecutive-shrunk-walk streak to zero, so normal resolution resumes and a
// later shrink starts a fresh streak.
func TestCycleRecoveredWalkResetsShrunkStreak(t *testing.T) {
	logger := scoutTestLogger()
	store := &fakeStore{st: state.State{
		Mapping:     mapping.Cache{FetchedAt: time.Now(), Records: []mapping.Record{{AniListID: 154587, Type: "TV", TvdbID: 123, SeasonTvdb: 1}}},
		Library:     library.Snapshot{Items: []library.Item{{Arr: library.ArrSonarr, ArrID: 7, Title: "Frieren", TvdbID: 123}}},
		ShrunkWalks: 3,
		Baselined:   true,
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
		Library:      library.NewWalker(&library.Config{Sonarr: sonarr, Logger: logger}),
		Mapping:      fakeMapping{},
		SeaDex:       &fakeSeaDex{entries: seadexFrierenEntry()},
		Matcher:      match.NewMatcher(notFoundAniList{}, logger),
		Comparer:     compare.NewComparer(compare.Config{}),
		Reporter:     report.NewReporter(logger),
		AniListStats: aniStatsFn(anilist.NewClient(noNetworkClient(), "http://unused.invalid/gql", 1, logger)),
	})

	if healthy := s.Cycle(context.Background()); !healthy {
		t.Fatal("Cycle healthy=false, want true on a recovered walk")
	}
	if store.st.ShrunkWalks != 0 {
		t.Errorf("persisted ShrunkWalks = %d, want 0 after a walk that passes the guard", store.st.ShrunkWalks)
	}
}

// TestCycleSeaDexFailureEscalatesAfterRepeatedFailures pins the WARN-to-ERROR
// escalation of the single seadex-fetch-failed log site (mirroring the
// shrunk-walk and mapping guards'): below the threshold a failed SeaDex fetch
// logs at WARN; on the 8th consecutive failure (the persisted streak reaching
// seadexFailureEscalationThreshold) the same site logs at ERROR (firing the
// existing SeadexScoutCycleError Loki rule) - exactly one line either way,
// with the streak persisted, prior findings preserved, and the "cycle
// degraded" completion line unchanged.
func TestCycleSeaDexFailureEscalatesAfterRepeatedFailures(t *testing.T) {
	tests := []struct {
		name        string
		priorStreak int
		wantError   bool
	}{
		{name: "below threshold stays WARN", priorStreak: seadexFailureEscalationThreshold - 2, wantError: false},
		{name: "8th consecutive failure escalates to ERROR", priorStreak: seadexFailureEscalationThreshold - 1, wantError: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logger, recorder := capture.New()
			prior := report.Alerted{
				AlertedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
				Finding:   report.StoredFinding{Title: "Existing", Status: compare.StatusBetter, AniListID: 154587},
			}
			store := &fakeStore{st: state.State{
				Mapping: mapping.Cache{
					FetchedAt: time.Now(),
					Records:   []mapping.Record{{AniListID: 154587, Type: "TV", TvdbID: 123, SeasonTvdb: 1}},
				},
				Findings:       map[string]report.Alerted{"prior": prior},
				SeadexFailures: tc.priorStreak,
				Baselined:      true,
			}}
			sonarr := &fakeSonarr{series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}}}
			s := New(&Deps{
				Logger:  logger,
				Store:   store,
				Library: library.NewWalker(&library.Config{Sonarr: sonarr, Logger: scoutTestLogger()}),
				Mapping: fakeMapping{},
				SeaDex:  &fakeSeaDex{err: errors.New("seadex down")},
			})

			if healthy := s.Cycle(context.Background()); !healthy {
				t.Fatal("Cycle healthy=false, want true (a SeaDex outage is degraded, not unhealthy)")
			}
			if got := store.st.SeadexFailures; got != tc.priorStreak+1 {
				t.Errorf("persisted SeadexFailures = %d, want %d (the streak must increment and persist)", got, tc.priorStreak+1)
			}
			if _, ok := store.st.Findings["prior"]; !ok {
				t.Errorf("persisted findings = %+v, want prior finding preserved", store.st.Findings)
			}
			var warns, errs int
			for _, r := range recorder.Records() {
				switch {
				case r.Level == slog.LevelError && strings.HasPrefix(r.Message, "seadex fetch failed"):
					errs++
				case r.Level == slog.LevelWarn && strings.HasPrefix(r.Message, "seadex fetch failed"):
					warns++
				}
			}
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

// TestCycleSuccessfulSeaDexFetchResetsFailureStreak pins the SeaDex failure
// streak's recovery rule: a cycle whose fetch succeeds resets the persisted
// consecutive-failure streak to zero (persisted by the cycle's closing save,
// no operator action), so a later outage starts a fresh streak instead of
// escalating on its first failed fetch.
func TestCycleSuccessfulSeaDexFetchResetsFailureStreak(t *testing.T) {
	logger := scoutTestLogger()
	store := &fakeStore{st: state.State{
		Mapping:        mapping.Cache{FetchedAt: time.Now(), Records: []mapping.Record{{AniListID: 154587, Type: "TV", TvdbID: 123, SeasonTvdb: 1}}},
		SeadexFailures: 3,
		Baselined:      true,
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
		Library:      library.NewWalker(&library.Config{Sonarr: sonarr, Logger: logger}),
		Mapping:      fakeMapping{},
		SeaDex:       &fakeSeaDex{entries: seadexFrierenEntry()},
		Matcher:      match.NewMatcher(notFoundAniList{}, logger),
		Comparer:     compare.NewComparer(compare.Config{}),
		Reporter:     report.NewReporter(logger),
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
		Mapping:        mapping.Cache{FetchedAt: time.Now(), Records: []mapping.Record{{AniListID: 154587, Type: "TV", TvdbID: 123, SeasonTvdb: 1}}},
		SeadexFailures: 3,
		Baselined:      true,
	}}
	sonarr := &fakeSonarr{series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}}}
	s := New(&Deps{
		Logger:  logger,
		Store:   store,
		Library: library.NewWalker(&library.Config{Sonarr: sonarr, Logger: logger}),
		Mapping: fakeMapping{},
		SeaDex:  &fakeSeaDex{},
	})

	if healthy := s.Cycle(context.Background()); !healthy {
		t.Fatal("Cycle healthy=false, want true (a zero-entries fetch is degraded, not unhealthy)")
	}
	if store.st.SeadexFailures != 0 {
		t.Errorf("persisted SeadexFailures = %d, want 0 (a zero-entries fetch is still a successful fetch; the documented contract resets the streak)", store.st.SeadexFailures)
	}
}

// TestCycleSteadyStateReportsAndSaves pins the daemon's steady-state operating
// mode end to end: an already-baselined instance must take the Report path (not
// Baseline), emit the new finding, resolve the stale prior finding, close with
// one "cycle complete" line, and persist the updated dedupe table.
func TestCycleSteadyStateReportsAndSaves(t *testing.T) {
	logger, recorder := capture.New()
	stale := report.Alerted{
		AlertedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Finding:   report.StoredFinding{Title: "Gone Title", Status: compare.StatusBetter, AniListID: 111},
	}
	store := &fakeStore{st: state.State{
		Mapping:   mapping.Cache{FetchedAt: time.Now(), Records: []mapping.Record{{AniListID: 154587, Type: "TV", TvdbID: 123, SeasonTvdb: 1}}},
		Findings:  map[string]report.Alerted{"stale": stale},
		Baselined: true,
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
		Library:      library.NewWalker(&library.Config{Sonarr: sonarr, Logger: scoutTestLogger()}),
		Mapping:      fakeMapping{},
		SeaDex:       &fakeSeaDex{entries: seadexFrierenEntry()},
		Matcher:      match.NewMatcher(notFoundAniList{}, scoutTestLogger()),
		Comparer:     compare.NewComparer(compare.Config{}),
		Reporter:     report.NewReporter(logger),
		AniListStats: aniStatsFn(anilist.NewClient(noNetworkClient(), "http://unused.invalid/gql", 1, scoutTestLogger())),
	})

	if healthy := s.Cycle(context.Background()); !healthy {
		t.Fatal("Cycle healthy=false, want true on a successful steady-state cycle")
	}
	if n := recorder.CountExact("cold start: findings baselined without notifying"); n != 0 {
		t.Errorf("steady-state cycle took the Baseline path %d times, want 0", n)
	}
	if n := recorder.CountExact("findings reported"); n != 1 {
		t.Errorf("'findings reported' count = %d, want 1 (the Report path)", n)
	}
	if n := recorder.CountExact("better release available"); n != 1 {
		t.Errorf("new finding notification count = %d, want 1", n)
	}
	if n := recorder.CountExact("finding resolved"); n != 1 {
		t.Errorf("resolved notification count = %d, want 1 (the stale prior finding)", n)
	}
	if n := recorder.CountExact("cycle complete"); n != 1 {
		t.Errorf("'cycle complete' count = %d, want 1", n)
	}
	loaded := store.st
	if _, ok := loaded.Findings["stale"]; ok {
		t.Error("stale finding still persisted after resolution")
	}
	if len(loaded.Findings) != 1 {
		t.Errorf("persisted findings = %d, want 1 (the new finding's dedupe entry)", len(loaded.Findings))
	}
	if !loaded.Baselined {
		t.Error("Baselined = false after a steady-state cycle, want true")
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

	if st.Baselined || len(st.Findings) != 0 || len(st.Library.Items) != 0 || len(st.Mapping.Records) != 0 {
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

// TestCycleShutdownDuringWalkWarnsNotErrors pins the redeploy log contract for
// the walk phase: a cycle cancelled mid-walk is unhealthy but must log the
// shutdown WARN, never the "library walk failed" ERROR that trips the
// SeadexScoutCycleError Loki alert on a routine redeploy.
func TestCycleShutdownDuringWalkWarnsNotErrors(t *testing.T) {
	logger, recorder := capture.New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &fakeStore{}
	s := New(&Deps{
		Logger:  logger,
		Store:   store,
		Library: library.NewWalker(&library.Config{Sonarr: &cancellingSonarr{cancel: cancel}, Logger: scoutTestLogger()}),
	})

	if healthy := s.Cycle(ctx); healthy {
		t.Fatal("Cycle healthy=true, want false when the walk is interrupted")
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

func (c *ctxCancellingAniList) FetchMany(context.Context, []int) (map[int]anilist.Media, error) {
	c.cancel()
	return nil, context.Canceled
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
	prior := report.Alerted{
		AlertedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Finding:   report.StoredFinding{Title: "Existing", Status: compare.StatusBetter, AniListID: 154587},
	}
	store := &fakeStore{st: state.State{
		Mapping:   mapping.Cache{FetchedAt: time.Now(), Records: []mapping.Record{{AniListID: 111, Type: "TV", TvdbID: 123}}},
		Findings:  map[string]report.Alerted{"prior": prior},
		Baselined: true,
	}}
	sonarr := &fakeSonarr{series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}}}
	s := New(&Deps{
		Logger:  logger,
		Store:   store,
		Library: library.NewWalker(&library.Config{Sonarr: sonarr, Logger: scoutTestLogger()}),
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
	if _, ok := store.st.Findings["prior"]; !ok {
		t.Errorf("prior finding not preserved on shutdown mid-matching: %+v", store.st.Findings)
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

func (c *cancellingSeaDex) FetchEntries(context.Context) ([]seadex.Entry, error) {
	c.cancel()
	return nil, context.Canceled
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
	prior := report.Alerted{
		AlertedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Finding:   report.StoredFinding{Title: "Existing", Status: compare.StatusBetter, AniListID: 154587},
	}
	store := &fakeStore{st: state.State{
		Mapping:   mapping.Cache{FetchedAt: time.Now(), Records: []mapping.Record{{AniListID: 111, Type: "TV", TvdbID: 123}}},
		Findings:  map[string]report.Alerted{"prior": prior},
		Baselined: true,
	}}
	sonarr := &fakeSonarr{series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}}}
	s := New(&Deps{
		Logger:  logger,
		Store:   store,
		Library: library.NewWalker(&library.Config{Sonarr: sonarr, Logger: scoutTestLogger()}),
		Mapping: fakeMapping{},
		SeaDex:  &cancellingSeaDex{cancel: cancel},
	})

	if healthy := s.Cycle(ctx); !healthy {
		t.Fatal("Cycle healthy=false, want true (a shutdown during the SeaDex fetch is not an arr failure)")
	}
	if n := recorder.CountExact("cycle interrupted by shutdown before comparison; findings preserved"); n != 1 {
		t.Errorf("shutdown WARN count = %d, want 1", n)
	}
	if n := recorder.CountExact("seadex fetch failed; skipping comparison, findings preserved"); n != 0 {
		t.Errorf("shutdown misattributed to a SeaDex outage %d times, want 0", n)
	}
	if n := recorder.CountExact("cycle degraded"); n != 0 {
		t.Errorf("'cycle degraded' count = %d, want 0 (an interrupted cycle did not complete, degraded or not)", n)
	}
	if _, ok := store.st.Findings["prior"]; !ok {
		t.Errorf("prior finding not preserved on shutdown mid-fetch: %+v", store.st.Findings)
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
		Mapping:        mapping.Cache{FetchedAt: time.Now(), Records: []mapping.Record{{AniListID: 111, Type: "TV", TvdbID: 123}}},
		SeadexFailures: 5,
		Baselined:      true,
	}}
	sonarr := &fakeSonarr{series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}}}
	s := New(&Deps{
		Logger:  logger,
		Store:   store,
		Library: library.NewWalker(&library.Config{Sonarr: sonarr, Logger: logger}),
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
		Mapping:   mapping.Cache{FetchedAt: time.Now().Add(-2 * time.Hour), Records: []mapping.Record{{AniListID: 154587, Type: "TV", TvdbID: 123, SeasonTvdb: 1}}},
		Baselined: true,
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
		Library:      library.NewWalker(&library.Config{Sonarr: sonarr, Logger: scoutTestLogger()}),
		Mapping:      mapping.NewLoader(noNetworkClient(), "http://unused.invalid/f.json", filepath.Join(t.TempDir(), "ov.json"), time.Hour, scoutTestLogger()),
		SeaDex:       &fakeSeaDex{entries: seadexFrierenEntry()},
		Matcher:      match.NewMatcher(notFoundAniList{}, scoutTestLogger()),
		Comparer:     compare.NewComparer(compare.Config{}),
		Reporter:     report.NewReporter(logger),
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
	staleAttr := false
	for _, r := range recorder.Records() {
		if r.Message != "mapping degraded" {
			continue
		}
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "stale_reason" {
				staleAttr = true
				return false
			}
			return true
		})
	}
	if !staleAttr {
		t.Error("\"mapping degraded\" WARN carries no stale_reason attribute; StaleMapError.LogAttrs was not appended")
	}
}

// TestSaveGenuineFailureLogsError pins save's fault contract: a save failure
// that is NOT a shutdown cancellation is a genuine write fault and must log
// "state save failed" at ERROR exactly once (the signal the
// SeadexScoutCycleError Loki alert fires on) - both on a live context and on a
// cancelled context whose detached retry also fails.
func TestSaveGenuineFailureLogsError(t *testing.T) {
	tests := []struct {
		name string
		ctx  func() context.Context
	}{
		{name: "live context", ctx: context.Background},
		{name: "cancelled context, detached retry also fails", ctx: func() context.Context {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logger, recorder := capture.New()
			store := &fakeStore{saveErr: errors.New("disk full")}
			s := New(&Deps{Logger: logger, Store: store})

			s.save(tc.ctx(), &state.State{Baselined: true})

			if store.saves != 0 {
				t.Errorf("saves = %d, want 0 (every attempt failed)", store.saves)
			}
			errCount := 0
			for _, r := range recorder.Records() {
				if r.Message == "state save failed" && r.Level == slog.LevelError {
					errCount++
				}
			}
			if errCount != 1 {
				t.Errorf("\"state save failed\" ERROR count = %d, want exactly 1", errCount)
			}
		})
	}
}

// TestLoadMappingEscalatesAfterRepeatedRejections pins the WARN-to-ERROR
// escalation of the single degraded-mapping log site: below the threshold a
// guard-rejected refresh logs "mapping degraded" at WARN; once the persisted
// streak reaches mapping.RejectionEscalationThreshold the same site logs at
// ERROR (firing the existing SeadexScoutCycleError Loki rule) with the remedy
// in the message and the streak/guard in the structured attrs - exactly one
// line either way (no double-logging), still returning the stale cache.
func TestLoadMappingEscalatesAfterRepeatedRejections(t *testing.T) {
	tests := []struct {
		name        string
		priorStreak int
		wantError   bool
	}{
		{name: "below threshold stays WARN", priorStreak: mapping.RejectionEscalationThreshold - 2, wantError: false},
		{name: "at threshold escalates to ERROR", priorStreak: mapping.RejectionEscalationThreshold - 1, wantError: true},
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
			var warns, errs int
			for _, r := range recorder.Records() {
				switch {
				case r.Level == slog.LevelError && strings.HasPrefix(r.Message, "mapping degraded"):
					errs++
				case r.Level == slog.LevelWarn && r.Message == "mapping degraded":
					warns++
				}
			}
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
	freshMapping := func() mapping.Cache {
		return mapping.Cache{FetchedAt: time.Now(), Records: []mapping.Record{{AniListID: 111, Type: "TV", TvdbID: 123}}}
	}
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
				mapSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					_, _ = w.Write([]byte("[]"))
				}))
				t.Cleanup(mapSrv.Close)
				return &Deps{
					Store:   &fakeStore{},
					Library: library.NewWalker(&library.Config{Sonarr: sonarrOK(), Logger: scoutTestLogger()}),
					Mapping: mapping.NewLoader(mapSrv.Client(), mapSrv.URL, filepath.Join(t.TempDir(), "ov.json"), time.Hour, scoutTestLogger()),
					SeaDex:  &fakeSeaDex{entries: []seadex.Entry{{AniListID: 154587}}},
					Logger:  logger,
				}
			},
		},
		{
			name:       "seadex fetch failed",
			wantReason: "seadex-fetch-failed",
			deps: func(t *testing.T, logger *slog.Logger) *Deps {
				t.Helper()
				return &Deps{
					Store:   &fakeStore{st: state.State{Mapping: freshMapping()}},
					Library: library.NewWalker(&library.Config{Sonarr: sonarrOK(), Logger: scoutTestLogger()}),
					Mapping: fakeMapping{},
					SeaDex:  &fakeSeaDex{err: errors.New("seadex down")},
					Logger:  logger,
				}
			},
		},
		{
			name:       "seadex zero entries",
			wantReason: "seadex-zero-entries",
			deps: func(t *testing.T, logger *slog.Logger) *Deps {
				t.Helper()
				return &Deps{
					Store:   &fakeStore{st: state.State{Mapping: freshMapping()}},
					Library: library.NewWalker(&library.Config{Sonarr: sonarrOK(), Logger: scoutTestLogger()}),
					Mapping: fakeMapping{},
					SeaDex:  &fakeSeaDex{},
					Logger:  logger,
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
					Store:    &fakeStore{st: state.State{Mapping: freshMapping()}},
					Library:  library.NewWalker(&library.Config{Sonarr: sonarrOK(), Logger: scoutTestLogger()}),
					Mapping:  fakeMapping{},
					SeaDex:   &fakeSeaDex{entries: []seadex.Entry{{AniListID: 999}}},
					Matcher:  match.NewMatcher(degradedMatcherAniList{}, scoutTestLogger()),
					Comparer: compare.NewComparer(compare.Config{}),
					Reporter: report.NewReporter(scoutTestLogger()),
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

// TestCycleUpgradeWithPriorFindingsTakesReportPath pins the upgrade-compat
// cell of the cold-start gate: a state predating the Baselined flag
// (Baselined=false) that already holds findings must stay on the normal
// Report path - re-baselining would silently swallow the cycle's
// notifications and resolutions.
func TestCycleUpgradeWithPriorFindingsTakesReportPath(t *testing.T) {
	logger, recorder := capture.New()
	stale := report.Alerted{
		AlertedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Finding:   report.StoredFinding{Title: "Gone Title", Status: compare.StatusBetter, AniListID: 111},
	}
	store := &fakeStore{st: state.State{
		Mapping:  mapping.Cache{FetchedAt: time.Now(), Records: []mapping.Record{{AniListID: 154587, Type: "TV", TvdbID: 123, SeasonTvdb: 1}}},
		Findings: map[string]report.Alerted{"stale": stale},
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
		Library:      library.NewWalker(&library.Config{Sonarr: sonarr, Logger: scoutTestLogger()}),
		Mapping:      fakeMapping{},
		SeaDex:       &fakeSeaDex{entries: seadexFrierenEntry()},
		Matcher:      match.NewMatcher(notFoundAniList{}, scoutTestLogger()),
		Comparer:     compare.NewComparer(compare.Config{}),
		Reporter:     report.NewReporter(logger),
		AniListStats: aniStatsFn(anilist.NewClient(noNetworkClient(), "http://unused.invalid/gql", 1, scoutTestLogger())),
	})

	if healthy := s.Cycle(context.Background()); !healthy {
		t.Fatal("Cycle healthy=false, want true on a successful upgrade cycle")
	}
	if n := recorder.CountExact("cold start: findings baselined without notifying"); n != 0 {
		t.Errorf("upgrade cycle took the Baseline path %d times, want 0 (prior findings must keep the Report path)", n)
	}
	if n := recorder.CountExact("findings reported"); n != 1 {
		t.Errorf("'findings reported' count = %d, want 1 (the Report path)", n)
	}
	if n := recorder.CountExact("better release available"); n != 1 {
		t.Errorf("new finding notification count = %d, want 1 (re-baselining would swallow it)", n)
	}
	if n := recorder.CountExact("finding resolved"); n != 1 {
		t.Errorf("resolved notification count = %d, want 1 (the stale prior finding must resolve)", n)
	}
	if !store.st.Baselined {
		t.Error("Baselined = false after the upgrade cycle, want true")
	}
	if store.st.BaselineIncomplete {
		t.Error("BaselineIncomplete = true after the upgrade cycle, want false (a legacy state never enters the incomplete-baseline window)")
	}
}

// TestCyclePartialWalkAndAniListDegradedPreservesBothFindingSets pins the
// combined-degradation preservation scope: a cycle where one series' episode
// fetch failed (a Failed placeholder, partial walk) AND a different id-less
// entry's AniList lookup failed transiently must preserve BOTH affected
// entries' prior findings - the preservation set is the union of the failed
// items and the incomplete lookups, so neither degradation may mask the
// other's preservation - while the unaffected majority still compares and
// emits normally and the cycle closes degraded (reason partial-walk, the
// switch's first arm).
func TestCyclePartialWalkAndAniListDegradedPreservesBothFindingSets(t *testing.T) {
	logger, recorder := capture.New()
	oldTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	priorFailed := report.Alerted{
		AlertedAt: oldTime,
		Finding:   report.StoredFinding{Title: "Broken Series", Status: compare.StatusBetter, AniListID: 222},
	}
	priorIdless := report.Alerted{
		AlertedAt: oldTime,
		Finding:   report.StoredFinding{Title: "Idless Show", Status: compare.StatusBetter, AniListID: 333},
	}
	store := &fakeStore{st: state.State{
		Mapping: mapping.Cache{FetchedAt: time.Now(), Records: []mapping.Record{
			{AniListID: 154587, Type: "TV", TvdbID: 123, SeasonTvdb: 1},
			{AniListID: 222, Type: "TV", TvdbID: 124, SeasonTvdb: 1},
			// Id-less record (a split AniList<->arr mapping): the entry NEEDS
			// the AniList title lookup, which fails transiently this cycle.
			{AniListID: 333, Type: "TV"},
		}},
		Findings:  map[string]report.Alerted{"prior-failed": priorFailed, "prior-idless": priorIdless},
		Baselined: true,
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
	s := New(&Deps{
		Logger:   logger,
		Store:    store,
		Library:  library.NewWalker(&library.Config{Sonarr: sonarr, Logger: scoutTestLogger()}),
		Mapping:  fakeMapping{},
		SeaDex:   &fakeSeaDex{entries: entries},
		Matcher:  match.NewMatcher(degradedMatcherAniList{}, scoutTestLogger()),
		Comparer: compare.NewComparer(compare.Config{}),
		Reporter: report.NewReporter(logger),
	})

	if healthy := s.Cycle(context.Background()); !healthy {
		t.Fatal("Cycle healthy=false, want true (partial walk + transient AniList degradation is degraded, not unhealthy)")
	}
	// The unaffected majority still compares and emits.
	if n := recorder.CountExact("better release available"); n != 1 {
		t.Errorf("clean item's finding notification count = %d, want 1", n)
	}
	// NEITHER affected entry's prior finding may resolve: the preservation
	// set must union the failed-walk ids with the incomplete-lookup ids.
	if n := recorder.CountExact("finding resolved"); n != 0 {
		t.Errorf("resolved count = %d, want 0 (both degradations' findings must be preserved)", n)
	}
	for key, want := range map[string]report.Alerted{"prior-failed": priorFailed, "prior-idless": priorIdless} {
		got, ok := store.st.Findings[key]
		if !ok {
			t.Errorf("prior finding %q was dropped, want it preserved: %+v", key, store.st.Findings)
			continue
		}
		if !got.AlertedAt.Equal(want.AlertedAt) {
			t.Errorf("preserved %q AlertedAt = %s, want the original %s", key, got.AlertedAt, want.AlertedAt)
		}
	}
	if len(store.st.Findings) != 3 {
		t.Errorf("persisted findings = %d, want 3 (the majority's new finding plus both preserved)", len(store.st.Findings))
	}
	if reasons := degradedReasons(recorder); len(reasons) != 1 || reasons[0] != "partial-walk" {
		t.Errorf("degraded reasons = %v, want [partial-walk] (the switch's first arm wins the combined degradation)", reasons)
	}
	if n := recorder.CountExact("cycle complete"); n != 0 {
		t.Errorf("'cycle complete' count = %d, want 0 on a degraded cycle", n)
	}
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
	prior := report.Alerted{
		AlertedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Finding:   report.StoredFinding{Title: "Existing", Status: compare.StatusBetter, AniListID: 154587},
	}
	store := &fakeStore{st: state.State{
		// Records fetched beyond the 1h refresh window force a refresh
		// attempt, whose transport cancels the cycle context mid-flight.
		Mapping:   mapping.Cache{FetchedAt: time.Now().Add(-2 * time.Hour), Records: []mapping.Record{{AniListID: 111, Type: "TV", TvdbID: 123}}},
		Findings:  map[string]report.Alerted{"prior": prior},
		Baselined: true,
	}}
	sonarr := &fakeSonarr{series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}}}
	s := New(&Deps{
		Logger:  logger,
		Store:   store,
		Library: library.NewWalker(&library.Config{Sonarr: sonarr, Logger: scoutTestLogger()}),
		Mapping: mapping.NewLoader(&http.Client{Transport: cancellingMappingTransport{cancel: cancel}}, "http://unused.invalid/f.json", filepath.Join(t.TempDir(), "ov.json"), time.Hour, scoutTestLogger()),
		SeaDex:  &fakeSeaDex{entries: []seadex.Entry{{AniListID: 999}}},
	})

	if healthy := s.Cycle(ctx); !healthy {
		t.Fatal("Cycle healthy=false, want true (a shutdown during the mapping load is not an arr failure)")
	}
	if n := recorder.CountExact("mapping degraded"); n != 0 {
		t.Errorf("'mapping degraded' fired %d times during a shutdown, want 0 (a cancelled load is the shutdown, not a Fribb fault)", n)
	}
	if n := recorder.CountExact("mapping unusable; skipping comparison, findings preserved"); n != 0 {
		t.Errorf("shutdown misattributed to an unusable map %d times, want 0", n)
	}
	if n := recorder.CountExact("cycle interrupted by shutdown before comparison; findings preserved"); n != 1 {
		t.Errorf("shutdown WARN count = %d, want 1", n)
	}
	if n := recorder.CountExact("cycle degraded"); n != 0 {
		t.Errorf("'cycle degraded' count = %d, want 0 (an interrupted cycle did not complete)", n)
	}
	if _, ok := store.st.Findings["prior"]; !ok {
		t.Errorf("prior finding not preserved on shutdown mid-load: %+v", store.st.Findings)
	}
}
