package scout

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/arrapi"
	"github.com/cplieger/seadex-scout/internal/audit"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/match"
	"github.com/cplieger/seadex-scout/internal/seadex"
	"github.com/cplieger/seadex-scout/internal/state"
	"github.com/cplieger/slogx/capture"
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
		files: map[int][]arrapi.EpisodeFile{
			7: {{SeasonNumber: 1, ReleaseGroup: "Erai-raws"}},
		},
	}
	s := New(&Deps{
		Logger:  logger,
		Store:   store,
		Library: library.NewWalker(&library.Config{Sonarr: sonarr, Logger: logger}),
		Mapping: fakeMapping{},
		SeaDex:  &fakeSeaDex{entries: seadexFrierenEntry()},
		Matcher: match.NewMatcher(notFoundAniList{}, logger),
		Auditor: audit.NewAuditor(audit.Config{SeaDexBaseURL: "https://releases.moe"}),
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
			files: map[int][]arrapi.EpisodeFile{
				7: {{SeasonNumber: 1, ReleaseGroup: "Erai-raws"}},
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
		Mapping: fakeMapping{},
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
		// A cached mapping keeps the map usable so the report reaches the
		// SeaDex arm (an unusable map is its own hard error, gated earlier).
		Store: &fakeStore{st: state.State{
			Mapping: mapping.Cache{FetchedAt: time.Now(), Records: []mapping.Record{{AniListID: 154587, Type: "TV", TvdbID: 123, SeasonTvdb: 1}}},
		}},
		Library: library.NewWalker(&library.Config{Sonarr: sonarr, Logger: logger}),
		Mapping: fakeMapping{},
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

// TestReportMappingUnusableErrors pins Report's mapping-usability gate: a
// mapping load failure with no usable cached index (NOT a StaleMapError) must
// fail the one-shot report with an error naming the map - ID matching, season
// scoping, and the not_on_seadex catalogue all depend on it, so publishing
// would contradict the whole-library audit contract.
func TestReportMappingUnusableErrors(t *testing.T) {
	logger := scoutTestLogger()
	sonarr := &fakeSonarr{series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}}}
	s := New(&Deps{
		Logger: logger,
		// Empty state + unreachable Fribb: the load fails with nothing stale
		// to fall back on, so the map is unusable (not a StaleMapError).
		Store:   &fakeStore{},
		Library: library.NewWalker(&library.Config{Sonarr: sonarr, Logger: logger}),
		Mapping: mapping.NewLoader(noNetworkClient(), "http://unused.invalid/f.json", filepath.Join(t.TempDir(), "ov.json"), time.Hour, logger),
		SeaDex:  &fakeSeaDex{entries: seadexFrierenEntry()},
	})

	_, err := s.Report(context.Background())
	if err == nil {
		t.Fatal("Report returned nil error, want a mapping-unusable error")
	}
	if !strings.Contains(err.Error(), "mapping unusable") {
		t.Errorf("error = %q, want mapping-unusable context", err.Error())
	}
}

// TestReportStaleMapWarnsAndStillAudits pins Report's stale-but-usable-map
// arm: a refresh failure that falls back to cached records (a StaleMapError)
// is degraded-but-auditable, so Report must succeed on the cached index while
// logging exactly one "report: mapping degraded" WARN carrying the structured
// stale_reason attribute (StaleMapError.LogAttrs) for Loki.
func TestReportStaleMapWarnsAndStillAudits(t *testing.T) {
	logger, recorder := capture.New()
	// Records present but fetched beyond the 1h refresh window, with the
	// Fribb URL unreachable: Load returns the cached index wrapped in a
	// *mapping.StaleMapError.
	store := &fakeStore{st: state.State{
		Mapping: mapping.Cache{FetchedAt: time.Now().Add(-2 * time.Hour), Records: []mapping.Record{{AniListID: 154587, Type: "TV", TvdbID: 123, SeasonTvdb: 1}}},
	}}
	sonarr := &fakeSonarr{
		series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}},
		files: map[int][]arrapi.EpisodeFile{
			7: {{SeasonNumber: 1, ReleaseGroup: "Erai-raws"}},
		},
	}
	s := New(&Deps{
		Logger:  logger,
		Store:   store,
		Library: library.NewWalker(&library.Config{Sonarr: sonarr, Logger: scoutTestLogger()}),
		Mapping: mapping.NewLoader(noNetworkClient(), "http://unused.invalid/f.json", filepath.Join(t.TempDir(), "ov.json"), time.Hour, scoutTestLogger()),
		SeaDex:  &fakeSeaDex{entries: seadexFrierenEntry()},
		Matcher: match.NewMatcher(notFoundAniList{}, scoutTestLogger()),
		Auditor: audit.NewAuditor(audit.Config{SeaDexBaseURL: "https://releases.moe"}),
	})

	rep, err := s.Report(context.Background())
	if err != nil {
		t.Fatalf("Report with a stale-but-usable map returned error: %v", err)
	}
	if len(rep.Rows) == 0 {
		t.Error("Report produced 0 rows, want the matched row audited from the stale cached map")
	}
	if n := recorder.CountExact("report: mapping degraded"); n != 1 {
		t.Errorf("'report: mapping degraded' WARN count = %d, want 1", n)
	}
	staleAttr := false
	for _, r := range recorder.Records() {
		if r.Message != "report: mapping degraded" {
			continue
		}
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "stale_reason" {
				staleAttr = true
				return false
			}
			return true
		})
	}
	if !staleAttr {
		t.Error("\"report: mapping degraded\" WARN carries no stale_reason attribute; StaleMapError.LogAttrs was not appended")
	}
}

// TestReportDegradedMatching pins report mode's two degraded-match arms
// (test c of mc-degradation-scoping): a transient AniList failure no longer
// aborts the one-shot report - it renders the audit with the affected entries
// listed in the incomplete-mapping section (the unaffected rows still audit,
// and the run exits through the normal success path) - while a shutdown
// mid-match still errors, since a truncated match set has no complete audit
// to render.
func TestReportDegradedMatching(t *testing.T) {
	t.Run("anilist transiently degraded renders incomplete section", func(t *testing.T) {
		logger, recorder := capture.New()
		sonarr := &fakeSonarr{series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}}}
		s := New(&Deps{
			Logger:  logger,
			Store:   &fakeStore{st: state.State{Mapping: mapping.Cache{FetchedAt: time.Now(), Records: []mapping.Record{{AniListID: 111, Type: "TV", TvdbID: 123}}}}},
			Library: library.NewWalker(&library.Config{Sonarr: sonarr, Logger: logger}),
			Mapping: fakeMapping{},
			SeaDex:  &fakeSeaDex{entries: []seadex.Entry{{AniListID: 999}}},
			Matcher: match.NewMatcher(degradedMatcherAniList{}, logger),
			Auditor: audit.NewAuditor(audit.Config{SeaDexBaseURL: "https://releases.moe"}),
		})

		rep, err := s.Report(context.Background())
		if err != nil {
			t.Fatalf("Report with a transient AniList failure returned error %v, want a rendered report with the incomplete section", err)
		}
		if len(rep.Incomplete) != 1 || rep.Incomplete[0].AniListID != 999 {
			t.Fatalf("rep.Incomplete = %+v, want the one affected entry (al_id 999)", rep.Incomplete)
		}
		if rep.Incomplete[0].SeaDexURL != "https://releases.moe/999" {
			t.Errorf("incomplete entry SeaDexURL = %q, want the releases.moe link", rep.Incomplete[0].SeaDexURL)
		}
		// The unaffected majority still audits: the Fribb-catalogued library
		// item (covered by no SeaDex match) renders as its not_on_seadex row.
		if len(rep.Rows) != 1 || rep.Rows[0].Verdict != audit.VerdictNotOnSeaDex {
			t.Errorf("rows = %+v, want the one not_on_seadex row for the unaffected library item", rep.Rows)
		}
		if n := recorder.CountExact("report: anilist degraded; affected entries listed in the incomplete section"); n != 1 {
			t.Errorf("report anilist-degraded WARN count = %d, want 1", n)
		}
	})
	t.Run("shutdown during matching still errors", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		logger := scoutTestLogger()
		sonarr := &fakeSonarr{series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}}}
		s := New(&Deps{
			Logger:  logger,
			Store:   &fakeStore{st: state.State{Mapping: mapping.Cache{FetchedAt: time.Now(), Records: []mapping.Record{{AniListID: 111, Type: "TV", TvdbID: 123}}}}},
			Library: library.NewWalker(&library.Config{Sonarr: sonarr, Logger: logger}),
			Mapping: fakeMapping{},
			SeaDex:  &fakeSeaDex{entries: []seadex.Entry{{AniListID: 999}}},
			Matcher: match.NewMatcher(&ctxCancellingAniList{cancel: cancel}, logger),
		})

		_, err := s.Report(ctx)
		if err == nil {
			t.Fatal("Report returned nil error, want a report-interrupted error")
		}
		if !strings.Contains(err.Error(), "report interrupted") {
			t.Errorf("error = %q, want report-interrupted context", err.Error())
		}
	})
}

// TestReportShutdownDuringMappingLoadNotMisattributed pins Report's half of
// the shutdown-misattribution contract: a SIGTERM landing during the report's
// Fribb refresh must neither log "report: mapping degraded" (blaming a healthy
// upstream; the WARN backs a Loki query) nor fail with "mapping unusable" -
// the report proceeds on the cached map and the cancellation surfaces from the
// SeaDex fetch instead.
func TestReportShutdownDuringMappingLoadNotMisattributed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger, recorder := capture.New()
	store := &fakeStore{st: state.State{
		Mapping: mapping.Cache{FetchedAt: time.Now().Add(-2 * time.Hour), Records: []mapping.Record{{AniListID: 111, Type: "TV", TvdbID: 123}}},
	}}
	sonarr := &fakeSonarr{series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}}}
	s := New(&Deps{
		Logger:  logger,
		Store:   store,
		Library: library.NewWalker(&library.Config{Sonarr: sonarr, Logger: scoutTestLogger()}),
		Mapping: mapping.NewLoader(&http.Client{Transport: cancellingMappingTransport{cancel: cancel}}, "http://unused.invalid/f.json", filepath.Join(t.TempDir(), "ov.json"), time.Hour, scoutTestLogger()),
		SeaDex:  &cancellingSeaDex{cancel: cancel},
	})

	_, err := s.Report(ctx)
	if err == nil {
		t.Fatal("Report returned nil error, want the cancellation surfaced")
	}
	if strings.Contains(err.Error(), "mapping unusable") {
		t.Errorf("error = %q, want the cancelled load NOT misattributed to an unusable map", err.Error())
	}
	if n := recorder.CountExact("report: mapping degraded"); n != 0 {
		t.Errorf("'report: mapping degraded' fired %d times during a shutdown, want 0 (a cancelled load is the shutdown, not a Fribb fault)", n)
	}
}
