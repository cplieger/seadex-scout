package scout

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/cplieger/arrapi/v3/sonarr"
	"github.com/cplieger/seadex-scout/internal/arrwalk"
	"github.com/cplieger/seadex-scout/internal/compare"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/match"
	"github.com/cplieger/seadex-scout/internal/notify"
	"github.com/cplieger/seadex-scout/internal/seadex"
	"github.com/cplieger/seadex-scout/internal/state"
	"github.com/cplieger/slogx/capture"
)

type specialsSonarr struct {
	episodesErr error
	fakeSonarr
	special sonarr.Episode
}

func (f *specialsSonarr) Episodes(context.Context, int) ([]sonarr.Episode, error) {
	if f.episodesErr != nil {
		return nil, f.episodesErr
	}
	return []sonarr.Episode{f.special}, nil
}

type placedMapping struct{}

func (placedMapping) Load(_ context.Context, prev *mapping.Cache) (mapping.Cache, *mapping.Index, error) {
	return *prev, mapping.NewIndex(mapping.Source{Records: prev.Records, Mappings: prev.Mappings}), nil
}

const (
	placedOVAID     = 5
	placedOVAFileID = 90
)

func placedOVACache() mapping.Cache {
	return mapping.Cache{
		FetchedAt: time.Now(),
		Records: []mapping.Record{{
			AniListID: placedOVAID, AniDBID: 70, Type: "OVA", TvdbID: 500, SeasonKind: mapping.SeasonPresent,
		}},
		Mappings: map[int]mapping.Mapping{70: {Specials: []int{9}, SpecialsTvdb: 500}},
	}
}

func placedOVAEntry() seadex.Entry {
	return seadex.Entry{
		AniListID: placedOVAID,
		Torrents: []seadex.Torrent{{
			ReleaseGroup: "MTBB",
			Tracker:      "Nyaa",
			InfoHash:     "s09",
			URL:          "https://nyaa.si/view/9",
			IsBest:       true,
			Files:        []seadex.File{{Name: "Oz Show S00E09 1080p.mkv", Length: 1}},
		}},
	}
}

func placedOVASonarr() *specialsSonarr {
	return &specialsSonarr{
		series:  []sonarr.Series{{ID: 7, Title: "Oz Show", IDs: sonarr.ExternalIDs{TVDB: 500}, Year: 2020}},
		files:   map[int][]sonarr.EpisodeFile{7: {{ID: placedOVAFileID, Season: 0, ReleaseGroup: "oz"}}},
		special: sonarr.Episode{Season: 0, Number: 9, HasFile: true, FileID: placedOVAFileID},
	}
}

func placedOVADeps(logger *slog.Logger, fs *specialsSonarr, notifier *notify.Notifier, sea *fakeSeaDex, every int) *Deps {
	return &Deps{
		Logger:       logger,
		Store:        &fakeStore{st: state.State{Mapping: placedOVACache()}},
		Library:      arrwalk.NewWalker(&arrwalk.Config{Sonarr: onePass{fs}, Logger: scoutTestLogger()}),
		Mapping:      placedMapping{},
		SeaDex:       sea,
		Matcher:      match.New(notFoundAniList{}, scoutTestLogger()),
		Comparer:     compare.New(compare.Config{}),
		Notifier:     notifier,
		PollInterval: pollIntervalForEvery(every),
	}
}

func TestCycleKeepsAPlacedSpecialFindingWhileSeasonZeroIsUnread(t *testing.T) {
	logger, recorder := capture.New()
	notifier := notify.NewNotifier(logger, nil)
	fs := placedOVASonarr()
	s := New(placedOVADeps(logger, fs, notifier, &fakeSeaDex{entries: []seadex.Entry{placedOVAEntry()}}, 1))

	if healthy := s.Cycle(t.Context()); !healthy {
		t.Fatal("Cycle(season 0 read) healthy=false, want true")
	}
	if total, _ := lastSummaryCounter(t, recorder, "total"); total != 1 {
		t.Fatalf("Cycle(season 0 read) findings = %d, want 1 (the fixture must raise the special's finding first)", total)
	}

	fs.episodesErr = errors.New("episode list read failed")
	if healthy := s.Cycle(t.Context()); !healthy {
		t.Fatal("Cycle(season 0 read failed) healthy=false, want true")
	}
	total, _ := lastSummaryCounter(t, recorder, "total")
	preserved, _ := lastSummaryCounter(t, recorder, "preserved")
	resolved, _ := lastSummaryCounter(t, recorder, "resolved")
	if total != 1 || preserved != 1 || resolved != 0 {
		t.Errorf("Cycle(season 0 read failed) total=%d preserved=%d resolved=%d, want 1/1/0", total, preserved, resolved)
	}

	fs.episodesErr = nil
	fs.files = map[int][]sonarr.EpisodeFile{7: {{Season: 1, ReleaseGroup: "oz"}}}
	fs.special = sonarr.Episode{Season: 0, Number: 9}
	if healthy := s.Cycle(t.Context()); !healthy {
		t.Fatal("Cycle(no season-0 file) healthy=false, want true")
	}
	total, _ = lastSummaryCounter(t, recorder, "total")
	preserved, _ = lastSummaryCounter(t, recorder, "preserved")
	resolved, _ = lastSummaryCounter(t, recorder, "resolved")
	if total != 0 || preserved != 0 || resolved != 1 {
		t.Errorf("Cycle(no season-0 file) total=%d preserved=%d resolved=%d, want 0/0/1: a series known to hold no season-0 file is evidence", total, preserved, resolved)
	}
	if got, want := recorder.AttrValuesExact("cycle complete", "specials_unread"), []string{"0", "1", "0"}; !slices.Equal(got, want) {
		t.Errorf("'cycle complete' specials_unread over read, failed, no season-0 file = %q, want %q", got, want)
	}
}

func TestCycleSizesAPlacedSpecialByItsDownloadOnly(t *testing.T) {
	logger, recorder := capture.New()
	s := New(placedOVADeps(logger, placedOVASonarr(), notify.NewNotifier(logger, nil), &fakeSeaDex{entries: []seadex.Entry{placedOVAEntry()}}, 1))
	if healthy := s.Cycle(t.Context()); !healthy {
		t.Fatal("Cycle healthy=false, want true")
	}
	const finding = "better release available"
	if got, ok := recorder.AttrValueExact(finding, "recommended_bytes"); !ok || got != "1" {
		t.Errorf("%q recommended_bytes = %q, %v, want \"1\": the special's download size is known", finding, got, ok)
	}
	if got, ok := recorder.AttrValueExact(finding, "size_change_bytes"); ok {
		t.Errorf("%q size_change_bytes = %q, want absent: the files a season-0 special replaces are unknown", finding, got)
	}
}

func TestTickKeepsAPlacedSpecialFindingWhileSeasonZeroIsUnread(t *testing.T) {
	logger, recorder := capture.New()
	notifier := notify.NewNotifier(logger, nil)
	fs := placedOVASonarr()
	fs.episodesErr = errors.New("episode list read failed")
	sea := &fakeSeaDex{entries: []seadex.Entry{placedOVAEntry()}, windowEntries: []seadex.Entry{placedOVAEntry()}}
	s := New(placedOVADeps(logger, fs, notifier, sea, 96))

	if healthy := s.Cycle(t.Context()); !healthy {
		t.Fatal("reconcile healthy=false, want true")
	}
	finding := compare.Finding{
		Status: compare.StatusBetter, Kind: "encode", Arr: "sonarr", Title: "Oz Show", AniListID: placedOVAID,
		Tracker: "Nyaa", CurrentGroup: "oz", RecommendedGroup: "MTBB", ReleaseURL: "https://nyaa.si/view/9",
	}
	notifier.Report([]compare.Finding{finding}, nil)

	if healthy := s.Cycle(t.Context()); !healthy {
		t.Fatal("tick healthy=false, want true")
	}
	if n := recorder.CountExact("tick complete"); n != 1 {
		t.Fatalf("'tick complete' count = %d, want 1 (the second Cycle must be the tick)", n)
	}
	total, _ := lastSummaryCounter(t, recorder, "total")
	preserved, _ := lastSummaryCounter(t, recorder, "preserved")
	if total != 1 || preserved != 1 {
		t.Errorf("tick(season 0 unread) total=%d preserved=%d, want 1/1", total, preserved)
	}
}

func TestPreservedAddsOnlyPlacedEntriesOnAnUnreadSeries(t *testing.T) {
	unread := library.Item{Arr: library.ArrSonarr, SeasonGroups: map[int][]string{0: {"oz"}}}
	read := unread
	read.Specials = map[int]library.ListedEpisode{9: {Group: "oz", HasFile: true}}
	matches := []match.Match{
		{Entry: seadex.Entry{AniListID: 1}, Item: &unread, Specials: []int{9}},
		{Entry: seadex.Entry{AniListID: 2}, Item: &unread},
		{Entry: seadex.Entry{AniListID: 3}, Item: &read, Specials: []int{9}},
		{Entry: seadex.Entry{AniListID: 4}, Specials: []int{9}},
	}
	failed := notify.Preserve{{Arr: library.ArrRadarr, AniListID: 7}: {}}
	got := preserved(failed, &match.Result{Matches: matches, IncompleteIDs: map[int]struct{}{8: {}}})
	want := notify.Preserve{
		{Arr: library.ArrSonarr, AniListID: 1}: {},
		{Arr: library.ArrRadarr, AniListID: 7}: {},
		{AniListID: 8}:                         {},
	}
	if !maps.Equal(got, want) {
		t.Errorf("preserved(failed radarr 7, incomplete 8, matches 1-4) = %v, want %v", got, want)
	}
}

func copyFindings() []compare.Finding {
	sonarrCopy := compare.Finding{
		Status: compare.StatusBetter, Arr: library.ArrSonarr, Title: "Oz Show", AniListID: placedOVAID,
		RecommendedGroup: "MTBB", ReleaseURL: "https://nyaa.si/view/9",
	}
	radarrCopy := sonarrCopy
	radarrCopy.Arr = library.ArrRadarr
	return []compare.Finding{sonarrCopy, radarrCopy}
}

func TestPreservedKeepsOnlyTheArrCopyWithoutEvidence(t *testing.T) {
	unreadSonarr := library.Item{Arr: library.ArrSonarr, SeasonGroups: map[int][]string{0: {"oz"}}}
	readSonarr := unreadSonarr
	readSonarr.Specials = map[int]library.ListedEpisode{9: {Group: "oz", HasFile: true}}
	filelessRadarr := library.Item{Arr: library.ArrRadarr}
	failedRadarr := library.Item{Arr: library.ArrRadarr, Failed: true}
	entry := seadex.Entry{AniListID: placedOVAID}
	cases := []struct {
		name    string
		matches []match.Match
		want    string
	}{
		{
			name: "sonarr_unread_radarr_fileless",
			matches: []match.Match{
				{Entry: entry, Item: &unreadSonarr, Arr: library.ArrSonarr, Specials: []int{9}},
				{Entry: entry, Item: &filelessRadarr, Arr: library.ArrRadarr, Specials: []int{9}},
			},
			want: library.ArrSonarr,
		},
		{
			name: "radarr_failed_sonarr_read",
			matches: []match.Match{
				{Entry: entry, Item: &readSonarr, Arr: library.ArrSonarr, Specials: []int{9}},
				{Entry: entry, Item: &failedRadarr, Arr: library.ArrRadarr, Specials: []int{9}},
			},
			want: library.ArrRadarr,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logger, recorder := capture.New()
			notifier := notify.NewNotifier(logger, nil)
			notifier.Report(copyFindings(), nil)
			result := &match.Result{Matches: tc.matches}
			_, failedItems := splitFailedMatches(result.Matches)
			notifier.Report(nil, preserved(failedItems, result))
			arrs := findingArrs(recorder)
			if len(arrs) != 3 || arrs[2] != tc.want {
				t.Errorf("finding arrs over both passes = %q, want both copies then only %q", arrs, tc.want)
			}
		})
	}
}

// orderSonarr is placedOVASonarr whose series 7 can switch to a non-aired
// order or fail its episode-file read, beside a healthy second series so a
// failed read leaves a partial walk rather than a failed one.
type orderSonarr struct {
	filesErr error
	*specialsSonarr
}

func (f *orderSonarr) EpisodeFiles(ctx context.Context, seriesID int) ([]sonarr.EpisodeFile, error) {
	if seriesID == 7 && f.filesErr != nil {
		return nil, f.filesErr
	}
	return f.specialsSonarr.EpisodeFiles(ctx, seriesID)
}

// TestCycleResolvesAFindingOnceTheSeriesUsesANonAiredOrder pins the
// resolution rule: a finding raised while the series was in aired order
// resolves at the first reconcile that reads it in another order, because that
// order alone withholds every comparison, so neither an unread season 0 nor a
// failed file read (which keep an aired series' findings) preserves it.
func TestCycleResolvesAFindingOnceTheSeriesUsesANonAiredOrder(t *testing.T) {
	for _, tc := range []struct {
		name      string
		breakRead func(*orderSonarr)
	}{
		{name: "files read", breakRead: func(*orderSonarr) {}},
		{name: "season 0 unread", breakRead: func(f *orderSonarr) { f.episodesErr = errors.New("episode list read failed") }},
		{name: "episode files unread", breakRead: func(f *orderSonarr) { f.filesErr = errors.New("episode file read failed") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logger, recorder := capture.New()
			notifier := notify.NewNotifier(logger, nil)
			fs := &orderSonarr{specialsSonarr: placedOVASonarr()}
			fs.series = append(fs.series, sonarr.Series{ID: 8, Title: "Other Show", IDs: sonarr.ExternalIDs{TVDB: 800}})
			deps := placedOVADeps(logger, fs.specialsSonarr, notifier, &fakeSeaDex{entries: []seadex.Entry{placedOVAEntry()}}, 1)
			deps.Library = arrwalk.NewWalker(&arrwalk.Config{Sonarr: onePass{fs}, Logger: scoutTestLogger()})
			s := New(deps)

			s.Cycle(t.Context())
			if total, _ := lastSummaryCounter(t, recorder, "total"); total != 1 {
				t.Fatalf("Cycle(aired order) findings = %d, want 1 (the fixture must raise the finding first)", total)
			}

			fs.series[0].Order = sonarr.Order{Type: "alternate", Name: "Book Order"}
			tc.breakRead(fs)
			s.Cycle(t.Context())
			total, _ := lastSummaryCounter(t, recorder, "total")
			preserved, _ := lastSummaryCounter(t, recorder, "preserved")
			resolved, _ := lastSummaryCounter(t, recorder, "resolved")
			if total != 0 || preserved != 0 || resolved != 1 {
				t.Errorf("Cycle(non-aired order) total=%d preserved=%d resolved=%d, want 0/0/1", total, preserved, resolved)
			}
		})
	}
}
