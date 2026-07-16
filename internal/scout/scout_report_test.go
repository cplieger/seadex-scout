package scout

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/arrapi"
	"github.com/cplieger/seadex-scout/internal/audit"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/match"
	"github.com/cplieger/seadex-scout/internal/state"
)

// TestReportGeneratesRowsAndNeverWritesState pins the one-shot report path: a
// successful run produces at least the matched row, and the state store sees
// no Save afterwards (the report is read-only on state, so it is safe to run
// beside a daemon cycle).
func TestReportGeneratesRowsAndNeverWritesState(t *testing.T) {
	logger := scoutTestLogger()
	store := &fakeStore{st: state.State{
		Mapping:   mapping.Cache{FetchedAt: time.Now(), Records: []mapping.Record{{AniListID: 154587, Type: "TV", TvdbID: 123, SeasonTvdb: 1}}},
		Baselined: true,
	}}

	sonarr := &fakeSonarr{
		series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}},
		episodes: map[int][]arrapi.Episode{
			7: {{SeasonNumber: 1, EpisodeFile: &arrapi.EpisodeFile{ReleaseGroup: "Erai-raws"}}},
		},
	}
	s := New(&Deps{
		Logger:  logger,
		Store:   store,
		Library: library.NewWalker(&library.Config{Sonarr: sonarr, Logger: logger}),
		Mapping: mapping.NewLoader(noNetworkClient(), "http://unused.invalid/f.json", filepath.Join(t.TempDir(), "ov.json"), time.Hour, logger),
		SeaDex:  &fakeSeaDex{entries: seadexFrierenEntry()},
		Matcher: match.NewMatcher(notFoundAniList{}, logger),
		Auditor: audit.NewAuditor(audit.Config{Logger: logger, SeaDexBaseURL: "https://releases.moe"}),
	})

	rep, err := s.Report(context.Background())
	if err != nil {
		t.Fatalf("Report returned error: %v", err)
	}
	if len(rep.Rows) == 0 {
		t.Fatal("Report produced 0 rows, want at least the matched Frieren row")
	}
	found := false
	for i := range rep.Rows {
		if rep.Rows[i].AniListID == 154587 {
			found = true
		}
	}
	if !found {
		t.Errorf("no row for AniList 154587 in %d rows", len(rep.Rows))
	}

	if store.saves != 0 {
		t.Errorf("Report saved state %d times; the one-shot report must be read-only on state", store.saves)
	}
}

// flakySonarr wraps fakeSonarr but fails GetEpisodes for the listed series
// IDs, so a walk succeeds while marking the snapshot partial.
type flakySonarr struct {
	failEpisodes map[int]bool
	fakeSonarr
}

func (f *flakySonarr) GetEpisodes(ctx context.Context, seriesID int) ([]arrapi.Episode, error) {
	if f.failEpisodes[seriesID] {
		return nil, errors.New("episode fetch failed")
	}
	return f.fakeSonarr.GetEpisodes(ctx, seriesID)
}

// TestReportPartialSnapshotErrors pins Report's completeness gate: a walk that
// skipped series after episode-fetch failures (Partial=true, nil error) must
// fail the one-shot report rather than publish a successful, timestamped audit
// that silently omits the skipped series - the whole-library contract the
// daemon cycle already enforces via its partial-snapshot gate.
func TestReportPartialSnapshotErrors(t *testing.T) {
	logger := scoutTestLogger()
	sonarr := &flakySonarr{
		fakeSonarr: fakeSonarr{
			series: []arrapi.Series{
				{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023},
				{ID: 8, Title: "Skipped Series", TvdbID: 124, Year: 2024},
			},
			episodes: map[int][]arrapi.Episode{
				7: {{SeasonNumber: 1, EpisodeFile: &arrapi.EpisodeFile{ReleaseGroup: "Erai-raws"}}},
			},
		},
		failEpisodes: map[int]bool{8: true},
	}
	s := New(&Deps{
		Logger:  logger,
		Store:   &fakeStore{},
		Library: library.NewWalker(&library.Config{Sonarr: sonarr, Logger: logger}),
	})

	_, err := s.Report(context.Background())
	if err == nil {
		t.Fatal("Report returned nil error, want a partial-snapshot error")
	}
	if !strings.Contains(err.Error(), "partial") {
		t.Errorf("error = %q, want partial-snapshot context", err.Error())
	}
}

// TestReportLibraryWalkFailureErrors pins Report's first error arm: a failed
// arr walk aborts the report with an error naming the walk (there is nothing
// to report against).
func TestReportLibraryWalkFailureErrors(t *testing.T) {
	logger := scoutTestLogger()
	s := New(&Deps{
		Logger:  logger,
		Store:   &fakeStore{},
		Library: library.NewWalker(&library.Config{Sonarr: &fakeSonarr{listErr: errors.New("sonarr down")}, Logger: logger}),
	})

	_, err := s.Report(context.Background())
	if err == nil {
		t.Fatal("Report returned nil error, want a library-walk error")
	}
	if !strings.Contains(err.Error(), "library walk") {
		t.Errorf("error = %q, want library-walk context", err.Error())
	}
}

// TestReportZeroSeaDexEntriesErrors pins Report's defense-in-depth zero-entry
// gate: seadex.FetchEntries errors on an empty completed catalogue, but a
// future client regression returning (nil, nil) must still fail the one-shot
// report (no report files) rather than publish a successful audit marking
// every library item not_on_seadex - mirroring Cycle's zero-entries
// degradation gate.
func TestReportZeroSeaDexEntriesErrors(t *testing.T) {
	logger := scoutTestLogger()
	sonarr := &fakeSonarr{series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}}}
	s := New(&Deps{
		Logger: logger,
		Store: &fakeStore{st: state.State{
			Mapping: mapping.Cache{FetchedAt: time.Now(), Records: []mapping.Record{{AniListID: 154587, Type: "TV", TvdbID: 123, SeasonTvdb: 1}}},
		}},
		Library: library.NewWalker(&library.Config{Sonarr: sonarr, Logger: logger}),
		Mapping: mapping.NewLoader(noNetworkClient(), "http://unused.invalid/f.json", filepath.Join(t.TempDir(), "ov.json"), time.Hour, logger),
		SeaDex:  &fakeSeaDex{},
	})

	_, err := s.Report(context.Background())
	if err == nil {
		t.Fatal("Report returned nil error, want a zero-entries error")
	}
	if !strings.Contains(err.Error(), "zero entries") {
		t.Errorf("error = %q, want zero-entries context", err.Error())
	}
}

// TestReportSeaDexFailureErrors pins Report's second error arm: unlike the
// daemon cycle (degraded-but-healthy), a one-shot report with no SeaDex data
// has nothing to compare, so a failed fetch is a hard error naming the fetch.
func TestReportSeaDexFailureErrors(t *testing.T) {
	logger := scoutTestLogger()
	sonarr := &fakeSonarr{series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}}}
	s := New(&Deps{
		Logger: logger,
		// A cached mapping keeps the loader usable so the report reaches the
		// SeaDex arm (an unusable map is its own hard error, gated earlier).
		Store: &fakeStore{st: state.State{
			Mapping: mapping.Cache{FetchedAt: time.Now(), Records: []mapping.Record{{AniListID: 154587, Type: "TV", TvdbID: 123, SeasonTvdb: 1}}},
		}},
		Library: library.NewWalker(&library.Config{Sonarr: sonarr, Logger: logger}),
		Mapping: mapping.NewLoader(noNetworkClient(), "http://unused.invalid/f.json", filepath.Join(t.TempDir(), "ov.json"), time.Hour, logger),
		SeaDex:  &fakeSeaDex{err: errors.New("seadex down")},
	})

	_, err := s.Report(context.Background())
	if err == nil {
		t.Fatal("Report returned nil error, want a seadex fetch error")
	}
	if !strings.Contains(err.Error(), "seadex fetch") {
		t.Errorf("error = %q, want seadex-fetch context", err.Error())
	}
}
