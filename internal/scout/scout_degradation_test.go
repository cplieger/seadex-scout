package scout

import (
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/arrapi/v2"
	"github.com/cplieger/seadex-scout/internal/anilist"
	"github.com/cplieger/seadex-scout/internal/arrwalk"
	"github.com/cplieger/seadex-scout/internal/compare"
	"github.com/cplieger/seadex-scout/internal/degradation"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/match"
	"github.com/cplieger/seadex-scout/internal/notify"
	"github.com/cplieger/seadex-scout/internal/seadex"
	"github.com/cplieger/seadex-scout/internal/state"
	"github.com/cplieger/slogx/capture"
)

// twoRecordMappingCache returns a fresh mapping cache holding the Frieren
// record plus a second seasoned TV record (AniList 222 / TVDB 124), for the
// degradation tests that need a second comparable series.
func twoRecordMappingCache() mapping.Cache {
	return mapping.Cache{FetchedAt: time.Now(), Records: []mapping.Record{
		{AniListID: 154587, Type: "TV", TvdbID: 123, SeasonTvdb: 1},
		{AniListID: 222, Type: "TV", TvdbID: 124, SeasonTvdb: 1},
	}}
}

// secondSeaDexEntry returns a curated best-release entry for AniList 222, the
// mapping-side twin of twoRecordMappingCache's second record.
func secondSeaDexEntry() seadex.Entry {
	return seadex.Entry{
		AniListID: 222,
		Torrents: []seadex.Torrent{{
			ReleaseGroup: "SubsPlease",
			Tracker:      "Nyaa",
			InfoHash:     "def",
			URL:          "https://nyaa.si/view/2",
			IsBest:       true,
			Files:        []seadex.File{{Name: "Second Show S01E01 1080p.mkv", Length: 1}},
		}},
	}
}

// TestCyclePartialWalkEscalatesAfterRepeatedPartialWalks pins the persisted
// partial-walk streak and its escalation: below the threshold a completed
// partial cycle only advances the counter (the per-cycle "reason=partial-walk"
// degraded line is the whole signal), and on the threshold cycle the same site
// escalates to ERROR - firing the SeadexScoutCycleError Loki rule - because a
// permanently failing series never self-heals and, inside the cold-start
// window, silences every finding. A completed WHOLE walk resets the streak, so
// a recovered arr starts fresh instead of escalating on its next blip.
func TestCyclePartialWalkEscalatesAfterRepeatedPartialWalks(t *testing.T) {
	tests := []struct {
		name        string
		priorStreak int
		wantError   bool
	}{
		{name: "below threshold does not escalate", priorStreak: degradation.ReconcileEscalationThreshold - 2, wantError: false},
		{name: "at threshold escalates to ERROR", priorStreak: degradation.ReconcileEscalationThreshold - 1, wantError: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logger, recorder := capture.New()
			store := &fakeStore{st: state.State{
				Mapping:      twoRecordMappingCache(),
				PartialWalks: tc.priorStreak,
			}}
			sonarr := &flakySonarr{
				series: []arrapi.Series{
					{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023},
					{ID: 8, Title: "Second Show", TvdbID: 124, Year: 2024},
				},
				files: map[int][]arrapi.EpisodeFile{
					7: {{SeasonNumber: 1, ReleaseGroup: "Erai-raws"}},
					8: {{SeasonNumber: 1, ReleaseGroup: "Erai-raws"}},
				},
				failEpisodes: map[int]bool{8: true},
			}
			s := New(&Deps{
				Logger:       logger,
				Store:        store,
				Library:      arrwalk.NewWalker(&arrwalk.Config{Sonarr: sonarr, Logger: scoutTestLogger()}),
				Mapping:      fakeMapping{},
				SeaDex:       &fakeSeaDex{entries: append(seadexFrierenEntry(), secondSeaDexEntry())},
				Matcher:      match.New(notFoundAniList{}, scoutTestLogger()),
				Comparer:     compare.New(compare.Config{}),
				Notifier:     notify.NewNotifier(scoutTestLogger(), nil),
				AniListStats: aniStatsFn(anilist.NewClient(noNetworkClient(), "http://unused.invalid/gql", anilist.WithRate(1), anilist.WithLogger(scoutTestLogger()))),
			})

			if healthy := s.Cycle(t.Context()); !healthy {
				t.Fatal("partial-walk cycle healthy=false, want true (a partial walk is degraded, not unhealthy)")
			}
			if got := store.st.PartialWalks; got != tc.priorStreak+1 {
				t.Errorf("persisted PartialWalks = %d, want %d (the streak must advance and persist)", got, tc.priorStreak+1)
			}
			errCount := recorder.CountLevel(slog.LevelError, "library walk partial repeatedly")
			if tc.wantError && errCount != 1 {
				t.Errorf("escalated ERROR count = %d, want exactly 1 at the threshold (single log site)", errCount)
			}
			if !tc.wantError && errCount != 0 {
				t.Errorf("below-threshold ERROR count = %d, want 0 (a partial blip must not alert)", errCount)
			}
			wantStreak := strconv.Itoa(degradation.ReconcileEscalationThreshold)
			if got, ok := recorder.AttrValue("library walk partial repeatedly", "consecutive_partial_walks"); tc.wantError && (!ok || got != wantStreak) {
				t.Errorf("escalation streak attr = %q ok=%v, want %q (the ERROR must carry the up-to-date streak)", got, ok, wantStreak)
			}
			if reasons := degradedReasons(recorder); len(reasons) != 1 || reasons[0] != "partial-walk" {
				t.Errorf("degraded reasons = %v, want [partial-walk]", reasons)
			}

			// A completed WHOLE walk ends the streak: the failing series
			// recovers, so the next blip starts counting from zero.
			sonarr.failEpisodes = nil
			if healthy := s.Cycle(t.Context()); !healthy {
				t.Fatal("recovered cycle healthy=false, want true on a complete walk")
			}
			if got := store.st.PartialWalks; got != 0 {
				t.Errorf("persisted PartialWalks after a complete walk = %d, want 0 (a whole walk resets the streak)", got)
			}
			if n := recorder.CountExact("cycle complete"); n != 1 {
				t.Errorf("'cycle complete' count = %d, want 1 (the recovered cycle is no longer degraded)", n)
			}
		})
	}
}

// TestCycleSeaDexFailureSanitizesLoggedErrorAtBothSites pins the emit-boundary
// reduction on the SeaDex failure path: the client's error can embed raw
// upstream bytes (the keyset-cursor arms format rejected created/id values with
// %q, bounded only by the 48 MB page cap), so BOTH log sites that carry it -
// the seadex-fetch-failed WARN and the "cycle degraded" completion line - must
// render it bounded, single-line and rune-sanitized. An unsanitized site would
// see the record dropped by Loki or make the 256 MiB process pay a multi-
// megabyte render every degraded cycle. The honest part of the message survives.
func TestCycleSeaDexFailureSanitizesLoggedErrorAtBothSites(t *testing.T) {
	logger, recorder := capture.New()
	hostile := "upstream said: " + strings.Repeat("A\u0007\u202e", 5000) + "\nrejected id"
	store := &fakeStore{st: state.State{
		Mapping: frierenMappingCache(),
	}}
	sonarr := &fakeSonarr{series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}}}
	s := New(&Deps{
		Notifier: notify.NewNotifier(logger, nil),
		Logger:   logger,
		Store:    store,
		Library:  arrwalk.NewWalker(&arrwalk.Config{Sonarr: sonarr, Logger: scoutTestLogger()}),
		Mapping:  fakeMapping{},
		SeaDex:   &fakeSeaDex{err: errors.New(hostile)},
	})

	if healthy := s.Cycle(t.Context()); !healthy {
		t.Fatal("Cycle healthy=false, want true (a SeaDex outage is degraded, not unhealthy)")
	}
	sites := []struct {
		msgSub string
		what   string
	}{
		{msgSub: "seadex fetch failed", what: "the seadex-failure WARN"},
		{msgSub: "cycle degraded", what: "the degraded completion line"},
	}
	for _, site := range sites {
		got, ok := recorder.AttrValue(site.msgSub, attrError)
		if !ok {
			t.Errorf("%s carries no %q attr, want the sanitized upstream error", site.what, attrError)
			continue
		}
		if len(got) > maxLoggedErrorBytes+100 {
			t.Errorf("%s error value = %d bytes, want bounded near maxLoggedErrorBytes (%d)", site.what, len(got), maxLoggedErrorBytes)
		}
		if strings.ContainsAny(got, "\n\r\u0007") || strings.Contains(got, "\u202e") {
			t.Errorf("%s error value carries a control/bidi rune (prefix %q)", site.what, got[:min(len(got), 60)])
		}
		if !strings.HasPrefix(got, "upstream said: ") {
			t.Errorf("%s error value lost the honest message prefix (prefix %q)", site.what, got[:min(len(got), 60)])
		}
	}
}

// TestCycleTagFilterEmptiedSideClosesDegraded pins the completion-line verdict
// for a side arr_tags filtering emptied: the walk succeeded, so the cycle stays
// HEALTHY (a config typo must not restart-loop the container), but it closes
// "cycle degraded" with reason=tags-emptied-side instead of "cycle complete".
// Without it the steady state was a daemon watching nothing while every cycle
// read fully successful: the shrink guard cannot cover this on a first-ever boot
// (there is no prior count to have shrunk from) and stops covering it once the
// guard's bounded tolerance accepts the smaller library, so the walker's
// per-cycle WARN was the only lasting signal. A walk whose filter keeps
// something still closes clean, so the arm cannot invert.
func TestCycleTagFilterEmptiedSideClosesDegraded(t *testing.T) {
	newScout := func(logger *slog.Logger, sonarr *fakeSonarr, include []string) *Scout {
		return New(&Deps{
			Logger: logger,
			Store: &fakeStore{st: state.State{
				Mapping: twoRecordMappingCache(),
			}},
			Library: arrwalk.NewWalker(&arrwalk.Config{
				Sonarr: sonarr, IncludeTags: include, Logger: scoutTestLogger(),
			}),
			Mapping:      fakeMapping{},
			SeaDex:       &fakeSeaDex{entries: seadexFrierenEntry()},
			Matcher:      match.New(notFoundAniList{}, scoutTestLogger()),
			Comparer:     compare.New(compare.Config{}),
			Notifier:     notify.NewNotifier(scoutTestLogger(), nil),
			AniListStats: aniStatsFn(anilist.NewClient(noNetworkClient(), "http://unused.invalid/gql", anilist.WithRate(1), anilist.WithLogger(scoutTestLogger()))),
		})
	}
	// Sonarr lists a series but carries no tags at all, so the configured
	// include label resolves to nothing and the side contributes zero items.
	sonarr := func() *fakeSonarr {
		return &fakeSonarr{
			series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}},
			files:  map[int][]arrapi.EpisodeFile{7: {{SeasonNumber: 1, ReleaseGroup: "Erai-raws"}}},
		}
	}

	logger, recorder := capture.New()
	if healthy := newScout(logger, sonarr(), []string{"anime"}).Cycle(t.Context()); !healthy {
		t.Fatal("emptied-side cycle healthy=false, want true (the walk succeeded; a dead filter must not restart-loop the container)")
	}
	if reasons := degradedReasons(recorder); len(reasons) != 1 || reasons[0] != "tags-emptied-side" {
		t.Errorf("degraded reasons = %v, want [tags-emptied-side]", reasons)
	}
	if n := recorder.CountExact("cycle complete"); n != 0 {
		t.Errorf("'cycle complete' count = %d, want 0 (a side watching nothing must not read fully successful)", n)
	}

	cleanLogger, cleanRecorder := capture.New()
	if healthy := newScout(cleanLogger, sonarr(), nil).Cycle(t.Context()); !healthy {
		t.Fatal("unfiltered cycle healthy=false, want true")
	}
	if n := cleanRecorder.CountExact("cycle complete"); n != 1 {
		t.Errorf("unfiltered 'cycle complete' count = %d, want 1 (no filter, nothing emptied); reasons = %v",
			n, degradedReasons(cleanRecorder))
	}
}

// TestWarnCatalogueLinkQuality pins the catalogue-wide tracker-link
// diagnostics the orchestrator owns (moved here from the seadex client with the
// diagnostic itself, l-f156): a torrent whose URL the publisher refuses -
// omitted/empty, a foreign host under a trusted tracker label, or a tracker
// this build does not know - is counted into ONE aggregate WARN so a tracker
// host migration or schema drift that strips every release link is alertable
// from Loki, while the unknown-tracker subset gets its own line naming the
// remedy only a seadex-scout release can apply. A usable canonical-host URL
// must count in neither.
func TestWarnCatalogueLinkQuality(t *testing.T) {
	entries := []seadex.Entry{
		{AniListID: 1, Torrents: []seadex.Torrent{
			{Tracker: "Nyaa", URL: "https://evil.example/view/1"},
			{Tracker: "SomeRandomTracker", URL: "https://example.com/x"},
			{Tracker: "Nyaa", URL: ""},
		}},
		{AniListID: 2, Torrents: []seadex.Torrent{
			{Tracker: "Nyaa", URL: "https://nyaa.si/view/123"},
		}},
	}

	logger, recorder := capture.New()
	New(&Deps{Logger: logger}).warnCatalogueLinkQuality(entries)

	const (
		aggregate = "seadex torrent URLs unusable; affected findings and feed items carry no release link"
		unknown   = "seadex trackers unknown to this build; add them to seadex-scout's tracker table to publish their links"
	)
	for _, tc := range []struct {
		msg       string
		wantCount int64
	}{
		{aggregate, 3},
		{unknown, 1},
	} {
		if got := recorder.CountExact(tc.msg); got != 1 {
			t.Errorf("%q logged %d times, want 1 aggregate line", tc.msg, got)
			continue
		}
		var count, entryCount int64
		for _, r := range recorder.Records() {
			if r.Message != tc.msg {
				continue
			}
			r.Attrs(func(a slog.Attr) bool {
				switch a.Key {
				case "count":
					count = a.Value.Int64()
				case "entries":
					entryCount = a.Value.Int64()
				}
				return true
			})
		}
		if count != tc.wantCount || entryCount != 2 {
			t.Errorf("%q carries count=%d entries=%d, want count=%d entries=2",
				tc.msg, count, entryCount, tc.wantCount)
		}
	}
}

// TestWarnCatalogueLinkQualitySilentWhenEveryLinkPublishes pins the OFF state
// of both alert-stable lines: a catalogue whose every torrent publishes - and
// the empty catalogue a failed fetch returns, which the client used to gate on
// success - must emit neither, so the Loki alerts keyed on them cannot fire on
// a clean cycle or on an upstream outage.
func TestWarnCatalogueLinkQualitySilentWhenEveryLinkPublishes(t *testing.T) {
	for name, entries := range map[string][]seadex.Entry{
		"clean": {{AniListID: 1, Torrents: []seadex.Torrent{
			{Tracker: "Nyaa", URL: "https://nyaa.si/view/1"},
			{Tracker: "AB", URL: "/torrents.php?id=1&torrentid=2"},
		}}},
		"failed fetch returns no entries": nil,
	} {
		t.Run(name, func(t *testing.T) {
			logger, recorder := capture.New()
			New(&Deps{Logger: logger}).warnCatalogueLinkQuality(entries)
			for _, msg := range []string{
				"seadex torrent URLs unusable; affected findings and feed items carry no release link",
				"seadex trackers unknown to this build; add them to seadex-scout's tracker table to publish their links",
			} {
				if got := recorder.CountExact(msg); got != 0 {
					t.Errorf("logged %q %d times, want 0", msg, got)
				}
			}
		})
	}
}
