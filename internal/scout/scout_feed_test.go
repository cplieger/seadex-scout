package scout

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/cplieger/arrapi"
	"github.com/cplieger/seadex-scout/internal/anilist"
	"github.com/cplieger/seadex-scout/internal/compare"
	"github.com/cplieger/seadex-scout/internal/indexer"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/match"
	"github.com/cplieger/seadex-scout/internal/notify"
	"github.com/cplieger/seadex-scout/internal/seadex"
	"github.com/cplieger/seadex-scout/internal/state"
	"github.com/cplieger/slogx/capture"
)

// TestCycleWalkFailureWithFeedStillRebuildsFeed pins the feed-vs-health split:
// with a Torznab feed configured, a failed arr walk still refreshes the feed
// (it needs only SeaDex + Fribb, so an arr outage must not freeze what the arrs
// grab) while the cycle itself stays unhealthy - logging the walk-failure
// ERROR and still closing with exactly one "cycle degraded" completion line
// (reason walk-failed) so the cycle deadman stays fed through an arr outage.
func TestCycleWalkFailureWithFeedStillRebuildsFeed(t *testing.T) {
	logger, recorder := capture.New()
	feed := &fakeFeed{}
	// Seed a fresh mapping cache so the map loads usable within the loader's
	// refresh window: this test pins the walk-failure arm, and an unusable map
	// deliberately keeps the previous feed (see TestCycleUnusableMapSkipsFeedRebuild).
	store := &fakeStore{st: state.State{
		Mapping: frierenMappingCache(),
	}}
	s := New(&Deps{
		Logger:  logger,
		Store:   store,
		Library: library.NewWalker(&library.Config{Sonarr: &fakeSonarr{listErr: errors.New("sonarr down")}, Logger: scoutTestLogger()}),
		Mapping: fakeMapping{},
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
	if n := recorder.CountExact("library walk failed; cycle unhealthy"); n != 1 {
		t.Errorf("walk-failure ERROR count = %d, want 1", n)
	}
	if n := recorder.CountExact("cycle degraded"); n != 1 {
		t.Errorf("'cycle degraded' count = %d, want 1 (the failed-walk cycle still completed after the feed refresh)", n)
	}
	if reasons := degradedReasons(recorder); len(reasons) != 1 || reasons[0] != "walk-failed" {
		t.Errorf("degraded reasons = %v, want [walk-failed]", reasons)
	}
}

// TestCycleWalkFailureWithFeedResetsSeaDexFailureStreak pins the documented
// SeadexFailures contract ("resets to 0 on any successful fetch") on the
// walk-failed-with-feed arm: that arm saves state and returns before the
// upstream gate, so the reset must not be deferred until the next full-compare
// cycle - a recovery during an arr outage would otherwise leave a stale streak
// frozen in state.json and falsely escalate the first later blip to ERROR.
func TestCycleWalkFailureWithFeedResetsSeaDexFailureStreak(t *testing.T) {
	store := &fakeStore{st: state.State{
		SeadexFailures: 7,
		Mapping:        frierenMappingCache(),
	}}
	s := New(&Deps{
		Logger:  scoutTestLogger(),
		Store:   store,
		Library: library.NewWalker(&library.Config{Sonarr: &fakeSonarr{listErr: errors.New("sonarr down")}, Logger: scoutTestLogger()}),
		Mapping: fakeMapping{},
		SeaDex:  &fakeSeaDex{entries: seadexFrierenEntry()},
		Feed:    &fakeFeed{},
	})

	if healthy := s.Cycle(context.Background()); healthy {
		t.Fatal("Cycle healthy=true, want false when the library walk fails (feed or not)")
	}
	if store.st.SeadexFailures != 0 {
		t.Errorf("persisted SeadexFailures = %d, want 0 (the fetch succeeded this cycle; the walk-failed save must persist the reset)", store.st.SeadexFailures)
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
	logger, recorder := capture.New()
	feed := &fakeFeed{err: errors.New("disk full")}
	store := &fakeStore{st: state.State{
		Mapping: frierenMappingCache(),
	}}
	sonarr := &fakeSonarr{
		series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}},
		files: map[int][]arrapi.EpisodeFile{
			7: {{SeasonNumber: 1, ReleaseGroup: "Erai-raws"}},
		},
	}
	s := New(&Deps{
		Logger:       logger,
		Store:        store,
		Library:      library.NewWalker(&library.Config{Sonarr: sonarr, Logger: logger}),
		Mapping:      fakeMapping{},
		SeaDex:       &fakeSeaDex{entries: seadexFrierenEntry()},
		Matcher:      match.NewMatcher(notFoundAniList{}, logger),
		Comparer:     compare.NewComparer(compare.Config{}),
		Notifier:     notify.NewNotifier(logger),
		AniListStats: aniStatsFn(anilist.NewClient(noNetworkClient(), "http://unused.invalid/gql", 1, logger)),
		Feed:         feed,
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
	if n := recorder.CountExact("indexer feed rebuild failed; keeping previous feed"); n != 1 {
		t.Errorf("feed-rebuild failure log count = %d, want exactly 1", n)
	}
	warns := 0
	for _, r := range recorder.Records() {
		if r.Message == "indexer feed rebuild failed; keeping previous feed" && r.Level == slog.LevelWarn {
			warns++
		}
	}
	if warns != 1 {
		t.Errorf("feed-rebuild failure WARN count = %d, want exactly 1", warns)
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
			if n := recorder.CountExact("cycle degraded"); n != 1 {
				t.Errorf("'cycle degraded' count = %d, want exactly 1 (the double outage still closes the cycle once)", n)
			}
			if reasons := degradedReasons(recorder); len(reasons) != 1 || reasons[0] != "walk-failed" {
				t.Errorf("degraded reasons = %v, want [walk-failed]", reasons)
			}
		})
	}
}

// probingFeed records what the info closure Cycle hands to Rebuild reports
// for a movie record, a series record, and an id absent from the index.
type probingFeed struct {
	got map[int]indexer.EntryInfo
}

func (p *probingFeed) Rebuild(_ context.Context, _ []seadex.Entry, info func(alID int) indexer.EntryInfo) error {
	p.got = map[int]indexer.EntryInfo{
		100: info(100),
		200: info(200),
		999: info(999),
	}
	return nil
}

// TestCycleFeedInfoClassifiesViaFribbIndex pins the per-show metadata closure
// Cycle hands the feed writer: IsMovie must report true for a MOVIE record,
// false for a TV record, and false for an unmapped id (the safe Anime
// default) - a wrong bit silently moves entries between Radarr's Movies
// (2000) and Sonarr's Anime (5070) RSS categories - and the TV record's
// mapped TVDB season must ride along for the season marker.
func TestCycleFeedInfoClassifiesViaFribbIndex(t *testing.T) {
	logger := scoutTestLogger()
	feed := &probingFeed{}
	// The persisted cache echoed by fakeMapping is exactly these two records,
	// so the index the info closure reads is fully controlled.
	store := &fakeStore{st: state.State{
		Mapping: mapping.Cache{FetchedAt: time.Now(), Records: []mapping.Record{
			{AniListID: 100, Type: "MOVIE"},
			{AniListID: 200, Type: "TV", TvdbID: 123, SeasonTvdb: 2},
		}},
	}}
	s := New(&Deps{
		Logger:  logger,
		Store:   store,
		Library: library.NewWalker(&library.Config{Sonarr: &fakeSonarr{listErr: errors.New("sonarr down")}, Logger: logger}),
		Mapping: fakeMapping{},
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
		if feed.got[id].IsMovie != wantMovie {
			t.Errorf("info(%d).IsMovie = %v, want %v", id, feed.got[id].IsMovie, wantMovie)
		}
	}
	if got := feed.got[200].SeasonTvdb; got != 2 {
		t.Errorf("info(200).SeasonTvdb = %d, want 2 (the Fribb season must reach the season marker)", got)
	}
}

// TestCycleWalkFailShutdownDuringSeaDexFetchStaysSilent pins the shutdown arm
// of logFeedOutageOnGatedCycle: when the arr walk genuinely failed but the
// SeaDex "failure" is the cycle context being cancelled mid-fetch (a
// redeploy), the feed-kept WARN must NOT fire - blaming SeaDex would
// misattribute a routine shutdown to an upstream outage (the genuine double
// outage keeps its WARN via TestCycleWalkAndSeaDexBothFailWarnsFeedKept).
func TestCycleWalkFailShutdownDuringSeaDexFetchStaysSilent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger, recorder := capture.New()
	feed := &fakeFeed{}
	// The echoed mapping cache loads usable, so the only failure ordering
	// exercised is walk-failed then fetch-cancelled.
	store := &fakeStore{st: state.State{
		Mapping: frierenMappingCache(),
	}}
	s := New(&Deps{
		Logger:  logger,
		Store:   store,
		Library: library.NewWalker(&library.Config{Sonarr: &fakeSonarr{listErr: errors.New("sonarr down")}, Logger: scoutTestLogger()}),
		Mapping: fakeMapping{},
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
	if n := recorder.CountExact("cycle degraded"); n != 0 {
		t.Errorf("'cycle degraded' count = %d, want 0 (a shutdown after the walk failure interrupted the cycle; no completion line)", n)
	}
}

// cancellingFeed cancels the shared cycle context from inside Rebuild and
// then fails it, modelling a SIGTERM/redeploy landing while the feed rebuild
// is writing.
type cancellingFeed struct{ cancel context.CancelFunc }

func (c *cancellingFeed) Rebuild(context.Context, []seadex.Entry, func(alID int) indexer.EntryInfo) error {
	c.cancel()
	return context.Canceled
}

// TestCycleShutdownDuringFeedRebuildStaysSilent pins the shutdown arm of
// rebuildFeed's failure log: a Rebuild that failed because the cycle context
// was cancelled mid-write (a redeploy) must NOT emit the "indexer feed
// rebuild failed" WARN - that would misattribute a routine shutdown to a feed
// fault (the same contract every other shutdown arm in this suite pins). The
// cycle then closes via the shutdown-during-matching WARN, healthy, with
// prior findings preserved.
func TestCycleShutdownDuringFeedRebuildStaysSilent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger, recorder := capture.New()
	feed := &cancellingFeed{cancel: cancel}
	prior := notify.Alerted{
		AlertedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Finding:   notify.StoredFinding{Title: "Existing", Status: compare.StatusBetter, AniListID: 154587},
	}
	store := &fakeStore{st: state.State{
		Mapping:   seasonlessMappingCache(),
		Findings:  map[string]notify.Alerted{"prior": prior},
		Baselined: true,
	}}
	sonarr := &fakeSonarr{series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}}}
	s := New(&Deps{
		Logger:  logger,
		Store:   store,
		Library: library.NewWalker(&library.Config{Sonarr: sonarr, Logger: scoutTestLogger()}),
		Mapping: fakeMapping{},
		SeaDex:  &fakeSeaDex{entries: []seadex.Entry{{AniListID: 999}}},
		Matcher: match.NewMatcher(degradedMatcherAniList{}, scoutTestLogger()),
		Feed:    feed,
	})

	if healthy := s.Cycle(ctx); !healthy {
		t.Fatal("Cycle healthy=false, want true (a shutdown mid-feed-rebuild is not an ingest failure)")
	}
	if n := recorder.CountExact("indexer feed rebuild failed; keeping previous feed"); n != 0 {
		t.Errorf("feed-failure WARN fired %d times during a shutdown, want 0 (a cancelled rebuild is the shutdown, not a feed fault)", n)
	}
	if n := recorder.CountExact("cycle interrupted by shutdown during matching"); n != 1 {
		t.Errorf("shutdown WARN count = %d, want 1", n)
	}
	if _, ok := store.st.Findings["prior"]; !ok {
		t.Errorf("prior finding not preserved on shutdown mid-cycle: %+v", store.st.Findings)
	}
}

// TestCycleUnusableMapWithSeaDexOutageWarnsFeedKept pins the mapping-unusable
// arm's feed-outage contract: with a feed configured, a cycle whose map is
// unusable AND whose SeaDex fetch failed (or returned zero entries) silently
// kept the previous feed - the feed-kept WARN must still fire so the double
// outage does not read as mapping-only in Loki, while the unusable-map gate's
// own WARN, degraded completion line, and no-rebuild behavior are unchanged.
func TestCycleUnusableMapWithSeaDexOutageWarnsFeedKept(t *testing.T) {
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
			sonarr := &fakeSonarr{series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}}}
			s := New(&Deps{
				Logger:  logger,
				Store:   &fakeStore{},
				Library: library.NewWalker(&library.Config{Sonarr: sonarr, Logger: scoutTestLogger()}),
				// Empty state + unreachable Fribb: the load fails with nothing
				// stale to fall back on, so the map is unusable.
				Mapping: mapping.NewLoader(noNetworkClient(), "http://unused.invalid/f.json", filepath.Join(t.TempDir(), "ov.json"), time.Hour, scoutTestLogger()),
				SeaDex:  tc.seadex,
				Feed:    feed,
			})

			if healthy := s.Cycle(context.Background()); !healthy {
				t.Fatal("Cycle healthy=false, want true (an unusable map is degraded, not unhealthy)")
			}
			if feed.calls != 0 {
				t.Errorf("feed Rebuild calls = %d, want 0 (nothing to rebuild from)", feed.calls)
			}
			if n := recorder.CountExact(tc.wantWarn); n != 1 {
				t.Errorf("%q count = %d, want 1 (a mapping + SeaDex double outage must not read as mapping-only)", tc.wantWarn, n)
			}
			if n := recorder.CountExact("mapping unusable; skipping comparison, findings preserved"); n != 1 {
				t.Errorf("unusable-map WARN count = %d, want 1", n)
			}
			if reasons := degradedReasons(recorder); len(reasons) != 1 || reasons[0] != "mapping-unusable" {
				t.Errorf("degraded reasons = %v, want [mapping-unusable]", reasons)
			}
		})
	}
}
