package arrwalk

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"reflect"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/cplieger/arrapi/v3"
	"github.com/cplieger/arrapi/v3/radarr"
	"github.com/cplieger/arrapi/v3/sonarr"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/slogx/capture"
)

func withFile(season, episode, absolute, file int) sonarr.Episode {
	return sonarr.Episode{Season: season, Number: episode, Absolute: absolute, HasFile: true, FileID: file}
}

func listedFile(id, season int, group string) sonarr.EpisodeFile {
	return sonarr.EpisodeFile{ID: id, Season: season, ReleaseGroup: group}
}

// The SonarrPass surface has no method returning episodes with embedded
// files, so the walker cannot ask for the larger read.
func TestSonarrPassCannotRequestEmbeddedFiles(t *testing.T) {
	typ := reflect.TypeFor[SonarrPass]()
	if _, ok := typ.MethodByName("EpisodesWithFiles"); ok {
		t.Error("SonarrPass declares EpisodesWithFiles, want only Episodes")
	}
	if _, ok := typ.MethodByName("Episodes"); !ok {
		t.Error("SonarrPass lacks Episodes")
	}
}

// TestEpisodeFileIDMissingFromFileListIsUnknown pins the second
// rule: the episode list and the file list are separate answers, so an
// episode naming a file id the file list lacks has an unknown file state. Its
// sizes, its season's listing and, in season 0, the specials stay uncompared,
// each with the existing WARN, on a comparable item.
func TestEpisodeFileIDMissingFromFileListIsUnknown(t *testing.T) {
	files := []sonarr.EpisodeFile{listedFile(10, 1, "A"), listedFile(20, 0, "B")}
	const placeWarn = "sonarr episode list marks a file it does not place; this series' upgrade sizes are unknown"
	const specialsWarn = "sonarr specials episode list marks a file it does not send; the series' films and specials stay uncompared"
	t.Run("positive season", func(t *testing.T) {
		fs := &fakeSonarr{
			series:   []sonarr.Series{{ID: 1, Title: "Show"}},
			files:    map[int][]sonarr.EpisodeFile{1: files},
			episodes: map[int][]sonarr.Episode{1: {withFile(1, 1, 1, 10), withFile(1, 2, 2, 11), withFile(0, 1, 0, 20)}},
		}
		logger, rec := capture.New()
		snap, _, err := NewWalker(&Config{Sonarr: onePass{fs}, Logger: logger}).Walk(t.Context())
		if err != nil {
			t.Fatalf("Walk: %v", err)
		}
		it := snap.Items[0]
		if it.SeasonEpisodes != nil || it.Listed[1] != nil || it.Failed || snap.Partial || len(it.Groups) == 0 {
			t.Errorf("Walk = SeasonEpisodes %v, Listed %+v, Failed %v, Partial %v, Groups %v; want unknown sizes and season 1 unlisted on a comparable item",
				it.SeasonEpisodes, it.Listed, it.Failed, snap.Partial, it.Groups)
		}
		if !rec.HasAttr(placeWarn, "id", "1") {
			t.Errorf("messages = %q, want the unplaced-file WARN naming series 1", rec.Messages())
		}
		if it.Specials[1].Group != "b" {
			t.Errorf("Specials = %+v, want S00E01 on file 20's group b", it.Specials)
		}
	})
	t.Run("season 0", func(t *testing.T) {
		fs := &fakeSonarr{
			series:   []sonarr.Series{{ID: 1, Title: "Show"}},
			files:    map[int][]sonarr.EpisodeFile{1: files},
			episodes: map[int][]sonarr.Episode{1: {withFile(1, 1, 1, 10), withFile(0, 1, 0, 20), withFile(0, 2, 0, 21)}},
		}
		logger, rec := capture.New()
		snap, _, err := NewWalker(&Config{Sonarr: onePass{fs}, Logger: logger}).Walk(t.Context())
		if err != nil {
			t.Fatalf("Walk: %v", err)
		}
		it := snap.Items[0]
		if it.Specials != nil || it.SeasonEpisodes != nil || it.Failed || snap.Partial {
			t.Errorf("Walk = Specials %+v, SeasonEpisodes %v, Failed %v, Partial %v; want unknown specials and sizes on a comparable item",
				it.Specials, it.SeasonEpisodes, it.Failed, snap.Partial)
		}
		if !rec.HasAttr(specialsWarn, "id", "1") || !rec.HasAttr(placeWarn, "id", "1") {
			t.Errorf("messages = %q, want the specials and unplaced-file WARNs naming series 1", rec.Messages())
		}
		if want := map[int]library.ListedEpisode{1: {Group: "a", HasFile: true}}; !maps.Equal(it.Listed[1], want) {
			t.Errorf("Listed[1] = %+v, want %+v", it.Listed[1], want)
		}
	})
	t.Run("every id listed", func(t *testing.T) {
		fs := &fakeSonarr{
			series:   []sonarr.Series{{ID: 1, Title: "Show"}},
			files:    map[int][]sonarr.EpisodeFile{1: files},
			episodes: map[int][]sonarr.Episode{1: {withFile(1, 1, 1, 10), withFile(0, 1, 0, 20)}},
		}
		snap, _, err := NewWalker(&Config{Sonarr: onePass{fs}, Logger: discardLogger()}).Walk(t.Context())
		if err != nil {
			t.Fatalf("Walk: %v", err)
		}
		want := map[int]map[int]library.Episode{1: {1: {File: 10, Absolute: 1}}, 0: {1: {File: 20}}}
		if it := snap.Items[0]; !maps.EqualFunc(it.SeasonEpisodes, want, maps.Equal) || it.Specials[1].Group != "b" {
			t.Errorf("Walk = SeasonEpisodes %v, Specials %+v; want %v and S00E01 on group b", it.SeasonEpisodes, it.Specials, want)
		}
	})
}

// TestFileNamedByNoEpisodeIsNotReplaced pins that a listed file no episode
// names, such as one part of an episode split across files, is in no
// episode's sizes and leaves the series' sizes, listing and specials known,
// with no WARN. Sonarr's upgrade deletes only the files linked to the episodes
// it imports (v4-develop and develop UpgradeMediaFileService.cs:40-42), so
// such a file is not replaced.
func TestFileNamedByNoEpisodeIsNotReplaced(t *testing.T) {
	part1, part2 := listedFile(10, 1, "A"), listedFile(11, 1, "A")
	part1.Size, part2.Size = 1000, 500
	special1, special2 := listedFile(20, 0, "B"), listedFile(21, 0, "B")
	fs := &fakeSonarr{
		series:   []sonarr.Series{{ID: 1, Title: "Show"}},
		files:    map[int][]sonarr.EpisodeFile{1: {part1, part2, special1, special2}},
		episodes: map[int][]sonarr.Episode{1: {withFile(1, 1, 1, 10), {Season: 1, Number: 2, Absolute: 2}, specialEp(1, 20), specialEp(2, 0)}},
	}
	logger, rec := capture.New()
	snap, _, err := NewWalker(&Config{Sonarr: onePass{fs}, Logger: logger}).Walk(t.Context())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	it := snap.Items[0]
	wantEpisodes := map[int]map[int]library.Episode{1: {1: {File: 10, Absolute: 1}}, 0: {1: {File: 20}}}
	if !maps.EqualFunc(it.SeasonEpisodes, wantEpisodes, maps.Equal) {
		t.Errorf("SeasonEpisodes = %v, want %v: files 11 and 21 on no episode", it.SeasonEpisodes, wantEpisodes)
	}
	wantListed := map[int]map[int]library.ListedEpisode{1: {1: {Group: "a", HasFile: true}, 2: {}}}
	if !maps.EqualFunc(it.Listed, wantListed, maps.Equal) {
		t.Errorf("Listed = %+v, want %+v", it.Listed, wantListed)
	}
	if want := map[int]library.ListedEpisode{1: {Group: "b", HasFile: true}, 2: {}}; !maps.Equal(it.Specials, want) {
		t.Errorf("Specials = %+v, want %+v", it.Specials, want)
	}
	if n := rec.CountLevel(slog.LevelWarn, ""); n != 0 {
		t.Errorf("WARN records = %d, want 0; messages %q", n, rec.Messages())
	}
}

// TestUpgradeBetweenReadsIsUnknown pins the walker's read order against an
// upgrade landing between its two reads: the file list holds the old file,
// and the episode list names the new file's id, which the file list lacks.
// Sonarr deletes the old file and links the new one (v4-develop
// EpisodeService.cs:257-290, develop :267-300), so the episode's file state is
// unknown and its sizes, listing and specials stay uncompared.
func TestUpgradeBetweenReadsIsUnknown(t *testing.T) {
	const placeWarn = "sonarr episode list marks a file it does not place; this series' upgrade sizes are unknown"
	const specialsWarn = "sonarr specials episode list marks a file it does not send; the series' films and specials stay uncompared"
	t.Run("positive season", func(t *testing.T) {
		fs := &fakeSonarr{
			series:   []sonarr.Series{{ID: 1, Title: "Show"}},
			files:    map[int][]sonarr.EpisodeFile{1: {listedFile(10, 1, "A"), listedFile(11, 1, "A"), listedFile(20, 0, "B")}},
			episodes: map[int][]sonarr.Episode{1: {withFile(1, 1, 1, 10), withFile(1, 2, 2, 12), specialEp(1, 20)}},
		}
		logger, rec := capture.New()
		snap, _, err := NewWalker(&Config{Sonarr: onePass{fs}, Logger: logger}).Walk(t.Context())
		if err != nil {
			t.Fatalf("Walk: %v", err)
		}
		it := snap.Items[0]
		if it.SeasonEpisodes != nil || it.Listed[1] != nil || it.Failed || snap.Partial || len(it.SeasonGroups[1]) == 0 {
			t.Errorf("Walk = SeasonEpisodes %v, Listed %+v, Failed %v, Partial %v, SeasonGroups %v; want unknown sizes and season 1 unlisted on a comparable item",
				it.SeasonEpisodes, it.Listed, it.Failed, snap.Partial, it.SeasonGroups)
		}
		if !rec.HasAttr(placeWarn, "id", "1") {
			t.Errorf("messages = %q, want the unplaced-file WARN naming series 1", rec.Messages())
		}
		if want := map[int]library.ListedEpisode{1: {Group: "b", HasFile: true}}; !maps.Equal(it.Specials, want) {
			t.Errorf("Specials = %+v, want %+v", it.Specials, want)
		}
	})
	t.Run("season 0", func(t *testing.T) {
		fs := &fakeSonarr{
			series:   []sonarr.Series{{ID: 1, Title: "Show"}},
			files:    map[int][]sonarr.EpisodeFile{1: {listedFile(10, 1, "A"), listedFile(20, 0, "B")}},
			episodes: map[int][]sonarr.Episode{1: {withFile(1, 1, 1, 10), specialEp(1, 21)}},
		}
		logger, rec := capture.New()
		snap, _, err := NewWalker(&Config{Sonarr: onePass{fs}, Logger: logger}).Walk(t.Context())
		if err != nil {
			t.Fatalf("Walk: %v", err)
		}
		it := snap.Items[0]
		if it.Specials != nil || it.SeasonEpisodes != nil || it.Failed || snap.Partial {
			t.Errorf("Walk = Specials %+v, SeasonEpisodes %v, Failed %v, Partial %v; want unknown specials and sizes on a comparable item",
				it.Specials, it.SeasonEpisodes, it.Failed, snap.Partial)
		}
		if !rec.HasAttr(specialsWarn, "id", "1") || !rec.HasAttr(placeWarn, "id", "1") {
			t.Errorf("messages = %q, want the specials and unplaced-file WARNs naming series 1", rec.Messages())
		}
		if want := map[int]library.ListedEpisode{1: {Group: "a", HasFile: true}}; !maps.Equal(it.Listed[1], want) {
			t.Errorf("Listed[1] = %+v, want %+v", it.Listed[1], want)
		}
	})
}

// TestFirstSpecialImportedBetweenReadsIsUnknown pins the second rule
// when the file list holds no season-0 file yet: Sonarr links the series' first
// special between the two reads (v4-develop EpisodeService.cs:284-288, develop
// :294-298), so the episode list names a season-0 file id the file list lacks.
// The specials are unknown, not known absent, with the existing specials WARN.
func TestFirstSpecialImportedBetweenReadsIsUnknown(t *testing.T) {
	const placeWarn = "sonarr episode list marks a file it does not place; this series' upgrade sizes are unknown"
	const specialsWarn = "sonarr specials episode list marks a file it does not send; the series' films and specials stay uncompared"
	for _, tc := range []struct {
		name    string
		special sonarr.Episode
	}{
		{"file id the list lacks", specialEp(1, 20)},
		{"file with no id", sonarr.Episode{Season: 0, Number: 1, HasFile: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeSonarr{
				series:   []sonarr.Series{{ID: 1, Title: "Show"}},
				files:    map[int][]sonarr.EpisodeFile{1: {listedFile(10, 1, "A")}},
				episodes: map[int][]sonarr.Episode{1: {withFile(1, 1, 1, 10), tc.special}},
			}
			logger, rec := capture.New()
			snap, _, err := NewWalker(&Config{Sonarr: onePass{fs}, Logger: logger}).Walk(t.Context())
			if err != nil {
				t.Fatalf("Walk: %v", err)
			}
			it := snap.Items[0]
			if !it.SpecialsUnknown() || it.Specials != nil || it.Failed || snap.Partial {
				t.Errorf("Walk = SpecialsUnknown %v, Specials %+v, Failed %v, Partial %v; want unknown specials on a comparable item",
					it.SpecialsUnknown(), it.Specials, it.Failed, snap.Partial)
			}
			if !rec.HasAttr(specialsWarn, "id", "1") || !rec.HasAttr(placeWarn, "id", "1") {
				t.Errorf("messages = %q, want the specials and unplaced-file WARNs naming series 1", rec.Messages())
			}
			if want := map[int]library.ListedEpisode{1: {Group: "a", HasFile: true}}; !maps.Equal(it.Listed[1], want) {
				t.Errorf("Listed[1] = %+v, want %+v", it.Listed[1], want)
			}
		})
	}
}

func TestWalkSonarrRecordsWhichFileEachEpisodeHolds(t *testing.T) {
	f1, f2 := epFile(1, "PMR"), epFile(1, "PMR")
	f1.ID, f1.Size = 11, 1000
	f2.ID, f2.Size = 12, 2000
	fs := &fakeSonarr{
		series: []sonarr.Series{{ID: 1, Title: "Show"}},
		files:  map[int][]sonarr.EpisodeFile{1: {f1, f2}},
		episodes: map[int][]sonarr.Episode{1: {
			withFile(1, 1, 1, 11), withFile(1, 2, 2, 12), withFile(1, 3, 3, 12),
			{Season: 1, Number: 4, Absolute: 4},
		}},
	}
	snap, _, err := NewWalker(&Config{Sonarr: onePass{fs}, Logger: discardLogger()}).Walk(t.Context())
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
// episode holding a file with no file id or no episode number leaves the
// whole series' episode map nil: the file may be one a download also covers,
// so counting that file as replaced could drop the episode. The series stays
// comparable and one WARN names it.
func TestWalkSonarrEpisodeHoldingAnUnplacedFileLeavesSizesUnknown(t *testing.T) {
	tests := []struct {
		episode sonarr.Episode
		name    string
	}{
		{name: "a file with no file id", episode: sonarr.Episode{Season: 1, Number: 13, Absolute: 13, HasFile: true}},
		{name: "a file with a negative file id", episode: sonarr.Episode{Season: 1, Number: 13, HasFile: true, FileID: -1}},
		{name: "a file on no episode number", episode: withFile(1, 0, 0, 12)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeSonarr{
				series:   []sonarr.Series{{ID: 1, Title: "Show"}},
				files:    map[int][]sonarr.EpisodeFile{1: {listedFile(12, 1, "PMR")}},
				episodes: map[int][]sonarr.Episode{1: {withFile(1, 12, 12, 12), tc.episode}},
			}
			logger, rec := capture.New()
			snap, _, err := NewWalker(&Config{Sonarr: onePass{fs}, Logger: logger}).Walk(t.Context())
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
		series:      []sonarr.Series{{ID: 1, Title: "Show"}, {ID: 2, Title: "Other"}},
		files:       map[int][]sonarr.EpisodeFile{1: {epFile(1, "PMR")}, 2: {listedFile(5, 1, "PMR")}},
		episodes:    map[int][]sonarr.Episode{2: {withFile(1, 1, 1, 5)}},
		episodesErr: map[int]error{1: errors.New("sonarr down")},
	}
	logger, rec := capture.New()
	snap, _, err := NewWalker(&Config{Sonarr: onePass{fs}, Logger: logger}).Walk(t.Context())
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
	if n := rec.CountExact("sonarr episode list read failed; this series' upgrade sizes and episode listing are unknown"); n != 1 {
		t.Errorf("episode list WARN logged %d times, want 1", n)
	}
}

func TestWalkRadarrRecordsTheMovieFileSize(t *testing.T) {
	fr := &fakeRadarr{movies: []radarr.Movie{{ID: 1, Title: "Film", HasFile: true, File: &radarr.MovieFile{ID: 9, Size: 3000, ReleaseGroup: "PMR"}}}}
	snap, _, err := NewWalker(&Config{Radarr: fr, Logger: discardLogger()}).Walk(t.Context())
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
	series    []sonarr.Series
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

func (f *pairedSonarr) Series(context.Context) ([]sonarr.Series, error) {
	return airedByDefault(f.series), nil
}

func (f *pairedSonarr) EpisodeFiles(ctx context.Context, _ int) ([]sonarr.EpisodeFile, error) {
	if err := f.enter(ctx); err != nil {
		return nil, err
	}
	return []sonarr.EpisodeFile{epFile(1, "PMR")}, nil
}

func (f *pairedSonarr) Episodes(ctx context.Context, _ int) ([]sonarr.Episode, error) {
	return nil, f.enter(ctx)
}

func (*pairedSonarr) Tags(context.Context) ([]arrapi.Tag, error) { return nil, nil }

func TestWalkSonarrEpisodeListReadStaysInsideTheConcurrencyBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := 2*episodeConcurrency + 1
		fs := &pairedSonarr{release: make(chan struct{})}
		for id := 1; id <= n; id++ {
			fs.series = append(fs.series, sonarr.Series{ID: id, Title: "Series"})
		}
		done := make(chan error, 1)
		go func() {
			_, _, err := NewWalker(&Config{Sonarr: onePass{fs}, Logger: discardLogger()}).Walk(t.Context())
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
