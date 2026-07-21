package library

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
	"github.com/cplieger/slogx/capture"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeSonarr is a scripted SonarrClient: GetSeries returns series (or listErr),
// GetEpisodeFiles returns files[id] (or epErr[id]), GetTags returns the canned
// tag list (or tagErr) and counts its calls so tests can pin the
// one-fetch-per-walk tag-resolution contract.
type fakeSonarr struct {
	files    map[int][]arrapi.EpisodeFile
	epErr    map[int]error
	listErr  error
	tagErr   error
	series   []arrapi.Series
	tags     []arrapi.Tag
	tagCalls int
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
	f.tagCalls++
	return f.tags, f.tagErr
}

func epFile(season int, group string) arrapi.EpisodeFile {
	return arrapi.EpisodeFile{SeasonNumber: season, ReleaseGroup: group}
}

// diffItem builds a minimal comparable Item for the DiffSnapshots tests.
func diffItem(arr string, id int, groups ...string) Item {
	return Item{Arr: arr, ArrID: id, Groups: groups, HasFile: len(groups) > 0}
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

	snap, err := w.Walk(context.Background())
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

	snap, err := w.Walk(context.Background())
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

			snap, err := w.Walk(context.Background())
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
		files: map[int][]arrapi.EpisodeFile{
			1: {epFile(1, "PMR")},
		},
		tags: []arrapi.Tag{{ID: 7, Label: "anime"}},
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

// TestWalkResolvesTagsWithOneFetchPerArr pins the single-fetch tag-resolution
// contract: the include AND exclude label sets resolve against ONE tag-list
// fetch per arr per walk (resolved locally via arrapi.TagIDs /
// UnmatchedLabels), and a walker with no tag filters configured never fetches
// the tag list at all.
func TestWalkResolvesTagsWithOneFetchPerArr(t *testing.T) {
	t.Run("include and exclude share one fetch per arr", func(t *testing.T) {
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

		snap, err := w.Walk(context.Background())
		if err != nil {
			t.Fatalf("Walk: %v", err)
		}
		if fs.tagCalls != 1 {
			t.Errorf("sonarr tag-list fetches = %d, want exactly 1 for both label sets", fs.tagCalls)
		}
		if fr.tagCalls != 1 {
			t.Errorf("radarr tag-list fetches = %d, want exactly 1 for both label sets", fr.tagCalls)
		}
		if len(snap.Items) != 2 {
			t.Fatalf("items = %+v, want the include-tagged series and movie only", snap.Items)
		}
		for _, it := range snap.Items {
			if it.Arr == ArrSonarr && it.ArrID == 2 {
				t.Error("excluded series (id 2) present, want it dropped by the exclude set from the shared fetch")
			}
		}
	})
	t.Run("no configured tag filters means no fetch", func(t *testing.T) {
		fs := &fakeSonarr{
			series: []arrapi.Series{{ID: 1, Title: "Alpha"}},
			files:  map[int][]arrapi.EpisodeFile{1: {epFile(1, "PMR")}},
		}
		fr := &fakeRadarr{movies: []arrapi.Movie{{ID: 2, Title: "Movie"}}}
		w := NewWalker(&Config{Sonarr: fs, Radarr: fr, Logger: discardLogger()})
		if _, err := w.Walk(context.Background()); err != nil {
			t.Fatalf("Walk: %v", err)
		}
		if fs.tagCalls != 0 || fr.tagCalls != 0 {
			t.Errorf("tag-list fetches = sonarr %d / radarr %d, want 0/0 with no tag filters configured", fs.tagCalls, fr.tagCalls)
		}
	})
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
	prev := &Snapshot{Items: []Item{
		diffItem(ArrSonarr, 1, "pmr"),
		diffItem(ArrSonarr, 2, "grp"),
		diffItem(ArrRadarr, 3, "movgrp"),
	}}
	cur := &Snapshot{Items: []Item{
		diffItem(ArrSonarr, 1, "pmr"),       // unchanged
		diffItem(ArrSonarr, 2, "lostyears"), // changed group set
		diffItem(ArrSonarr, 4, "newgrp"),    // added
		// Radarr id 3 removed
	}}
	d := DiffSnapshots(prev, cur)
	if d.Added != 1 || d.Removed != 1 || d.Changed != 1 {
		t.Errorf("diff = %+v, want Added=1 Removed=1 Changed=1", d)
	}
}

// TestDiffSnapshotsPartialAware pins the per-key partial suppression on the
// diff: only a key that is a Failed placeholder (in cur for removals, in prev
// for additions) is suppressed, while an item genuinely absent from a Partial
// snapshot still diffs - a published partial walk keeps every failed series
// as a placeholder, so absence means the arr no longer lists it. The blanket
// "partial suppresses every Sonarr addition/removal" behavior is retired: it
// permanently masked real removals and additions once partial walks started
// retaining Failed placeholders.
func TestDiffSnapshotsPartialAware(t *testing.T) {
	failed := func(arr string, id int) Item {
		return Item{Arr: arr, ArrID: id, Failed: true}
	}
	t.Run("failed placeholder in cur suppresses only its own removal", func(t *testing.T) {
		// Series A's episode fetch failed this walk (a Failed placeholder);
		// series B is truly gone from Sonarr. B reports removed even though
		// cur is Partial; A does not, and a change on a clean item counts.
		prev := &Snapshot{Items: []Item{
			diffItem(ArrSonarr, 1, "pmr"),  // A: fetch failed this walk
			diffItem(ArrSonarr, 2, "grp"),  // B: genuinely removed
			diffItem(ArrSonarr, 3, "seed"), // C: group changed
		}}
		cur := &Snapshot{Partial: true, Items: []Item{
			failed(ArrSonarr, 1),
			diffItem(ArrSonarr, 3, "lostyears"),
		}}
		d := DiffSnapshots(prev, cur)
		if d.Removed != 1 || d.Changed != 1 || d.Added != 0 {
			t.Errorf("diff = %+v, want Removed=1 (only the truly gone series) Changed=1 Added=0", d)
		}
	})
	t.Run("failed placeholder in prev suppresses only its own addition", func(t *testing.T) {
		prev := &Snapshot{Partial: true, Items: []Item{
			diffItem(ArrSonarr, 1, "pmr"),
			failed(ArrSonarr, 2), // recovers this walk
		}}
		cur := &Snapshot{Items: []Item{
			diffItem(ArrSonarr, 1, "pmr"),
			diffItem(ArrSonarr, 2, "grp"),    // recovered, not an arrival
			diffItem(ArrSonarr, 4, "newgrp"), // genuinely added
		}}
		d := DiffSnapshots(prev, cur)
		if d.Added != 1 || d.Removed != 0 || d.Changed != 0 {
			t.Errorf("diff = %+v, want Added=1 (only the genuinely new series) with the recovery suppressed", d)
		}
	})
	t.Run("radarr transitions count during a sonarr partial", func(t *testing.T) {
		// Partial is set only by Sonarr episode-fetch failures and Radarr
		// items never carry Failed, so their presence changes always count.
		prev := &Snapshot{Items: []Item{
			diffItem(ArrSonarr, 1, "pmr"),
			diffItem(ArrRadarr, 3, "movgrp"), // genuinely removed
		}}
		cur := &Snapshot{Partial: true, Items: []Item{
			failed(ArrSonarr, 1),
			diffItem(ArrRadarr, 4, "newmov"), // genuinely added
		}}
		d := DiffSnapshots(prev, cur)
		if d.Added != 1 || d.Removed != 1 || d.Changed != 0 {
			t.Errorf("diff = %+v, want Added=1 Removed=1 (radarr transitions) with the sonarr failure suppressed", d)
		}
	})
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
			snap, err := w.Walk(context.Background())
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
	ctx, cancel := context.WithCancel(context.Background())
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

// fakeRadarr is a scripted RadarrClient. GetTags counts its calls so tests can
// pin the one-fetch-per-walk tag-resolution contract.
type fakeRadarr struct {
	listErr  error
	tagErr   error
	movies   []arrapi.Movie
	tags     []arrapi.Tag
	tagCalls int
}

func (f *fakeRadarr) GetMovies(context.Context) ([]arrapi.Movie, error) {
	return f.movies, f.listErr
}

func (f *fakeRadarr) GetTags(context.Context) ([]arrapi.Tag, error) {
	f.tagCalls++
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
	prev := &Snapshot{Items: []Item{{
		Arr:     ArrSonarr,
		ArrID:   1,
		Groups:  []string{"pmr"},
		Current: fingerprint(&fileInfo{group: "pmr", sceneName: "[PMR] Example [1080p][x264]", videoCodec: "AVC"}),
		HasFile: true,
	}}}
	cur := &Snapshot{Items: []Item{{
		Arr:     ArrSonarr,
		ArrID:   1,
		Groups:  []string{"pmr"},
		Current: fingerprint(&fileInfo{group: "pmr", sceneName: "[PMR] Example [1080p][x265]", videoCodec: "HEVC"}),
		HasFile: true,
	}}}

	d := DiffSnapshots(prev, cur)
	if d.Added != 0 || d.Removed != 0 || d.Changed != 1 {
		t.Errorf("diff = %+v, want exactly one changed item for same-group fingerprint drift", d)
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
	if _, err := w.Walk(context.Background()); !errors.Is(err, context.Canceled) {
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
	if _, err := w.Walk(context.Background()); !errors.Is(err, context.Canceled) {
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
	_, err := w.Walk(context.Background())
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
	if _, err := w.Walk(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
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

	snap, err := w.Walk(context.Background())
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

func TestWalkSonarrUnmatchedIncludeTagLogsWarning(t *testing.T) {
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
	logger, rec := capture.New()
	w := NewWalker(&Config{Sonarr: fs, IncludeTags: []string{"anime", "nonexistent"}, Logger: logger})

	snap, err := w.Walk(context.Background())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(snap.Items) != 1 || snap.Items[0].ArrID != 1 {
		t.Fatalf("items = %+v, want only the tag-included series (id 1)", snap.Items)
	}
	if rec.CountLevel(slog.LevelWarn, "") == 0 {
		t.Error("no warning logged, want a warning that a configured tag matched no arr tag")
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
	const secret = "sekrit-expanded-api-key-9f8e7d"
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
	w := NewWalker(&Config{Sonarr: fs, IncludeTags: []string{"anime", secret}, Logger: logger})

	if _, err := w.Walk(context.Background()); err != nil {
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

	snap, err := w.Walk(context.Background())
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

// TestWalkRadarrTopLevelListErrorIsFatal covers the Radarr side of the
// health semantic: a failed top-level movie list fails the whole walk
// (mirrors TestWalkTopLevelListErrorIsFatal for Sonarr).
func TestWalkRadarrTopLevelListErrorIsFatal(t *testing.T) {
	fr := &fakeRadarr{listErr: errors.New("radarr unreachable")}
	w := NewWalker(&Config{Radarr: fr, Logger: discardLogger()})
	if _, err := w.Walk(context.Background()); err == nil {
		t.Fatal("Walk returned nil error, want the GetMovies failure propagated")
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

	snap, err := w.Walk(context.Background())
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
	snap, err := w.Walk(context.Background())
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
	snap, err := w.Walk(context.Background())
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
	snap, err := w.Walk(context.Background())
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

// TestDiffSnapshotsDetectsSeasonGroupAttributionChange pins the third leg of
// the documented Changed contract: an item whose overall group set and
// fingerprint are unchanged but whose per-season group attribution moved
// (the groups swapped seasons) must still count as Changed.
func TestDiffSnapshotsDetectsSeasonGroupAttributionChange(t *testing.T) {
	prev := &Snapshot{Items: []Item{{
		Arr:          ArrSonarr,
		ArrID:        1,
		Groups:       []string{"lostyears", "pmr"},
		SeasonGroups: map[int][]string{1: {"pmr"}, 2: {"lostyears"}},
		HasFile:      true,
	}}}
	cur := &Snapshot{Items: []Item{{
		Arr:          ArrSonarr,
		ArrID:        1,
		Groups:       []string{"lostyears", "pmr"},
		SeasonGroups: map[int][]string{1: {"lostyears"}, 2: {"pmr"}},
		HasFile:      true,
	}}}
	d := DiffSnapshots(prev, cur)
	if d.Added != 0 || d.Removed != 0 || d.Changed != 1 {
		t.Errorf("diff = %+v, want exactly one changed item for a season-attribution-only change", d)
	}
}

// TestDiffSnapshotsKeysByArrAndID pins the documented "keyed by arr + id"
// contract: a Sonarr item and a Radarr item sharing the same numeric arr id
// are distinct entries, so removing only the Radarr one counts exactly one
// removal and no change on the same-id Sonarr item.
func TestDiffSnapshotsKeysByArrAndID(t *testing.T) {
	prev := &Snapshot{Items: []Item{
		{Arr: ArrSonarr, ArrID: 1, Groups: []string{"pmr"}, HasFile: true},
		{Arr: ArrRadarr, ArrID: 1, Groups: []string{"movgrp"}, HasFile: true},
	}}
	cur := &Snapshot{Items: []Item{
		{Arr: ArrSonarr, ArrID: 1, Groups: []string{"pmr"}, HasFile: true},
	}}
	d := DiffSnapshots(prev, cur)
	if d.Added != 0 || d.Removed != 1 || d.Changed != 0 {
		t.Errorf("diff = %+v, want Removed=1 Changed=0 (arr-qualified keys keep same-id items distinct)", d)
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

	snap, err := w.Walk(context.Background())
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
	snap, err := w.Walk(context.Background())
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

// TestWalkCombinesBothArrsIntoOneSnapshot pins Walk's both-sides contract: a
// walker configured with Sonarr AND Radarr merges both item sets into one
// snapshot, each item labelled with its source arr.
func TestWalkCombinesBothArrsIntoOneSnapshot(t *testing.T) {
	fs := &fakeSonarr{
		series: []arrapi.Series{{ID: 1, Title: "Series"}},
		files: map[int][]arrapi.EpisodeFile{
			1: {epFile(1, "PMR")},
		},
	}
	fr := &fakeRadarr{
		movies: []arrapi.Movie{{ID: 2, Title: "Movie", HasFile: true, MovieFile: &arrapi.MovieFile{ReleaseGroup: "LostYears"}}},
	}
	w := NewWalker(&Config{Sonarr: fs, Radarr: fr, Logger: discardLogger()})
	snap, err := w.Walk(context.Background())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(snap.Items) != 2 {
		t.Fatalf("items = %d, want 2 (one per arr side)", len(snap.Items))
	}
	arrs := map[string]int{}
	for _, it := range snap.Items {
		arrs[it.Arr]++
	}
	if arrs[ArrSonarr] != 1 || arrs[ArrRadarr] != 1 {
		t.Errorf("arr distribution = %v, want one sonarr and one radarr item", arrs)
	}
}

// TestDiffSnapshotsSkipsFailedPlaceholders pins the Failed keys' exclusion
// from comparison: a Failed placeholder carries no comparable file state, so
// its key is never Changed, its own removal is suppressed while it is Failed
// in cur, and its recovery is not an addition when it was Failed in prev.
func TestDiffSnapshotsSkipsFailedPlaceholders(t *testing.T) {
	t.Run("failed placeholder in cur is not a change or removal", func(t *testing.T) {
		prev := &Snapshot{Items: []Item{diffItem(ArrSonarr, 1, "pmr")}}
		cur := &Snapshot{Partial: true, Items: []Item{{Arr: ArrSonarr, ArrID: 1, Failed: true}}}
		if d := DiffSnapshots(prev, cur); d != (Diff{}) {
			t.Errorf("diff = %+v, want zero Diff (a Failed placeholder carries no comparable state)", d)
		}
	})
	t.Run("failed placeholder in prev is not an addition when the series returns", func(t *testing.T) {
		prev := &Snapshot{Partial: true, Items: []Item{{Arr: ArrSonarr, ArrID: 1, Failed: true}}}
		cur := &Snapshot{Items: []Item{diffItem(ArrSonarr, 1, "pmr")}}
		if d := DiffSnapshots(prev, cur); d != (Diff{}) {
			t.Errorf("diff = %+v, want zero Diff (a returning series after a Failed walk is not added)", d)
		}
	})
	t.Run("failed placeholder gone from cur is a removal", func(t *testing.T) {
		prev := &Snapshot{Partial: true, Items: []Item{{Arr: ArrSonarr, ArrID: 1, Failed: true}}}
		cur := &Snapshot{}
		if d := DiffSnapshots(prev, cur); d != (Diff{Removed: 1}) {
			t.Errorf("diff = %+v, want Removed=1 (a Failed placeholder still carries arr presence)", d)
		}
	})
	t.Run("key debuting as a failed placeholder is an addition", func(t *testing.T) {
		prev := &Snapshot{}
		cur := &Snapshot{Partial: true, Items: []Item{{Arr: ArrSonarr, ArrID: 2, Failed: true}}}
		if d := DiffSnapshots(prev, cur); d != (Diff{Added: 1}) {
			t.Errorf("diff = %+v, want Added=1 (a new series whose first fetch failed is still an arrival)", d)
		}
	})
	t.Run("failed placeholder on both sides is no transition", func(t *testing.T) {
		prev := &Snapshot{Partial: true, Items: []Item{{Arr: ArrSonarr, ArrID: 3, Failed: true}}}
		cur := &Snapshot{Partial: true, Items: []Item{{Arr: ArrSonarr, ArrID: 3, Failed: true}}}
		if d := DiffSnapshots(prev, cur); d != (Diff{}) {
			t.Errorf("diff = %+v, want zero Diff (failed on both sides is no transition)", d)
		}
	})
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
			_, err := w.Walk(context.Background())
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
	snap, err := w.Walk(context.Background())
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
		snap, err := w.Walk(context.Background())
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
		snap, err := w.Walk(context.Background())
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

	if _, err := w.Walk(context.Background()); err != nil {
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

	if _, err := w.Walk(context.Background()); err != nil {
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
			_, err := w.Walk(context.Background())
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
