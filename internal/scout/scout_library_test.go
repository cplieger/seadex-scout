package scout

import (
	"testing"
	"time"

	"github.com/cplieger/arrapi/v2"
	"github.com/cplieger/seadex-scout/internal/arrwalk"
	"github.com/cplieger/seadex-scout/internal/audit"
	"github.com/cplieger/seadex-scout/internal/compare"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/match"
	"github.com/cplieger/seadex-scout/internal/notify"
	"github.com/cplieger/seadex-scout/internal/seadex"
	"github.com/cplieger/seadex-scout/internal/state"
	"github.com/cplieger/slogx/capture"
)

// librarySeasonTwoEntry is a season-2 SeaDex entry for the Frieren series,
// which the library holds no file for, so the audit reads it as no_file.
func librarySeasonTwoEntry() seadex.Entry {
	return seadex.Entry{AniListID: 333, Torrents: []seadex.Torrent{{
		ReleaseGroup: "SubsPlease", Tracker: "Nyaa", URL: "https://nyaa.si/view/3", IsBest: true,
		Files: []seadex.File{{Name: "Frieren S02E01 1080p.mkv", Length: 1}},
	}}}
}

// libraryScout builds a scout over one Frieren series holding Erai-raws for
// season 1, against the Frieren entry plus librarySeasonTwoEntry.
func libraryScout(t *testing.T, sonarr arrwalk.SonarrClient, st *state.State) (*Scout, *capture.Recorder) {
	t.Helper()
	logger, recorder := capture.New()
	st.Mapping = mapping.Cache{FetchedAt: time.Now(), Records: []mapping.Record{
		{AniListID: 154587, Type: "TV", TvdbID: 123, SeasonTvdb: 1},
		{AniListID: 333, Type: "TV", TvdbID: 123, SeasonTvdb: 2},
	}}
	return New(&Deps{
		Logger:   logger,
		Store:    &fakeStore{st: *st},
		Library:  arrwalk.NewWalker(&arrwalk.Config{Sonarr: sonarr, Logger: scoutTestLogger()}),
		Mapping:  fakeMapping{},
		SeaDex:   &fakeSeaDex{entries: append(seadexFrierenEntry(), librarySeasonTwoEntry())},
		Matcher:  match.New(notFoundAniList{}, scoutTestLogger()),
		Comparer: compare.New(compare.Config{}),
		Notifier: notify.NewNotifier(logger, nil),
		Auditor:  audit.New(audit.Config{}),
	}), recorder
}

func frierenSonarr() *fakeSonarr {
	return &fakeSonarr{
		series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}},
		files:  map[int][]arrapi.EpisodeFile{7: {{SeasonNumber: 1, ReleaseGroup: "Erai-raws"}}},
	}
}

func TestReconcileLogsTheLibraryOnACleanWalk(t *testing.T) {
	s, rec := libraryScout(t, frierenSonarr(), &state.State{})
	if !s.Cycle(t.Context()) {
		t.Fatal("Cycle healthy=false, want true")
	}
	if n := rec.CountExact("library summary"); n != 1 {
		t.Fatalf("library summary count = %d, want 1", n)
	}
	for key, want := range map[string]string{
		"rows": "2", "have_unlisted": "1", "no_file": "1", "anime_items": "1",
		"items_with_entry": "1", "items_all_best": "0", "items_all_best_or_alt": "0",
	} {
		if got, _ := rec.AttrValue("library summary", key); got != want {
			t.Errorf("library summary %s = %q, want %q", key, got, want)
		}
	}
}

func TestReconcileWithholdsTheLibraryOnAnIncompleteWalk(t *testing.T) {
	partial := &flakySonarr{
		series: []arrapi.Series{
			{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023},
			{ID: 8, Title: "Broken Series", TvdbID: 124, Year: 2024},
		},
		files:        map[int][]arrapi.EpisodeFile{7: {{SeasonNumber: 1, ReleaseGroup: "Erai-raws"}}},
		failEpisodes: map[int]bool{8: true},
	}
	// The guard carries the suspect side's PRIOR items, so the prior Frieren is
	// what the compare reads.
	shrunkPrior := &state.State{Library: library.Snapshot{Items: []library.Item{
		{
			Arr: library.ArrSonarr, ArrID: 7, Title: "Frieren", TvdbID: 123, HasFile: true,
			Groups: []string{"erai-raws"}, SeasonGroups: map[int][]string{1: {"erai-raws"}},
		},
		{Arr: library.ArrSonarr, ArrID: 1, Title: "A"},
		{Arr: library.ArrSonarr, ArrID: 2, Title: "B"},
	}}}
	for name, tc := range map[string]struct {
		sonarr arrwalk.SonarrClient
		prior  *state.State
		reason string
	}{
		"partial walk":  {sonarr: partial, prior: &state.State{}, reason: "partial-walk"},
		"shrunken walk": {sonarr: frierenSonarr(), prior: shrunkPrior, reason: "library-shrunk"},
	} {
		t.Run(name, func(t *testing.T) {
			s, rec := libraryScout(t, tc.sonarr, tc.prior)
			if !s.Cycle(t.Context()) {
				t.Fatal("Cycle healthy=false, want true")
			}
			if reasons := degradedReasons(rec); len(reasons) != 1 || reasons[0] != tc.reason {
				t.Fatalf("degraded reasons = %v, want [%s]: the fixture no longer builds the case it names", reasons, tc.reason)
			}
			if rec.CountExact("better release available") == 0 {
				t.Fatal("no finding emitted, want the compare to have run")
			}
			if n := rec.CountExact("library summary"); n != 0 {
				t.Errorf("library summary count = %d, want 0 on an incomplete walk", n)
			}
		})
	}
}
