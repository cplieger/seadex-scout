package scout

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/cplieger/arrapi"
	"github.com/cplieger/seadex-scout/internal/anilist"
	"github.com/cplieger/seadex-scout/internal/compare"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/report"
	"github.com/cplieger/seadex-scout/internal/seadex"
	"github.com/cplieger/seadex-scout/internal/state"
	"github.com/cplieger/slogx/capture"
)

type fakeSonarr struct {
	episodes map[int][]arrapi.Episode
	listErr  error
	series   []arrapi.Series
}

func (f *fakeSonarr) GetSeries(context.Context) ([]arrapi.Series, error) {
	return f.series, f.listErr
}

func (f *fakeSonarr) GetEpisodes(_ context.Context, seriesID int) ([]arrapi.Episode, error) {
	return f.episodes[seriesID], nil
}

func (f *fakeSonarr) ResolveTagIDs(context.Context, ...string) (map[int]struct{}, []string, error) {
	return nil, nil, nil
}

// flakySonarr wraps fakeSonarr but fails GetEpisodes for the listed series
// IDs, so a walk succeeds while marking the snapshot partial.
type flakySonarr struct {
	failEpisodes map[int]bool
	fakeSonarr
}

func (f *flakySonarr) GetEpisodes(ctx context.Context, seriesID int) ([]arrapi.Episode, error) {
	if f.failEpisodes[seriesID] {
		return nil, errors.New("episode fetch failed")
	}
	return f.fakeSonarr.GetEpisodes(ctx, seriesID)
}

// fakeSeaDex is an in-package SeaDexSource: it returns fixed entries or an
// error so orchestration tests drive cycle outcomes directly, without the
// PocketBase adapter or an httptest server (the seadex package's own suite
// covers adapter behavior).
type fakeSeaDex struct {
	err     error
	entries []seadex.Entry
}

func (f *fakeSeaDex) FetchEntries(context.Context) ([]seadex.Entry, error) {
	return f.entries, f.err
}

// fakeFeed records FeedWriter.Rebuild calls, optionally failing them.
type fakeFeed struct {
	err     error
	calls   int
	entries int
}

func (f *fakeFeed) Rebuild(_ context.Context, entries []seadex.Entry, _ func(alID int) bool) error {
	f.calls++
	f.entries = len(entries)
	return f.err
}

// fakeStore is an in-package StateStore: it holds State in memory so
// orchestration tests drive cycle state transitions directly, without real
// paths or atomic disk I/O (the state package's own suite covers the file
// adapter round-trip). Load and Save honor context cancellation like the real
// store's atomic reads and writes, so the shutdown paths (loadState's silent
// fallback, save's detached retry) stay exercised.
type fakeStore struct {
	loadErr error
	saveErr error
	st      state.State
	saves   int
}

func (f *fakeStore) Load(ctx context.Context) (state.State, error) {
	if err := ctx.Err(); err != nil {
		return state.State{}, err
	}
	if f.loadErr != nil {
		return state.State{}, f.loadErr
	}
	return f.st, nil
}

func (f *fakeStore) Save(ctx context.Context, st *state.State) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.saveErr != nil {
		return f.saveErr
	}
	f.st = *st
	f.saves++
	return nil
}

// seadexFrierenEntry returns the single curated Frieren entry (one best Nyaa
// release, one file) the orchestration tests feed cycles with.
func seadexFrierenEntry() []seadex.Entry {
	return []seadex.Entry{{
		AniListID: 154587,
		Torrents: []seadex.Torrent{{
			ReleaseGroup: "SubsPlease",
			Tracker:      "Nyaa",
			InfoHash:     "abc",
			URL:          "https://nyaa.si/view/1",
			IsBest:       true,
			Files:        []seadex.File{{Name: "Frieren S01E01 1080p.mkv", Length: 1}},
		}},
	}}
}

func scoutTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// errTransport fails every request with a plain (non-transient) error, so
// orchestration tests stay hermetic: the deliberately-unreachable
// unused.invalid deps fail at the transport instead of issuing a real DNS
// query through the host resolver (http.DefaultClient also carries no
// timeout, so a slow resolver could stall the suite), and the mapping
// loader's retry wrapper returns after one attempt without sleeping.
type errTransport struct{}

func (errTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("hermetic test transport: no network")
}

// noNetworkClient returns an HTTP client whose every request fails, for deps
// whose fetch must fail (or is never exercised) in orchestration tests.
func noNetworkClient() *http.Client {
	return &http.Client{Transport: errTransport{}}
}

// aniStatsFn adapts an AniList client's Stats to the Deps.AniListStats
// callback, mirroring the composition root's wiring.
func aniStatsFn(c *anilist.Client) func() (int64, int64) {
	return func() (int64, int64) {
		st := c.Stats()
		return st.Calls, st.RateLimitWaits
	}
}

// TestLoadStateCanceledContextIsNotAFault pins loadState's shutdown handling:
// a SIGTERM already visible while state loads is the redeploy, not a state
// corruption or read fault, so no ERROR record may be emitted (the shipped
// Loki rule alerts on every ERROR) - the following context-aware cycle stage
// reports the shutdown once at WARN.
func TestLoadStateCanceledContextIsNotAFault(t *testing.T) {
	logger, recorder := capture.New()
	store := &fakeStore{st: state.State{Baselined: true}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	s := New(&Deps{Logger: logger, Store: store})
	st := s.loadState(ctx)

	if st.Baselined {
		t.Error("loadState under a canceled context returned loaded state, want empty state")
	}
	if n := recorder.CountExact("state load failed; starting from empty state"); n != 0 {
		t.Errorf("canceled state load was logged as a fault %d times, want 0", n)
	}
}

func TestCycleLibraryWalkFailureIsUnhealthy(t *testing.T) {
	logger := scoutTestLogger()
	store := &fakeStore{}
	s := New(&Deps{
		Logger:  logger,
		Store:   store,
		Library: library.NewWalker(&library.Config{Sonarr: &fakeSonarr{listErr: errors.New("sonarr down")}, Logger: logger}),
	})

	if healthy := s.Cycle(context.Background()); healthy {
		t.Fatal("Cycle returned healthy=true, want false when the library walk fails")
	}
}

func TestCycleSeaDexFailureIsHealthyAndPreservesFindings(t *testing.T) {
	logger := scoutTestLogger()
	prior := report.Alerted{
		AlertedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Finding: compare.Finding{
			Title:     "Existing finding",
			DedupeKey: "prior",
			Status:    compare.StatusBetter,
			AniListID: 154587,
		},
	}
	store := &fakeStore{st: state.State{
		Mapping: mapping.Cache{
			FetchedAt: time.Now(),
			Records:   []mapping.Record{{AniListID: 154587, Type: "TV", TvdbID: 123, SeasonTvdb: 1}},
		},
		Findings:  map[string]report.Alerted{"prior": prior},
		Baselined: true,
	}}
	sonarr := &fakeSonarr{
		series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}},
		episodes: map[int][]arrapi.Episode{
			7: {{SeasonNumber: 1, EpisodeFile: &arrapi.EpisodeFile{ReleaseGroup: "Erai-raws"}}},
		},
	}
	s := New(&Deps{
		Logger:  logger,
		Store:   store,
		Library: library.NewWalker(&library.Config{Sonarr: sonarr, Logger: logger}),
		Mapping: mapping.NewLoader(noNetworkClient(), "http://unused.invalid/fribb.json", filepath.Join(t.TempDir(), "overrides.json"), time.Hour, logger),
		SeaDex:  &fakeSeaDex{err: errors.New("seadex down")},
	})

	if healthy := s.Cycle(context.Background()); !healthy {
		t.Fatal("Cycle returned healthy=false, want true for degraded SeaDex failure")
	}
	loaded := store.st
	if len(loaded.Library.Items) != 1 || loaded.Library.Items[0].Title != "Frieren" {
		t.Errorf("library snapshot after degraded cycle = %+v, want refreshed Frieren snapshot", loaded.Library)
	}
	got, ok := loaded.Findings["prior"]
	if !ok {
		t.Fatalf("prior finding was not preserved: %+v", loaded.Findings)
	}
	if got.Finding.Title != prior.Finding.Title || !got.AlertedAt.Equal(prior.AlertedAt) {
		t.Errorf("preserved finding = %+v, want %+v", got, prior)
	}
}

// TestNewNilLoggerFallsBackToDefault pins New's nil-Logger tolerance: a Deps
// without a Logger must fall back to slog.Default() so later cycle logging
// does not dereference a nil *slog.Logger. capture.Default mutates the global
// default logger, so this test must not call t.Parallel.
func TestNewNilLoggerFallsBackToDefault(t *testing.T) {
	recorder := capture.Default(t)
	s := New(&Deps{Store: &fakeStore{loadErr: errors.New("boom")}})

	s.loadState(context.Background())

	if n := recorder.CountExact("state load failed; starting from empty state"); n != 1 {
		t.Errorf("state-load failure logged through the default logger %d times, want 1", n)
	}
}

// TestSave_retriesCanceledContextWithDetachedDeadline pins the
// cancellation-safe state persistence contract: a save whose context was
// already cancelled (a redeploy SIGTERM landing mid-cycle) must still persist
// state via the detached context.WithoutCancel retry, or the AniList memo and
// finding dedupe state would be discarded on every routine shutdown.
func TestSave_retriesCanceledContextWithDetachedDeadline(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"), logger)
	s := &Scout{deps: Deps{Store: store}, log: logger}
	want := state.State{Baselined: true}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.save(ctx, &want)

	got, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load() after save with canceled context: %v", err)
	}
	if !got.Baselined {
		t.Errorf("Load().Baselined = false, want true")
	}
}
