package scout

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/arrapi"
	"github.com/cplieger/seadex-scout/internal/anilist"
	"github.com/cplieger/seadex-scout/internal/compare"
	"github.com/cplieger/seadex-scout/internal/indexer"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/notify"
	"github.com/cplieger/seadex-scout/internal/seadex"
	"github.com/cplieger/seadex-scout/internal/state"
	"github.com/cplieger/slogx/capture"
)

type fakeSonarr struct {
	files   map[int][]arrapi.EpisodeFile
	listErr error
	series  []arrapi.Series
}

func (f *fakeSonarr) GetSeries(context.Context) ([]arrapi.Series, error) {
	return f.series, f.listErr
}

func (f *fakeSonarr) GetEpisodeFiles(_ context.Context, seriesID int) ([]arrapi.EpisodeFile, error) {
	return f.files[seriesID], nil
}

func (f *fakeSonarr) GetTags(context.Context) ([]arrapi.Tag, error) {
	return nil, nil
}

// flakySonarr wraps fakeSonarr but fails GetEpisodeFiles for the listed series
// IDs, so a walk succeeds while marking the snapshot partial.
type flakySonarr struct {
	failEpisodes map[int]bool
	fakeSonarr
}

func (f *flakySonarr) GetEpisodeFiles(ctx context.Context, seriesID int) ([]arrapi.EpisodeFile, error) {
	if f.failEpisodes[seriesID] {
		return nil, errors.New("episode fetch failed")
	}
	return f.fakeSonarr.GetEpisodeFiles(ctx, seriesID)
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

func (f *fakeFeed) Rebuild(_ context.Context, entries []seadex.Entry, _ func(alID int) indexer.EntryInfo) error {
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

// fakeMapping is an in-package MappingSource for the fresh-cache reuse path:
// it echoes the persisted cache and indexes its records, exactly what the
// real loader does inside its refresh window, without the no-network HTTP
// client, dummy URL, and override-path ceremony the concrete loader forces on
// every test. Scenarios exercising the loader's own behavior (a stale map, an
// unusable refresh, a cancelled in-flight fetch, acceptance-guard rejections)
// keep the real loader; its fetch/degradation coverage lives in the mapping
// package's suite.
type fakeMapping struct{}

func (fakeMapping) Load(_ context.Context, prev *mapping.Cache) (mapping.Cache, *mapping.Index, error) {
	return *prev, mapping.NewIndex(prev.Records), nil
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

// frierenMappingCache returns a fresh (inside the refresh window) mapping
// cache holding the single Frieren record the orchestration tests key on -
// the mapping-side twin of seadexFrierenEntry.
func frierenMappingCache() mapping.Cache {
	return mapping.Cache{FetchedAt: time.Now(), Records: []mapping.Record{{AniListID: 154587, Type: "TV", TvdbID: 123, SeasonTvdb: 1}}}
}

// seasonlessMappingCache returns a fresh mapping cache holding the single
// seasonless TV record (AniList 111) the shutdown/degradation tests key on.
func seasonlessMappingCache() mapping.Cache {
	return mapping.Cache{FetchedAt: time.Now(), Records: []mapping.Record{{AniListID: 111, Type: "TV", TvdbID: 123}}}
}

func scoutTestLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
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

// TestCycleLibraryWalkFailureIsUnhealthy pins the failed-walk log contract
// for an alert-only deployment (no feed): the cycle is unhealthy, logs the
// walk-failure ERROR (the SeadexScoutCycleError signal), AND still closes
// with exactly one "cycle degraded" completion line (reason walk-failed) so
// the cycle deadman does not false-fire during an arr outage longer than its
// window - the two alerts stay orthogonal: ERROR = arr fault, missing
// completion line = wedged loop.
func TestCycleLibraryWalkFailureIsUnhealthy(t *testing.T) {
	logger, recorder := capture.New()
	store := &fakeStore{}
	s := New(&Deps{
		Logger:  logger,
		Store:   store,
		Library: library.NewWalker(&library.Config{Sonarr: &fakeSonarr{listErr: errors.New("sonarr down")}, Logger: scoutTestLogger()}),
	})

	if healthy := s.Cycle(context.Background()); healthy {
		t.Fatal("Cycle returned healthy=true, want false when the library walk fails")
	}
	if n := recorder.CountExact("library walk failed; cycle unhealthy"); n != 1 {
		t.Errorf("walk-failure ERROR count = %d, want 1", n)
	}
	if n := recorder.CountExact("cycle degraded"); n != 1 {
		t.Errorf("'cycle degraded' count = %d, want 1 (the failed-walk cycle still completed; the deadman counts completion lines)", n)
	}
	if reasons := degradedReasons(recorder); len(reasons) != 1 || reasons[0] != "walk-failed" {
		t.Errorf("degraded reasons = %v, want [walk-failed]", reasons)
	}
	if n := recorder.CountExact("cycle complete"); n != 0 {
		t.Errorf("'cycle complete' count = %d, want 0 on a failed walk", n)
	}
}

func TestCycleSeaDexFailureIsHealthyAndPreservesFindings(t *testing.T) {
	logger := scoutTestLogger()
	prior := notify.Alerted{
		AlertedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Finding: notify.StoredFinding{
			Title:     "Existing finding",
			Status:    compare.StatusBetter,
			AniListID: 154587,
		},
	}
	store := &fakeStore{st: state.State{
		Mapping:   frierenMappingCache(),
		Findings:  map[string]notify.Alerted{"prior": prior},
		Baselined: true,
	}}
	sonarr := &fakeSonarr{
		series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}},
		files: map[int][]arrapi.EpisodeFile{
			7: {{SeasonNumber: 1, ReleaseGroup: "Erai-raws"}},
		},
	}
	s := New(&Deps{
		Logger:  logger,
		Store:   store,
		Library: library.NewWalker(&library.Config{Sonarr: sonarr, Logger: logger}),
		Mapping: fakeMapping{},
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

// TestSaveRetriesDetachedOnCancelledContext pins the
// cancellation-safe state persistence contract: a save whose context was
// already cancelled (a redeploy SIGTERM landing mid-cycle) must still persist
// state via the detached context.WithoutCancel retry, or the AniList memo and
// finding dedupe state would be discarded on every routine shutdown.
func TestSaveRetriesDetachedOnCancelledContext(t *testing.T) {
	t.Parallel()

	logger := scoutTestLogger()
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"), logger)
	s := New(&Deps{Logger: logger, Store: store})
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

// recordsContainString reports whether any captured record's message or
// rendered attribute value contains sub, via capture's message matcher and
// its wildcard attr matcher (msgSub "" = every record, key "" = every attr).
func recordsContainString(recorder *capture.Recorder, sub string) bool {
	return recorder.Contains(sub) || recorder.AttrContains("", "", sub)
}

// TestWalkFailureLogsAndReportErrorAreLogSafe pins the credential-redaction
// boundary on the walk-failure paths: the configured arr URL may carry
// userinfo (config.Validate only warns on that shape) and a transport failure
// wraps a *url.Error embedding the full request URL, so neither the cycle's
// walk-failure log sites nor Report's returned error (logged at ERROR by
// main) may carry the embedded credentials — httpx.LogSafeError must reduce
// the *url.Error to its cause before any log or return boundary.
func TestWalkFailureLogsAndReportErrorAreLogSafe(t *testing.T) {
	const userinfoSentinel = "hunter2pass"
	const querySentinel = "sekrettoken"
	sentinels := []string{userinfoSentinel, querySentinel}
	walkErr := fmt.Errorf("sonarr: %w", &url.Error{
		Op:  "Get",
		URL: "http://user:" + userinfoSentinel + "@sonarr.local/api/v3/series?apikey=" + querySentinel,
		Err: errors.New("connect: connection refused"),
	})

	logger, recorder := capture.New()
	s := New(&Deps{
		Logger:  logger,
		Store:   &fakeStore{},
		Library: library.NewWalker(&library.Config{Sonarr: &fakeSonarr{listErr: walkErr}, Logger: scoutTestLogger()}),
	})

	if healthy := s.Cycle(context.Background()); healthy {
		t.Fatal("Cycle returned healthy=true, want false when the library walk fails")
	}
	if n := recorder.CountExact("library walk failed; cycle unhealthy"); n != 1 {
		t.Fatalf("walk-failure ERROR count = %d, want 1 (the redaction test needs the log to fire)", n)
	}
	for _, sentinel := range sentinels {
		if recordsContainString(recorder, sentinel) {
			t.Errorf("cycle logs contain credential sentinel %q, want *url.Error reduced before logging", sentinel)
		}
	}

	// Feed-configured: the walk failure falls through to handleLibraryGate,
	// whose "cycle degraded" walk-failed line (a second LogSafeError site)
	// must stay credential-free as well.
	feedLogger, feedRecorder := capture.New()
	sFeed := New(&Deps{
		Logger: feedLogger,
		Store: &fakeStore{st: state.State{
			Mapping: frierenMappingCache(),
		}},
		Library: library.NewWalker(&library.Config{Sonarr: &fakeSonarr{listErr: walkErr}, Logger: scoutTestLogger()}),
		Mapping: fakeMapping{},
		SeaDex:  &fakeSeaDex{entries: seadexFrierenEntry()},
		Feed:    &fakeFeed{},
	})
	if healthy := sFeed.Cycle(context.Background()); healthy {
		t.Fatal("feed-configured Cycle returned healthy=true, want false when the library walk fails")
	}
	if n := feedRecorder.CountExact("cycle degraded"); n != 1 {
		t.Fatalf("feed-configured 'cycle degraded' count = %d, want 1 (the redaction assertion needs the log to fire)", n)
	}
	for _, sentinel := range sentinels {
		if recordsContainString(feedRecorder, sentinel) {
			t.Errorf("feed-configured cycle logs contain credential sentinel %q, want *url.Error reduced before logging", sentinel)
		}
	}

	_, err := s.Report(context.Background())
	if err == nil {
		t.Fatal("Report returned nil error, want the walk failure")
	}
	if !strings.Contains(err.Error(), "library walk") {
		t.Errorf("Report error %q does not name the failing stage, want a 'library walk' wrap", err)
	}
	for _, sentinel := range sentinels {
		if strings.Contains(err.Error(), sentinel) {
			t.Errorf("Report error %q contains credential sentinel %q, want *url.Error reduced before returning", err, sentinel)
		}
	}
}

// fakeRadarr is a scripted RadarrClient for orchestration tests: GetMovies
// returns movies (or listErr); tags are unused here.
type fakeRadarr struct {
	listErr error
	movies  []arrapi.Movie
}

func (f *fakeRadarr) GetMovies(context.Context) ([]arrapi.Movie, error) {
	return f.movies, f.listErr
}

func (f *fakeRadarr) GetTags(context.Context) ([]arrapi.Tag, error) {
	return nil, nil
}

// recordAttr returns the string value of key on the first captured record
// whose message is msg and that carries the attr, and whether one was found.
func recordAttr(recorder *capture.Recorder, msg, key string) (string, bool) {
	for _, r := range recorder.Records() {
		if r.Message != msg {
			continue
		}
		val, found := "", false
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == key {
				val, found = a.Value.String(), true
				return false
			}
			return true
		})
		if found {
			return val, true
		}
	}
	return "", false
}

// TestWalkFailureLogsCarryArrIdentity pins the failed-side attribution on the
// walk-failure log boundaries: httpx.LogSafeError reduces a transport failure
// to the *url.Error's underlying cause, discarding library.Walk's textual
// "walking sonarr/radarr" wrapper, so with both arrs enabled the reduced
// error alone cannot say which dependency failed - each boundary (the
// walk-failure ERROR, the alert-only walk-failed completion line, and the
// feed-configured library gate's walk-failed completion line) must carry the
// bounded `arr` attribute recovered from the typed walk-side error.
func TestWalkFailureLogsCarryArrIdentity(t *testing.T) {
	transportErr := func(host string) error {
		return &url.Error{
			Op:  "Get",
			URL: "http://" + host + "/api/v3",
			Err: errors.New("connect: connection refused"),
		}
	}

	// Alert-only deployment (no feed): stopAfterWalkFailure owns both the
	// ERROR and the walk-failed completion line.
	t.Run("sonarr alert-only", func(t *testing.T) {
		logger, recorder := capture.New()
		s := New(&Deps{
			Logger:  logger,
			Store:   &fakeStore{},
			Library: library.NewWalker(&library.Config{Sonarr: &fakeSonarr{listErr: transportErr("sonarr.local")}, Logger: scoutTestLogger()}),
		})
		if healthy := s.Cycle(context.Background()); healthy {
			t.Fatal("Cycle returned healthy=true, want false when the library walk fails")
		}
		if arr, ok := recordAttr(recorder, "library walk failed; cycle unhealthy", "arr"); !ok || arr != library.ArrSonarr {
			t.Errorf("walk-failure ERROR arr attr = %q (found=%t), want %q", arr, ok, library.ArrSonarr)
		}
		if arr, ok := recordAttr(recorder, "cycle degraded", "arr"); !ok || arr != library.ArrSonarr {
			t.Errorf("walk-failed completion-line arr attr = %q (found=%t), want %q", arr, ok, library.ArrSonarr)
		}
	})

	// Feed-configured deployment: the walk failure falls through to
	// handleLibraryGate, whose walk-failed completion line is the third
	// boundary.
	t.Run("radarr with feed", func(t *testing.T) {
		logger, recorder := capture.New()
		s := New(&Deps{
			Logger: logger,
			Store: &fakeStore{st: state.State{
				Mapping: frierenMappingCache(),
			}},
			Library: library.NewWalker(&library.Config{Radarr: &fakeRadarr{listErr: transportErr("radarr.local")}, Logger: scoutTestLogger()}),
			Mapping: fakeMapping{},
			SeaDex:  &fakeSeaDex{entries: seadexFrierenEntry()},
			Feed:    &fakeFeed{},
		})
		if healthy := s.Cycle(context.Background()); healthy {
			t.Fatal("feed-configured Cycle returned healthy=true, want false when the library walk fails")
		}
		if arr, ok := recordAttr(recorder, "library walk failed; cycle unhealthy", "arr"); !ok || arr != library.ArrRadarr {
			t.Errorf("walk-failure ERROR arr attr = %q (found=%t), want %q", arr, ok, library.ArrRadarr)
		}
		if arr, ok := recordAttr(recorder, "cycle degraded", "arr"); !ok || arr != library.ArrRadarr {
			t.Errorf("library-gate walk-failed completion-line arr attr = %q (found=%t), want %q", arr, ok, library.ArrRadarr)
		}
	})
}

// failOnceStore fails the first Save with a genuine (non-cancellation) error
// and succeeds on any later attempt, counting attempts, so a test can tell a
// single failed attempt apart from a failed-then-retried pair.
type failOnceStore struct {
	st       state.State
	attempts int
}

func (f *failOnceStore) Load(context.Context) (state.State, error) { return f.st, nil }

func (f *failOnceStore) Save(_ context.Context, st *state.State) error {
	f.attempts++
	if f.attempts == 1 {
		return errors.New("disk full")
	}
	f.st = *st
	return nil
}

// TestSaveGenuineFailureOnLiveContextIsNotRetried pins the retry SCOPE of
// save's documented contract: the detached context.WithoutCancel retry exists
// only for a shutdown cancellation (a redeploy SIGTERM landing mid-cycle). A
// genuine write failure on a live context must stay a single attempt that
// logs the "state save failed" ERROR - retrying it would paper over a real
// disk fault with a second write nothing asked for.
func TestSaveGenuineFailureOnLiveContextIsNotRetried(t *testing.T) {
	logger, recorder := capture.New()
	store := &failOnceStore{}
	s := New(&Deps{Logger: logger, Store: store})

	s.save(context.Background(), &state.State{Baselined: true})

	if store.attempts != 1 {
		t.Errorf("Save attempts = %d, want 1 (only a cancellation takes the detached retry)", store.attempts)
	}
	if store.st.Baselined {
		t.Error("state was persisted by a retry, want the genuinely-failed save left unpersisted")
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
}

// TestLoadStateDeadlineExceededIsNotAFault pins the deadline arm of
// loadState's shutdown tolerance: a load failing with
// context.DeadlineExceeded is handled like a cancellation - empty state and
// no "state load failed" ERROR (the shipped Loki rule alerts on every ERROR)
// - even when the cycle context itself is still live.
func TestLoadStateDeadlineExceededIsNotAFault(t *testing.T) {
	logger, recorder := capture.New()
	s := New(&Deps{Logger: logger, Store: &fakeStore{loadErr: context.DeadlineExceeded}})

	st := s.loadState(context.Background())

	if st.Baselined || len(st.Findings) != 0 {
		t.Errorf("loadState on a deadline-exceeded load = %+v, want empty state", st)
	}
	if n := recorder.CountExact("state load failed; starting from empty state"); n != 0 {
		t.Errorf("deadline-exceeded state load was logged as a fault %d times, want 0", n)
	}
}
