package scout

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/cplieger/arrapi"
	"github.com/cplieger/seadex-scout/internal/compare"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/report"
	"github.com/cplieger/seadex-scout/internal/seadex"
	"github.com/cplieger/seadex-scout/internal/state"
)

type fakeSonarr struct {
	episodes map[int][]arrapi.Episode
	listErr  error
	series   []arrapi.Series
}

func (f *fakeSonarr) GetSeries(context.Context) ([]arrapi.Series, error) {
	return f.series, f.listErr
}

func (f *fakeSonarr) GetEpisodes(_ context.Context, seriesID int) ([]arrapi.Episode, error) {
	return f.episodes[seriesID], nil
}

func (f *fakeSonarr) ResolveTagIDs(context.Context, ...string) (map[int]struct{}, []string, error) {
	return nil, nil, nil
}

func scoutTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestCycleLibraryWalkFailureIsUnhealthy(t *testing.T) {
	logger := scoutTestLogger()
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"), logger)
	s := New(&Deps{
		Logger:  logger,
		Store:   store,
		Library: library.NewWalker(&library.Config{Sonarr: &fakeSonarr{listErr: errors.New("sonarr down")}, Logger: logger}),
	})

	if healthy := s.Cycle(context.Background()); healthy {
		t.Fatal("Cycle returned healthy=true, want false when the library walk fails")
	}
}

func TestCycleSeaDexFailureIsHealthyAndPreservesFindings(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("{"))
	}))
	defer server.Close()
	logger := scoutTestLogger()
	statePath := filepath.Join(t.TempDir(), "state.json")
	store := state.NewStore(statePath, logger)
	prior := report.Alerted{
		AlertedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Finding: compare.Finding{
			Title:     "Existing finding",
			DedupeKey: "prior",
			Status:    compare.StatusBetter,
			AniListID: 154587,
		},
	}
	initial := &state.State{
		Mapping: mapping.Cache{
			FetchedAt: time.Now(),
			Records:   []mapping.Record{{AniListID: 154587, Type: "TV", TvdbID: 123, SeasonTvdb: 1}},
		},
		Findings:  map[string]report.Alerted{"prior": prior},
		Baselined: true,
	}
	if err := store.Save(context.Background(), initial); err != nil {
		t.Fatalf("seed state: %v", err)
	}
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
		Mapping: mapping.NewLoader(server.Client(), "http://unused.invalid/fribb.json", filepath.Join(t.TempDir(), "overrides.json"), time.Hour, logger),
		SeaDex:  seadex.NewClient(server.Client(), server.URL, 0, logger),
	})

	if healthy := s.Cycle(context.Background()); !healthy {
		t.Fatal("Cycle returned healthy=false, want true for degraded SeaDex failure")
	}
	loaded, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("load saved state: %v", err)
	}
	if len(loaded.Library.Items) != 1 || loaded.Library.Items[0].Title != "Frieren" {
		t.Errorf("library snapshot after degraded cycle = %+v, want refreshed Frieren snapshot", loaded.Library)
	}
	got, ok := loaded.Findings["prior"]
	if !ok {
		t.Fatalf("prior finding was not preserved: %+v", loaded.Findings)
	}
	if got.Finding.Title != prior.Finding.Title || !got.AlertedAt.Equal(prior.AlertedAt) {
		t.Errorf("preserved finding = %+v, want %+v", got, prior)
	}
}
