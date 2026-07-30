package scout

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/arrapi"
	"github.com/cplieger/seadex-scout/internal/anilist"
	"github.com/cplieger/seadex-scout/internal/arrwalk"
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
//
// It serves BOTH halves of the seam. A full fetch answers entries/err; a
// windowed fetch answers windowEntries/windowErr, so one fake can drive a
// reconcile and a tick in the same test without either half's scripting
// leaking into the other. Every call is recorded: opts carries the Options of
// each FetchEntries in order (which is how a test asserts a reconcile asked
// for FetchFull and a tick for FetchWindow with a live Since), and countSince
// carries each CountWindow's since.
type fakeSeaDex struct {
	err     error
	entries []seadex.Entry

	// countFn scripts the tick's probe. Nil reports len(windowEntries), the
	// consistent reading for a test that only sets a window.
	countFn       func(ctx context.Context, since time.Time) (int, error)
	windowEntries []seadex.Entry
	windowErr     error

	opts       []seadex.Options
	countSince []time.Time
}

func (f *fakeSeaDex) FetchEntries(_ context.Context, opts seadex.Options) ([]seadex.Entry, error) {
	f.opts = append(f.opts, opts)
	if opts.Mode == seadex.FetchWindow {
		return f.windowEntries, f.windowErr
	}
	return f.entries, f.err
}

func (f *fakeSeaDex) CountWindow(ctx context.Context, since time.Time) (int, error) {
	f.countSince = append(f.countSince, since)
	if f.countFn != nil {
		return f.countFn(ctx, since)
	}
	return len(f.windowEntries), nil
}

// fetchModes reports the mode of every FetchEntries call in order, which is
// what a dispatcher test asserts against.
func (f *fakeSeaDex) fetchModes() []seadex.FetchMode {
	modes := make([]seadex.FetchMode, 0, len(f.opts))
	for _, o := range f.opts {
		modes = append(modes, o.Mode)
	}
	return modes
}

// fakeFeed records FeedWriter.Rebuild and FeedWriter.Advance calls SEPARATELY,
// optionally failing either. Keeping the two counts apart is the point: a tick
// must advance and never rebuild, and a reconcile the reverse, so a shared
// counter could not tell a correct dispatch from an inverted one.
type fakeFeed struct {
	err        error
	advanceErr error
	calls      int
	entries    int

	advanceCalls   int
	advanceEntries int
	// advanceWindows records each Advance window's AniList IDs, so a test can
	// assert WHICH entries were folded in, not just how many.
	advanceWindows [][]int
}

func (f *fakeFeed) Rebuild(_ context.Context, entries []seadex.Entry, _ indexer.EntryInfoFunc) error {
	f.calls++
	f.entries = len(entries)
	return f.err
}

func (f *fakeFeed) Advance(_ context.Context, window []seadex.Entry, _ indexer.EntryInfoFunc) error {
	f.advanceCalls++
	f.advanceEntries = len(window)
	ids := make([]int, 0, len(window))
	for i := range window {
		ids = append(ids, window[i].AniListID)
	}
	f.advanceWindows = append(f.advanceWindows, ids)
	return f.advanceErr
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

// unreachableMapLoader returns a Fribb loader whose every refresh fails at the
// transport, with a per-test override path so no real overrides.json can be
// found: with no cached records in state it is the "map unusable" fixture (a
// plain load error, not a mapping.StaleMapError).
func unreachableMapLoader(t *testing.T, logger *slog.Logger) *mapping.Loader {
	t.Helper()
	return mapping.NewLoader(noNetworkClient(), "http://unused.invalid/f.json", filepath.Join(t.TempDir(), "ov.json"), time.Hour, logger)
}

// emptyRecordsMapLoader returns a Fribb loader whose upstream answers an empty
// record array, so a fresh 200 indexes to nothing and the map is unusable
// through the accept path rather than the transport.
func emptyRecordsMapLoader(t *testing.T, logger *slog.Logger) *mapping.Loader {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("[]"))
	}))
	t.Cleanup(srv.Close)
	return mapping.NewLoader(srv.Client(), srv.URL, filepath.Join(t.TempDir(), "ov.json"), time.Hour, logger)
}

// aniStatsFn adapts an AniList client's Stats to the Deps.AniListStats
// callback, mirroring the composition root's wiring.
func aniStatsFn(c *anilist.Client) func() AniListStats {
	return func() AniListStats {
		st := c.Stats()
		return AniListStats{Calls: st.Calls, RateLimitWaits: st.RateLimitWaits}
	}
}

// TestLoadStateCanceledContextIsNotAFault pins loadState's shutdown handling:
// a SIGTERM already visible while state loads is the redeploy, not a state
// corruption or read fault, so no ERROR record may be emitted (the shipped
// Loki rule alerts on every ERROR) - the following context-aware cycle stage
// reports the shutdown once at WARN.
func TestLoadStateCanceledContextIsNotAFault(t *testing.T) {
	logger, recorder := capture.New()
	store := &fakeStore{st: state.State{ShrunkWalks: 1}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	s := New(&Deps{Logger: logger, Store: store})
	st := s.loadState(ctx)

	if st.ShrunkWalks != 0 {
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
		Library: arrwalk.NewWalker(&arrwalk.Config{Sonarr: &fakeSonarr{listErr: errors.New("sonarr down")}, Logger: scoutTestLogger()}),
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

// TestCycleSeaDexFailureIsHealthyAndReportsNothing pins the SeaDex-outage
// gate: the cycle is degraded-but-healthy, still saves the refreshed library
// snapshot, and never reaches Report. Reporting is what makes not-reporting
// meaningful, so an outage must leave the notifier's current set standing
// rather than replace it with an empty one - otherwise a SeaDex hiccup would
// stop reporting every condition that is still true.
func TestCycleSeaDexFailureIsHealthyAndReportsNothing(t *testing.T) {
	logger, recorder := capture.New()
	store := &fakeStore{st: state.State{Mapping: frierenMappingCache()}}
	sonarr := &fakeSonarr{
		series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}},
		files: map[int][]arrapi.EpisodeFile{
			7: {{SeasonNumber: 1, ReleaseGroup: "Erai-raws"}},
		},
	}
	s := New(&Deps{
		Logger:   logger,
		Store:    store,
		Library:  arrwalk.NewWalker(&arrwalk.Config{Sonarr: sonarr, Logger: scoutTestLogger()}),
		Mapping:  fakeMapping{},
		SeaDex:   &fakeSeaDex{err: errors.New("seadex down")},
		Notifier: notify.NewNotifier(logger, nil),
	})

	if healthy := s.Cycle(context.Background()); !healthy {
		t.Fatal("Cycle returned healthy=false, want true for degraded SeaDex failure")
	}
	loaded := store.st
	if len(loaded.Library.Items) != 1 || loaded.Library.Items[0].Title != "Frieren" {
		t.Errorf("library snapshot after degraded cycle = %+v, want refreshed Frieren snapshot", loaded.Library)
	}
	if n := recorder.CountExact("findings reported"); n != 0 {
		t.Errorf("SeaDex-outage cycle reported findings %d times, want 0 (the gate runs no compare)", n)
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
	want := state.State{ShrunkWalks: 1}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.save(ctx, &want)

	got, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load() after save with canceled context: %v", err)
	}
	if got.ShrunkWalks != 1 {
		t.Errorf("Load().ShrunkWalks = %d, want 1", got.ShrunkWalks)
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
		Library: arrwalk.NewWalker(&arrwalk.Config{Sonarr: &fakeSonarr{listErr: walkErr}, Logger: scoutTestLogger()}),
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
		Library: arrwalk.NewWalker(&arrwalk.Config{Sonarr: &fakeSonarr{listErr: walkErr}, Logger: scoutTestLogger()}),
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

	// The report boundary is the reporter role's, so build it that way rather
	// than calling Report on a cycle Scout.
	reporter := NewReporter(&ReportDeps{
		Logger:  scoutTestLogger(),
		Store:   &fakeStore{},
		Library: arrwalk.NewWalker(&arrwalk.Config{Sonarr: &fakeSonarr{listErr: walkErr}, Logger: scoutTestLogger()}),
	})
	_, err := reporter.Report(context.Background())
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
			Library: arrwalk.NewWalker(&arrwalk.Config{Sonarr: &fakeSonarr{listErr: transportErr("sonarr.local")}, Logger: scoutTestLogger()}),
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
			Library: arrwalk.NewWalker(&arrwalk.Config{Radarr: &fakeRadarr{listErr: transportErr("radarr.local")}, Logger: scoutTestLogger()}),
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

	s.save(context.Background(), &state.State{ShrunkWalks: 1})

	if store.attempts != 1 {
		t.Errorf("Save attempts = %d, want 1 (only a cancellation takes the detached retry)", store.attempts)
	}
	if store.st.ShrunkWalks != 0 {
		t.Error("state was persisted by a retry, want the genuinely-failed save left unpersisted")
	}
	errCount := recorder.CountLevel(slog.LevelError, "state save failed")
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

	if st.ShrunkWalks != 0 || len(st.Memo.Entries) != 0 {
		t.Errorf("loadState on a deadline-exceeded load = %+v, want empty state", st)
	}
	if n := recorder.CountExact("state load failed; starting from empty state"); n != 0 {
		t.Errorf("deadline-exceeded state load was logged as a fault %d times, want 0", n)
	}
}

// TestSavePreservationRefusalWarnsInsteadOfErroring pins the log-level
// classification alerts.yaml depends on: a Save the store deliberately
// REFUSED in order to preserve unclassifiable on-disk bytes
// (state.ErrSavePreserved) is a degradation, not a write fault, so it must
// log "state save skipped; on-disk state preserved" at WARN and never the
// "state save failed" ERROR the SeadexScoutCycleError rule fires on -
// including on the cancelled-context path, where a redeploy SIGTERM landing
// in Load's read window is exactly what sets the block.
func TestSavePreservationRefusalWarnsInsteadOfErroring(t *testing.T) {
	tests := []struct {
		name string
		ctx  func() context.Context
	}{
		{name: "live context", ctx: context.Background},
		{name: "cancelled context, detached retry also refused", ctx: func() context.Context {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logger, recorder := capture.New()
			refusal := fmt.Errorf("state: save /config/state.json: blocked after an unclassified read failure: %w", state.ErrSavePreserved)
			s := New(&Deps{Logger: logger, Store: &fakeStore{saveErr: refusal}})

			s.save(tc.ctx(), &state.State{ShrunkWalks: 1})

			if n := recorder.CountLevel(slog.LevelWarn, "state save skipped; on-disk state preserved"); n != 1 {
				t.Errorf("preservation-refusal WARN count = %d, want exactly 1", n)
			}
			if n := recorder.CountLevel(slog.LevelError, "state save failed"); n != 0 {
				t.Errorf("\"state save failed\" ERROR count = %d, want 0 (alerts.yaml documents this refusal as deliberately NOT reaching SeadexScoutCycleError)", n)
			}
		})
	}
}

// slowCancelStore spends part of the shutdown budget inside its FIRST Save
// before reporting the cancellation, then records the deadline its retry was
// handed - the only way to observe how much grace the second attempt was given.
type slowCancelStore struct {
	spend        time.Duration
	retryBudget  time.Duration
	attempts     int
	retryHadDead bool
}

func (s *slowCancelStore) Load(context.Context) (state.State, error) { return state.State{}, nil }

func (s *slowCancelStore) Save(ctx context.Context, _ *state.State) error {
	s.attempts++
	if s.attempts == 1 {
		time.Sleep(s.spend)
		return context.Canceled
	}
	if dl, ok := ctx.Deadline(); ok {
		s.retryHadDead, s.retryBudget = true, time.Until(dl)
	}
	return nil
}

// TestSaveRetryAlwaysGetsTheAnchoredGrace pins saveGrace as the budget the
// detached retry gets measured from the CANCELLATION, not from entry into save:
// a first attempt that already spent time on pre-SIGTERM cycle work must still
// hand the retry a full grace, since that time never belonged to the container
// stop grace. Starving it to zero would skip the retry in exactly the slow-volume
// case it exists for, losing the AniList memo and logging a routine redeploy as a
// write fault.
func TestSaveRetryAlwaysGetsTheAnchoredGrace(t *testing.T) {
	const spend = saveGrace / 100
	store := &slowCancelStore{spend: spend}
	s := New(&Deps{Logger: slog.New(slog.DiscardHandler), Store: store})

	s.save(context.Background(), &state.State{ShrunkWalks: 1})

	if store.attempts != 2 {
		t.Fatalf("Save attempts = %d, want 2 (the cancellation takes the detached retry)", store.attempts)
	}
	if !store.retryHadDead {
		t.Fatal("retry context carried no deadline, want the anchored shutdown budget")
	}
	if store.retryBudget <= saveGrace-spend {
		t.Errorf("retry budget = %v, want ~%v (a full grace anchored at the cancellation, not shortened by the first attempt)", store.retryBudget, saveGrace)
	}
}
