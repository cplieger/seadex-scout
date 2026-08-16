package arrwalk

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/cplieger/arrapi"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/slogx/capture"
)

// The walker produces the internal/library model; these test-local aliases keep
// the walk assertions reading as they did when the model lived in this package.
// The walker itself always names the model explicitly (library.Item), so the
// dependency direction stays visible in the production code.
type (
	Item     = library.Item
	Snapshot = library.Snapshot
)

const (
	ArrSonarr = library.ArrSonarr
	ArrRadarr = library.ArrRadarr
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeSonarr is a scripted SonarrClient: GetSeries returns series (or listErr),
// GetEpisodeFiles returns files[id] (or epErr[id]), GetTags returns the canned
// tag list (or tagErr).
type fakeSonarr struct {
	files   map[int][]arrapi.EpisodeFile
	epErr   map[int]error
	listErr error
	tagErr  error
	series  []arrapi.Series
	tags    []arrapi.Tag
}

func (f *fakeSonarr) GetSeries(context.Context) ([]arrapi.Series, error) {
	return f.series, f.listErr
}

func (f *fakeSonarr) GetEpisodeFiles(_ context.Context, seriesID int) ([]arrapi.EpisodeFile, error) {
	if err := f.epErr[seriesID]; err != nil {
		return nil, err
	}
	return f.files[seriesID], nil
}

func (f *fakeSonarr) GetTags(context.Context) ([]arrapi.Tag, error) {
	return f.tags, f.tagErr
}

func epFile(season int, group string) arrapi.EpisodeFile {
	return arrapi.EpisodeFile{SeasonNumber: season, ReleaseGroup: group}
}

// TestWalkSonarrPartialEpisodeFailure pins the "ingest succeeded == healthy"
// semantic: a sub-budget per-series episode-fetch failure keeps the series as
// a Failed placeholder (identity only, no file data, so a transient fetch
// failure is not misread as a real no-file item) while the walk as a whole
// succeeds and the other series carry their groups. The Partial assertion also
// pins the producer side of the Snapshot.Partial contract internal/scout's
// pre-compare gate depends on. Run under -race, it also
// exercises the bounded-concurrency episode fetch.
func TestWalkSonarrPartialEpisodeFailure(t *testing.T) {
	fs := &fakeSonarr{
		series: []arrapi.Series{
			{ID: 1, Title: "Alpha"},
			{ID: 2, Title: "Bravo"},
			{ID: 3, Title: "Charlie"},
		},
		files: map[int][]arrapi.EpisodeFile{
			1: {epFile(1, "PMR")},
			3: {epFile(1, "LostYears")},
		},
		epErr: map[int]error{2: errors.New("episode fetch boom")},
	}
	w := NewWalker(&Config{Sonarr: fs, Logger: discardLogger()})

	snap, err := w.Walk(t.Context())
	if err != nil {
		t.Fatalf("Walk returned error, want nil (partial failure is not fatal): %v", err)
	}
	if len(snap.Items) != 3 {
		t.Fatalf("items = %d, want 3 (the failed series stays as a Failed placeholder)", len(snap.Items))
	}
	if !snap.Partial {
		t.Error("Snapshot.Partial = false, want true")
	}

	byID := make(map[int]Item, len(snap.Items))
	for _, it := range snap.Items {
		byID[it.ArrID] = it
	}
	if got := byID[1].Groups; len(got) != 1 || got[0] != "pmr" {
		t.Errorf("Alpha groups = %v, want [pmr]", got)
	}
	if !byID[1].HasFile {
		t.Error("Alpha HasFile = false, want true")
	}
	if byID[1].Failed {
		t.Error("Alpha Failed = true, want false (its fetch succeeded)")
	}
	bravo, ok := byID[2]
	if !ok {
		t.Fatal("Bravo (episode fetch failed) is absent, want a Failed placeholder item")
	}
	if !bravo.Failed || bravo.HasFile || len(bravo.Groups) != 0 || bravo.Title != "Bravo" {
		t.Errorf("Bravo placeholder = %+v, want Failed=true with identity and no file data", bravo)
	}
	if got := byID[3].Groups; len(got) != 1 || got[0] != "lostyears" {
		t.Errorf("Charlie groups = %v, want [lostyears]", got)
	}
}

// TestWalkSonarrFailureBudgetFailsWalk pins the walk failure budget: once
// episodeFailureBudget series have failed their episode fetch, the walk fails
// as a whole (an arr outage is an ingest failure, so the cycle goes unhealthy)
// instead of grinding through every remaining series, and no snapshot is
// published.
func TestWalkSonarrFailureBudgetFailsWalk(t *testing.T) {
	fs := &fakeSonarr{epErr: map[int]error{}}
	for id := 1; id <= episodeFailureBudget+3; id++ {
		fs.series = append(fs.series, arrapi.Series{ID: id, Title: "Series"})
		fs.epErr[id] = errors.New("sonarr down")
	}
	w := NewWalker(&Config{Sonarr: fs, Logger: discardLogger()})

	snap, err := w.Walk(t.Context())
	if err == nil {
		t.Fatal("Walk returned nil error, want the walk failure budget error")
	}
	if !strings.Contains(err.Error(), "failure budget") {
		t.Errorf("error = %q, want it to name the walk failure budget", err.Error())
	}
	if len(snap.Items) != 0 || !snap.TakenAt.IsZero() {
		t.Errorf("snapshot = %+v, want the zero Snapshot on a budget failure", snap)
	}
}

// TestWalkSonarrTotalEpisodeFailureFailsWalk pins the sub-budget total-failure
// rule: a library whose kept series count is below episodeFailureBudget can
// never trip the absolute budget, so when EVERY kept series' episode fetch
// fails (a total episode-endpoint outage: GetSeries ok, each per-series fetch
// failing) the walk must fail as a whole - an ingest failure, so the cycle
// goes unhealthy - instead of publishing a "partial" snapshot with zero
// usable file data that would read healthy through the outage.
func TestWalkSonarrTotalEpisodeFailureFailsWalk(t *testing.T) {
	tests := []struct {
		name   string
		series int
	}{
		{name: "single kept series", series: 1},
		{name: "several kept series below budget", series: episodeFailureBudget - 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeSonarr{epErr: map[int]error{}}
			for id := 1; id <= tc.series; id++ {
				fs.series = append(fs.series, arrapi.Series{ID: id, Title: "Series"})
				fs.epErr[id] = errors.New("episode endpoint down")
			}
			w := NewWalker(&Config{Sonarr: fs, Logger: discardLogger()})

			snap, err := w.Walk(t.Context())
			if err == nil {
				t.Fatal("Walk returned nil error, want the total episode-failure error")
			}
			want := fmt.Sprintf("all %d kept series failed", tc.series)
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), want)
			}
			if len(snap.Items) != 0 || !snap.TakenAt.IsZero() {
				t.Errorf("snapshot = %+v, want the zero Snapshot on a total failure", snap)
			}
		})
	}
}

// TestWalkAppliesIncludeTagFilter verifies the arr-side include-tag filter drops
// series that lack an included tag before they enter the snapshot.
func TestWalkAppliesIncludeTagFilter(t *testing.T) {
	fs := &fakeSonarr{
		series: []arrapi.Series{
			{ID: 1, Title: "Kept", Tags: []int{7}},
			{ID: 2, Title: "Dropped", Tags: []int{3}},
		},
		files: map[int][]arrapi.EpisodeFile{
			1: {epFile(1, "PMR")},
		},
		tags: []arrapi.Tag{{ID: 7, Label: "anime"}},
	}
	w := NewWalker(&Config{Sonarr: fs, IncludeTags: []string{"anime"}, Logger: discardLogger()})

	snap, err := w.Walk(t.Context())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(snap.Items) != 1 || snap.Items[0].ArrID != 1 {
		t.Fatalf("items = %+v, want only the tag-included series (id 1)", snap.Items)
	}
}

// TestWalkAppliesIncludeAndExcludeTagFiltersTogether pins the combined
// filter shape the deployed config uses: an include set AND an exclude set
// configured on the same walk keep only the included, non-excluded items.
func TestWalkAppliesIncludeAndExcludeTagFiltersTogether(t *testing.T) {
	fs := &fakeSonarr{
		series: []arrapi.Series{
			{ID: 1, Title: "Kept", Tags: []int{7}},
			{ID: 2, Title: "Excluded", Tags: []int{7, 9}},
		},
		files: map[int][]arrapi.EpisodeFile{
			1: {epFile(1, "PMR")},
		},
		tags: []arrapi.Tag{{ID: 7, Label: "anime"}, {ID: 9, Label: "skip"}},
	}
	fr := &fakeRadarr{
		movies: []arrapi.Movie{{ID: 10, Title: "Kept Movie", Tags: []int{7}}},
		tags:   []arrapi.Tag{{ID: 7, Label: "anime"}, {ID: 9, Label: "skip"}},
	}
	w := NewWalker(&Config{
		Sonarr:      fs,
		Radarr:      fr,
		IncludeTags: []string{"anime"},
		ExcludeTags: []string{"skip"},
		Logger:      discardLogger(),
	})

	snap, err := w.Walk(t.Context())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(snap.Items) != 2 {
		t.Fatalf("items = %+v, want the include-tagged series and movie only", snap.Items)
	}
	for _, it := range snap.Items {
		if it.Arr == ArrSonarr && it.ArrID == 2 {
			t.Error("excluded series (id 2) present, want it dropped by the exclude set")
		}
	}
}

// TestWalkWithoutTagFiltersDoesNotDependOnTagEndpoint verifies that an
// unconfigured tag filter leaves the tag endpoint outside the walk's behavior.
func TestWalkWithoutTagFiltersDoesNotDependOnTagEndpoint(t *testing.T) {
	tagErr := errors.New("tag endpoint unavailable")
	fs := &fakeSonarr{
		series: []arrapi.Series{{ID: 1, Title: "Alpha"}},
		files:  map[int][]arrapi.EpisodeFile{1: {epFile(1, "PMR")}},
		tagErr: tagErr,
	}
	fr := &fakeRadarr{
		movies: []arrapi.Movie{{ID: 2, Title: "Movie"}},
		tagErr: tagErr,
	}
	w := NewWalker(&Config{Sonarr: fs, Radarr: fr, Logger: discardLogger()})

	snap, err := w.Walk(t.Context())
	if err != nil {
		t.Fatalf("Walk with no tag filters: %v", err)
	}
	if len(snap.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(snap.Items))
	}
	if got := snap.Items[0].Key(); got != "sonarr:1" {
		t.Errorf("first item key = %q, want %q", got, "sonarr:1")
	}
	if got := snap.Items[1].Key(); got != "radarr:2" {
		t.Errorf("second item key = %q, want %q", got, "radarr:2")
	}
}

func TestIsDualAudio(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"Japanese / English", true},
		{"jpn/eng", true},
		{"Japanese, English", true},
		{"Japanese/English/Commentary", true},
		{"Japanese", false},
		{"", false},
		{"jpn / jpn", false}, // same language repeated is not dual audio
		{"  eng  /  eng ", false},
	}
	for _, tc := range tests {
		if got := isDualAudio(tc.in); got != tc.want {
			t.Errorf("isDualAudio(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestKeepByTags(t *testing.T) {
	set := func(ids ...int) map[int]struct{} {
		m := make(map[int]struct{}, len(ids))
		for _, id := range ids {
			m[id] = struct{}{}
		}
		return m
	}
	tests := []struct {
		name             string
		include, exclude map[int]struct{}
		itemTags         []int
		want             bool
	}{
		{"no filters keeps all", nil, nil, []int{1}, true},
		{"include match kept", set(2), nil, []int{1, 2}, true},
		{"include miss dropped", set(9), nil, []int{1}, false},
		{"exclude match dropped", nil, set(5), []int{5}, false},
		{"exclude miss kept", nil, set(5), []int{1}, true},
		{"exclude wins over include", set(2), set(5), []int{2, 5}, false},
		{"configured include with no resolved IDs drops all", map[int]struct{}{}, nil, []int{1}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := keepByTags(tc.itemTags, tc.include, tc.exclude); got != tc.want {
				t.Errorf("keepByTags(%v) = %v, want %v", tc.itemTags, got, tc.want)
			}
		})
	}
}

// boundedSonarr blocks each GetEpisodeFiles until released, recording the peak
// number of simultaneous in-flight fetches so a test can prove the walker
// bounds concurrency at episodeConcurrency.
type boundedSonarr struct {
	started   chan int
	release   chan struct{}
	series    []arrapi.Series
	mu        sync.Mutex
	active    int
	maxActive int
}

func (f *boundedSonarr) GetSeries(context.Context) ([]arrapi.Series, error) {
	return f.series, nil
}

func (f *boundedSonarr) GetEpisodeFiles(ctx context.Context, seriesID int) ([]arrapi.EpisodeFile, error) {
	f.mu.Lock()
	f.active++
	if f.active > f.maxActive {
		f.maxActive = f.active
	}
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.active--
		f.mu.Unlock()
	}()

	select {
	case f.started <- seriesID:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case <-f.release:
		return []arrapi.EpisodeFile{epFile(1, "PMR")}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (f *boundedSonarr) GetTags(context.Context) ([]arrapi.Tag, error) {
	return nil, nil
}

func (f *boundedSonarr) max() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxActive
}

func TestWalkSonarrBoundsEpisodeFetchConcurrency(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		seriesCount := episodeConcurrency + 3
		fs := &boundedSonarr{
			started: make(chan int, seriesCount),
			release: make(chan struct{}, seriesCount),
		}
		for id := 1; id <= seriesCount; id++ {
			fs.series = append(fs.series, arrapi.Series{ID: id, Title: "Series"})
		}
		w := NewWalker(&Config{Sonarr: fs, Logger: discardLogger()})

		done := make(chan error, 1)
		go func() {
			snap, err := w.Walk(t.Context())
			if err == nil && len(snap.Items) != seriesCount {
				err = errors.New("walk returned the wrong item count")
			}
			done <- err
		}()

		synctest.Wait()
		if got := len(fs.started); got != episodeConcurrency {
			t.Fatalf("started episode fetches = %d, want %d before release", got, episodeConcurrency)
		}
		if got := fs.max(); got != episodeConcurrency {
			t.Fatalf("max concurrent episode fetches = %d, want %d", got, episodeConcurrency)
		}

		fs.release <- struct{}{}
		synctest.Wait()
		if got := len(fs.started); got != episodeConcurrency+1 {
			t.Fatalf("started episode fetches after one release = %d, want %d", got, episodeConcurrency+1)
		}

		for range seriesCount - 1 {
			fs.release <- struct{}{}
		}
		synctest.Wait()
		if err := <-done; err != nil {
			t.Fatalf("Walk: %v", err)
		}
		if got := fs.max(); got > episodeConcurrency {
			t.Fatalf("max concurrent episode fetches = %d, want <= %d", got, episodeConcurrency)
		}
	})
}

// cancelingSonarr cancels the walk context from inside GetEpisodeFiles,
// simulating a shutdown/timeout during the episode fetch.
type cancelingSonarr struct {
	cancel context.CancelFunc
	series []arrapi.Series
}

func (f *cancelingSonarr) GetSeries(context.Context) ([]arrapi.Series, error) {
	return f.series, nil
}

func (f *cancelingSonarr) GetEpisodeFiles(ctx context.Context, _ int) ([]arrapi.EpisodeFile, error) {
	f.cancel()
	<-ctx.Done()
	return nil, ctx.Err()
}

func (f *cancelingSonarr) GetTags(context.Context) ([]arrapi.Tag, error) {
	return nil, nil
}

func TestWalkSonarrEpisodeCancellationIsFatalWithoutWarn(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	fs := &cancelingSonarr{
		series: []arrapi.Series{{ID: 1, Title: "Alpha"}},
		cancel: cancel,
	}
	logger, rec := capture.New()
	w := NewWalker(&Config{Sonarr: fs, Logger: logger})

	_, err := w.Walk(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Walk error = %v, want context.Canceled", err)
	}
	if rec.CountLevel(slog.LevelWarn, "") > 0 {
		t.Fatal("Walk logged a warning for context cancellation; want cancellation treated as shutdown, not an arr fault")
	}
}

// fakeRadarr is a scripted RadarrClient.
type fakeRadarr struct {
	listErr error
	tagErr  error
	movies  []arrapi.Movie
	tags    []arrapi.Tag
}

func (f *fakeRadarr) GetMovies(context.Context) ([]arrapi.Movie, error) {
	return f.movies, f.listErr
}

func (f *fakeRadarr) GetTags(context.Context) ([]arrapi.Tag, error) {
	return f.tags, f.tagErr
}

func TestWalkRadarrAppliesExcludeTagsAndBuildsMovieItem(t *testing.T) {
	fr := &fakeRadarr{
		movies: []arrapi.Movie{
			{
				ID:              10,
				Title:           "Kept Movie",
				ImdbID:          "tt0000010",
				TmdbID:          1234,
				Year:            2024,
				Tags:            []int{1},
				AlternateTitles: []arrapi.AlternateTitle{{Title: "Alt Movie"}, {Title: "   "}},
				HasFile:         true,
				MovieFile: &arrapi.MovieFile{
					ReleaseGroup: "PMR",
					SceneName:    "[PMR] Kept Movie (2024) [1080p][x265][Dual Audio]",
					MediaInfo:    &arrapi.MediaInfo{VideoCodec: "HEVC", AudioLanguages: "Japanese / English"},
				},
			},
			{ID: 20, Title: "Dropped Movie", Tags: []int{9}, HasFile: true, MovieFile: &arrapi.MovieFile{ReleaseGroup: "Other"}},
		},
		tags: []arrapi.Tag{{ID: 9, Label: "skip"}},
	}
	w := NewWalker(&Config{Radarr: fr, ExcludeTags: []string{"skip"}, RadarrURL: "https://radarr.example", Logger: discardLogger()})

	snap, err := w.Walk(t.Context())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(snap.Items) != 1 {
		t.Fatalf("items = %+v, want only the movie without the excluded tag", snap.Items)
	}
	item := snap.Items[0]
	if item.Arr != ArrRadarr || item.ArrID != 10 || item.Title != "Kept Movie" {
		t.Fatalf("movie identity = %+v, want kept Radarr movie id 10", item)
	}
	if item.ArrURL != "https://radarr.example/movie/1234" {
		t.Errorf("ArrURL = %q, want Radarr deep link", item.ArrURL)
	}
	if !item.HasFile {
		t.Error("HasFile = false, want true")
	}
	if len(item.Groups) != 1 || item.Groups[0] != "pmr" {
		t.Errorf("Groups = %v, want [pmr]", item.Groups)
	}
	if item.Current.Group != "pmr" || item.Current.Codec != "x265" || item.Current.Resolution != "1080p" || !item.Current.DualAudio {
		t.Errorf("Current = %+v, want normalized pmr/x265/1080p dual-audio fingerprint", item.Current)
	}
	if len(item.AltTitles) != 1 || item.AltTitles[0] != "Alt Movie" {
		t.Errorf("AltTitles = %v, want only the non-empty alternate title", item.AltTitles)
	}
}

// TestWalkSonarrTagResolutionCancellationIsFatal proves that a context
// cancellation surfaced by the tag-list fetch aborts the whole walk rather
// than fail-opening the filter.
func TestWalkSonarrTagResolutionCancellationIsFatal(t *testing.T) {
	fs := &fakeSonarr{
		series: []arrapi.Series{{ID: 1, Title: "Alpha"}},
		tagErr: context.Canceled,
	}
	w := NewWalker(&Config{Sonarr: fs, IncludeTags: []string{"anime"}, Logger: discardLogger()})
	if _, err := w.Walk(t.Context()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Walk error = %v, want context.Canceled propagated from tag resolution", err)
	}
}

// TestWalkRadarrTagResolutionCancellationIsFatal is the Radarr-side counterpart:
// the Radarr walk previously had no post-resolution cancellation check, so a
// cancellation during tag resolution must now propagate.
func TestWalkRadarrTagResolutionCancellationIsFatal(t *testing.T) {
	fr := &fakeRadarr{
		movies: []arrapi.Movie{{ID: 1, Title: "Movie"}},
		tagErr: context.Canceled,
	}
	w := NewWalker(&Config{Radarr: fr, ExcludeTags: []string{"skip"}, Logger: discardLogger()})
	if _, err := w.Walk(t.Context()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Walk error = %v, want context.Canceled propagated from tag resolution", err)
	}
}

// TestWalkSonarrTagResolutionErrorFailsClosed proves an ordinary
// (non-cancellation) tag-resolution failure fails the whole walk (fail
// closed): silently disabling the filter would admit every item past the
// configured arr_tags scoping for the cycle.
func TestWalkSonarrTagResolutionErrorFailsClosed(t *testing.T) {
	boom := errors.New("arr tag lookup boom")
	fs := &fakeSonarr{
		series: []arrapi.Series{
			{ID: 1, Title: "Alpha", Tags: []int{1}},
			{ID: 2, Title: "Bravo", Tags: []int{2}},
		},
		files: map[int][]arrapi.EpisodeFile{
			1: {epFile(1, "PMR")},
			2: {epFile(1, "LostYears")},
		},
		tagErr: boom,
	}
	w := NewWalker(&Config{Sonarr: fs, IncludeTags: []string{"anime"}, Logger: discardLogger()})
	_, err := w.Walk(t.Context())
	if !errors.Is(err, boom) {
		t.Fatalf("Walk error = %v, want the tag-resolution failure propagated (fail closed)", err)
	}
	// The tag list is fetched once for both label sets, so the error names the
	// arr_tags resolution step rather than a single label set.
	if !strings.Contains(err.Error(), "arr_tags") {
		t.Errorf("error = %q, want it to name the arr_tags resolution step", err.Error())
	}
}

// TestWalkSonarrTagResolutionLiveTimeoutFailsClosed pins the
// per-request-timeout contract: arrapi wraps each request in its own
// context.WithTimeout, so a DeadlineExceeded surfaced by the tag-list fetch
// while the walk context is still live is a real resolution failure and fails
// the walk closed like any other tag error.
func TestWalkSonarrTagResolutionLiveTimeoutFailsClosed(t *testing.T) {
	fs := &fakeSonarr{
		series: []arrapi.Series{{ID: 1, Title: "Alpha", Tags: []int{1}}},
		files:  map[int][]arrapi.EpisodeFile{1: {epFile(1, "PMR")}},
		tagErr: context.DeadlineExceeded,
	}
	w := NewWalker(&Config{Sonarr: fs, IncludeTags: []string{"anime"}, Logger: discardLogger()})
	if _, err := w.Walk(t.Context()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Walk error = %v, want the live-context timeout propagated (fail closed)", err)
	}
}

// TestWalkSonarrSeriesItemAggregatesGroupsSeasonsAndFingerprint exercises the
// multi-episode aggregation seriesItem performs through the public Walk API: a
// series with four episode files across two seasons and two groups (pmr x3,
// lostyears x1) must expose the distinct groups, the mixed-group flag, the
// per-season group sets, and a Current fingerprint derived from the dominant
// group's episode MediaInfo (representative picks pmr, the most common group).
func TestWalkSonarrSeriesItemAggregatesGroupsSeasonsAndFingerprint(t *testing.T) {
	fs := &fakeSonarr{
		series: []arrapi.Series{{ID: 1, Title: "Multi", TvdbID: 555, Year: 2023}},
		files: map[int][]arrapi.EpisodeFile{
			1: {
				{
					SeasonNumber: 1,
					ReleaseGroup: "PMR",
					SceneName:    "[PMR] Multi S01E01 [1080p][x265]",
					MediaInfo:    &arrapi.MediaInfo{VideoCodec: "HEVC", AudioLanguages: "Japanese / English"},
				},
				epFile(1, "PMR"),
				epFile(1, "LostYears"),
				epFile(2, "PMR"),
			},
		},
	}
	w := NewWalker(&Config{Sonarr: fs, Logger: discardLogger()})

	snap, err := w.Walk(t.Context())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(snap.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(snap.Items))
	}
	it := snap.Items[0]

	if !slices.Equal(it.Groups, []string{"lostyears", "pmr"}) {
		t.Errorf("Groups = %v, want [lostyears pmr]", it.Groups)
	}
	if !slices.Equal(it.SeasonGroups[1], []string{"lostyears", "pmr"}) {
		t.Errorf("SeasonGroups[1] = %v, want [lostyears pmr]", it.SeasonGroups[1])
	}
	if !slices.Equal(it.SeasonGroups[2], []string{"pmr"}) {
		t.Errorf("SeasonGroups[2] = %v, want [pmr]", it.SeasonGroups[2])
	}
	// representative picks the dominant group (pmr: 3 files, lostyears: 1) and the
	// fingerprint is classified from that dominant file's episode MediaInfo.
	if it.Current.Group != "pmr" {
		t.Errorf("Current.Group = %q, want pmr (dominant group)", it.Current.Group)
	}
	if it.Current.Codec != "x265" || it.Current.Resolution != "1080p" || !it.Current.DualAudio {
		t.Errorf("Current = %+v, want x265/1080p/dual-audio from the episode MediaInfo", it.Current)
	}
}

func TestWalkSonarrSeriesWithNoFilesHasNoGroups(t *testing.T) {
	fs := &fakeSonarr{
		series: []arrapi.Series{{ID: 1, Title: "Monitored NoFiles", TvdbID: 42}},
		// GetEpisodeFiles lists only episodes with files, so a fileless series
		// yields an empty list (the fetch itself succeeds).
		files: map[int][]arrapi.EpisodeFile{1: {}},
	}
	w := NewWalker(&Config{Sonarr: fs, Logger: discardLogger()})

	snap, err := w.Walk(t.Context())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(snap.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(snap.Items))
	}
	it := snap.Items[0]
	if it.HasFile {
		t.Error("HasFile = true, want false for a series with no episode files")
	}
	if len(it.Groups) != 0 {
		t.Errorf("Groups = %v, want empty for a series with no files", it.Groups)
	}
	if it.SeasonGroups != nil {
		t.Errorf("SeasonGroups = %v, want nil for a series with no files", it.SeasonGroups)
	}
	if it.Current.Group != "" {
		t.Errorf("Current.Group = %q, want empty (fingerprint skipped for a fileless series, matching the fileless-movie shape)", it.Current.Group)
	}
}

// TestWalkUnmatchedTagWarningNeverEmitsTagValues pins the credential-safety
// contract of the unmatched-tag diagnostic: configured arr_tags values pass
// through allowlisted ${VAR} expansion, so a typo like ${SONARR_API_KEY} can
// place a secret in the label set. The warning is pinned structurally to the
// count-only shape (exact message, exactly the which + unmatched_count
// attributes), so any future full OR partial tag-value field fails the test
// without relying on spotting a particular secret substring.
func TestWalkUnmatchedTagWarningNeverEmitsTagValues(t *testing.T) {
	const sentinel = "sekrit-expanded-tag-9f8e7d"
	fs := &fakeSonarr{
		series: []arrapi.Series{
			{ID: 1, Title: "Kept", Tags: []int{7}},
		},
		files: map[int][]arrapi.EpisodeFile{
			1: {epFile(1, "PMR")},
		},
		tags: []arrapi.Tag{{ID: 7, Label: "anime"}},
	}
	logger, rec := capture.New()
	w := NewWalker(&Config{Sonarr: fs, IncludeTags: []string{"anime", sentinel}, Logger: logger})

	if _, err := w.Walk(t.Context()); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	warnings := 0
	for _, r := range rec.Records() {
		if r.Message != "configured tags matched no arr tag" {
			continue
		}
		warnings++
		if n := r.NumAttrs(); n != 2 {
			t.Errorf("unmatched-tag warning carries %d attributes, want exactly 2 (which, unmatched_count)", n)
		}
		r.Attrs(func(a slog.Attr) bool {
			switch a.Key {
			case "which":
				if got := a.Value.String(); got != "arr_tags.include" {
					t.Errorf("which = %q, want %q", got, "arr_tags.include")
				}
			case "unmatched_count":
				if got := a.Value.String(); got != "1" {
					t.Errorf("unmatched_count = %q, want %q", got, "1")
				}
			default:
				t.Errorf("unexpected attribute %s=%q on the count-only unmatched-tag warning", a.Key, a.Value)
			}
			return true
		})
	}
	if warnings != 1 {
		t.Fatalf("got %d unmatched-tag warnings, want exactly 1", warnings)
	}
}

func TestWalkRadarrContextCancellationAfterListIsFatal(t *testing.T) {
	fr := &fakeRadarr{movies: []arrapi.Movie{{ID: 1, Title: "Movie", HasFile: false}}}
	w := NewWalker(&Config{Radarr: fr, Logger: discardLogger()})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := w.Walk(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Walk error = %v, want context.Canceled surfaced by the post-walk guard", err)
	}
}

func TestWalkRadarrMovieWithoutFileHasNoGroups(t *testing.T) {
	fr := &fakeRadarr{
		movies: []arrapi.Movie{
			{ID: 10, Title: "No File Movie", TmdbID: 99, HasFile: false},
			{ID: 20, Title: "Flagged But Nil File", HasFile: true, MovieFile: nil},
		},
	}
	w := NewWalker(&Config{Radarr: fr, Logger: discardLogger()})

	snap, err := w.Walk(t.Context())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(snap.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(snap.Items))
	}
	for _, it := range snap.Items {
		if it.HasFile {
			t.Errorf("%s HasFile = true, want false for a movie with no file", it.Title)
		}
		if len(it.Groups) != 0 {
			t.Errorf("%s Groups = %v, want empty", it.Title, it.Groups)
		}
		if it.Current.Group != "" {
			t.Errorf("%s Current.Group = %q, want empty (fingerprint skipped for a fileless movie)", it.Title, it.Current.Group)
		}
	}
}

// TestWalkSonarrLogsLiveContextTimeout pins the per-request-timeout behavior:
// arrapi wraps each request in its own context.WithTimeout, so a slow
// GetEpisodeFiles surfaces as context.DeadlineExceeded while the walk context
// is still live. That is a real fetch failure, so the series becomes a Failed
// placeholder AND the per-series warning is logged with the series identity -
// not silently swallowed as shutdown noise. The walk as a whole still succeeds
// (a partial snapshot).
func TestWalkSonarrLogsLiveContextTimeout(t *testing.T) {
	fs := &fakeSonarr{
		series: []arrapi.Series{
			{ID: 1, Title: "Alpha"},
			{ID: 2, Title: "Bravo"},
		},
		files: map[int][]arrapi.EpisodeFile{
			1: {epFile(1, "PMR")},
		},
		epErr: map[int]error{2: context.DeadlineExceeded},
	}
	logger, rec := capture.New()
	w := NewWalker(&Config{Sonarr: fs, Logger: logger})

	snap, err := w.Walk(t.Context())
	if err != nil {
		t.Fatalf("Walk returned error, want nil (a live-context per-request timeout is not fatal): %v", err)
	}
	if len(snap.Items) != 2 {
		t.Fatalf("items = %d, want 2 (the timed-out series stays as a Failed placeholder)", len(snap.Items))
	}
	if !rec.Contains("sonarr episode fetch failed; series kept as failed placeholder") {
		t.Errorf("messages = %q, want a per-series episode-fetch-failed warning", rec.Messages())
	}
	if !rec.HasAttr("sonarr episode fetch failed; series kept as failed placeholder", "series", "Bravo") {
		t.Error("episode-fetch-failed warning does not name Bravo in its series attr")
	}
}

// TestWalkSonarrSilentOnContextCancel is the companion: when the walk context
// itself is cancelled (a shutdown/redeploy), a series whose fetch returns the
// cancellation is omitted WITHOUT a per-series warning (the walk-level
// cancellation is propagated by Walk instead), so a redeploy does not spam one
// warning per in-flight series.
func TestWalkSonarrSilentOnContextCancel(t *testing.T) {
	fs := &fakeSonarr{
		series: []arrapi.Series{{ID: 1, Title: "Alpha"}},
		epErr:  map[int]error{1: context.Canceled},
	}
	logger, rec := capture.New()
	w := NewWalker(&Config{Sonarr: fs, Logger: logger})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := w.Walk(ctx); err == nil {
		t.Fatal("Walk returned nil error, want the walk-context cancellation propagated")
	}
	if rec.Contains("sonarr episode fetch failed; series kept as failed placeholder") {
		t.Errorf("messages = %q, want no per-series warning on walk-context cancellation", rec.Messages())
	}
}

// TestWalkNoArrsWithNilLoggerReturnsEmptySnapshot pins the NewWalker nil-Logger
// default: a Config with no Logger (and no arrs) must produce a walker that
// walks without panicking and stamps the snapshot time.
func TestWalkNoArrsWithNilLoggerReturnsEmptySnapshot(t *testing.T) {
	w := NewWalker(&Config{})
	snap, err := w.Walk(t.Context())
	if err != nil {
		t.Fatalf("Walk with no arrs: %v", err)
	}
	if len(snap.Items) != 0 {
		t.Fatalf("items = %d, want 0", len(snap.Items))
	}
	if snap.TakenAt.IsZero() {
		t.Error("TakenAt is zero, want the walk timestamp set")
	}
}

// TestWalkPreCancelledContextIsFatalWithNoArrs pins the final cancellation
// guard: even with both arr sides disabled (so neither side-specific helper
// runs its own ctx check), an already-cancelled context fails the walk instead
// of returning a snapshot mislabelled as complete.
func TestWalkPreCancelledContextIsFatalWithNoArrs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := NewWalker(&Config{})
	snap, err := w.Walk(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Walk error = %v, want context.Canceled", err)
	}
	if len(snap.Items) != 0 || !snap.TakenAt.IsZero() {
		t.Errorf("snapshot = %+v, want the zero Snapshot on cancellation", snap)
	}
}

// TestWalkSonarrPartialFailureLogsAggregateSkipWarning asserts the aggregate
// "snapshot is partial" warning carries the skipped/kept counts when several
// series fail their episode fetch.
func TestWalkSonarrPartialFailureLogsAggregateSkipWarning(t *testing.T) {
	fs := &fakeSonarr{
		series: []arrapi.Series{
			{ID: 1, Title: "Alpha"},
			{ID: 2, Title: "Bravo"},
			{ID: 3, Title: "Charlie"},
		},
		files: map[int][]arrapi.EpisodeFile{
			1: {epFile(1, "PMR")},
		},
		epErr: map[int]error{
			2: errors.New("boom two"),
			3: errors.New("boom three"),
		},
	}
	logger, rec := capture.New()
	w := NewWalker(&Config{Sonarr: fs, Logger: logger})
	snap, err := w.Walk(t.Context())
	if err != nil {
		t.Fatalf("Walk returned error, want nil (partial failure is not fatal): %v", err)
	}
	if len(snap.Items) != 3 {
		t.Fatalf("items = %d, want 3 (one clean item plus two Failed placeholders)", len(snap.Items))
	}
	if !rec.Contains("snapshot is partial") {
		t.Fatalf("messages = %q, want an aggregate partial-snapshot warning", rec.Messages())
	}
	if !rec.HasAttr("snapshot is partial", "skipped", "2") {
		t.Error("partial-snapshot warning skipped attr != 2")
	}
	if !rec.HasAttr("snapshot is partial", "kept", "3") {
		t.Error("partial-snapshot warning kept attr != 3")
	}
}

// TestWalkSonarrRepresentativeTieBreaksToFirstFile pins the documented
// tie-break: when two groups are equally common on a series, the reported
// fingerprint comes from the FIRST such file, not the last.
func TestWalkSonarrRepresentativeTieBreaksToFirstFile(t *testing.T) {
	fs := &fakeSonarr{
		series: []arrapi.Series{{ID: 1, Title: "Tie"}},
		files: map[int][]arrapi.EpisodeFile{
			1: {
				{SeasonNumber: 1, ReleaseGroup: "AAA", SceneName: "[AAA] Tie S01E01 [1080p][x265]"},
				{SeasonNumber: 1, ReleaseGroup: "BBB", SceneName: "[BBB] Tie S01E02 [720p][x264]"},
			},
		},
	}
	w := NewWalker(&Config{Sonarr: fs, Logger: discardLogger()})
	snap, err := w.Walk(t.Context())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	it := snap.Items[0]
	if it.Current.Group != "aaa" {
		t.Errorf("Current.Group = %q, want aaa (tie broken by the first file)", it.Current.Group)
	}
	if it.Current.Resolution != "1080p" || it.Current.Codec != "x265" {
		t.Errorf("Current = %+v, want the first file's 1080p/x265 fingerprint", it.Current)
	}
}

// TestWalkSonarrGroupLessEpisodeFileAggregatesAsNoGroup pins the NOGRP
// library-side fallback: a file with an empty ReleaseGroup aggregates as the
// comparable "nogrp" group (Groups, SeasonGroups, and the fingerprint) instead
// of vanishing from the comparison.
func TestWalkSonarrGroupLessEpisodeFileAggregatesAsNoGroup(t *testing.T) {
	fs := &fakeSonarr{
		series: []arrapi.Series{{ID: 1, Title: "GroupLess"}},
		files: map[int][]arrapi.EpisodeFile{
			1: {{
				SeasonNumber: 1,
				ReleaseGroup: "",
				RelativePath: "Season 01/GroupLess S01E01 1080p.mkv",
			}},
		},
	}
	w := NewWalker(&Config{Sonarr: fs, Logger: discardLogger()})
	snap, err := w.Walk(t.Context())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	it := snap.Items[0]
	if !it.HasFile {
		t.Error("HasFile = false, want true")
	}
	if !slices.Equal(it.Groups, []string{"nogrp"}) {
		t.Errorf("Groups = %v, want [nogrp] (group-less file compares as NOGRP)", it.Groups)
	}
	if !slices.Equal(it.SeasonGroups[1], []string{"nogrp"}) {
		t.Errorf("SeasonGroups[1] = %v, want [nogrp]", it.SeasonGroups[1])
	}
	if it.Current.Group != "nogrp" {
		t.Errorf("Current.Group = %q, want nogrp", it.Current.Group)
	}
	if it.Current.Resolution != "1080p" {
		t.Errorf("Current.Resolution = %q, want 1080p (classified from the relative path)", it.Current.Resolution)
	}
}

// TestWalkSonarrSeriesItemCarriesIdentityFieldsAndDeepLink pins the identity
// fields seriesItem copies from the arr record - the IDs and titles the
// matcher keys on (byTvdb/byImdb/title fallback) - plus the Sonarr web deep
// link built from SonarrURL and the series title slug.
func TestWalkSonarrSeriesItemCarriesIdentityFieldsAndDeepLink(t *testing.T) {
	fs := &fakeSonarr{
		series: []arrapi.Series{{
			ID:              7,
			Title:           "Ident",
			TitleSlug:       "ident-slug",
			TvdbID:          555,
			TmdbID:          777,
			ImdbID:          "tt0000555",
			Year:            2023,
			AlternateTitles: []arrapi.AlternateTitle{{Title: "Alt Ident"}, {Title: "   "}},
		}},
		files: map[int][]arrapi.EpisodeFile{
			7: {epFile(1, "PMR")},
		},
	}
	w := NewWalker(&Config{Sonarr: fs, SonarrURL: "https://sonarr.example", Logger: discardLogger()})

	snap, err := w.Walk(t.Context())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(snap.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(snap.Items))
	}
	it := snap.Items[0]
	if it.Arr != ArrSonarr || it.ArrID != 7 || it.Title != "Ident" {
		t.Errorf("identity = arr %q id %d title %q, want sonarr/7/Ident", it.Arr, it.ArrID, it.Title)
	}
	if it.TvdbID != 555 || it.TmdbID != 777 || it.ImdbID != "tt0000555" || it.Year != 2023 {
		t.Errorf("ids = tvdb %d tmdb %d imdb %q year %d, want 555/777/tt0000555/2023", it.TvdbID, it.TmdbID, it.ImdbID, it.Year)
	}
	if !slices.Equal(it.AltTitles, []string{"Alt Ident"}) {
		t.Errorf("AltTitles = %v, want only the non-empty alternate title", it.AltTitles)
	}
	if it.ArrURL != "https://sonarr.example/series/ident-slug" {
		t.Errorf("ArrURL = %q, want the Sonarr /series/{titleSlug} deep link", it.ArrURL)
	}
}

// TestWalkCleanSonarrWalkIsNotPartial pins the negative side of the
// Snapshot.Partial producer contract: a walk where every kept series fetches
// its episodes successfully must publish Partial=false, so the diff's
// partial-suppression logic is not permanently engaged. It also pins the log
// contract's negative side: a zero-failure walk logs NO warning, so the
// aggregate partial-snapshot warn gate (failed > 0) cannot silently invert
// to fire on every healthy cycle.
func TestWalkCleanSonarrWalkIsNotPartial(t *testing.T) {
	fs := &fakeSonarr{
		series: []arrapi.Series{{ID: 1, Title: "Alpha"}},
		files: map[int][]arrapi.EpisodeFile{
			1: {epFile(1, "PMR")},
		},
	}
	logger, rec := capture.New()
	w := NewWalker(&Config{Sonarr: fs, Logger: logger})
	snap, err := w.Walk(t.Context())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if snap.Partial {
		t.Error("Snapshot.Partial = true, want false for a clean walk with no skipped series")
	}
	if rec.CountLevel(slog.LevelWarn, "") > 0 {
		t.Errorf("clean walk logged a warning, want none (no partial-snapshot warning with zero failures); messages = %q", rec.Messages())
	}
}

// budgetSonarr blocks each GetEpisodeFiles until released, then fails it, so a
// test can trip the walk failure budget one fetch at a time and observe how
// many fetches ever started.
type budgetSonarr struct {
	started chan int
	release chan struct{}
	series  []arrapi.Series
}

func (f *budgetSonarr) GetSeries(context.Context) ([]arrapi.Series, error) {
	return f.series, nil
}

func (f *budgetSonarr) GetEpisodeFiles(ctx context.Context, seriesID int) ([]arrapi.EpisodeFile, error) {
	select {
	case f.started <- seriesID:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case <-f.release:
		return nil, errors.New("sonarr down")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (f *budgetSonarr) GetTags(context.Context) ([]arrapi.Tag, error) {
	return nil, nil
}

// TestWalkSonarrBudgetTripSkipsQueuedFetches pins the cancel-on-budget
// behavior of fetchEpisodeItems: once episodeFailureBudget fetches have
// failed, the fan-out context is cancelled, so queued series never reach
// GetEpisodeFiles. Exactly episodeConcurrency fetches start up front; each
// released failure lets one more start, except the last, which trips the
// budget — so the total started is episodeConcurrency + episodeFailureBudget
// - 1 and the walk fails with the budget error. Deleting the cancelFan() call
// on the budget branch (or mutating >= to >) leaves an extra fetch blocked
// forever, which synctest detects as a durable deadlock.
func TestWalkSonarrBudgetTripSkipsQueuedFetches(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		seriesCount := episodeConcurrency + 10
		fs := &budgetSonarr{
			started: make(chan int, seriesCount),
			release: make(chan struct{}),
		}
		for id := 1; id <= seriesCount; id++ {
			fs.series = append(fs.series, arrapi.Series{ID: id, Title: "Series"})
		}
		w := NewWalker(&Config{Sonarr: fs, Logger: discardLogger()})

		done := make(chan error, 1)
		go func() {
			_, err := w.Walk(t.Context())
			done <- err
		}()

		synctest.Wait()
		if got := len(fs.started); got != episodeConcurrency {
			t.Fatalf("started episode fetches = %d, want %d before any release", got, episodeConcurrency)
		}

		// Fail exactly episodeFailureBudget fetches, one at a time.
		for range episodeFailureBudget {
			fs.release <- struct{}{}
			synctest.Wait()
		}

		err := <-done
		if err == nil || !strings.Contains(err.Error(), "failure budget") {
			t.Fatalf("Walk error = %v, want the walk failure budget error", err)
		}
		want := episodeConcurrency + episodeFailureBudget - 1
		if got := len(fs.started); got != want {
			t.Errorf("started episode fetches = %d, want %d (queued series skipped after the budget trip)", got, want)
		}
	})
}

func TestWalkSonarrExactBudgetFailureCountFailsWalk(t *testing.T) {
	fs := &fakeSonarr{files: map[int][]arrapi.EpisodeFile{}, epErr: map[int]error{}}
	total := episodeFailureBudget + 1
	for id := 1; id <= total; id++ {
		fs.series = append(fs.series, arrapi.Series{ID: id, Title: "Series"})
		if id <= episodeFailureBudget {
			fs.epErr[id] = errors.New("sonarr down")
		} else {
			fs.files[id] = []arrapi.EpisodeFile{epFile(1, "PMR")}
		}
	}
	w := NewWalker(&Config{Sonarr: fs, Logger: discardLogger()})
	snap, err := w.Walk(t.Context())
	if err == nil || !strings.Contains(err.Error(), "failure budget") {
		t.Fatalf("Walk error = %v, want the walk failure budget error", err)
	}
	if len(snap.Items) != 0 {
		t.Errorf("items = %d, want the zero Snapshot on a budget failure", len(snap.Items))
	}
}

// TestWalkSonarrZeroKeptSeriesSucceeds pins the empty-library boundary of the
// total-failure rule: a Sonarr side whose kept series set is empty (an empty
// arr library, or every series dropped by the tag filters) walks clean -
// zero kept with zero failures is not a total episode-fetch outage, so the
// walk succeeds with an empty, non-partial snapshot instead of tripping the
// "all kept series failed" guard.
func TestWalkSonarrZeroKeptSeriesSucceeds(t *testing.T) {
	t.Run("empty series list", func(t *testing.T) {
		fs := &fakeSonarr{}
		w := NewWalker(&Config{Sonarr: fs, Logger: discardLogger()})
		snap, err := w.Walk(t.Context())
		if err != nil {
			t.Fatalf("Walk with an empty Sonarr library: %v", err)
		}
		if len(snap.Items) != 0 || snap.Partial {
			t.Errorf("snapshot = %+v, want empty and not partial", snap)
		}
	})
	t.Run("all series dropped by the include filter", func(t *testing.T) {
		fs := &fakeSonarr{
			series: []arrapi.Series{{ID: 1, Title: "Dropped", Tags: []int{3}}},
			tags:   []arrapi.Tag{{ID: 7, Label: "anime"}},
		}
		w := NewWalker(&Config{Sonarr: fs, IncludeTags: []string{"anime"}, Logger: discardLogger()})
		snap, err := w.Walk(t.Context())
		if err != nil {
			t.Fatalf("Walk with every series tag-filtered out: %v", err)
		}
		if len(snap.Items) != 0 || snap.Partial {
			t.Errorf("snapshot = %+v, want empty and not partial", snap)
		}
	})
}

// TestWalkSonarrEpisodeFailureRedactsErrorURL pins the credential boundary of
// the recoverable per-series warning: an arr transport error is a *url.Error
// embedding the full request URL (with any configured userinfo), and the
// warning sits outside the walk-level LogSafeError boundary, so the log site
// must apply the same reduction itself before the line reaches Loki.
func TestWalkSonarrEpisodeFailureRedactsErrorURL(t *testing.T) {
	transportErr := &url.Error{
		Op:  "Get",
		URL: "http://user:LEAK-SENTINEL@sonarr:8989/api/v3/episodefile?seriesId=1",
		Err: errors.New("connection refused"),
	}
	fs := &fakeSonarr{
		series: []arrapi.Series{
			{ID: 1, Title: "Alpha"},
			{ID: 2, Title: "Bravo"},
		},
		files: map[int][]arrapi.EpisodeFile{
			2: {epFile(1, "PMR")},
		},
		epErr: map[int]error{1: transportErr},
	}
	logger, rec := capture.New()
	w := NewWalker(&Config{Sonarr: fs, Logger: logger})

	if _, err := w.Walk(t.Context()); err != nil {
		t.Fatalf("Walk returned error, want a successful partial walk: %v", err)
	}
	if !rec.HasAttr("sonarr episode fetch failed; series kept as failed placeholder", "error", "connection refused") {
		t.Errorf("episode-fetch-failed warning does not carry the reduced transport error; records = %+v", rec.Records())
	}
	for _, r := range rec.Records() {
		r.Attrs(func(a slog.Attr) bool {
			if strings.Contains(a.Value.String(), "LEAK-SENTINEL") {
				t.Errorf("captured record %q attr %q leaks the userinfo credential", r.Message, a.Key)
			}
			return true
		})
	}
}

// TestWalkSonarrEpisodeFailureSanitizesTitle pins the log-injection boundary
// of the same warning: the upstream Sonarr series title passes through
// runesafe.Sanitize before landing in the series attribute, so terminal
// escapes and bidi overrides cannot forge log content.
func TestWalkSonarrEpisodeFailureSanitizesTitle(t *testing.T) {
	const rawTitle = "Frieren\x1b[2J \u202egpj.exe"
	fs := &fakeSonarr{
		series: []arrapi.Series{
			{ID: 1, Title: rawTitle},
			{ID: 2, Title: "Healthy"},
		},
		files: map[int][]arrapi.EpisodeFile{
			2: {epFile(1, "PMR")},
		},
		epErr: map[int]error{1: errors.New("episode fetch boom")},
	}
	logger, rec := capture.New()
	w := NewWalker(&Config{Sonarr: fs, Logger: logger})

	if _, err := w.Walk(t.Context()); err != nil {
		t.Fatalf("Walk returned error, want a successful partial walk: %v", err)
	}
	if !rec.HasAttr("sonarr episode fetch failed; series kept as failed placeholder", "series", "Frieren [2J  gpj.exe") {
		t.Errorf("episode-fetch-failed warning does not carry the sanitized series title; records = %+v", rec.Records())
	}
}

// TestWalkErrorCarriesArrIdentity pins the typed walk-side error contract the
// scout's log boundaries depend on: a per-side walk failure preserves the
// exact "walking <arr>: <cause>" text (report-mode CLI output reads it
// unchanged), keeps the cause reachable through the chain, and carries the
// failed side as a bounded value WalkErrArr recovers - the identity rides the
// type because httpx.LogSafeError discards textual wrappers at the scout's
// production log boundaries.
func TestWalkErrorCarriesArrIdentity(t *testing.T) {
	cause := errors.New("connect: connection refused")
	tests := []struct {
		name       string
		cfg        Config
		wantPrefix string
		wantArr    string
	}{
		{
			name:       "sonarr",
			cfg:        Config{Sonarr: &fakeSonarr{listErr: cause}},
			wantPrefix: "walking sonarr: ",
			wantArr:    ArrSonarr,
		},
		{
			name:       "radarr",
			cfg:        Config{Radarr: &fakeRadarr{listErr: cause}},
			wantPrefix: "walking radarr: ",
			wantArr:    ArrRadarr,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.cfg.Logger = discardLogger()
			w := NewWalker(&tc.cfg)
			_, err := w.Walk(t.Context())
			if err == nil {
				t.Fatal("Walk returned nil error, want the list failure")
			}
			if got, want := err.Error(), tc.wantPrefix+cause.Error(); got != want {
				t.Errorf("Walk error text = %q, want %q", got, want)
			}
			if !errors.Is(err, cause) {
				t.Error("Walk error does not unwrap to its cause; the chain must stay intact")
			}
			if got := WalkErrArr(err); got != tc.wantArr {
				t.Errorf("WalkErrArr = %q, want %q", got, tc.wantArr)
			}
		})
	}

	// An error that names no side (Walk's final cancellation guard, or any
	// non-walk error) yields the empty identity, so the scout's log
	// boundaries omit the attr instead of logging a bogus one.
	if got := WalkErrArr(context.Canceled); got != "" {
		t.Errorf("WalkErrArr(context.Canceled) = %q, want empty", got)
	}
}

// TestWalkSonarrDeadIncludeTagFilterWarnsAndEmptiesSide pins the dead-filter
// diagnostic: when no configured include label resolves to an arr tag, the
// non-nil empty id set makes keepByTags drop every item, so the side
// contributes zero items while the cycle still reads healthy. The distinct
// second warning is the only signal that separates a dead filter from one
// stray label, and it carries the label COUNT rather than the labels
// themselves (they pass through ${VAR} expansion).
func TestWalkSonarrDeadIncludeTagFilterWarnsAndEmptiesSide(t *testing.T) {
	fs := &fakeSonarr{
		series: []arrapi.Series{{ID: 1, Title: "Alpha", Tags: []int{7}}},
		files:  map[int][]arrapi.EpisodeFile{1: {epFile(1, "PMR")}},
		tags:   []arrapi.Tag{{ID: 7, Label: "anime"}},
	}
	logger, rec := capture.New()
	w := NewWalker(&Config{Sonarr: fs, IncludeTags: []string{"nope", "also-nope"}, Logger: logger})

	snap, err := w.Walk(t.Context())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(snap.Items) != 0 {
		t.Fatalf("items = %+v, want none (an include set resolving to zero ids admits nothing)", snap.Items)
	}
	if !snap.FilteredEmpty {
		t.Error("snapshot FilteredEmpty=false; a dead include filter must mark the walk so the cycle closes degraded instead of reading complete forever")
	}
	const msg = "no configured tag resolved to an arr tag; an include set therefore admits nothing, an exclude set drops nothing"
	if n := rec.CountExact(msg); n != 1 {
		t.Fatalf("dead-filter warnings = %d, want exactly 1; messages = %q", n, rec.Messages())
	}
	if !rec.HasAttr(msg, "which", "arr_tags.include") {
		t.Error("dead-filter warning does not name arr_tags.include in its which attr")
	}
	if !rec.HasAttr(msg, "configured_count", "2") {
		t.Error("dead-filter warning does not carry configured_count=2")
	}
	for _, r := range rec.Records() {
		r.Attrs(func(a slog.Attr) bool {
			if v := a.Value.String(); v == "nope" || v == "also-nope" {
				t.Errorf("record %q attr %q emits a configured tag VALUE (%q); the diagnostic is count-only", r.Message, a.Key, v)
			}
			return true
		})
	}
}

// TestWalkRadarrMissingFilePayloadWarns pins both halves of the degradation
// handling for a Radarr movie that reports HasFile with no MovieFile payload:
// the item is kept as a library placeholder (not comparable, so the compare and
// the diff scope it out instead of reading a false no-file state), and the
// aggregate WARN names the count so the operator can re-scan. A movie that is
// honestly fileless must be neither counted nor turned into a placeholder, and
// the snapshot stays complete - this is a per-item data defect, not a walk that
// failed to read the library.
func TestWalkRadarrMissingFilePayloadWarns(t *testing.T) {
	fr := &fakeRadarr{movies: []arrapi.Movie{
		{ID: 10, Title: "Flagged But Nil File", HasFile: true, MovieFile: nil},
		{ID: 20, Title: "Honestly Fileless", HasFile: false},
		{ID: 30, Title: "With File", HasFile: true, MovieFile: &arrapi.MovieFile{ReleaseGroup: "PMR"}},
	}}
	logger, rec := capture.New()
	w := NewWalker(&Config{Radarr: fr, Logger: logger})

	snap, err := w.Walk(t.Context())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(snap.Items) != 3 {
		t.Fatalf("items = %d, want 3 (a payload-less movie is kept, not dropped)", len(snap.Items))
	}
	if snap.Partial {
		t.Error("snapshot is Partial; a missing file payload is a per-item defect, not an unread library (the report refuses a partial snapshot)")
	}
	comparable := map[int]bool{}
	for i := range snap.Items {
		comparable[snap.Items[i].ArrID] = snap.Items[i].Comparable()
	}
	if comparable[10] {
		t.Error("the payload-less movie is comparable; its file state is missing, not empty, so the compare would resolve its prior finding")
	}
	if !comparable[20] || !comparable[30] {
		t.Errorf("comparable = %v, want the honestly fileless (20) and the with-file (30) movies comparable", comparable)
	}
	const msg = "radarr movies report a file but carry no file payload; they are kept as placeholders with no comparable file state - re-scan those movies in Radarr (the per-movie ids are logged at debug level)"
	if n := rec.CountExact(msg); n != 1 {
		t.Fatalf("no-payload warnings = %d, want exactly 1; messages = %q", n, rec.Messages())
	}
	if !rec.HasAttr(msg, "movies", "1") {
		v, _ := rec.AttrValue(msg, "movies")
		t.Errorf("no-payload warning movies attr = %q, want 1 (only the payload-less movie counts)", v)
	}
	if !rec.HasAttr(msg, "kept", "3") {
		v, _ := rec.AttrValue(msg, "kept")
		t.Errorf("no-payload warning kept attr = %q, want 3", v)
	}
}

// TestWalkRadarrCleanWalkLogsNoPayloadWarning is the negative side: a Radarr
// side whose every movie is honest logs no degradation warning, so the gate
// cannot invert to fire on every healthy cycle.
func TestWalkRadarrCleanWalkLogsNoPayloadWarning(t *testing.T) {
	fr := &fakeRadarr{movies: []arrapi.Movie{
		{ID: 10, Title: "With File", HasFile: true, MovieFile: &arrapi.MovieFile{ReleaseGroup: "PMR"}},
		{ID: 20, Title: "Fileless", HasFile: false},
	}}
	logger, rec := capture.New()
	w := NewWalker(&Config{Radarr: fr, Logger: logger})

	if _, err := w.Walk(t.Context()); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	const msg = "radarr movies report a file but carry no file payload; they are kept as placeholders with no comparable file state - re-scan those movies in Radarr (the per-movie ids are logged at debug level)"
	if n := rec.CountExact(msg); n != 0 {
		t.Errorf("clean radarr walk logged %d no-payload warnings, want none; messages = %q", n, rec.Messages())
	}
}

// TestWalkCompleteLogReportsConfiguredArrSides pins the two independent
// configured-side booleans on the walk-completion record: it is the
// operator-visible account of WHICH arr supplied the snapshot, so an inversion
// or a collapse of one side onto the other must fail here.
func TestWalkCompleteLogReportsConfiguredArrSides(t *testing.T) {
	tests := map[string]struct {
		cfg        Config
		wantSonarr string
		wantRadarr string
	}{
		"sonarr only": {
			cfg:        Config{Sonarr: &fakeSonarr{}},
			wantSonarr: "true",
			wantRadarr: "false",
		},
		"radarr only": {
			cfg:        Config{Radarr: &fakeRadarr{}},
			wantSonarr: "false",
			wantRadarr: "true",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			logger, rec := capture.New()
			tc.cfg.Logger = logger
			w := NewWalker(&tc.cfg)

			if _, err := w.Walk(t.Context()); err != nil {
				t.Fatalf("Walk: %v", err)
			}
			const msg = "library walk complete"
			if n := rec.CountExact(msg); n != 1 {
				t.Fatalf("completion records = %d, want 1; messages = %q", n, rec.Messages())
			}
			if !rec.HasAttr(msg, "sonarr", tc.wantSonarr) {
				got, _ := rec.AttrValue(msg, "sonarr")
				t.Errorf("sonarr attr = %q, want %q", got, tc.wantSonarr)
			}
			if !rec.HasAttr(msg, "radarr", tc.wantRadarr) {
				got, _ := rec.AttrValue(msg, "radarr")
				t.Errorf("radarr attr = %q, want %q", got, tc.wantRadarr)
			}
		})
	}
}

// TestWalkWarnsWhenTagFilteringEmptiesASide pins the dead-but-resolving-filter
// diagnostic: every configured arr_tags label resolves to a real tag id, but no
// item carries it (renamed or unassigned on the arr side), so the side
// contributes zero items while the cycle still reads healthy. resolveOne warns
// only when a LABEL missed and the scout's shrink gate needs a prior snapshot,
// so this WARN is the only signal that separates a silently-emptied side from a
// genuinely empty library - hence both negative arms below (a filter that keeps
// something, and an arr that listed nothing) as well as the positive one. Counts
// only: the arr and listed attrs never carry label values, which pass through
// ${VAR} expansion.
func TestWalkWarnsWhenTagFilteringEmptiesASide(t *testing.T) {
	const msg = "arr_tags filtering kept no items from a non-empty arr library; this side contributes nothing this cycle"
	// The empty-list WARN is the ONLY signal for a silently-emptied side
	// (warnFilteredEmpty returns early on listed == 0 and the scout shrink gate
	// is whole-library), so both arms are pinned here rather than leaving the
	// path executed-but-unchecked.
	const emptyMsg = "arr listed no items; this side contributes nothing this cycle - check the arr url and that the instance holds the expected library"
	anime := []arrapi.Tag{{ID: 7, Label: "anime"}}

	t.Run("sonarr side emptied by a resolving filter", func(t *testing.T) {
		fs := &fakeSonarr{
			series: []arrapi.Series{
				{ID: 1, Title: "Untagged", Tags: []int{3}},
				{ID: 2, Title: "Also Untagged"},
			},
			tags: anime,
		}
		logger, rec := capture.New()
		w := NewWalker(&Config{Sonarr: fs, IncludeTags: []string{"anime"}, Logger: logger})

		snap, err := w.Walk(t.Context())
		if err != nil {
			t.Fatalf("Walk: %v", err)
		}
		if len(snap.Items) != 0 {
			t.Fatalf("items = %+v, want none (no series carries the resolved tag)", snap.Items)
		}
		if n := rec.CountExact(msg); n != 1 {
			t.Fatalf("dead-filter warnings = %d, want exactly 1; messages = %q", n, rec.Messages())
		}
		if !rec.HasAttr(msg, "arr", ArrSonarr) {
			got, _ := rec.AttrValue(msg, "arr")
			t.Errorf("arr attr = %q, want %q", got, ArrSonarr)
		}
		if !rec.HasAttr(msg, "listed", "2") {
			got, _ := rec.AttrValue(msg, "listed")
			t.Errorf("listed attr = %q, want 2 (the arr listed two series)", got)
		}
		if !snap.FilteredEmpty {
			t.Error("snapshot FilteredEmpty=false; an emptied side must degrade the cycle, not just WARN once per cycle forever")
		}
	})

	t.Run("radarr side emptied by a resolving filter", func(t *testing.T) {
		fr := &fakeRadarr{
			movies: []arrapi.Movie{{ID: 10, Title: "Untagged Movie", Tags: []int{3}}},
			tags:   anime,
		}
		logger, rec := capture.New()
		w := NewWalker(&Config{Radarr: fr, IncludeTags: []string{"anime"}, Logger: logger})

		snap, err := w.Walk(t.Context())
		if err != nil {
			t.Fatalf("Walk: %v", err)
		}
		if len(snap.Items) != 0 {
			t.Fatalf("items = %+v, want none", snap.Items)
		}
		if n := rec.CountExact(msg); n != 1 {
			t.Fatalf("dead-filter warnings = %d, want exactly 1; messages = %q", n, rec.Messages())
		}
		if !rec.HasAttr(msg, "arr", ArrRadarr) {
			got, _ := rec.AttrValue(msg, "arr")
			t.Errorf("arr attr = %q, want %q", got, ArrRadarr)
		}
		if !rec.HasAttr(msg, "listed", "1") {
			got, _ := rec.AttrValue(msg, "listed")
			t.Errorf("listed attr = %q, want 1", got)
		}
		if !snap.FilteredEmpty {
			t.Error("snapshot FilteredEmpty=false; an emptied side must degrade the cycle, not just WARN once per cycle forever")
		}
	})

	t.Run("no warning when the filter keeps something", func(t *testing.T) {
		fs := &fakeSonarr{
			series: []arrapi.Series{
				{ID: 1, Title: "Kept", Tags: []int{7}},
				{ID: 2, Title: "Dropped", Tags: []int{3}},
			},
			files: map[int][]arrapi.EpisodeFile{1: {epFile(1, "PMR")}},
			tags:  anime,
		}
		logger, rec := capture.New()
		w := NewWalker(&Config{Sonarr: fs, IncludeTags: []string{"anime"}, Logger: logger})

		snap, err := w.Walk(t.Context())
		if err != nil {
			t.Fatalf("Walk: %v", err)
		}
		if len(snap.Items) != 1 {
			t.Fatalf("items = %d, want 1", len(snap.Items))
		}
		if n := rec.CountExact(msg); n != 0 {
			t.Errorf("dead-filter warnings = %d, want none (the filter kept an item); messages = %q", n, rec.Messages())
		}
		if n := rec.CountExact(emptyMsg); n != 0 {
			t.Errorf("empty-list warnings = %d, want none (the arr listed two series); messages = %q", n, rec.Messages())
		}
		if snap.FilteredEmpty {
			t.Error("snapshot FilteredEmpty=true on a filter that kept an item; the flag must not degrade every filtered cycle")
		}
	})

	t.Run("no warning for a genuinely empty arr library", func(t *testing.T) {
		fs := &fakeSonarr{tags: anime}
		logger, rec := capture.New()
		w := NewWalker(&Config{Sonarr: fs, IncludeTags: []string{"anime"}, Logger: logger})

		snap, err := w.Walk(t.Context())
		if err != nil {
			t.Fatalf("Walk: %v", err)
		}
		if snap.FilteredEmpty {
			t.Error("snapshot FilteredEmpty=true for a genuinely empty arr; a legitimately empty Radarr must not degrade every cycle forever")
		}
		if n := rec.CountExact(msg); n != 0 {
			t.Errorf("dead-filter warnings = %d, want none (the arr listed nothing, so filtering emptied nothing); messages = %q", n, rec.Messages())
		}
		if n := rec.CountExact(emptyMsg); n != 1 {
			t.Fatalf("empty-list warnings = %d, want exactly 1; messages = %q", n, rec.Messages())
		}
		if !rec.HasAttr(emptyMsg, "arr", ArrSonarr) {
			got, _ := rec.AttrValue(emptyMsg, "arr")
			t.Errorf("arr attr = %q, want %q", got, ArrSonarr)
		}
	})
}

// TestWalkStripsBaseURLCredentialsFromItemArrURL pins the Item-level credential
// invariant Item.ArrURL documents: the walker builds the deep-link THROUGH
// SafeLogURL, so a reverse-proxy Basic Auth credential configured in
// sonarr.url / radarr.public_url never enters an Item, a Snapshot, a Finding, or
// an audit Row. The audit render's sink-side SafeLogURL call is documented as
// belt-and-braces, so nothing else fails if a construction-side wrap is
// dropped. The link must also stay usable, so the
// assertion is the exact credential-free deep-link, not merely the absence of
// the secret.
func TestWalkStripsBaseURLCredentialsFromItemArrURL(t *testing.T) {
	fs := &fakeSonarr{
		series: []arrapi.Series{{ID: 1, Title: "Alpha", TitleSlug: "alpha"}},
		files:  map[int][]arrapi.EpisodeFile{1: {epFile(1, "PMR")}},
	}
	fr := &fakeRadarr{movies: []arrapi.Movie{{
		ID:        2,
		Title:     "Movie",
		TmdbID:    1234,
		HasFile:   true,
		MovieFile: &arrapi.MovieFile{ReleaseGroup: "PMR"},
	}}}
	w := NewWalker(&Config{
		Sonarr:    fs,
		Radarr:    fr,
		SonarrURL: "https://sonarr-user:SONARR-LEAK@sonarr.example",
		RadarrURL: "https://radarr-user:RADARR-LEAK@radarr.example",
		Logger:    discardLogger(),
	})

	snap, err := w.Walk(t.Context())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	want := map[string]string{
		"sonarr:1": "https://sonarr.example/series/alpha",
		"radarr:2": "https://radarr.example/movie/1234",
	}
	if len(snap.Items) != len(want) {
		t.Fatalf("items = %d, want %d", len(snap.Items), len(want))
	}
	for i := range snap.Items {
		it := &snap.Items[i]
		if got := it.ArrURL; got != want[it.Key()] {
			t.Errorf("%s ArrURL = %q, want %q (the configured base URL's credential is stripped at construction)",
				it.Key(), got, want[it.Key()])
		}
	}
}

// TestWalkWarnsWhenSonarrDeclaresFilesButSendsNone pins fetchSeriesItem's
// declared-files-but-empty diagnostic: the series list says the series has
// episode files while the per-series episode-file call succeeds empty, so the
// item compares as fileless and the daemon would otherwise resolve the series'
// prior finding as a genuine no-file state with no signal at all. All three arms
// of the gate are pinned, because the nil-Statistics and zero-count arms are what
// keep an honest fileless series quiet.
func TestWalkWarnsWhenSonarrDeclaresFilesButSendsNone(t *testing.T) {
	const msg = "sonarr series declares episode files but its episode-file list came back empty; it compares as fileless"
	tests := []struct {
		name  string
		stats *arrapi.SeriesStatistics
		files map[int][]arrapi.EpisodeFile
		want  int
	}{
		{"declared files but none sent", &arrapi.SeriesStatistics{EpisodeFileCount: 12}, nil, 1},
		{"declared files and files sent", &arrapi.SeriesStatistics{EpisodeFileCount: 12}, map[int][]arrapi.EpisodeFile{1: {epFile(1, "PMR")}}, 0},
		{"no files declared", &arrapi.SeriesStatistics{}, nil, 0},
		{"no statistics block", nil, nil, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeSonarr{
				series: []arrapi.Series{{ID: 1, Title: "Alpha", Statistics: tc.stats}},
				files:  tc.files,
			}
			logger, rec := capture.New()
			w := NewWalker(&Config{Sonarr: fs, Logger: logger})

			if _, err := w.Walk(t.Context()); err != nil {
				t.Fatalf("Walk: %v", err)
			}
			if n := rec.CountExact(msg); n != tc.want {
				t.Fatalf("declared-files warnings = %d, want %d; messages = %q", n, tc.want, rec.Messages())
			}
			if tc.want == 1 && !rec.HasAttr(msg, "declared_files", "12") {
				got, _ := rec.AttrValue(msg, "declared_files")
				t.Errorf("declared_files attr = %q, want 12", got)
			}
		})
	}
}
