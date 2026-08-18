package scout

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/arrapi/v2"
	"github.com/cplieger/seadex-scout/internal/arrwalk"
	"github.com/cplieger/seadex-scout/internal/audit"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/match"
	"github.com/cplieger/seadex-scout/internal/seadex"
	"github.com/cplieger/seadex-scout/internal/seadexapi"
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
		Mapping: frierenMappingCache(),
	}}

	sonarr := &fakeSonarr{
		series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}},
		files: map[int][]arrapi.EpisodeFile{
			7: {{SeasonNumber: 1, ReleaseGroup: "Erai-raws"}},
		},
	}
	s := NewReporter(&ReportDeps{
		Logger:  logger,
		Store:   store,
		Library: arrwalk.NewWalker(&arrwalk.Config{Sonarr: sonarr, Logger: logger}),
		Mapping: fakeMapping{},
		SeaDex:  &fakeSeaDex{entries: seadexFrierenEntry()},
		Matcher: match.NewMatcher(notFoundAniList{}, logger),
		Auditor: audit.NewAuditor(audit.Config{}),
	})

	rep, err := s.Report(t.Context())
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

// TestReportSummaryLineCarriesCounts pins the one-shot report's summary line:
// seadex_entries, library_items, rows, and incomplete_mappings are the
// operator's only per-run account of what the report covered, so each must
// carry its own count. The scenario separates the two pairs a swap could hide:
// seadex_entries (2) from library_items (1), and rows (1) from
// incomplete_mappings (0). library_items and rows are both 1 here, so a swap
// of THAT pair stays invisible - a second Fribb-catalogued library item would
// add its own not_on_seadex row and move both together.
func TestReportSummaryLineCarriesCounts(t *testing.T) {
	logger, recorder := capture.New()
	store := &fakeStore{st: state.State{Mapping: frierenMappingCache()}}
	sonarr := &fakeSonarr{
		series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}},
		files: map[int][]arrapi.EpisodeFile{
			7: {{SeasonNumber: 1, ReleaseGroup: "Erai-raws"}},
		},
	}
	// A second entry with no Fribb record keeps seadex_entries (2) distinct
	// from library_items (1) and rows (1); its definitive not-found answer
	// leaves incomplete_mappings at 0.
	entries := append(seadexFrierenEntry(), seadex.Entry{AniListID: 999})
	s := NewReporter(&ReportDeps{
		Logger:  logger,
		Store:   store,
		Library: arrwalk.NewWalker(&arrwalk.Config{Sonarr: sonarr, Logger: scoutTestLogger()}),
		Mapping: fakeMapping{},
		SeaDex:  &fakeSeaDex{entries: entries},
		Matcher: match.NewMatcher(notFoundAniList{}, scoutTestLogger()),
		Auditor: audit.NewAuditor(audit.Config{}),
	})

	if _, err := s.Report(t.Context()); err != nil {
		t.Fatalf("Report returned error: %v", err)
	}
	wantAttrs := map[string]string{
		"seadex_entries":      "2",
		"library_items":       "1",
		"rows":                "1",
		"incomplete_mappings": "0",
	}
	for key, want := range wantAttrs {
		if got, ok := recordAttr(recorder, "report generated", key); !ok || got != want {
			t.Errorf("'report generated' %s = %q (found=%t), want %q", key, got, ok, want)
		}
	}
}

// TestReportPartialSnapshotErrors pins Report's completeness gate: a walk that
// skipped series after episode-fetch failures (Partial=true, nil error) must
// fail the one-shot report rather than publish a successful, timestamped audit
// that silently omits skipped series. Report mode requires a complete snapshot;
// the daemon instead compares the clean subset and preserves failed items' findings.
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
	s := NewReporter(&ReportDeps{
		Logger:  logger,
		Store:   &fakeStore{},
		Library: arrwalk.NewWalker(&arrwalk.Config{Sonarr: sonarr, Logger: logger}),
	})

	_, err := s.Report(t.Context())
	if err == nil {
		t.Fatal("Report returned nil error, want a partial-snapshot error")
	}
	if !strings.Contains(err.Error(), "partial") {
		t.Errorf("error = %q, want partial-snapshot context", err.Error())
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
	s := NewReporter(&ReportDeps{
		Logger: logger,
		Store: &fakeStore{st: state.State{
			Mapping: frierenMappingCache(),
		}},
		Library: arrwalk.NewWalker(&arrwalk.Config{Sonarr: sonarr, Logger: logger}),
		Mapping: fakeMapping{},
		SeaDex:  &fakeSeaDex{},
	})

	_, err := s.Report(t.Context())
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
	s := NewReporter(&ReportDeps{
		Logger: logger,
		// A cached mapping keeps the map usable so the report reaches the
		// SeaDex arm (an unusable map is its own hard error, gated earlier).
		Store: &fakeStore{st: state.State{
			Mapping: frierenMappingCache(),
		}},
		Library: arrwalk.NewWalker(&arrwalk.Config{Sonarr: sonarr, Logger: logger}),
		Mapping: fakeMapping{},
		SeaDex:  &fakeSeaDex{err: errors.New("seadex down")},
	})

	_, err := s.Report(t.Context())
	if err == nil {
		t.Fatal("Report returned nil error, want a seadex fetch error")
	}
	if !strings.Contains(err.Error(), "seadex fetch") {
		t.Errorf("error = %q, want seadex-fetch context", err.Error())
	}
}

// cancelingSeaDex cancels the run context from inside FetchEntries and then
// fails, reproducing a shutdown that races an upstream failure whose error text
// embeds unbounded upstream bytes.
type cancelingSeaDex struct {
	cancel context.CancelFunc
	err    error
}

func (c *cancelingSeaDex) FetchEntries(context.Context, seadexapi.Options) ([]seadex.Entry, error) {
	c.cancel()
	return nil, c.err
}

func (c *cancelingSeaDex) CountWindow(context.Context, time.Time) (int, error) {
	c.cancel()
	return 0, c.err
}

// TestReportSeaDexCancellationBoundsErrorText pins Report's SeaDex arm on the
// shutdown path: the log-boundary reduction is unconditional, so a cancelled
// fetch whose error carries raw upstream bytes still yields a bounded,
// single-line error, while the wrap keeps context.Canceled matchable for main's
// WARN-not-ERROR shutdown classification.
func TestReportSeaDexCancellationBoundsErrorText(t *testing.T) {
	logger := scoutTestLogger()
	sonarr := &fakeSonarr{series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	oversized := strings.Repeat("A", 4*maxLoggedErrorBytes) + "\nsecond line"
	s := NewReporter(&ReportDeps{
		Logger: logger,
		Store: &fakeStore{st: state.State{
			Mapping: frierenMappingCache(),
		}},
		Library: arrwalk.NewWalker(&arrwalk.Config{Sonarr: sonarr, Logger: logger}),
		Mapping: fakeMapping{},
		SeaDex: &cancelingSeaDex{
			cancel: cancel,
			err:    fmt.Errorf("seadex page: %s: %w", oversized, context.Canceled),
		},
	})

	_, err := s.Report(ctx)
	if err == nil {
		t.Fatal("Report returned nil error, want a cancelled seadex fetch error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Report error = %v, want it to wrap context.Canceled", err)
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "seadex fetch: ") {
		t.Errorf("Report error = %q, want seadex-fetch stage context", msg)
	}
	if strings.ContainsAny(msg, "\n\r") {
		t.Error("Report error text contains a newline, want a single line")
	}
	if len(msg) > maxLoggedErrorBytes+128 {
		t.Errorf("Report error text is %d bytes, want it bounded near maxLoggedErrorBytes (%d)", len(msg), maxLoggedErrorBytes)
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
	s := NewReporter(&ReportDeps{
		Logger: logger,
		// Empty state + unreachable Fribb: the load fails with nothing stale
		// to fall back on, so the map is unusable (not a StaleMapError).
		Store:   &fakeStore{},
		Library: arrwalk.NewWalker(&arrwalk.Config{Sonarr: sonarr, Logger: logger}),
		Mapping: unreachableMapLoader(t, logger),
		SeaDex:  &fakeSeaDex{entries: seadexFrierenEntry()},
	})

	_, err := s.Report(t.Context())
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
	s := NewReporter(&ReportDeps{
		Logger:  logger,
		Store:   store,
		Library: arrwalk.NewWalker(&arrwalk.Config{Sonarr: sonarr, Logger: scoutTestLogger()}),
		Mapping: unreachableMapLoader(t, scoutTestLogger()),
		SeaDex:  &fakeSeaDex{entries: seadexFrierenEntry()},
		Matcher: match.NewMatcher(notFoundAniList{}, scoutTestLogger()),
		Auditor: audit.NewAuditor(audit.Config{}),
	})

	rep, err := s.Report(t.Context())
	if err != nil {
		t.Fatalf("Report with a stale-but-usable map returned error: %v", err)
	}
	if len(rep.Rows) == 0 {
		t.Error("Report produced 0 rows, want the matched row audited from the stale cached map")
	}
	if n := recorder.CountExact("report: mapping degraded"); n != 1 {
		t.Errorf("'report: mapping degraded' WARN count = %d, want 1", n)
	}
	if _, ok := recordAttr(recorder, "report: mapping degraded", "stale_reason"); !ok {
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
		s := NewReporter(&ReportDeps{
			Logger:  logger,
			Store:   &fakeStore{st: state.State{Mapping: seasonlessMappingCache()}},
			Library: arrwalk.NewWalker(&arrwalk.Config{Sonarr: sonarr, Logger: logger}),
			Mapping: fakeMapping{},
			SeaDex:  &fakeSeaDex{entries: []seadex.Entry{{AniListID: 999}}},
			Matcher: match.NewMatcher(degradedMatcherAniList{}, logger),
			Auditor: audit.NewAuditor(audit.Config{}),
		})

		rep, err := s.Report(t.Context())
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
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		logger := scoutTestLogger()
		sonarr := &fakeSonarr{series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}}}
		s := NewReporter(&ReportDeps{
			Logger:  logger,
			Store:   &fakeStore{st: state.State{Mapping: seasonlessMappingCache()}},
			Library: arrwalk.NewWalker(&arrwalk.Config{Sonarr: sonarr, Logger: logger}),
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
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	logger, recorder := capture.New()
	store := &fakeStore{st: state.State{
		Mapping: mapping.Cache{FetchedAt: time.Now().Add(-2 * time.Hour), Records: []mapping.Record{{AniListID: 111, Type: "TV", TvdbID: 123}}},
	}}
	sonarr := &fakeSonarr{series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}}}
	s := NewReporter(&ReportDeps{
		Logger:  logger,
		Store:   store,
		Library: arrwalk.NewWalker(&arrwalk.Config{Sonarr: sonarr, Logger: scoutTestLogger()}),
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

// TestReportCanceledBeforeWalkPreservesCancellation pins reportSnapshot's
// side-less walk-error branch: a context already cancelled when a no-arr walk
// reaches library.Walk's final cancellation guard must surface an error that
// still wraps context.Canceled (so main's shutdown classification logs WARN,
// not the ERROR that trips the cycle-fault alert) AND carries the "library
// walk" stage context operators read. The existing report tests cover
// arr-tagged walk failures and cancellation during matching, not this branch.
func TestReportCanceledBeforeWalkPreservesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := NewReporter(&ReportDeps{
		Logger:  scoutTestLogger(),
		Store:   &fakeStore{},
		Library: arrwalk.NewWalker(&arrwalk.Config{Logger: scoutTestLogger()}),
	})

	_, err := s.Report(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Report error = %v, want it to wrap context.Canceled", err)
	}
	if !strings.HasPrefix(err.Error(), "library walk: ") {
		t.Errorf("Report error = %q, want library-walk stage context", err)
	}
}

// TestReportSurfacesOverridesRefusalAndMappingDegraded pins that the one-shot
// report gets the same two mapping diagnostics the daemon cycle does, because
// both run the same shared loader: the l-f69 overrides-refusal ERROR (an opt-in
// file the operator's pinned mappings are inert without) and the contextual
// "report: mapping degraded" record for the failed refresh. The two are
// independent signals - a broken overrides file is an operator-config fault
// while a failed refresh is an upstream one - so a run hitting both must emit
// both.
func TestReportSurfacesOverridesRefusalAndMappingDegraded(t *testing.T) {
	logger, recorder := capture.New()
	// A stale-but-usable cached map, so the refresh is attempted (and fails at
	// the transport) rather than skipped as fresh.
	store := &fakeStore{st: state.State{
		Mapping: mapping.Cache{FetchedAt: time.Now().Add(-2 * time.Hour), Records: []mapping.Record{{AniListID: 154587, Type: "TV", TvdbID: 123, SeasonTvdb: 1}}},
	}}
	// A directory at the overrides path fails the bounded read with a
	// non-ErrNotExist error whatever the test user's privileges.
	overrides := filepath.Join(t.TempDir(), "overrides.json")
	if err := os.Mkdir(overrides, 0o750); err != nil {
		t.Fatalf("mkdir overrides dir: %v", err)
	}
	sonarr := &fakeSonarr{
		series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}},
		files: map[int][]arrapi.EpisodeFile{
			7: {{SeasonNumber: 1, ReleaseGroup: "Erai-raws"}},
		},
	}
	s := NewReporter(&ReportDeps{
		Logger:  logger,
		Store:   store,
		Library: arrwalk.NewWalker(&arrwalk.Config{Sonarr: sonarr, Logger: scoutTestLogger()}),
		Mapping: mapping.NewLoader(noNetworkClient(), "http://unused.invalid/f.json", overrides, time.Hour, logger),
		SeaDex:  &fakeSeaDex{entries: seadexFrierenEntry()},
		Matcher: match.NewMatcher(notFoundAniList{}, scoutTestLogger()),
		Auditor: audit.NewAuditor(audit.Config{}),
	})

	if _, err := s.Report(t.Context()); err != nil {
		t.Fatalf("Report returned error: %v (neither a refused overrides file nor a stale map blocks the report)", err)
	}
	if n := recorder.CountLevel(slog.LevelError, "overrides.json unreadable"); n != 1 {
		t.Errorf("overrides-refusal ERROR count in report mode = %d, want 1; logs = %v", n, recorder.Messages())
	}
	if n := recorder.CountExact("report: mapping degraded"); n != 1 {
		t.Errorf("'report: mapping degraded' fired %d times, want 1 (the failed refresh is still reported); logs = %v", n, recorder.Messages())
	}
}

// TestReportWarnsWhenTheWalkShrankBelowHalf pins the one-shot report's shrink
// disclosure, which is the only thing that stops a silently-incomplete artifact.
//
// The daemon GATES its whole compare on this exact shape (handleLibraryGate's
// shrink guard): a non-failed walk retaining under half the last persisted
// snapshot is a suspicious truncation, not a real change. The report cannot
// gate - it is read-only and it is the operator's fallback view while the cycle
// is stuck - so it renders and must SAY so instead. Without the line the
// timestamped report omits every missing series and reads as authoritative,
// which is the same incompleteness reportSnapshot refuses a partial snapshot
// over. The complementary case matters as much: a prior-snapshot-less run (a
// report-only deployment never persists one) has no baseline and must stay
// quiet rather than guess.
func TestReportWarnsWhenTheWalkShrankBelowHalf(t *testing.T) {
	const shrinkWarn = "report: library walk shrank below half the last persisted snapshot; " +
		"the audit covers the smaller library - inspect the arrs and arr_tags"
	// priorItems is the persisted baseline; the walk below returns ONE series,
	// so 1*2 < 4 trips degradation.Shrunk while 1*2 >= 2 does not.
	priorItems := func(n int) []library.Item {
		items := make([]library.Item, 0, n)
		for i := range n {
			items = append(items, library.Item{Arr: library.ArrSonarr, ArrID: i + 1, TvdbID: 900 + i})
		}
		return items
	}
	for name, tc := range map[string]struct {
		prior    []library.Item
		wantWarn int
	}{
		"a walk retaining under half the prior snapshot discloses it": {priorItems(4), 1},
		"a walk retaining half or more stays quiet":                   {priorItems(2), 0},
		"no prior snapshot means no baseline to compare against":      {nil, 0},
	} {
		t.Run(name, func(t *testing.T) {
			logger, recorder := capture.New()
			store := &fakeStore{st: state.State{
				Mapping: frierenMappingCache(),
				Library: library.Snapshot{Items: tc.prior},
			}}
			sonarr := &fakeSonarr{
				series: []arrapi.Series{{ID: 7, Title: "Frieren", TvdbID: 123, Year: 2023}},
				files: map[int][]arrapi.EpisodeFile{
					7: {{SeasonNumber: 1, ReleaseGroup: "Erai-raws"}},
				},
			}
			s := NewReporter(&ReportDeps{
				Logger:  logger,
				Store:   store,
				Library: arrwalk.NewWalker(&arrwalk.Config{Sonarr: sonarr, Logger: scoutTestLogger()}),
				Mapping: fakeMapping{},
				SeaDex:  &fakeSeaDex{entries: seadexFrierenEntry()},
				Matcher: match.NewMatcher(notFoundAniList{}, scoutTestLogger()),
				Auditor: audit.NewAuditor(audit.Config{}),
			})

			rep, err := s.Report(t.Context())
			if err != nil {
				t.Fatalf("Report returned error: %v", err)
			}
			// The report must still be PRODUCED either way: it is the fallback
			// view, so disclosing the shrink must never become a refusal.
			if len(rep.Rows) == 0 {
				t.Error("Report produced 0 rows; a disclosed shrink must not withhold the artifact")
			}
			if n := recorder.CountExact(shrinkWarn); n != tc.wantWarn {
				t.Errorf("shrink WARN count = %d, want %d: %v", n, tc.wantWarn, recorder.Messages())
			}
			if tc.wantWarn == 0 {
				return
			}
			for key, want := range map[string]string{"items": "1", "prior_items": "4"} {
				if got, ok := recordAttr(recorder, shrinkWarn, key); !ok || got != want {
					t.Errorf("shrink WARN %s = %q (found=%t), want %q; the operator sizes the gap from these two counts",
						key, got, ok, want)
				}
			}
		})
	}
}
