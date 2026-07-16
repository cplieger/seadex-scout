package scout

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/cplieger/arrapi"
	"github.com/cplieger/seadex-scout/internal/anilist"
	"github.com/cplieger/seadex-scout/internal/compare"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/match"
	"github.com/cplieger/seadex-scout/internal/report"
	"github.com/cplieger/seadex-scout/internal/seadex"
	"github.com/cplieger/seadex-scout/internal/state"
	"github.com/cplieger/slogx/capture"
)

// fakeFeed records FeedWriter.Rebuild calls, optionally failing them.
type fakeFeed struct {
	err     error
	calls   int
	entries int
}

func (f *fakeFeed) Rebuild(_ context.Context, entries []seadex.Entry, _ func(alID int) bool) error {
	f.calls++
	f.entries = len(entries)
	return f.err
}

// TestCycleWalkFailureWithFeedStillRebuildsFeed pins the feed-vs-health split:
// with a Torznab feed configured, a failed arr walk still refreshes the feed
// (it needs only SeaDex + Fribb, so an arr outage must not freeze what the arrs
// grab) while the cycle itself stays unhealthy.
func TestCycleWalkFailureWithFeedStillRebuildsFeed(t *testing.T) {
	logger := scoutTestLogger()
	feed := &fakeFeed{}
	// Seed a fresh mapping cache so the map loads usable within the loader's
	// refresh window: this test pins the walk-failure arm, and an unusable map
	// deliberately keeps the previous feed (see TestCycleUnusableMapSkipsFeedRebuild).
	store := &fakeStore{st: state.State{
		Mapping: mapping.Cache{FetchedAt: time.Now(), Records: []mapping.Record{{AniListID: 154587, Type: "TV", TvdbID: 123, SeasonTvdb: 1}}},
	}}
	s := New(&Deps{
		Logger:  logger,
		Store:   store,
		Library: library.NewWalker(&library.Config{Sonarr: &fakeSonarr{listErr: errors.New("sonarr down")}, Logger: logger}),
		Mapping: mapping.NewLoader(noNetworkClient(), "http://unused.invalid/f.json", filepath.Join(t.TempDir(), "ov.json"), time.Hour, logger),
		SeaDex:  &fakeSeaDex{entries: seadexFrierenEntry()},
		Feed:    feed,
	})

	if healthy := s.Cycle(context.Background()); healthy {
		t.Fatal("Cycle healthy=true, want false when the library walk fails (feed or not)")
	}
	if feed.calls != 1 {
		t.Errorf("feed Rebuild calls = %d, want 1 (an arr outage must not freeze the feed)", feed.calls)
	}
	if feed.entries != 1 {
		t.Errorf("feed rebuilt with %d entries, want the 1 fetched SeaDex entry", feed.entries)
	}
}

// TestCycleSeaDexFailureSkipsFeedRebuild pins the keep-last-good-feed arm: when
// the SeaDex fetch fails there is no snapshot to rebuild from, so Rebuild must
// not run (the previous persisted feed is kept, never replaced with nothing).
func TestCycleSeaDexFailureSkipsFeedRebuild(t *testing.T) {
	logger := scoutTestLogger()
	feed := &fakeFeed{}
	sonarr := &fakeSonarr{series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}}}
	s := New(&Deps{
		Logger:  logger,
		Store:   &fakeStore{},
		Library: library.NewWalker(&library.Config{Sonarr: sonarr, Logger: logger}),
		Mapping: mapping.NewLoader(noNetworkClient(), "http://unused.invalid/f.json", filepath.Join(t.TempDir(), "ov.json"), time.Hour, logger),
		SeaDex:  &fakeSeaDex{err: errors.New("seadex down")},
		Feed:    feed,
	})

	if healthy := s.Cycle(context.Background()); !healthy {
		t.Fatal("Cycle healthy=false, want true (a SeaDex failure is degraded, not unhealthy)")
	}
	if feed.calls != 0 {
		t.Errorf("feed Rebuild calls = %d, want 0 (a failed fetch keeps the previous feed)", feed.calls)
	}
}

// TestCycleUnusableMapSkipsFeedRebuild pins the unusable-map arm of the feed
// gate: a mapping load failure with no usable cached index (NOT a
// mapping.StaleMapError) must keep the previous feed, exactly like a failed
// SeaDex fetch - rebuilding would classify every entry as anime and silently
// drop all SeaDex movies from Radarr's RSS view.
func TestCycleUnusableMapSkipsFeedRebuild(t *testing.T) {
	logger := scoutTestLogger()
	feed := &fakeFeed{}
	sonarr := &fakeSonarr{series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}}}
	s := New(&Deps{
		Logger:  logger,
		Store:   &fakeStore{},
		Library: library.NewWalker(&library.Config{Sonarr: sonarr, Logger: logger}),
		// Empty state + unreachable Fribb: the load fails with nothing stale to
		// fall back on, so the map is unusable (not a StaleMapError).
		Mapping: mapping.NewLoader(noNetworkClient(), "http://unused.invalid/f.json", filepath.Join(t.TempDir(), "ov.json"), time.Hour, logger),
		SeaDex:  &fakeSeaDex{entries: seadexFrierenEntry()},
		Feed:    feed,
	})

	if healthy := s.Cycle(context.Background()); !healthy {
		t.Fatal("Cycle healthy=false, want true (an unusable map is degraded, not unhealthy)")
	}
	if feed.calls != 0 {
		t.Errorf("feed Rebuild calls = %d, want 0 (an unusable map keeps the previous feed)", feed.calls)
	}
}

// TestCycleFeedRebuildErrorIsNonFatal pins the feed-failure isolation: a
// Rebuild error is logged and the compare half of the cycle still completes
// (healthy, state baselined), so a broken feed write can never take down the
// monitoring half.
func TestCycleFeedRebuildErrorIsNonFatal(t *testing.T) {
	logger := scoutTestLogger()
	feed := &fakeFeed{err: errors.New("disk full")}
	store := &fakeStore{st: state.State{
		Mapping: mapping.Cache{FetchedAt: time.Now(), Records: []mapping.Record{{AniListID: 154587, Type: "TV", TvdbID: 123, SeasonTvdb: 1}}},
	}}
	sonarr := &fakeSonarr{
		series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}},
		episodes: map[int][]arrapi.Episode{
			7: {{SeasonNumber: 1, EpisodeFile: &arrapi.EpisodeFile{ReleaseGroup: "Erai-raws"}}},
		},
	}
	s := New(&Deps{
		Logger:   logger,
		Store:    store,
		Library:  library.NewWalker(&library.Config{Sonarr: sonarr, Logger: logger}),
		Mapping:  mapping.NewLoader(noNetworkClient(), "http://unused.invalid/f.json", filepath.Join(t.TempDir(), "ov.json"), time.Hour, logger),
		SeaDex:   &fakeSeaDex{entries: seadexFrierenEntry()},
		Matcher:  match.NewMatcher(notFoundAniList{}, logger),
		Comparer: compare.NewComparer(compare.Config{Logger: logger}),
		Reporter: report.NewReporter(logger),
		AniList:  anilist.NewClient(noNetworkClient(), "http://unused.invalid/gql", 1, logger),
		Feed:     feed,
	})

	if healthy := s.Cycle(context.Background()); !healthy {
		t.Fatal("Cycle healthy=false, want true when only the feed rebuild fails")
	}
	if feed.calls != 1 {
		t.Errorf("feed Rebuild calls = %d, want 1", feed.calls)
	}
	if !store.st.Baselined {
		t.Error("state Baselined=false; the compare half must complete despite a feed rebuild failure")
	}
}

// TestCycleWalkAndSeaDexBothFailWarnsFeedKept pins the multi-dependency-outage
// visibility arm of the pre-compare gate: with a feed configured and the arr
// walk failed, a SeaDex failure (or an empty fetch) that silently kept the
// previous feed must still be surfaced with its own WARN, so a double outage
// does not read as arr-only in Loki.
func TestCycleWalkAndSeaDexBothFailWarnsFeedKept(t *testing.T) {
	tests := []struct {
		name     string
		seadex   *fakeSeaDex
		wantWarn string
	}{
		{
			name:     "seadex fetch fails",
			seadex:   &fakeSeaDex{err: errors.New("seadex down")},
			wantWarn: "seadex fetch failed; indexer feed kept previous feed",
		},
		{
			name:     "seadex returns zero entries",
			seadex:   &fakeSeaDex{},
			wantWarn: "seadex returned zero entries; indexer feed kept previous feed",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logger, recorder := capture.New()
			feed := &fakeFeed{}
			s := New(&Deps{
				Logger:  logger,
				Store:   &fakeStore{},
				Library: library.NewWalker(&library.Config{Sonarr: &fakeSonarr{listErr: errors.New("sonarr down")}, Logger: scoutTestLogger()}),
				Mapping: mapping.NewLoader(noNetworkClient(), "http://unused.invalid/f.json", filepath.Join(t.TempDir(), "ov.json"), time.Hour, scoutTestLogger()),
				SeaDex:  tc.seadex,
				Feed:    feed,
			})

			if healthy := s.Cycle(context.Background()); healthy {
				t.Fatal("Cycle healthy=true, want false when the library walk fails")
			}
			if feed.calls != 0 {
				t.Errorf("feed Rebuild calls = %d, want 0 (nothing to rebuild from)", feed.calls)
			}
			if n := recorder.CountExact(tc.wantWarn); n != 1 {
				t.Errorf("%q count = %d, want 1", tc.wantWarn, n)
			}
		})
	}
}

// probingFeed records what the isMovie closure Cycle hands to Rebuild reports
// for a movie record, a series record, and an id absent from the index.
type probingFeed struct {
	got map[int]bool
}

func (p *probingFeed) Rebuild(_ context.Context, _ []seadex.Entry, isMovie func(alID int) bool) error {
	p.got = map[int]bool{
		100: isMovie(100),
		200: isMovie(200),
		999: isMovie(999),
	}
	return nil
}

// TestCycleFeedIsMovieClosureClassifiesViaFribbIndex pins the one bit of the
// Fribb map the feed writer consumes: the isMovie closure must report true for
// a MOVIE record, false for a TV record, and false for an unmapped id (the
// safe Anime default). A wrong bit silently moves entries between Radarr's
// Movies (2000) and Sonarr's Anime (5070) RSS categories.
func TestCycleFeedIsMovieClosureClassifiesViaFribbIndex(t *testing.T) {
	logger := scoutTestLogger()
	feed := &probingFeed{}
	// A fresh cache (within the refresh window) makes the loader reuse the
	// records without fetching, so the index is exactly these two records.
	store := &fakeStore{st: state.State{
		Mapping: mapping.Cache{FetchedAt: time.Now(), Records: []mapping.Record{
			{AniListID: 100, Type: "MOVIE"},
			{AniListID: 200, Type: "TV", TvdbID: 123},
		}},
	}}
	s := New(&Deps{
		Logger:  logger,
		Store:   store,
		Library: library.NewWalker(&library.Config{Sonarr: &fakeSonarr{listErr: errors.New("sonarr down")}, Logger: logger}),
		Mapping: mapping.NewLoader(noNetworkClient(), "http://unused.invalid/f.json", filepath.Join(t.TempDir(), "ov.json"), time.Hour, logger),
		SeaDex:  &fakeSeaDex{entries: seadexFrierenEntry()},
		Feed:    feed,
	})

	if healthy := s.Cycle(context.Background()); healthy {
		t.Fatal("Cycle healthy=true, want false (the walk failed; only the feed refreshes)")
	}
	if feed.got == nil {
		t.Fatal("feed Rebuild was not called")
	}
	for id, wantMovie := range map[int]bool{100: true, 200: false, 999: false} {
		if feed.got[id] != wantMovie {
			t.Errorf("isMovie(%d) = %v, want %v", id, feed.got[id], wantMovie)
		}
	}
}

// TestCycleWalkFailShutdownDuringSeaDexFetchStaysSilent pins the shutdown arm
// of logFeedOutageOnWalkFail: when the arr walk genuinely failed but the
// SeaDex "failure" is the cycle context being cancelled mid-fetch (a
// redeploy), the feed-kept WARN must NOT fire - blaming SeaDex would
// misattribute a routine shutdown to an upstream outage (the genuine double
// outage keeps its WARN via TestCycleWalkAndSeaDexBothFailWarnsFeedKept).
func TestCycleWalkFailShutdownDuringSeaDexFetchStaysSilent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger, recorder := capture.New()
	feed := &fakeFeed{}
	// A fresh mapping cache keeps the loader off the network, so the only
	// failure ordering exercised is walk-failed then fetch-cancelled.
	store := &fakeStore{st: state.State{
		Mapping: mapping.Cache{FetchedAt: time.Now(), Records: []mapping.Record{{AniListID: 154587, Type: "TV", TvdbID: 123, SeasonTvdb: 1}}},
	}}
	s := New(&Deps{
		Logger:  logger,
		Store:   store,
		Library: library.NewWalker(&library.Config{Sonarr: &fakeSonarr{listErr: errors.New("sonarr down")}, Logger: scoutTestLogger()}),
		Mapping: mapping.NewLoader(noNetworkClient(), "http://unused.invalid/f.json", filepath.Join(t.TempDir(), "ov.json"), time.Hour, scoutTestLogger()),
		SeaDex:  &cancellingSeaDex{cancel: cancel},
		Feed:    feed,
	})

	if healthy := s.Cycle(ctx); healthy {
		t.Fatal("Cycle healthy=true, want false (the walk genuinely failed)")
	}
	if feed.calls != 0 {
		t.Errorf("feed Rebuild calls = %d, want 0 (nothing to rebuild from)", feed.calls)
	}
	if n := recorder.CountExact("seadex fetch failed; indexer feed kept previous feed"); n != 0 {
		t.Errorf("feed-kept WARN fired %d times during a shutdown, want 0", n)
	}
}
