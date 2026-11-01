package scout

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/cplieger/arrapi/v3"
	"github.com/cplieger/arrapi/v3/sonarr"
	"github.com/cplieger/seadex-scout/internal/anilist"
	"github.com/cplieger/seadex-scout/internal/arrwalk"
	"github.com/cplieger/seadex-scout/internal/compare"
	"github.com/cplieger/seadex-scout/internal/degradation"
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
// (it needs only SeaDex + animap, so an arr outage must not freeze what the arrs
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
		Notifier: notify.NewNotifier(logger, nil),
		Logger:   logger,
		Store:    store,
		Library:  arrwalk.NewWalker(&arrwalk.Config{Sonarr: onePass{&fakeSonarr{listErr: errors.New("sonarr down")}}, Logger: scoutTestLogger()}),
		Mapping:  fakeMapping{},
		SeaDex:   &fakeSeaDex{entries: seadexFrierenEntry()},
		Feed:     feed,
	})

	if healthy := s.Cycle(t.Context()); healthy {
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
		Notifier: notify.NewNotifier(scoutTestLogger(), nil),
		Logger:   scoutTestLogger(),
		Store:    store,
		Library:  arrwalk.NewWalker(&arrwalk.Config{Sonarr: onePass{&fakeSonarr{listErr: errors.New("sonarr down")}}, Logger: scoutTestLogger()}),
		Mapping:  fakeMapping{},
		SeaDex:   &fakeSeaDex{entries: seadexFrierenEntry()},
		Feed:     &fakeFeed{},
	})

	if healthy := s.Cycle(t.Context()); healthy {
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
	sonarr := &fakeSonarr{series: []sonarr.Series{{ID: 7, Title: "Frieren", IDs: sonarr.ExternalIDs{TVDB: 123}, Year: 2023}}}
	// The echoed mapping cache loads usable, so the ONLY reason the rebuild is
	// skipped is the failed fetch (the unusable-map arm is
	// TestCycleUnusableMapSkipsFeedRebuild's).
	s := New(&Deps{
		Notifier: notify.NewNotifier(logger, nil),
		Logger:   logger,
		Store:    &fakeStore{st: state.State{Mapping: frierenMappingCache()}},
		Library:  arrwalk.NewWalker(&arrwalk.Config{Sonarr: onePass{sonarr}, Logger: logger}),
		Mapping:  fakeMapping{},
		SeaDex:   &fakeSeaDex{err: errors.New("seadex down")},
		Feed:     feed,
	})

	if healthy := s.Cycle(t.Context()); !healthy {
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
	sonarr := &fakeSonarr{series: []sonarr.Series{{ID: 7, Title: "Frieren", IDs: sonarr.ExternalIDs{TVDB: 123}, Year: 2023}}}
	s := New(&Deps{
		Notifier: notify.NewNotifier(logger, nil),
		Logger:   logger,
		Store:    &fakeStore{},
		Library:  arrwalk.NewWalker(&arrwalk.Config{Sonarr: onePass{sonarr}, Logger: logger}),
		// Empty state + unreachable mapping: the load fails with nothing stale to
		// fall back on, so the map is unusable (not a StaleMapError).
		Mapping: unreachableMapLoader(t, logger),
		SeaDex:  &fakeSeaDex{entries: seadexFrierenEntry()},
		Feed:    feed,
	})

	if healthy := s.Cycle(t.Context()); !healthy {
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
		series: []sonarr.Series{{ID: 7, Title: "Frieren", IDs: sonarr.ExternalIDs{TVDB: 123}, Year: 2023}},
		files: map[int][]sonarr.EpisodeFile{
			7: {{Season: 1, ReleaseGroup: "Erai-raws"}},
		},
	}
	s := New(&Deps{
		Logger:       logger,
		Store:        store,
		Library:      arrwalk.NewWalker(&arrwalk.Config{Sonarr: onePass{sonarr}, Logger: logger}),
		Mapping:      fakeMapping{},
		SeaDex:       &fakeSeaDex{entries: seadexFrierenEntry()},
		Matcher:      match.New(match.Config{AniList: notFoundAniList{}, Logger: logger}),
		Comparer:     compare.New(compare.Config{}),
		Notifier:     notify.NewNotifier(logger, nil),
		AniListStats: aniStatsFn(anilist.NewClient(noNetworkClient(), "http://unused.invalid/gql", anilist.WithRate(1), anilist.WithLogger(logger))),
		Feed:         feed,
	})

	if healthy := s.Cycle(t.Context()); !healthy {
		t.Fatal("Cycle healthy=false, want true when only the feed rebuild fails")
	}
	if feed.calls != 1 {
		t.Errorf("feed Rebuild calls = %d, want 1", feed.calls)
	}
	if n := recorder.CountExact("findings reported"); n != 1 {
		t.Errorf("'findings reported' count = %d, want 1; the compare half must complete despite a feed rebuild failure", n)
	}
	if n := recorder.CountExact("indexer feed rebuild failed; keeping previous feed"); n != 1 {
		t.Errorf("feed-rebuild failure log count = %d, want exactly 1", n)
	}
	warns := recorder.CountLevel(slog.LevelWarn, "indexer feed rebuild failed; keeping previous feed")
	if warns != 1 {
		t.Errorf("feed-rebuild failure WARN count = %d, want exactly 1", warns)
	}
}

// TestCycleWalkAndSeaDexBothFailWarnsFeedKept pins the multi-dependency-outage
// visibility arm of the pre-compare gate: with a feed configured and the arr
// walk failed, a SeaDex failure that silently kept the previous feed must still
// be surfaced with its own WARN, so a double outage does not read as arr-only
// in Loki.
func TestCycleWalkAndSeaDexBothFailWarnsFeedKept(t *testing.T) {
	tests := []struct {
		name     string
		seadex   *fakeSeaDex
		wantWarn string
	}{
		{
			name:     "seadex fetch fails",
			seadex:   &fakeSeaDex{err: errors.New("seadex down")},
			wantWarn: "seadex fetch failed; skipping comparison, findings re-stated unchanged this cycle",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logger, recorder := capture.New()
			feed := &fakeFeed{}
			s := New(&Deps{
				Notifier: notify.NewNotifier(logger, nil),
				Logger:   logger,
				Store:    &fakeStore{},
				Library:  arrwalk.NewWalker(&arrwalk.Config{Sonarr: onePass{&fakeSonarr{listErr: errors.New("sonarr down")}}, Logger: scoutTestLogger()}),
				Mapping:  unreachableMapLoader(t, scoutTestLogger()),
				SeaDex:   tc.seadex,
				Feed:     feed,
			})

			if healthy := s.Cycle(t.Context()); healthy {
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

// probingFeed records what the FeedInfo Cycle hands to Rebuild reports for a
// movie record, a series record and an id absent from the index, and for a
// TVDB id in and out of the persisted non-aired-order set.
type probingFeed struct {
	got      map[int]indexer.EntryInfo
	nonAired map[int]bool
}

func (p *probingFeed) Rebuild(_ context.Context, _ []seadex.Entry, info indexer.FeedInfo) error {
	p.got = map[int]indexer.EntryInfo{
		100: info.Entry(100),
		200: info.Entry(200),
		999: info.Entry(999),
	}
	p.nonAired = map[int]bool{102261: info.NonAiredOrder(102261), 123: info.NonAiredOrder(123)}
	return nil
}

// Advance is unreachable in this fake's tests (they all reconcile) but must
// exist to satisfy the seam; it records nothing so a misrouted dispatch shows
// up as an empty got map rather than a plausible-looking one.
func (*probingFeed) Advance(context.Context, []seadex.Entry, indexer.FeedInfo) error {
	return nil
}

// TestCycleFeedInfoClassifiesViaMappingIndex pins the per-show metadata closure
// Cycle hands the feed writer: Kind must be KindMovie for a TMDB-routed record,
// KindSeries for a TVDB-routed one, and KindUnknown for an unmapped id (which renders
// as the safe Anime default) - a wrong kind silently moves entries between Radarr's Movies
// (2000) and Sonarr's Anime (5070) RSS categories - and the TV record's
// mapped TVDB season must ride along for the season marker. The persisted
// non-aired-order set reaches the writer by TVDB id, so a stored
// render is judged by the series it names.
func TestCycleFeedInfoClassifiesViaMappingIndex(t *testing.T) {
	logger := scoutTestLogger()
	feed := &probingFeed{}
	// The persisted cache echoed by fakeMapping is exactly these two records,
	// so the index the info closure reads is fully controlled.
	store := &fakeStore{st: state.State{
		Mapping: mapping.Cache{FetchedAt: time.Now(), Records: []mapping.Record{
			{AniListID: 100, TmdbMovies: []int{900}},
			{AniListID: 200, TvdbID: 123, SeasonTvdb: 2},
		}},
		NonAiredOrderTvdbIDs: []int{102261},
	}}
	s := New(&Deps{
		Notifier: notify.NewNotifier(logger, nil),
		Logger:   logger,
		Store:    store,
		Library:  arrwalk.NewWalker(&arrwalk.Config{Sonarr: onePass{&fakeSonarr{listErr: errors.New("sonarr down")}}, Logger: logger}),
		Mapping:  fakeMapping{},
		SeaDex:   &fakeSeaDex{entries: seadexFrierenEntry()},
		Feed:     feed,
	})

	if healthy := s.Cycle(t.Context()); healthy {
		t.Fatal("Cycle healthy=true, want false (the walk failed; only the feed refreshes)")
	}
	if feed.got == nil {
		t.Fatal("feed Rebuild was not called")
	}
	for id, want := range map[int]indexer.MediaKind{100: indexer.KindMovie, 200: indexer.KindSeries, 999: indexer.KindUnknown} {
		if got := feed.got[id].Kind; got != want {
			t.Errorf("info(%d).Kind = %v, want %v", id, got, want)
		}
	}
	if got := feed.got[200]; got.Season != 2 || !got.SeasonKnown {
		t.Errorf("info(200) season = %d (known %v), want a resolved season 2 (the mapped season must reach the season marker)", got.Season, got.SeasonKnown)
	}
	for id, want := range map[int]bool{102261: true, 123: false} {
		if feed.nonAired[id] != want {
			t.Errorf("NonAiredOrder(%d) = %v, want %v (the persisted set, kept across the failed walk)", id, feed.nonAired[id], want)
		}
	}
}

// TestCycleWalkFailShutdownDuringSeaDexFetchStaysSilent pins the shutdown arm
// of the walk-failed gate: when the arr walk genuinely failed but the
// SeaDex "failure" is the cycle context being cancelled mid-fetch (a
// redeploy), the SeaDex WARN must NOT fire - blaming SeaDex would
// misattribute a routine shutdown to an upstream outage (the genuine double
// outage keeps its WARN via TestCycleWalkAndSeaDexBothFailWarnsFeedKept).
func TestCycleWalkFailShutdownDuringSeaDexFetchStaysSilent(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
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
		Library: arrwalk.NewWalker(&arrwalk.Config{Sonarr: onePass{&fakeSonarr{listErr: errors.New("sonarr down")}}, Logger: scoutTestLogger()}),
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
	if n := recorder.CountExact("seadex fetch failed; skipping comparison, findings re-stated unchanged this cycle"); n != 0 {
		t.Errorf("seadex-failure WARN fired %d times during a shutdown, want 0 (recordSeaDexFetch must stay silent on a cancelled fetch)", n)
	}
	if n := recorder.CountExact("cycle degraded"); n != 0 {
		t.Errorf("'cycle degraded' count = %d, want 0 (a shutdown after the walk failure interrupted the cycle; no completion line)", n)
	}
}

// cancellingFeed cancels the shared cycle context from inside Rebuild and
// then fails it, modelling a SIGTERM/redeploy landing while the feed rebuild
// is writing.
type cancellingFeed struct{ cancel context.CancelFunc }

func (c *cancellingFeed) Rebuild(context.Context, []seadex.Entry, indexer.FeedInfo) error {
	c.cancel()
	return context.Canceled
}

func (c *cancellingFeed) Advance(context.Context, []seadex.Entry, indexer.FeedInfo) error {
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
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	logger, recorder := capture.New()
	feed := &cancellingFeed{cancel: cancel}
	store := &fakeStore{st: state.State{
		Mapping: seasonlessMappingCache(),
	}}
	sonarr := &fakeSonarr{series: []sonarr.Series{{ID: 7, Title: "Frieren", IDs: sonarr.ExternalIDs{TVDB: 123}, Year: 2023}}}
	s := New(&Deps{
		Logger:  logger,
		Store:   store,
		Library: arrwalk.NewWalker(&arrwalk.Config{Sonarr: onePass{sonarr}, Logger: scoutTestLogger()}),
		Mapping: fakeMapping{},
		SeaDex:  &fakeSeaDex{entries: []seadex.Entry{{AniListID: 999}}},
		Matcher: match.New(match.Config{AniList: degradedMatcherAniList{}, Logger: scoutTestLogger()}),
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
}

// TestCycleUnusableMapWithSeaDexOutageWarnsFeedKept pins the mapping-unusable
// arm's feed-outage contract: with a feed configured, a cycle whose map is
// unusable AND whose SeaDex fetch failed silently kept the previous feed - the
// SeaDex WARN must still fire so the double outage does not read as
// mapping-only in Loki, while the unusable-map gate's own WARN, degraded
// completion line, and no-rebuild behavior are unchanged.
func TestCycleUnusableMapWithSeaDexOutageWarnsFeedKept(t *testing.T) {
	tests := []struct {
		name     string
		seadex   *fakeSeaDex
		wantWarn string
	}{
		{
			name:     "seadex fetch fails",
			seadex:   &fakeSeaDex{err: errors.New("seadex down")},
			wantWarn: "seadex fetch failed; skipping comparison, findings re-stated unchanged this cycle",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logger, recorder := capture.New()
			feed := &fakeFeed{}
			sonarr := &fakeSonarr{series: []sonarr.Series{{ID: 7, Title: "Frieren", IDs: sonarr.ExternalIDs{TVDB: 123}, Year: 2023}}}
			s := New(&Deps{
				Notifier: notify.NewNotifier(logger, nil),
				Logger:   logger,
				Store:    &fakeStore{},
				Library:  arrwalk.NewWalker(&arrwalk.Config{Sonarr: onePass{sonarr}, Logger: scoutTestLogger()}),
				// Empty state + unreachable mapping: the load fails with nothing
				// stale to fall back on, so the map is unusable.
				Mapping: unreachableMapLoader(t, scoutTestLogger()),
				SeaDex:  tc.seadex,
				Feed:    feed,
			})

			if healthy := s.Cycle(t.Context()); !healthy {
				t.Fatal("Cycle healthy=false, want true (an unusable map is degraded, not unhealthy)")
			}
			if feed.calls != 0 {
				t.Errorf("feed Rebuild calls = %d, want 0 (nothing to rebuild from)", feed.calls)
			}
			if n := recorder.CountExact(tc.wantWarn); n != 1 {
				t.Errorf("%q count = %d, want 1 (a mapping + SeaDex double outage must not read as mapping-only)", tc.wantWarn, n)
			}
			if n := recorder.CountExact("mapping unusable; skipping comparison, findings re-stated unchanged this cycle"); n != 1 {
				t.Errorf("unusable-map WARN count = %d, want 1", n)
			}
			// Same signal the SeaDex-outage line carries: with a feed wired, the
			// arrs keep being served the PREVIOUS curation through the outage, and
			// this attribute is the only place the line says so.
			if kept, ok := recordAttr(recorder, "mapping unusable; skipping comparison, findings re-stated unchanged this cycle", "feed_kept"); !ok || kept != "true" {
				t.Errorf("unusable-map WARN feed_kept = %q (found=%t), want \"true\"", kept, ok)
			}
			// The persisted rejection streak is zero here (an unreachable mapping is
			// not a guard rejection), and the attribute names a condition with its
			// own remedy, so a zero must not be attached: a Loki query for the
			// guard-rejection incident would otherwise match every network blip.
			if streak, ok := recordAttr(recorder, "mapping degraded", "stale_consecutive_rejections"); ok {
				t.Errorf("mapping-degraded WARN carries stale_consecutive_rejections = %q with no rejection streak, want the attribute absent", streak)
			}
			if reasons := degradedReasons(recorder); len(reasons) != 1 || reasons[0] != "mapping-unusable" {
				t.Errorf("degraded reasons = %v, want [mapping-unusable]", reasons)
			}
		})
	}
}

// TestSeaDexFailureLogCarriesFeedKept pins the feed_kept attribute on the
// SeaDex-outage log line: it is the only signal telling an operator whether
// the arrs are still being served the PREVIOUS Torznab curation feed through
// the outage, so an inverted or dropped boolean sends them triaging an
// alert-only deployment as if a feed were stale (or the reverse) with nothing
// else in the line to contradict it.
func TestSeaDexFailureLogCarriesFeedKept(t *testing.T) {
	tests := []struct {
		feed FeedWriter
		name string
		want string
	}{
		{name: "feed configured", feed: &fakeFeed{}, want: "true"},
		{name: "alert-only deployment", feed: nil, want: "false"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logger, recorder := capture.New()
			sonarr := &fakeSonarr{series: []sonarr.Series{{ID: 7, Title: "Frieren", IDs: sonarr.ExternalIDs{TVDB: 123}, Year: 2023}}}
			s := New(&Deps{
				Notifier: notify.NewNotifier(logger, nil),
				Logger:   logger,
				Store:    &fakeStore{st: state.State{Mapping: frierenMappingCache()}},
				Library:  arrwalk.NewWalker(&arrwalk.Config{Sonarr: onePass{sonarr}, Logger: scoutTestLogger()}),
				Mapping:  fakeMapping{},
				SeaDex:   &fakeSeaDex{err: errors.New("seadex down")},
				Feed:     tc.feed,
			})

			if healthy := s.Cycle(t.Context()); !healthy {
				t.Fatal("Cycle healthy=false, want true (a SeaDex outage is degraded, not unhealthy)")
			}
			got, ok := recordAttr(recorder, "seadex fetch failed; skipping comparison, findings re-stated unchanged this cycle", "feed_kept")
			if !ok || got != tc.want {
				t.Errorf("seadex-failure feed_kept = %q (found=%t), want %q", got, ok, tc.want)
			}
		})
	}
}

// TestCycleWalkFailureWithFeedPreservesPriorSnapshot pins the walk-failed arm's
// persistence SCOPE: with a feed configured Cycle falls through to
// handleLibraryGate, which saves the refreshed mapping cache beside the prior
// snapshot's items. Persisting the failed walk's itemless snapshot would leave
// st.Library empty, so the NEXT cycle's compare mass-resolves every finding
// against an empty prior library - findings vanish with no error anywhere.
func TestCycleWalkFailureWithFeedPreservesPriorSnapshot(t *testing.T) {
	store := &fakeStore{st: state.State{
		Mapping: frierenMappingCache(),
		Library: library.Snapshot{Items: []library.Item{{Arr: library.ArrSonarr, ArrID: 7, Title: "Frieren", TvdbID: 123}}},
	}}
	s := New(&Deps{
		Notifier: notify.NewNotifier(scoutTestLogger(), nil),
		Logger:   scoutTestLogger(),
		Store:    store,
		Library:  arrwalk.NewWalker(&arrwalk.Config{Sonarr: onePass{&fakeSonarr{listErr: errors.New("sonarr down")}}, Logger: scoutTestLogger()}),
		Mapping:  fakeMapping{},
		SeaDex:   &fakeSeaDex{entries: seadexFrierenEntry()},
		Feed:     &fakeFeed{},
	})

	if healthy := s.Cycle(t.Context()); healthy {
		t.Fatal("Cycle healthy=true, want false when the library walk fails")
	}
	if store.saves != 1 {
		t.Errorf("saves = %d, want 1 (the walk-failed arm persists the refreshed mapping cache)", store.saves)
	}
	if len(store.st.Library.Items) != 1 || store.st.Library.Items[0].Title != "Frieren" {
		t.Errorf("persisted library = %+v, want the prior snapshot untouched (a failed walk must never replace it, or the next cycle mass-resolves every finding)", store.st.Library)
	}
}

// taggedSonarr answers Tags, so a walk's arr_tags filter can leave a listed series
// out of the items.
type taggedSonarr struct {
	*fakeSonarr
	tags []arrapi.Tag
}

func (f taggedSonarr) Tags(context.Context) ([]arrapi.Tag, error) { return f.tags, nil }

// fixedMapping keeps the persisted cache and serves one index, so a cycle's
// feed and compare read the same records and season ranges a test built.
type fixedMapping struct{ idx *mapping.Index }

func (f fixedMapping) Load(_ context.Context, prev *mapping.Cache) (mapping.Cache, *mapping.Index, error) {
	return *prev, f.idx, nil
}

// judgedFeed records, for each Rebuild, judge's verdict on the FeedInfo the
// reconcile handed the feed writer.
type judgedFeed struct {
	judge func(indexer.FeedInfo) string
	fakeFeed
	verdicts []string
}

func (f *judgedFeed) Rebuild(ctx context.Context, entries []seadex.Entry, info indexer.FeedInfo) error {
	f.verdicts = append(f.verdicts, f.judge(info))
	return f.fakeFeed.Rebuild(ctx, entries, info)
}

// lastVerdict is the verdict on the latest Rebuild, "no rebuild" before one.
func (f *judgedFeed) lastVerdict() string {
	if len(f.verdicts) == 0 {
		return "no rebuild"
	}
	return f.verdicts[len(f.verdicts)-1]
}

// TestCycleNonAiredOrderSeriesStaysWithheldUntilListedInAiredOrder pins the
// feed's order rule across reconciles through the real walker, cycle and
// feedInfo (docs/torznab-indexer.md "Series in another episode order"):
// untagged, book-order Monogatari gets no TVDB season or range until a clean
// pass lists it in aired order. Short lists, a deletion, a failed list
// read and any failure, shutdown or shrink-guard carry after the list keep it
// withheld, and a list read in full adds its non-aired orders whatever follows.
// Both the persisted state and each pass's own Rebuild are checked.
func TestCycleNonAiredOrderSeriesStaysWithheldUntilListedInAiredOrder(t *testing.T) {
	const monogatariTvdb = 102261
	tagged := func(id int, title string) sonarr.Series {
		return sonarr.Series{ID: id, Title: title, IDs: sonarr.ExternalIDs{TVDB: 1000 + id}, Year: 2024, Tags: []int{1}}
	}
	untagged := func(id int, title string) sonarr.Series {
		return sonarr.Series{ID: id, Title: title, IDs: sonarr.ExternalIDs{TVDB: 1000 + id}, Year: 2024}
	}
	frieren := sonarr.Series{ID: 7, Title: "Frieren", IDs: sonarr.ExternalIDs{TVDB: 123}, Year: 2023, Tags: []int{1}}
	book := sonarr.Series{ID: 2189, Title: "Monogatari", IDs: sonarr.ExternalIDs{TVDB: monogatariTvdb}, Year: 2009, Order: sonarr.Order{Type: "alternate", Name: "Book Order"}}
	airedBook := book
	airedBook.Order = sonarr.Order{Type: "official", Name: "Aired Order", Official: true}
	second, third := tagged(8, "Second Show"), tagged(9, "Third Show")
	four, five, six := untagged(10, "Fourth Show"), untagged(11, "Fifth Show"), untagged(12, "Sixth Show")
	full := []sonarr.Series{frieren, second, third, book, four, five, six}
	// A season record on Monogatari, so an aired-order series gets season 2 and
	// its range. Its twin needs a kept series to title it, so arr_tags already
	// rules it out here (TestFeedEntryInfoWithholdsTwinForNonOfficialSeries).
	idx := mapping.NewIndex(mapping.Source{Records: append(shrinkMappingCache().Records,
		mapping.Record{AniListID: 1, TvdbID: monogatariTvdb, AniDBID: 9001, SeasonKind: mapping.SeasonPresent, SeasonTvdb: 2},
	), Mappings: map[int]mapping.Mapping{
		9001: {Seasons: []mapping.SeasonRange{{Season: 2, First: 1, Last: 26}}, SeasonsTvdb: monogatariTvdb},
	}})
	const (
		verdictWithheld = "withheld"
		verdictReleased = "released"
	)
	judge := func(info indexer.FeedInfo) string {
		season := info.Entry(1)
		switch {
		case info.NonAiredOrder(monogatariTvdb) && !season.SeasonKnown && season.Seasons == nil:
			return verdictWithheld
		case !info.NonAiredOrder(monogatariTvdb) && season.SeasonKnown && season.Season == 2 &&
			slices.Equal(season.Seasons, []indexer.SeasonRange{{Season: 2, First: 1, Last: 26}}):
			return verdictReleased
		default:
			return fmt.Sprintf("mixed: NonAiredOrder=%t entry=%+v", info.NonAiredOrder(monogatariTvdb), season)
		}
	}
	persisted := func(store *fakeStore) string {
		return judge(feedInfo(idx, &store.st.Library, store.st.NonAiredOrderTvdbIDs, match.Memo{}))
	}
	anime := []arrapi.Tag{{ID: 1, Label: "anime"}}
	// cycleLogged runs one pass whose walker logs to walkLog; a nil rad leaves
	// Radarr unconfigured and a nil feed the Torznab feed.
	cycleLogged := func(ctx context.Context, t *testing.T, son arrwalk.SonarrClient, rad arrwalk.RadarrClient, store *fakeStore, feed *judgedFeed, walkLog *slog.Logger) bool {
		t.Helper()
		deps := &Deps{
			Logger: scoutTestLogger(),
			Store:  store,
			Library: arrwalk.NewWalker(&arrwalk.Config{
				Sonarr: son, Radarr: rad,
				IncludeTags: []string{"anime"}, Logger: walkLog,
			}),
			Mapping:      fixedMapping{idx: idx},
			SeaDex:       &fakeSeaDex{entries: seadexFrierenEntry()},
			Matcher:      match.New(match.Config{AniList: notFoundAniList{}, Logger: scoutTestLogger()}),
			Comparer:     compare.New(compare.Config{}),
			Notifier:     notify.NewNotifier(scoutTestLogger(), nil),
			AniListStats: aniStatsFn(anilist.NewClient(noNetworkClient(), "http://unused.invalid/gql", anilist.WithRate(1), anilist.WithLogger(scoutTestLogger()))),
		}
		if feed != nil {
			deps.Feed = feed
		}
		return New(deps).Cycle(ctx)
	}
	cycleOn := func(ctx context.Context, t *testing.T, son arrwalk.SonarrClient, rad arrwalk.RadarrClient, store *fakeStore, feed *judgedFeed) bool {
		t.Helper()
		return cycleLogged(ctx, t, son, rad, store, feed, scoutTestLogger())
	}
	cycle := func(t *testing.T, fake *fakeSonarr, store *fakeStore, feed *judgedFeed, series []sonarr.Series) {
		t.Helper()
		fake.series = series
		if !cycleOn(t.Context(), t, onePass{taggedSonarr{fakeSonarr: fake, tags: anime}}, nil, store, feed) {
			t.Fatal("Cycle healthy=false, want true")
		}
	}
	// seed runs the first full pass on an empty state, the first pass after
	// upgrading to a release that reads orders: its own rebuild already withholds.
	seed := func(t *testing.T) (*fakeSonarr, *fakeStore, *judgedFeed) {
		t.Helper()
		fake := &fakeSonarr{files: map[int][]sonarr.EpisodeFile{7: {{Season: 1, ReleaseGroup: "Erai-raws"}}}}
		store := &fakeStore{st: state.State{Mapping: shrinkMappingCache()}}
		feed := &judgedFeed{judge: judge}
		cycle(t, fake, store, feed, full)
		if got := countItemsByArr(store.st.Library.Items)[library.ArrSonarr]; got != 3 {
			t.Fatalf("sonarr items = %d, want 3 (arr_tags keeps the three tagged series)", got)
		}
		if got := feed.lastVerdict(); got != verdictWithheld {
			t.Errorf("first pass's Rebuild saw Monogatari %s, want %s by the pass that first reads its book order", got, verdictWithheld)
		}
		if got := persisted(store); got != verdictWithheld {
			t.Fatalf("seeded persisted feed info: Monogatari %s, want %s", got, verdictWithheld)
		}
		return fake, store, feed
	}
	runs := []struct {
		name  string
		lists [][]sonarr.Series
	}{
		// Kept items fall under half while the list does not, then the list
		// falls under half of the full one while every tagged series returns.
		{name: "items then list short", lists: [][]sonarr.Series{{frieren, four, five, six}, {frieren, second, third}}},
		{name: "a long run of short lists", lists: slices.Repeat([][]sonarr.Series{{frieren}}, 2*degradation.ShrunkWalkAcceptThreshold)},
		{name: "an empty list", lists: [][]sonarr.Series{{}}},
		{name: "series deleted from sonarr", lists: [][]sonarr.Series{{frieren, second, third, four, five, six}}},
		{name: "a full list between short ones", lists: [][]sonarr.Series{{frieren}, full, {frieren}, {frieren, second, third}}},
	}
	for _, run := range runs {
		t.Run(run.name+" keeps it withheld", func(t *testing.T) {
			fake, store, feed := seed(t)
			for pass, list := range run.lists {
				cycle(t, fake, store, feed, list)
				if got := feed.lastVerdict(); got != verdictWithheld {
					t.Errorf("pass %d (%d series listed): Rebuild saw Monogatari %s, want %s", pass+1, len(list), got, verdictWithheld)
				}
				if got := persisted(store); got != verdictWithheld {
					t.Fatalf("pass %d (%d series listed): persisted feed info has Monogatari %s, want %s", pass+1, len(list), got, verdictWithheld)
				}
			}
		})
	}
	t.Run("an aired-order listing releases it", func(t *testing.T) {
		fake, store, feed := seed(t)
		cycle(t, fake, store, feed, []sonarr.Series{frieren, airedBook})
		if got := feed.lastVerdict(); got != verdictWithheld {
			t.Errorf("a shrink-guarded aired-order listing's Rebuild saw Monogatari %s, want %s", got, verdictWithheld)
		}
		cycle(t, fake, store, feed, []sonarr.Series{frieren, second, third, airedBook, four, five, six})
		if got := feed.lastVerdict(); got != verdictReleased {
			t.Errorf("the clean aired-order listing's own Rebuild saw Monogatari %s, want %s", got, verdictReleased)
		}
		if got := persisted(store); got != verdictReleased {
			t.Errorf("persisted feed info after the aired-order listing has Monogatari %s, want %s", got, verdictReleased)
		}
	})

	// Every exit of a pass, in both directions: a Sonarr series list read in
	// full adds its non-aired orders whatever follows it, a shutdown included,
	// only a clean pass lets its aired orders release an id, and without a list
	// the set stays as it was.
	sonarrExit := func(exit exitSonarr) func([]sonarr.Series) arrwalk.SonarrClient {
		return func(series []sonarr.Series) arrwalk.SonarrClient {
			exit.taggedSonarr = taggedSonarr{fakeSonarr: &fakeSonarr{series: series, files: map[int][]sonarr.EpisodeFile{7: {{Season: 1, ReleaseGroup: "Erai-raws"}}}}, tags: anime}
			if exit.late != 0 {
				exit.started = make(chan struct{})
			}
			return onePass{exit}
		}
	}
	sonarrWith := func(tagErr error, filesErr map[int]error) func([]sonarr.Series) arrwalk.SonarrClient {
		return sonarrExit(exitSonarr{tagErr: tagErr, filesErr: filesErr})
	}
	failFiles := func(err error, ids ...int) map[int]error {
		m := make(map[int]error, len(ids))
		for _, id := range ids {
			m[id] = err
		}
		return m
	}
	readBoom := errors.New("episode file read failed")
	changed := &arrapi.ServerChangedError{From: "4.0.20.3014", To: "5.0.0.2701"}
	priorItems := []library.Item{
		{Arr: library.ArrSonarr, ArrID: 7, TvdbID: 123, Title: "Frieren"},
		{Arr: library.ArrSonarr, ArrID: 8, TvdbID: 1008, Title: "Second Show"},
		{Arr: library.ArrSonarr, ArrID: 9, TvdbID: 1009, Title: "Third Show"},
	}
	budgetList := func(mono sonarr.Series) []sonarr.Series {
		return append(full[:3:3], mono, tagged(20, "A"), tagged(21, "B"), tagged(22, "C"))
	}
	priorMovies := []library.Item{
		{Arr: library.ArrRadarr, ArrID: 1, TmdbID: 501, Title: "Film One"},
		{Arr: library.ArrRadarr, ArrID: 2, TmdbID: 502, Title: "Film Two"},
	}
	// items is the Sonarr item count a healthy pass persists. A shutdown, at
	// the series list, at the tag read after it or at the walk's completion
	// line, rebuilds no feed and saves the loaded state only when the list
	// added an id (docs/torznab-indexer.md "Series in another episode order").
	exits := []struct {
		sonarr      func([]sonarr.Series) arrwalk.SonarrClient
		radarr      arrwalk.RadarrClient
		name        string
		shutdownAt  string
		list        func(mono sonarr.Series) []sonarr.Series
		priorMovies []library.Item
		shrunkWalks int
		items       int
		listRead    bool
		clean       bool
		healthy     bool
	}{
		{name: "the pass succeeds", sonarr: sonarrWith(nil, nil), listRead: true, clean: true, healthy: true, items: 3},
		{name: "the pass cannot be opened", sonarr: func([]sonarr.Series) arrwalk.SonarrClient { return pinFails{errors.New("pin failed")} }},
		{name: "the series list fails", sonarr: func(series []sonarr.Series) arrwalk.SonarrClient {
			return onePass{taggedSonarr{fakeSonarr: &fakeSonarr{series: series, listErr: errors.New("sonarr down")}, tags: anime}}
		}},
		{name: "the tag read fails", sonarr: sonarrWith(errors.New("tag read failed"), nil), listRead: true},
		{name: "one kept series' episode-file read fails", sonarr: sonarrWith(nil, failFiles(readBoom, 8)), listRead: true, healthy: true, items: 3},
		{
			name:     "one kept series' episode list read fails",
			sonarr:   sonarrExit(exitSonarr{episodesErr: failFiles(errors.New("episode list read failed"), 8)}),
			listRead: true, healthy: true, items: 3,
		},
		{name: "every kept series' episode read fails", sonarr: sonarrWith(nil, failFiles(readBoom, 7, 8, 9)), listRead: true},
		{name: "the episode failure budget runs out", list: budgetList, sonarr: sonarrWith(nil, failFiles(readBoom, 8, 9, 20, 21, 22)), listRead: true},
		{name: "an episode-file read meets another sonarr version", sonarr: sonarrWith(nil, failFiles(changed, 8)), listRead: true},
		{name: "the tag read meets another sonarr version", sonarr: sonarrWith(changed, nil), listRead: true},
		{
			name:     "the failure budget trips while a read meets another sonarr version",
			list:     budgetList,
			sonarr:   sonarrExit(exitSonarr{filesErr: failFiles(readBoom, 8, 9, 20, 21, 22), late: 7, lateErr: changed}),
			listRead: true,
		},
		{name: "the radarr read fails", sonarr: sonarrWith(nil, nil), radarr: &fakeRadarr{listErr: errors.New("radarr down")}, listRead: true},
		{
			name:     "the shrink guard carries the prior items",
			list:     func(mono sonarr.Series) []sonarr.Series { return []sonarr.Series{frieren, mono, four, five, six} },
			sonarr:   sonarrWith(nil, nil),
			listRead: true, healthy: true, items: 3,
		},
		{
			name:        "the shrink guard accepts the smaller library",
			list:        func(mono sonarr.Series) []sonarr.Series { return []sonarr.Series{frieren, mono, four, five, six} },
			sonarr:      sonarrWith(nil, nil),
			shrunkWalks: degradation.ShrunkWalkAcceptThreshold - 1,
			listRead:    true, clean: true, healthy: true, items: 1,
		},
		{
			name:        "the radarr shrink guard carries its prior items",
			sonarr:      sonarrWith(nil, nil),
			radarr:      &fakeRadarr{},
			priorMovies: priorMovies,
			listRead:    true, healthy: true, items: 3,
		},
		{name: "a shutdown cancels the pass at the series list", sonarr: sonarrWith(nil, nil), shutdownAt: readSeries, healthy: true, items: 3},
		{name: "a shutdown cancels the pass after the series list", sonarr: sonarrWith(nil, nil), shutdownAt: readTags, listRead: true, healthy: true, items: 3},
		{name: "a shutdown lands as the walk completes", sonarr: sonarrWith(nil, nil), shutdownAt: walkComplete, listRead: true, healthy: true, items: 3},
	}
	// joined is prior plus the list's non-aired orders, released the clean
	// pass's set with its aired orders gone, and unread the prior set; adds
	// is whether joined holds an id prior lacks.
	directions := []struct {
		name                     string
		mono                     sonarr.Series
		prior                    []int
		released, joined, unread string
		adds                     bool
	}{
		{name: "a book-order listing", mono: book, released: verdictWithheld, joined: verdictWithheld, unread: verdictReleased, adds: true},
		{name: "an aired-order listing", mono: airedBook, prior: []int{monogatariTvdb}, released: verdictReleased, joined: verdictWithheld, unread: verdictWithheld},
	}
	for _, exit := range exits {
		for _, dir := range directions {
			t.Run(exit.name+" after "+dir.name, func(t *testing.T) {
				list := []sonarr.Series{frieren, second, third, dir.mono, four, five, six}
				if exit.list != nil {
					list = exit.list(dir.mono)
				}
				store := &fakeStore{st: state.State{
					Mapping: shrinkMappingCache(), Library: library.Snapshot{Items: slices.Concat(priorItems, exit.priorMovies)}, NonAiredOrderTvdbIDs: dir.prior,
				}}
				if exit.shrunkWalks > 0 {
					store.st.ShrunkWalksByArr = map[string]int{library.ArrSonarr: exit.shrunkWalks}
				}
				ctx, cancel := context.WithCancel(t.Context())
				t.Cleanup(cancel)
				son := exit.sonarr(list)
				walkLog := scoutTestLogger()
				switch exit.shutdownAt {
				case readSeries, readTags:
					son = shutdownAt{SonarrClient: son, cancel: cancel, read: exit.shutdownAt}
				case walkComplete:
					walkLog = slog.New(cancelOnMessage{Handler: slog.DiscardHandler, msg: walkComplete, cancel: cancel})
				}
				feed := &judgedFeed{judge: judge}
				if got := cycleLogged(ctx, t, son, exit.radarr, store, feed, walkLog); got != exit.healthy {
					t.Errorf("Cycle healthy = %t, want %t", got, exit.healthy)
				}
				want := dir.unread
				switch {
				case exit.clean:
					want = dir.released
				case exit.listRead:
					want = dir.joined
				}
				wantRebuilds := 1
				if exit.shutdownAt != "" {
					wantRebuilds = 0
					wantSaves := 0
					if exit.listRead && dir.adds {
						wantSaves = 1
					}
					if store.saves != wantSaves {
						t.Errorf("state saves = %d, want %d", store.saves, wantSaves)
					}
				}
				if got := len(feed.verdicts); got != wantRebuilds {
					t.Fatalf("Rebuild calls = %d, want %d", got, wantRebuilds)
				}
				if got := feed.lastVerdict(); wantRebuilds == 1 && got != want {
					t.Errorf("Rebuild saw Monogatari %s, want %s", got, want)
				}
				if got := persisted(store); got != want {
					t.Errorf("persisted feed info has Monogatari %s, want %s", got, want)
				}
				var wantSet []int
				if want == verdictWithheld {
					wantSet = []int{monogatariTvdb}
				}
				if got := store.st.NonAiredOrderTvdbIDs; !slices.Equal(got, wantSet) {
					t.Errorf("persisted NonAiredOrderTvdbIDs = %v, want %v", got, wantSet)
				}
				if got := countItemsByArr(store.st.Library.Items)[library.ArrSonarr]; exit.healthy && got != exit.items {
					t.Errorf("persisted sonarr items = %d (%+v), want %d", got, store.st.Library.Items, exit.items)
				}
			})
		}
	}
	t.Run("a failed pass's aired-order listing releases it only at the next clean pass", func(t *testing.T) {
		store := &fakeStore{st: state.State{Mapping: shrinkMappingCache(), NonAiredOrderTvdbIDs: []int{monogatariTvdb}}}
		feed := &judgedFeed{judge: judge}
		list := []sonarr.Series{frieren, second, third, airedBook}
		if cycleOn(t.Context(), t, sonarrWith(nil, failFiles(changed, 8))(list), nil, store, feed) {
			t.Fatal("Cycle healthy=true, want false when a read meets another sonarr version")
		}
		if got := feed.lastVerdict(); got != verdictWithheld {
			t.Errorf("the failed pass's Rebuild saw Monogatari %s, want %s", got, verdictWithheld)
		}
		if got := persisted(store); got != verdictWithheld {
			t.Errorf("persisted feed info after the failed pass has Monogatari %s, want %s", got, verdictWithheld)
		}
		if !cycleOn(t.Context(), t, sonarrWith(nil, nil)(list), nil, store, feed) {
			t.Fatal("Cycle healthy=false, want true")
		}
		if got := feed.lastVerdict(); got != verdictReleased {
			t.Errorf("the clean pass's aired-order Rebuild saw Monogatari %s, want %s", got, verdictReleased)
		}
		if got := persisted(store); got != verdictReleased {
			t.Errorf("persisted feed info after the clean pass has Monogatari %s, want %s", got, verdictReleased)
		}
	})
	t.Run("a pass with no feed persists the list's set", func(t *testing.T) {
		store := &fakeStore{st: state.State{Mapping: shrinkMappingCache()}}
		son := sonarrWith(errors.New("tag read failed"), nil)([]sonarr.Series{frieren, second, third, book})
		if cycleOn(t.Context(), t, son, nil, store, nil) {
			t.Fatal("Cycle healthy=true, want false when the tag read fails")
		}
		if got, want := store.st.NonAiredOrderTvdbIDs, []int{monogatariTvdb}; !slices.Equal(got, want) {
			t.Errorf("persisted NonAiredOrderTvdbIDs = %v, want %v", got, want)
		}
	})
}

// exitSonarr is a tagged Sonarr pass whose tag read or named series' episode
// file or episode list reads fail, so a walk can end on each read after the
// series list. When late is set, that series' episode-file read waits for the
// fan-out to stop and then fails with lateErr, while each read failing in
// filesErr waits until it is in flight.
type exitSonarr struct {
	taggedSonarr
	tagErr      error
	lateErr     error
	filesErr    map[int]error
	episodesErr map[int]error
	started     chan struct{}
	late        int
}

func (f exitSonarr) Tags(ctx context.Context) ([]arrapi.Tag, error) {
	if f.tagErr != nil {
		return nil, f.tagErr
	}
	return f.taggedSonarr.Tags(ctx)
}

func (f exitSonarr) EpisodeFiles(ctx context.Context, seriesID int) ([]sonarr.EpisodeFile, error) {
	if f.late != 0 && seriesID == f.late {
		close(f.started)
		<-ctx.Done()
		return nil, f.lateErr
	}
	if err := f.filesErr[seriesID]; err != nil {
		if f.started != nil {
			select {
			case <-f.started:
			case <-ctx.Done():
			}
		}
		return nil, err
	}
	return f.taggedSonarr.EpisodeFiles(ctx, seriesID)
}

func (f exitSonarr) Episodes(ctx context.Context, seriesID int) ([]sonarr.Episode, error) {
	if err := f.episodesErr[seriesID]; err != nil {
		return nil, err
	}
	return f.taggedSonarr.Episodes(ctx, seriesID)
}

// Where a shutdown lands in a pass: at the series list (which then fails
// cancelled), at the tag read, the first read after the list, or on the
// walker's completion line, after Walk's last context check.
const (
	readSeries   = "series"
	readTags     = "tags"
	walkComplete = "library walk complete"
)

// shutdownAt is a Sonarr whose read named by read lands with the shutdown
// that cancels the cycle.
type shutdownAt struct {
	arrwalk.SonarrClient
	cancel context.CancelFunc
	read   string
}

func (s shutdownAt) Pin(ctx context.Context) (arrwalk.SonarrPass, error) {
	pass, err := s.SonarrClient.Pin(ctx)
	if err != nil {
		return nil, err
	}
	return cancelAt{SonarrPass: pass, cancel: s.cancel, read: s.read}, nil
}

type cancelAt struct {
	arrwalk.SonarrPass
	cancel context.CancelFunc
	read   string
}

func (p cancelAt) Series(ctx context.Context) ([]sonarr.Series, error) {
	if p.read == readSeries {
		p.cancel()
		return nil, context.Canceled
	}
	return p.SonarrPass.Series(ctx)
}

func (p cancelAt) Tags(ctx context.Context) ([]arrapi.Tag, error) {
	if p.read == readTags {
		p.cancel()
	}
	return p.SonarrPass.Tags(ctx)
}

// cancelOnMessage cancels synchronously when a record with msg is handled,
// as a SIGTERM landing at that log line.
type cancelOnMessage struct {
	slog.Handler
	cancel context.CancelFunc
	msg    string
}

func (cancelOnMessage) Enabled(context.Context, slog.Level) bool { return true }

func (h cancelOnMessage) Handle(ctx context.Context, r slog.Record) error {
	if r.Message == h.msg {
		h.cancel()
	}
	return h.Handler.Handle(ctx, r)
}

func (h cancelOnMessage) WithAttrs(attrs []slog.Attr) slog.Handler {
	return cancelOnMessage{Handler: h.Handler.WithAttrs(attrs), cancel: h.cancel, msg: h.msg}
}

func (h cancelOnMessage) WithGroup(name string) slog.Handler {
	return cancelOnMessage{Handler: h.Handler.WithGroup(name), cancel: h.cancel, msg: h.msg}
}

// pinFails is a Sonarr whose walk cannot open its pass.
type pinFails struct{ err error }

func (p pinFails) Pin(context.Context) (arrwalk.SonarrPass, error) { return nil, p.err }
