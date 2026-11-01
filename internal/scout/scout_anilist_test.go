package scout

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/cplieger/arrapi/v3/sonarr"
	"github.com/cplieger/seadex-scout/internal/arrwalk"
	"github.com/cplieger/seadex-scout/internal/compare"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/match"
	"github.com/cplieger/seadex-scout/internal/notify"
	"github.com/cplieger/seadex-scout/internal/seadex"
	"github.com/cplieger/seadex-scout/internal/state"
	"github.com/cplieger/slogx/capture"
)

const seasonEntryID = 20

// seasonSonarr serves series 7 with two season-1 episodes on files 90 and 91,
// beside a healthy series 8, so a failed file read on 7 leaves a partial walk.
type seasonSonarr struct {
	filesErr error
	fakeSonarr
}

func (f *seasonSonarr) EpisodeFiles(ctx context.Context, seriesID int) ([]sonarr.EpisodeFile, error) {
	if seriesID == 7 && f.filesErr != nil {
		return nil, f.filesErr
	}
	return f.fakeSonarr.EpisodeFiles(ctx, seriesID)
}

func (*seasonSonarr) Episodes(_ context.Context, seriesID int) ([]sonarr.Episode, error) {
	if seriesID != 7 {
		return nil, nil
	}
	return []sonarr.Episode{
		{Season: 1, Number: 1, HasFile: true, FileID: 90},
		{Season: 1, Number: 2, HasFile: true, FileID: 91},
	}, nil
}

func newSeasonSonarr() *seasonSonarr {
	return &seasonSonarr{
		series: []sonarr.Series{
			{ID: 7, Title: "Oz Show", IDs: sonarr.ExternalIDs{TVDB: 500}, Year: 2020},
			{ID: 8, Title: "Other Show", IDs: sonarr.ExternalIDs{TVDB: 800}},
		},
		files: map[int][]sonarr.EpisodeFile{7: {
			{ID: 90, Season: 1, ReleaseGroup: "oz"},
			{ID: 91, Season: 1, ReleaseGroup: "oz"},
		}},
	}
}

// seasonCache maps entry 20 to TVDB season 1 of series 500; placed adds the
// map's placement of it on episode 1 alone, a proper part of the season.
func seasonCache(placed bool) mapping.Cache {
	c := mapping.Cache{
		FetchedAt: time.Now(),
		Records: []mapping.Record{{
			AniListID: seasonEntryID, AniDBID: 71, TvdbID: 500, SeasonTvdb: 1, SeasonKind: mapping.SeasonPresent,
		}},
	}
	if placed {
		c.Mappings = map[int]mapping.Mapping{71: {Episodes: []int{1}, EpisodesTvdb: 500, EpisodesSeason: 1}}
	}
	return c
}

func seasonEntry() seadex.Entry {
	return seadex.Entry{
		AniListID: seasonEntryID,
		Torrents: []seadex.Torrent{{
			ReleaseGroup: "MTBB", Tracker: "Nyaa", InfoHash: "s01", URL: "https://nyaa.si/view/1", IsBest: true,
			Files: []seadex.File{{Name: "Oz Show S01E01 1080p.mkv", Length: 1}},
		}},
	}
}

// TestCycleResolvesAnAniListExcludedFindingWhileFilesAreUnread pins that the
// AniList guard needs no file evidence: once Sonarr's AniList ids for a series
// exclude a season entry, its standing finding resolves even when that pass's
// file read failed, as a non-aired order's does, and the walk still reports
// the failed series. An entry the map places on part of the season keeps its
// finding instead, because the episode listing the failed read lacks decides
// whether the guard applies to it.
func TestCycleResolvesAnAniListExcludedFindingWhileFilesAreUnread(t *testing.T) {
	const withheldMsg = "comparison withheld: Sonarr's AniList ids exclude this entry"
	for _, tc := range []struct {
		name                       string
		placed                     bool
		total, preserved, resolved int64
		withheldRecords            int
	}{
		{name: "season entry", total: 0, preserved: 0, resolved: 1, withheldRecords: 1},
		{name: "entry placed on part of the season", placed: true, total: 1, preserved: 1, resolved: 0, withheldRecords: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logger, recorder := capture.New()
			notifier := notify.NewNotifier(logger, nil)
			fs := newSeasonSonarr()
			s := New(&Deps{
				Logger:       logger,
				Store:        &fakeStore{st: state.State{Mapping: seasonCache(tc.placed)}},
				Library:      arrwalk.NewWalker(&arrwalk.Config{Sonarr: onePass{fs}, Logger: scoutTestLogger()}),
				Mapping:      placedMapping{},
				SeaDex:       &fakeSeaDex{entries: []seadex.Entry{seasonEntry()}},
				Matcher:      match.New(match.Config{AniList: notFoundAniList{}, Logger: scoutTestLogger()}),
				Comparer:     compare.New(compare.Config{Logger: logger}),
				Notifier:     notifier,
				PollInterval: pollIntervalForEvery(1),
			})

			s.Cycle(t.Context())
			if total, _ := lastSummaryCounter(t, recorder, "total"); total != 1 {
				t.Fatalf("Cycle(no AniList ids) findings = %d, want 1 (the fixture must raise the finding first)", total)
			}

			// Code Geass's AniList ids on Sonarr 5, none of them entry 20.
			fs.series[0].IDs.AniList = []int{1575, 2904, 12685, 97880}
			fs.filesErr = errors.New("episode file read failed")
			s.Cycle(t.Context())
			total, _ := lastSummaryCounter(t, recorder, "total")
			preserved, _ := lastSummaryCounter(t, recorder, "preserved")
			resolved, _ := lastSummaryCounter(t, recorder, "resolved")
			if total != tc.total || preserved != tc.preserved || resolved != tc.resolved {
				t.Errorf("Cycle(ids exclude the entry, files unread) total=%d preserved=%d resolved=%d, want %d/%d/%d",
					total, preserved, resolved, tc.total, tc.preserved, tc.resolved)
			}
			if got := recorder.CountExact(withheldMsg); got != tc.withheldRecords {
				t.Errorf("%q records = %d, want %d", withheldMsg, got, tc.withheldRecords)
			}
			if got, want := recorder.AttrValuesExact("cycle degraded", "reason"), []string{"partial-walk"}; !slices.Equal(got, want) {
				t.Errorf("'cycle degraded' reasons = %q, want %q: the failed read is still reported", got, want)
			}
			if got, want := recorder.AttrValuesExact("cycle degraded", "failed_items"), []string{"1"}; !slices.Equal(got, want) {
				t.Errorf("'cycle degraded' failed_items = %q, want %q", got, want)
			}
		})
	}
}
