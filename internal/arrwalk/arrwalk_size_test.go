package arrwalk

import (
	"context"
	"errors"
	"maps"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/cplieger/arrapi/v2"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/slogx/capture"
)

func withFile(season, episode, absolute, file int) arrapi.Episode {
	return arrapi.Episode{SeasonNumber: season, EpisodeNumber: episode, AbsoluteEpisodeNumber: absolute, EpisodeFile: &arrapi.EpisodeFile{ID: file}}
}

func TestWalkSonarrRecordsWhichFileEachEpisodeHolds(t *testing.T) {
	f1, f2 := epFile(1, "PMR"), epFile(1, "PMR")
	f1.ID, f1.Size = 11, 1000
	f2.ID, f2.Size = 12, 2000
	fs := &fakeSonarr{
		series: []arrapi.Series{{ID: 1, Title: "Show"}},
		files:  map[int][]arrapi.EpisodeFile{1: {f1, f2}},
		episodes: map[int][]arrapi.Episode{1: {
			withFile(1, 1, 1, 11), withFile(1, 2, 2, 12), withFile(1, 3, 3, 12),
			{SeasonNumber: 1, EpisodeNumber: 4, AbsoluteEpisodeNumber: 4},
		}},
	}
	snap, err := NewWalker(&Config{Sonarr: fs, Logger: discardLogger()}).Walk(t.Context())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	item := snap.Items[0]
	if want := map[int]int64{11: 1000, 12: 2000}; !maps.Equal(item.FileBytes, want) {
		t.Errorf("FileBytes = %v, want %v", item.FileBytes, want)
	}
	want := map[int]library.Episode{1: {File: 11, Absolute: 1}, 2: {File: 12, Absolute: 2}, 3: {File: 12, Absolute: 3}}
	if len(item.SeasonEpisodes) != 1 || !maps.Equal(item.SeasonEpisodes[1], want) {
		t.Errorf("SeasonEpisodes = %v, want season 1 %v", item.SeasonEpisodes, want)
	}
}

// TestWalkSonarrEpisodeHoldingAnUnplacedFileLeavesSizesUnknown pins that an
// episode holding a file the list does not tie to an id and number leaves the
// whole series' episode map nil: the file may be one a download also covers,
// so counting that file as replaced could drop the episode. The series stays
// comparable and one WARN names it.
func TestWalkSonarrEpisodeHoldingAnUnplacedFileLeavesSizesUnknown(t *testing.T) {
	tests := []struct {
		episode arrapi.Episode
		name    string
	}{
		{name: "a file with no payload", episode: arrapi.Episode{SeasonNumber: 1, EpisodeNumber: 13, AbsoluteEpisodeNumber: 13, HasFile: true}},
		{name: "a payload with no file id", episode: arrapi.Episode{SeasonNumber: 1, EpisodeNumber: 13, HasFile: true, EpisodeFile: &arrapi.EpisodeFile{}}},
		{name: "a file on no episode number", episode: withFile(1, 0, 0, 12)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeSonarr{
				series:   []arrapi.Series{{ID: 1, Title: "Show"}},
				files:    map[int][]arrapi.EpisodeFile{1: {epFile(1, "PMR")}},
				episodes: map[int][]arrapi.Episode{1: {withFile(1, 12, 12, 12), tc.episode}},
			}
			logger, rec := capture.New()
			snap, err := NewWalker(&Config{Sonarr: fs, Logger: logger}).Walk(t.Context())
			if err != nil {
				t.Fatalf("Walk: %v", err)
			}
			if got := snap.Items[0]; got.SeasonEpisodes != nil || len(got.Groups) == 0 || got.Failed || snap.Partial {
				t.Errorf("Walk(%s) = SeasonEpisodes %v, Groups %v, Failed %v, Partial %v, want nil map on a comparable item",
					tc.name, got.SeasonEpisodes, got.Groups, got.Failed, snap.Partial)
			}
			if !rec.HasAttr("sonarr episode list marks a file it does not place; this series' upgrade sizes are unknown", "id", "1") {
				t.Errorf("Walk(%s) messages = %q, want the unplaced-file WARN naming series 1", tc.name, rec.Messages())
			}
		})
	}
}

// TestWalkSonarrEpisodeListFailureLeavesSizesUnknown pins that a failed
// episode list read costs only that series' sizes: the walk succeeds, the
// series keeps its groups, its episode map stays nil, and one WARN names it.
func TestWalkSonarrEpisodeListFailureLeavesSizesUnknown(t *testing.T) {
	fs := &fakeSonarr{
		series:      []arrapi.Series{{ID: 1, Title: "Show"}, {ID: 2, Title: "Other"}},
		files:       map[int][]arrapi.EpisodeFile{1: {epFile(1, "PMR")}, 2: {epFile(1, "PMR")}},
		episodes:    map[int][]arrapi.Episode{2: {withFile(1, 1, 1, 5)}},
		episodesErr: map[int]error{1: errors.New("sonarr down")},
	}
	logger, rec := capture.New()
	snap, err := NewWalker(&Config{Sonarr: fs, Logger: logger}).Walk(t.Context())
	if err != nil {
		t.Fatalf("Walk: %v, want a complete walk", err)
	}
	if snap.Partial || len(snap.Items) != 2 {
		t.Fatalf("snapshot partial = %v with %d items, want a complete 2-item snapshot", snap.Partial, len(snap.Items))
	}
	if got := snap.Items[0]; got.SeasonEpisodes != nil || len(got.Groups) == 0 || got.Failed {
		t.Errorf("failed series: SeasonEpisodes %v, Groups %v, Failed %v, want nil map, groups kept, not failed", got.SeasonEpisodes, got.Groups, got.Failed)
	}
	if snap.Items[1].SeasonEpisodes == nil {
		t.Error("the other series lost its episode map")
	}
	if n := rec.CountExact("sonarr episode list read failed; this series' upgrade sizes are unknown"); n != 1 {
		t.Errorf("episode list WARN logged %d times, want 1", n)
	}
}

func TestWalkRadarrRecordsTheMovieFileSize(t *testing.T) {
	fr := &fakeRadarr{movies: []arrapi.Movie{{ID: 1, Title: "Film", HasFile: true, MovieFile: &arrapi.MovieFile{ID: 9, Size: 3000, ReleaseGroup: "PMR"}}}}
	snap, err := NewWalker(&Config{Radarr: fr, Logger: discardLogger()}).Walk(t.Context())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if want := map[int]int64{9: 3000}; !maps.Equal(snap.Items[0].FileBytes, want) {
		t.Errorf("FileBytes = %v, want %v", snap.Items[0].FileBytes, want)
	}
}

// pairedSonarr blocks both per-series reads until released and records the
// peak number of reads in flight, so a test can prove the episode list read
// shares the episode-file fetch's concurrency bound.
type pairedSonarr struct {
	release   chan struct{}
	series    []arrapi.Series
	mu        sync.Mutex
	active    int
	maxActive int
}

func (f *pairedSonarr) enter(ctx context.Context) error {
	f.mu.Lock()
	f.active++
	f.maxActive = max(f.maxActive, f.active)
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.active--
		f.mu.Unlock()
	}()
	select {
	case <-f.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *pairedSonarr) Series(context.Context) ([]arrapi.Series, error) { return f.series, nil }

func (f *pairedSonarr) EpisodeFiles(ctx context.Context, _ int) ([]arrapi.EpisodeFile, error) {
	if err := f.enter(ctx); err != nil {
		return nil, err
	}
	return []arrapi.EpisodeFile{epFile(1, "PMR")}, nil
}

func (f *pairedSonarr) Episodes(ctx context.Context, _ int) ([]arrapi.Episode, error) {
	return nil, f.enter(ctx)
}

func (f *pairedSonarr) Tags(context.Context) ([]arrapi.Tag, error) { return nil, nil }

func TestWalkSonarrEpisodeListReadStaysInsideTheConcurrencyBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := 2*episodeConcurrency + 1
		fs := &pairedSonarr{release: make(chan struct{})}
		for id := 1; id <= n; id++ {
			fs.series = append(fs.series, arrapi.Series{ID: id, Title: "Series"})
		}
		done := make(chan error, 1)
		go func() {
			_, err := NewWalker(&Config{Sonarr: fs, Logger: discardLogger()}).Walk(t.Context())
			done <- err
		}()
		for range 2 * n {
			synctest.Wait()
			fs.release <- struct{}{}
		}
		if err := <-done; err != nil {
			t.Fatalf("Walk: %v", err)
		}
		if fs.maxActive > episodeConcurrency {
			t.Errorf("peak reads in flight = %d, want at most %d", fs.maxActive, episodeConcurrency)
		}
	})
}
