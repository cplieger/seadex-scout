package scout

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
)

// msgCaptureHandler records slog message strings so a cold-start cycle's
// Baseline-vs-Report path can be asserted from the emitted log lines.
type msgCaptureHandler struct{ msgs *[]string }

func (h msgCaptureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h msgCaptureHandler) Handle(_ context.Context, r slog.Record) error {
	*h.msgs = append(*h.msgs, r.Message)
	return nil
}
func (h msgCaptureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h msgCaptureHandler) WithGroup(string) slog.Handler      { return h }

func newCaptureLogger(msgs *[]string) *slog.Logger {
	return slog.New(msgCaptureHandler{msgs: msgs})
}

func countMessages(msgs []string, msg string) int {
	n := 0
	for _, m := range msgs {
		if m == msg {
			n++
		}
	}
	return n
}

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

// TestSaveRetriesDetachedOnCancelledContext pins the SIGTERM save-retry: the
// first Save fails with context.Canceled on a cancelled cycle context and does
// not write, so save must retry once with a detached context so the state (the
// expensive AniList memo) survives a redeploy.
func TestSaveRetriesDetachedOnCancelledContext(t *testing.T) {
	logger := scoutTestLogger()
	path := filepath.Join(t.TempDir(), "state.json")
	s := New(&Deps{Logger: logger, Store: state.NewStore(path, logger)})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	s.save(ctx, &state.State{Baselined: true})

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("state file not written; the detached-context retry did not run: %v", err)
	}
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
	seaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"totalPages":1,"items":[{"alID":154587,"expand":{"trs":[]}}]}`)
	}))
	defer seaSrv.Close()

	logger := scoutTestLogger()
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"), logger)
	prior := report.Alerted{
		AlertedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Finding:   compare.Finding{Title: "Existing", DedupeKey: "prior", Status: compare.StatusBetter, AniListID: 154587},
	}
	if err := store.Save(context.Background(), &state.State{
		Findings:  map[string]report.Alerted{"prior": prior},
		Baselined: true,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	sonarr := &fakeSonarr{series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}}}
	s := New(&Deps{
		Logger:  logger,
		Store:   store,
		Library: library.NewWalker(&library.Config{Sonarr: sonarr, Logger: logger}),
		Mapping: mapping.NewLoader(mapSrv.Client(), mapSrv.URL, filepath.Join(t.TempDir(), "ov.json"), time.Hour, logger),
		SeaDex:  seadex.NewClient(seaSrv.Client(), seaSrv.URL, 0, logger),
	})

	if healthy := s.Cycle(context.Background()); !healthy {
		t.Fatal("Cycle healthy=false, want true when the map is unusable (degraded, not unhealthy)")
	}
	loaded, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, ok := loaded.Findings["prior"]; !ok {
		t.Errorf("prior finding not preserved on unusable-map cycle: %+v", loaded.Findings)
	}
	if len(loaded.Library.Items) != 1 || loaded.Library.Items[0].Title != "Frieren" {
		t.Errorf("library snapshot not refreshed: %+v", loaded.Library)
	}
}

// TestCycleAniListDegradedPreservesFindings pins the AniList-degraded branch:
// when a needed AniList fallback lookup fails transiently the match Result is
// Degraded, so the cycle preserves prior findings (comparing would falsely
// resolve them) yet stays healthy.
func TestCycleAniListDegradedPreservesFindings(t *testing.T) {
	seaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"totalPages":1,"items":[{"alID":999,"expand":{"trs":[]}}]}`)
	}))
	defer seaSrv.Close()

	logger := scoutTestLogger()
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"), logger)
	prior := report.Alerted{
		AlertedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Finding:   compare.Finding{Title: "Existing", DedupeKey: "prior", Status: compare.StatusBetter, AniListID: 154587},
	}
	if err := store.Save(context.Background(), &state.State{
		Mapping:   mapping.Cache{FetchedAt: time.Now(), Records: []mapping.Record{{AniListID: 111, Type: "TV", TvdbID: 123}}},
		Findings:  map[string]report.Alerted{"prior": prior},
		Baselined: true,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	sonarr := &fakeSonarr{series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}}}
	s := New(&Deps{
		Logger:  logger,
		Store:   store,
		Library: library.NewWalker(&library.Config{Sonarr: sonarr, Logger: logger}),
		Mapping: mapping.NewLoader(http.DefaultClient, "http://unused.invalid/f.json", filepath.Join(t.TempDir(), "ov.json"), time.Hour, logger),
		SeaDex:  seadex.NewClient(seaSrv.Client(), seaSrv.URL, 0, logger),
		Matcher: match.NewMatcher(degradedMatcherAniList{}, logger),
	})

	if healthy := s.Cycle(context.Background()); !healthy {
		t.Fatal("Cycle healthy=false, want true when AniList is transiently degraded")
	}
	loaded, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, ok := loaded.Findings["prior"]; !ok {
		t.Errorf("prior finding not preserved on AniList-degraded cycle: %+v", loaded.Findings)
	}
}

// TestCycleColdStartBaselinesSilently pins the cold-start Baseline branch: a
// fresh instance (no baselined findings yet) must seed the dedupe table WITHOUT
// emitting any per-finding notification, so a pre-existing backlog is not dumped
// as a burst of alerts. The captured log distinguishes the Baseline path from
// the steady-state Report path.
func TestCycleColdStartBaselinesSilently(t *testing.T) {
	seaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"totalPages":1,"items":[{"alID":154587,"expand":{"trs":[{"releaseGroup":"SubsPlease","tracker":"Nyaa","infoHash":"abc","url":"https://nyaa.si/view/1","isBest":true,"files":[{"name":"Frieren S01E01 1080p.mkv","length":1}]}]}}]}`)
	}))
	defer seaSrv.Close()

	records := &[]string{}
	reporter := report.NewReporter(newCaptureLogger(records))
	logger := scoutTestLogger()
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"), logger)
	if err := store.Save(context.Background(), &state.State{
		Mapping: mapping.Cache{FetchedAt: time.Now(), Records: []mapping.Record{{AniListID: 154587, Type: "TV", TvdbID: 123, SeasonTvdb: 1}}},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	sonarr := &fakeSonarr{
		series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}},
		episodes: map[int][]arrapi.Episode{
			7: {{SeasonNumber: 1, EpisodeFile: &arrapi.EpisodeFile{ReleaseGroup: "Erai-raws"}}},
		},
	}
	s := New(&Deps{
		Logger:   logger,
		Store:    store,
		Library:  library.NewWalker(&library.Config{Sonarr: sonarr, Logger: logger}),
		Mapping:  mapping.NewLoader(http.DefaultClient, "http://unused.invalid/f.json", filepath.Join(t.TempDir(), "ov.json"), time.Hour, logger),
		SeaDex:   seadex.NewClient(seaSrv.Client(), seaSrv.URL, 0, logger),
		Matcher:  match.NewMatcher(notFoundAniList{}, logger),
		Comparer: compare.NewComparer(compare.Config{Logger: logger}),
		Reporter: reporter,
		AniList:  anilist.NewClient(http.DefaultClient, "http://unused.invalid/gql", 1, logger),
	})

	if healthy := s.Cycle(context.Background()); !healthy {
		t.Fatal("Cycle healthy=false, want true on a successful cold-start cycle")
	}
	if n := countMessages(*records, "better release available"); n != 0 {
		t.Errorf("cold start emitted %d finding notifications, want 0 (backlog must be baselined silently)", n)
	}
	if n := countMessages(*records, "findings reported"); n != 0 {
		t.Errorf("cold start took the Report path (%d 'findings reported'), want the Baseline path", n)
	}
	if n := countMessages(*records, "cold start: findings baselined without notifying"); n != 1 {
		t.Errorf("cold-start baseline summary count = %d, want 1", n)
	}
	loaded, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !loaded.Baselined {
		t.Error("state Baselined=false after cold start, want true")
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
	seaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"totalPages":1,"items":[]}`)
	}))
	defer seaSrv.Close()

	records := &[]string{}
	reporter := report.NewReporter(newCaptureLogger(records))
	logger := scoutTestLogger()
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"), logger)
	prior := report.Alerted{
		AlertedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Finding:   compare.Finding{Title: "Existing", DedupeKey: "prior", Status: compare.StatusBetter, AniListID: 154587},
	}
	if err := store.Save(context.Background(), &state.State{
		Mapping: mapping.Cache{
			FetchedAt: time.Now(),
			Records:   []mapping.Record{{AniListID: 154587, Type: "TV", TvdbID: 123, SeasonTvdb: 1}},
		},
		Findings:  map[string]report.Alerted{"prior": prior},
		Baselined: true,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	sonarr := &fakeSonarr{series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}}}
	s := New(&Deps{
		Logger:   logger,
		Store:    store,
		Library:  library.NewWalker(&library.Config{Sonarr: sonarr, Logger: logger}),
		Mapping:  mapping.NewLoader(http.DefaultClient, "http://unused.invalid/f.json", filepath.Join(t.TempDir(), "ov.json"), time.Hour, logger),
		SeaDex:   seadex.NewClient(seaSrv.Client(), seaSrv.URL, 0, logger),
		Matcher:  match.NewMatcher(notFoundAniList{}, logger),
		Comparer: compare.NewComparer(compare.Config{Logger: logger}),
		Reporter: reporter,
		AniList:  anilist.NewClient(http.DefaultClient, "http://unused.invalid/gql", 1, logger),
	})

	if healthy := s.Cycle(context.Background()); !healthy {
		t.Fatal("Cycle healthy=false, want true when SeaDex returns an anomalous empty result")
	}
	loaded, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, ok := loaded.Findings["prior"]; !ok {
		t.Errorf("prior finding not preserved on empty-SeaDex cycle: %+v", loaded.Findings)
	}
	if len(loaded.Library.Items) != 1 || loaded.Library.Items[0].Title != "Frieren" {
		t.Errorf("library snapshot not refreshed: %+v", loaded.Library)
	}
	if n := countMessages(*records, "finding resolved"); n != 0 {
		t.Errorf("empty-SeaDex cycle emitted %d resolved finding logs, want 0", n)
	}
	if n := countMessages(*records, "findings reported"); n != 0 {
		t.Errorf("empty-SeaDex cycle ran Reporter.Report %d times, want 0", n)
	}
}
