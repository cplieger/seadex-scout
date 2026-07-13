package library

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/cplieger/arrapi"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeSonarr is a scripted SonarrClient: GetSeries returns series (or listErr),
// GetEpisodes returns episodes[id] (or epErr[id]), ResolveTagIDs returns the
// canned tag set.
type fakeSonarr struct {
	episodes  map[int][]arrapi.Episode
	epErr     map[int]error
	tagIDs    map[int]struct{}
	listErr   error
	tagErr    error
	series    []arrapi.Series
	unmatched []string
}

func (f *fakeSonarr) GetSeries(context.Context) ([]arrapi.Series, error) {
	return f.series, f.listErr
}

func (f *fakeSonarr) GetEpisodes(_ context.Context, seriesID int) ([]arrapi.Episode, error) {
	if err := f.epErr[seriesID]; err != nil {
		return nil, err
	}
	return f.episodes[seriesID], nil
}

func (f *fakeSonarr) ResolveTagIDs(context.Context, ...string) (map[int]struct{}, []string, error) {
	return f.tagIDs, f.unmatched, f.tagErr
}

func epFile(group string) *arrapi.EpisodeFile {
	return &arrapi.EpisodeFile{ReleaseGroup: group}
}

// TestWalkSonarrPartialEpisodeFailure pins the "ingest succeeded == healthy"
// semantic: a per-series episode-fetch failure omits that series from the
// snapshot (so a transient fetch failure is not misread as a real no-file
// item) while the walk as a whole succeeds and the other series carry their
// groups. Run under -race, it also exercises the bounded-concurrency episode
// fetch.
func TestWalkSonarrPartialEpisodeFailure(t *testing.T) {
	fs := &fakeSonarr{
		series: []arrapi.Series{
			{ID: 1, Title: "Alpha"},
			{ID: 2, Title: "Bravo"},
			{ID: 3, Title: "Charlie"},
		},
		episodes: map[int][]arrapi.Episode{
			1: {{SeasonNumber: 1, EpisodeFile: epFile("PMR")}},
			3: {{SeasonNumber: 1, EpisodeFile: epFile("LostYears")}},
		},
		epErr: map[int]error{2: errors.New("episode fetch boom")},
	}
	w := NewWalker(&Config{Sonarr: fs, Logger: discardLogger()})

	snap, err := w.Walk(context.Background())
	if err != nil {
		t.Fatalf("Walk returned error, want nil (partial failure is not fatal): %v", err)
	}
	if len(snap.Items) != 2 {
		t.Fatalf("items = %d, want 2 (the failed series is omitted)", len(snap.Items))
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
	if _, ok := byID[2]; ok {
		t.Error("Bravo (episode fetch failed) is present, want it omitted from the snapshot")
	}
	if got := byID[3].Groups; len(got) != 1 || got[0] != "lostyears" {
		t.Errorf("Charlie groups = %v, want [lostyears]", got)
	}
}

// TestWalkTopLevelListErrorIsFatal covers the other half of the health
// semantic: a failed top-level series list fails the whole walk.
func TestWalkTopLevelListErrorIsFatal(t *testing.T) {
	fs := &fakeSonarr{listErr: errors.New("sonarr unreachable")}
	w := NewWalker(&Config{Sonarr: fs, Logger: discardLogger()})
	if _, err := w.Walk(context.Background()); err == nil {
		t.Fatal("Walk returned nil error, want the GetSeries failure propagated")
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
		episodes: map[int][]arrapi.Episode{
			1: {{SeasonNumber: 1, EpisodeFile: epFile("PMR")}},
		},
		tagIDs: map[int]struct{}{7: {}},
	}
	w := NewWalker(&Config{Sonarr: fs, IncludeTags: []string{"anime"}, Logger: discardLogger()})

	snap, err := w.Walk(context.Background())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(snap.Items) != 1 || snap.Items[0].ArrID != 1 {
		t.Fatalf("items = %+v, want only the tag-included series (id 1)", snap.Items)
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

func TestDiffSnapshots(t *testing.T) {
	item := func(arr string, id int, groups ...string) Item {
		return Item{Arr: arr, ArrID: id, Groups: groups, HasFile: len(groups) > 0}
	}
	prev := &Snapshot{Items: []Item{
		item(ArrSonarr, 1, "pmr"),
		item(ArrSonarr, 2, "grp"),
		item(ArrRadarr, 3, "movgrp"),
	}}
	cur := &Snapshot{Items: []Item{
		item(ArrSonarr, 1, "pmr"),       // unchanged
		item(ArrSonarr, 2, "lostyears"), // changed group set
		item(ArrSonarr, 4, "newgrp"),    // added
		// Radarr id 3 removed
	}}
	d := DiffSnapshots(prev, cur)
	if d.Added != 1 || d.Removed != 1 || d.Changed != 1 {
		t.Errorf("diff = %+v, want Added=1 Removed=1 Changed=1", d)
	}
}

// boundedSonarr blocks each GetEpisodes until released, recording the peak
// number of simultaneous in-flight fetches so a test can prove the walker
// bounds concurrency at episodeConcurrency.
type boundedSonarr struct {
	mu        sync.Mutex
	started   chan int
	release   chan struct{}
	series    []arrapi.Series
	active    int
	maxActive int
}

func (f *boundedSonarr) GetSeries(context.Context) ([]arrapi.Series, error) {
	return f.series, nil
}

func (f *boundedSonarr) GetEpisodes(ctx context.Context, seriesID int) ([]arrapi.Episode, error) {
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
		return []arrapi.Episode{{SeasonNumber: 1, EpisodeFile: epFile("PMR")}}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (f *boundedSonarr) ResolveTagIDs(context.Context, ...string) (map[int]struct{}, []string, error) {
	return nil, nil, nil
}

func (f *boundedSonarr) max() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxActive
}

func TestWalkSonarrBoundsEpisodeFetchConcurrency(t *testing.T) {
	ctx := t.Context()

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
		snap, err := w.Walk(ctx)
		if err != nil {
			done <- err
			return
		}
		if len(snap.Items) != seriesCount {
			done <- errors.New("walk returned the wrong item count")
			return
		}
		done <- nil
	}()

	for range episodeConcurrency {
		select {
		case <-fs.started:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for the initial episode fetch workers")
		}
	}
	select {
	case id := <-fs.started:
		t.Fatalf("episode fetch for series %d started before a worker slot was released", id)
	case <-time.After(25 * time.Millisecond):
	}
	if got := fs.max(); got != episodeConcurrency {
		t.Fatalf("max concurrent episode fetches before release = %d, want %d", got, episodeConcurrency)
	}

	for range seriesCount {
		fs.release <- struct{}{}
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Walk: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Walk to finish after releasing workers")
	}
	if got := fs.max(); got > episodeConcurrency {
		t.Fatalf("max concurrent episode fetches = %d, want <= %d", got, episodeConcurrency)
	}
}

// recordingHandler captures the levels of every log record so a test can assert
// no WARN was emitted.
type recordingHandler struct {
	mu     sync.Mutex
	levels []slog.Level
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.levels = append(h.levels, r.Level)
	return nil
}

func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *recordingHandler) WithGroup(string) slog.Handler { return h }

func (h *recordingHandler) sawWarn() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Contains(h.levels, slog.LevelWarn)
}

// cancelingSonarr cancels the walk context from inside GetEpisodes, simulating a
// shutdown/timeout during the episode fetch.
type cancelingSonarr struct {
	series []arrapi.Series
	cancel context.CancelFunc
}

func (f *cancelingSonarr) GetSeries(context.Context) ([]arrapi.Series, error) {
	return f.series, nil
}

func (f *cancelingSonarr) GetEpisodes(ctx context.Context, _ int) ([]arrapi.Episode, error) {
	f.cancel()
	<-ctx.Done()
	return nil, ctx.Err()
}

func (f *cancelingSonarr) ResolveTagIDs(context.Context, ...string) (map[int]struct{}, []string, error) {
	return nil, nil, nil
}

func TestWalkSonarrEpisodeCancellationIsFatalWithoutWarn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	fs := &cancelingSonarr{
		series: []arrapi.Series{{ID: 1, Title: "Alpha"}},
		cancel: cancel,
	}
	handler := &recordingHandler{}
	w := NewWalker(&Config{Sonarr: fs, Logger: slog.New(handler)})

	_, err := w.Walk(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Walk error = %v, want context.Canceled", err)
	}
	if handler.sawWarn() {
		t.Fatal("Walk logged a warning for context cancellation; want cancellation treated as shutdown, not an arr fault")
	}
}

// fakeRadarr is a scripted RadarrClient.
type fakeRadarr struct {
	tagIDs    map[int]struct{}
	listErr   error
	tagErr    error
	movies    []arrapi.Movie
	unmatched []string
}

func (f *fakeRadarr) GetMovies(context.Context) ([]arrapi.Movie, error) {
	return f.movies, f.listErr
}

func (f *fakeRadarr) ResolveTagIDs(context.Context, ...string) (map[int]struct{}, []string, error) {
	return f.tagIDs, f.unmatched, f.tagErr
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
		tagIDs: map[int]struct{}{9: {}},
	}
	w := NewWalker(&Config{Radarr: fr, ExcludeTags: []string{"skip"}, RadarrURL: "https://radarr.example", Logger: discardLogger()})

	snap, err := w.Walk(context.Background())
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

func TestDiffSnapshotsDetectsFingerprintChangeWithSameGroup(t *testing.T) {
	w := NewWalker(&Config{Logger: discardLogger()})
	prev := &Snapshot{Items: []Item{{
		Arr:     ArrSonarr,
		ArrID:   1,
		Groups:  []string{"pmr"},
		Current: w.fingerprint(&fileInfo{group: "pmr", sceneName: "[PMR] Example [1080p][x264]", videoCodec: "AVC"}),
		HasFile: true,
	}}}
	cur := &Snapshot{Items: []Item{{
		Arr:     ArrSonarr,
		ArrID:   1,
		Groups:  []string{"pmr"},
		Current: w.fingerprint(&fileInfo{group: "pmr", sceneName: "[PMR] Example [1080p][x265]", videoCodec: "HEVC"}),
		HasFile: true,
	}}}

	d := DiffSnapshots(prev, cur)
	if d.Added != 0 || d.Removed != 0 || d.Changed != 1 {
		t.Errorf("diff = %+v, want exactly one changed item for same-group fingerprint drift", d)
	}
}

// TestWalkSonarrTagResolutionCancellationIsFatal proves that a context
// cancellation surfaced by ResolveTagIDs aborts the whole walk rather than
// fail-opening the filter (h-f9).
func TestWalkSonarrTagResolutionCancellationIsFatal(t *testing.T) {
	fs := &fakeSonarr{
		series: []arrapi.Series{{ID: 1, Title: "Alpha"}},
		tagErr: context.Canceled,
	}
	w := NewWalker(&Config{Sonarr: fs, IncludeTags: []string{"anime"}, Logger: discardLogger()})
	if _, err := w.Walk(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Walk error = %v, want context.Canceled propagated from tag resolution", err)
	}
}

// TestWalkRadarrTagResolutionCancellationIsFatal is the Radarr-side counterpart:
// the Radarr walk previously had no post-resolution cancellation check, so a
// cancellation during tag resolution must now propagate (h-f9).
func TestWalkRadarrTagResolutionCancellationIsFatal(t *testing.T) {
	fr := &fakeRadarr{
		movies: []arrapi.Movie{{ID: 1, Title: "Movie"}},
		tagErr: context.Canceled,
	}
	w := NewWalker(&Config{Radarr: fr, ExcludeTags: []string{"skip"}, Logger: discardLogger()})
	if _, err := w.Walk(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Walk error = %v, want context.Canceled propagated from tag resolution", err)
	}
}

// TestWalkSonarrTagResolutionErrorFailsOpen proves an ordinary (non-cancellation)
// tag-resolution failure still disables the filter and keeps the walk healthy
// (h-f9 preserves fail-open for real tag errors).
func TestWalkSonarrTagResolutionErrorFailsOpen(t *testing.T) {
	fs := &fakeSonarr{
		series: []arrapi.Series{
			{ID: 1, Title: "Alpha", Tags: []int{1}},
			{ID: 2, Title: "Bravo", Tags: []int{2}},
		},
		episodes: map[int][]arrapi.Episode{
			1: {{SeasonNumber: 1, EpisodeFile: epFile("PMR")}},
			2: {{SeasonNumber: 1, EpisodeFile: epFile("LostYears")}},
		},
		tagErr: errors.New("arr tag lookup boom"),
	}
	w := NewWalker(&Config{Sonarr: fs, IncludeTags: []string{"anime"}, Logger: discardLogger()})
	snap, err := w.Walk(context.Background())
	if err != nil {
		t.Fatalf("Walk returned error, want fail-open on non-cancellation tag error: %v", err)
	}
	if len(snap.Items) != 2 {
		t.Fatalf("items = %d, want 2 (filter disabled, all series kept)", len(snap.Items))
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
		episodes: map[int][]arrapi.Episode{
			1: {
				{SeasonNumber: 1, EpisodeFile: &arrapi.EpisodeFile{
					ReleaseGroup: "PMR",
					SceneName:    "[PMR] Multi S01E01 [1080p][x265]",
					MediaInfo:    &arrapi.MediaInfo{VideoCodec: "HEVC", AudioLanguages: "Japanese / English"},
				}},
				{SeasonNumber: 1, EpisodeFile: epFile("PMR")},
				{SeasonNumber: 1, EpisodeFile: epFile("LostYears")},
				{SeasonNumber: 2, EpisodeFile: epFile("PMR")},
			},
		},
	}
	w := NewWalker(&Config{Sonarr: fs, Logger: discardLogger()})

	snap, err := w.Walk(context.Background())
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
	if !it.MixedGroups {
		t.Error("MixedGroups = false, want true (series spans two groups)")
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
