package report

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/seadex-scout/internal/compare"
	"github.com/cplieger/slogx/capture"
)

func newCapturedReporter() (*Reporter, *capture.Recorder) {
	logger, recorder := capture.New()
	return NewReporter(logger), recorder
}

func testFinding(key, title string) compare.Finding {
	return compare.Finding{
		Kind:             "encode",
		Reason:           "encoder marker: x265",
		Arr:              "sonarr",
		CurrentGroup:     "erai-raws",
		RecommendedGroup: "SubsPlease",
		Tracker:          "Nyaa",
		Title:            title,
		Resolution:       "1080p",
		Severity:         compare.SevWarn,
		Codec:            "x265",
		ReleaseURL:       "https://nyaa.si/view/1",
		InfoHash:         "hash-" + key,
		DedupeKey:        key,
		Status:           compare.StatusBetter,
		AniListID:        154587,
		Links: []compare.ReleaseLink{
			{Tracker: "Nyaa", URL: "https://nyaa.si/view/1"},
			{Tracker: "AB", URL: "https://animebytes.tv/torrents.php?id=1"},
		},
		DualAudio: true,
	}
}

// storedTestFinding is testFinding projected onto the persisted dedupe
// record, for building prior-state maps the way a previous cycle would have.
func storedTestFinding(key, title string) StoredFinding {
	f := testFinding(key, title)
	return storedFinding(&f)
}

func TestReporterBaselineSeedsWithoutFindingNotification(t *testing.T) {
	reporter, recorder := newCapturedReporter()
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	finding := testFinding("same", "Frieren")

	got := reporter.Baseline([]compare.Finding{finding}, now)

	alert, ok := got["same"]
	if !ok {
		t.Fatalf("Baseline did not store finding under its dedupe key: %+v", got)
	}
	if !alert.AlertedAt.Equal(now) {
		t.Errorf("AlertedAt = %s, want %s", alert.AlertedAt, now)
	}
	if alert.Finding.Title != "Frieren" {
		t.Errorf("stored finding title = %q, want Frieren", alert.Finding.Title)
	}
	if got := recorder.Count("better release available"); got != 0 {
		t.Errorf("Baseline emitted %d finding notifications, want 0", got)
	}
	if got := recorder.Count("cold start: findings baselined without notifying"); got != 1 {
		t.Errorf("Baseline cold-start summary count = %d, want 1", got)
	}
}

func TestReporterReportSuppressesExistingAndEmitsNewAndResolved(t *testing.T) {
	reporter, recorder := newCapturedReporter()
	oldTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	prior := map[string]Alerted{
		"same": {AlertedAt: oldTime, Finding: storedTestFinding("same", "Frieren")},
		"old":  {AlertedAt: oldTime, Finding: storedTestFinding("old", "Old Title")},
	}

	current := reporter.Report([]compare.Finding{
		testFinding("same", "Frieren"),
		testFinding("new", "Bocchi"),
	}, prior, nil, now)

	if !current["same"].AlertedAt.Equal(oldTime) {
		t.Errorf("suppressed finding AlertedAt = %s, want original %s", current["same"].AlertedAt, oldTime)
	}
	if !current["new"].AlertedAt.Equal(now) {
		t.Errorf("new finding AlertedAt = %s, want %s", current["new"].AlertedAt, now)
	}
	if _, ok := current["old"]; ok {
		t.Errorf("resolved finding still present in current state: %+v", current["old"])
	}
	// CountExact: this msg is pinned by the Loki better-release alert rule, so
	// a superstring message must fail here, not false-pass a substring Count.
	if got := recorder.CountExact("better release available"); got != 1 {
		t.Errorf("new finding notification count = %d, want 1", got)
	}
	if got := recorder.Count("finding resolved"); got != 1 {
		t.Errorf("resolved notification count = %d, want 1", got)
	}
	if got := recorder.Count("findings reported"); got != 1 {
		t.Errorf("summary count = %d, want 1", got)
	}
}

// TestReporterReportPreservesFailedItemsFindings pins the partial-walk
// resolution scoping: a prior finding whose AniList ID is in failedItems (its
// item's episode fetch failed this cycle, so it was excluded from the compare)
// must be carried forward unresolved - kept in the returned dedupe state with
// its original alert time and WITHOUT a "finding resolved" line - while a
// prior finding for a cleanly-compared item that produced no finding is
// resolved as usual.
func TestReporterReportPreservesFailedItemsFindings(t *testing.T) {
	reporter, recorder := newCapturedReporter()
	oldTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	failedFinding := testFinding("failed-item", "Broken Series")
	failedFinding.AniListID = 222
	resolvable := testFinding("clean-gone", "Aligned Now")
	resolvable.AniListID = 333
	prior := map[string]Alerted{
		"failed-item": {AlertedAt: oldTime, Finding: storedFinding(&failedFinding)},
		"clean-gone":  {AlertedAt: oldTime, Finding: storedFinding(&resolvable)},
	}

	current := reporter.Report(nil, prior, map[int]struct{}{222: {}}, time.Now())

	preserved, ok := current["failed-item"]
	if !ok {
		t.Fatalf("failed item's prior finding was resolved, want it preserved: %+v", current)
	}
	if !preserved.AlertedAt.Equal(oldTime) {
		t.Errorf("preserved AlertedAt = %s, want original %s", preserved.AlertedAt, oldTime)
	}
	if _, ok := current["clean-gone"]; ok {
		t.Errorf("clean item's stale finding still present, want it resolved: %+v", current)
	}
	if got := recorder.CountExact("finding resolved"); got != 1 {
		t.Errorf("resolved notification count = %d, want 1 (the clean item only)", got)
	}
}

// TestFindingLogSanitizesArrURL pins the logging trust boundary on the arr
// deep-link: a base URL configured with reverse-proxy Basic Auth credentials
// and a query token must never cross into the emitted slog attributes, while
// an ordinary credential-free deep-link passes through unchanged.
func TestFindingLogSanitizesArrURL(t *testing.T) {
	reporter, recorder := newCapturedReporter()
	finding := testFinding("cred", "Frieren")
	finding.ArrURL = "https://user:password@sonarr.example/series/frieren?token=secret#frag"

	reporter.Report([]compare.Finding{finding}, nil, nil, time.Now())

	var got string
	for _, rec := range recorder.Records() {
		if rec.Message != "better release available" {
			continue
		}
		rec.Attrs(func(a slog.Attr) bool {
			if a.Key == "arr_url" {
				got, _ = a.Value.Any().(string)
			}
			return true
		})
	}
	if got != "https://sonarr.example/series/frieren" {
		t.Errorf("logged arr_url = %q, want credentials, query, and fragment stripped", got)
	}
}

// TestStoredFindingTrimsToResolutionFields pins the dedupe record's
// persistence contract: the record persists exactly the fields the resolution
// path reads back (emitResolved's line plus the failed-item preservation
// keyed on AniListID), so everything else in a Finding - including a
// credentialed ArrURL, whose on-disk sanitization the trim made moot - never
// lands in state.json. All three storage sites project identically: a new
// finding, a suppressed (already-alerted) finding, and a cold-start baseline.
func TestStoredFindingTrimsToResolutionFields(t *testing.T) {
	reporter, _ := newCapturedReporter()
	finding := testFinding("cred", "Frieren")
	finding.ArrURL = "https://user:password@sonarr.example/series/frieren?token=secret#frag"
	finding.Season = 2
	now := time.Now()
	want := StoredFinding{
		Arr:              "sonarr",
		CurrentGroup:     "erai-raws",
		RecommendedGroup: "SubsPlease",
		Title:            "Frieren",
		Status:           compare.StatusBetter,
		AniListID:        154587,
		Season:           2,
	}

	current := reporter.Report([]compare.Finding{finding}, nil, nil, now)
	if got := current["cred"].Finding; got != want {
		t.Errorf("new-finding stored record = %+v, want %+v", got, want)
	}

	suppressed := reporter.Report([]compare.Finding{finding}, current, nil, now)
	if got := suppressed["cred"].Finding; got != want {
		t.Errorf("suppressed-finding stored record = %+v, want %+v", got, want)
	}

	baseline := reporter.Baseline([]compare.Finding{finding}, now)
	if got := baseline["cred"].Finding; got != want {
		t.Errorf("baselined stored record = %+v, want %+v", got, want)
	}
}

// TestAlertedDecodesLegacyFullFindingRecord pins the trimmed record's
// state-file compatibility: a dedupe record persisted BEFORE the trim carries
// the full sanitized Finding (arr_url, links, dedupe_key, severity, ...), and
// decoding it into the trimmed StoredFinding must succeed with every
// read-back field intact and the extra fields ignored - an upgrade keeps
// dedupe continuity (keys and alert times) without a state reset. A json tag
// drifting from compare.Finding's would silently blank a resolution field on
// every upgraded install; this fails instead.
func TestAlertedDecodesLegacyFullFindingRecord(t *testing.T) {
	legacy := `{
		"alerted_at": "2026-01-01T00:00:00Z",
		"finding": {
			"kind": "encode",
			"classification_reason": "encoder marker: x265",
			"arr": "sonarr",
			"current_group": "erai-raws",
			"recommended_group": "SubsPlease",
			"tracker": "Nyaa",
			"title": "Frieren",
			"resolution": "1080p",
			"severity": "warn",
			"codec": "x265",
			"release_url": "https://nyaa.si/view/1",
			"arr_url": "https://sonarr.example/series/frieren",
			"info_hash": "hash-1",
			"dedupe_key": "legacy-key",
			"status": "better_release",
			"recommended_groups": ["SubsPlease"],
			"links": [{"tracker": "Nyaa", "url": "https://nyaa.si/view/1"}],
			"al_id": 154587,
			"season": 2,
			"dual_audio": true
		}
	}`
	var got Alerted
	if err := json.Unmarshal([]byte(legacy), &got); err != nil {
		t.Fatalf("decoding a pre-trim dedupe record: %v", err)
	}
	want := StoredFinding{
		Arr:              "sonarr",
		CurrentGroup:     "erai-raws",
		RecommendedGroup: "SubsPlease",
		Title:            "Frieren",
		Status:           compare.StatusBetter,
		AniListID:        154587,
		Season:           2,
	}
	if got.Finding != want {
		t.Errorf("decoded record = %+v, want %+v", got.Finding, want)
	}
	if wantAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC); !got.AlertedAt.Equal(wantAt) {
		t.Errorf("decoded AlertedAt = %s, want %s", got.AlertedAt, wantAt)
	}
}

// TestReporterUnverifiableFindingIsInfoNotBetterRelease pins the alert-rule
// safety of the unverifiable status: the SeadexScoutBetterReleaseFound Loki
// rule counts only msg="better release available" warn lines, so an
// unverifiable finding must emit ONE line at INFO with its own message and
// contribute zero warn-level better-release lines - and, like every finding,
// it dedupes across cycles by its key (the second Report emits nothing new).
func TestReporterUnverifiableFindingIsInfoNotBetterRelease(t *testing.T) {
	reporter, recorder := newCapturedReporter()
	finding := testFinding("unv", "Unknown Evidence")
	finding.Severity = compare.SevInfo
	finding.Status = compare.StatusUnverifiable

	prior := reporter.Report([]compare.Finding{finding}, nil, nil, time.Now())
	reporter.Report([]compare.Finding{finding}, prior, nil, time.Now())

	if got := recorder.CountExact("release group unverifiable, manual review"); got != 1 {
		t.Errorf("unverifiable notification count across two cycles = %d, want 1 (dedupe by identity)", got)
	}
	if got := recorder.CountExact("better release available"); got != 0 {
		t.Errorf("better-release line count = %d, want 0 (the alert rule must not fire)", got)
	}
	for _, rec := range recorder.Records() {
		if rec.Message != "release group unverifiable, manual review" {
			continue
		}
		if rec.Level != slog.LevelInfo {
			t.Errorf("unverifiable finding emitted at %s, want INFO", rec.Level)
		}
	}
}

// TestMessage maps every finding status to its human-facing slog message,
// pinning the msg= text that Loki alert rules key on. The default arm covers
// an unmapped status.
func TestMessage(t *testing.T) {
	cases := []struct {
		name   string
		status compare.Status
		want   string
	}{
		{name: "better", status: compare.StatusBetter, want: "better release available"},
		{name: "mixed group", status: compare.StatusMixedGroup, want: "series spans multiple release groups, manual review"},
		{name: "incomplete", status: compare.StatusIncomplete, want: "SeaDex entry is incomplete"},
		{name: "theoretical", status: compare.StatusTheoretical, want: "SeaDex lists a theoretical best only"},
		{name: "unverifiable", status: compare.StatusUnverifiable, want: "release group unverifiable, manual review"},
		{name: "unmapped status", status: compare.Status("unmapped_status"), want: "seadex finding"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := message(tc.status); got != tc.want {
				t.Errorf("message(%q) = %q, want %q", tc.status, got, tc.want)
			}
		})
	}
}

// TestReporterEmitLevelFollowsSeverity pins the severity-to-level mapping the
// Loki alert rules key on: a SevWarn finding must emit at WARN (the
// SeadexScoutBetterReleaseFound rule filters level="WARN") and a SevInfo
// finding at INFO. The existing tests count messages only, so a flipped level
// would silently break every shipped alert without failing a test.
func TestReporterEmitLevelFollowsSeverity(t *testing.T) {
	reporter, recorder := newCapturedReporter()
	warn := testFinding("w", "Warn Title") // testFinding severity is SevWarn
	info := testFinding("i", "Info Title")
	info.Severity = compare.SevInfo
	info.Status = compare.StatusIncomplete

	reporter.Report([]compare.Finding{warn, info}, nil, nil, time.Now())

	sawWarn, sawInfo := false, false
	for _, rec := range recorder.Records() {
		switch rec.Message {
		case "better release available":
			sawWarn = true
			if rec.Level != slog.LevelWarn {
				t.Errorf("SevWarn finding emitted at %s, want WARN (the Loki alert filters level=WARN)", rec.Level)
			}
		case "SeaDex entry is incomplete":
			sawInfo = true
			if rec.Level != slog.LevelInfo {
				t.Errorf("SevInfo finding emitted at %s, want INFO", rec.Level)
			}
		}
	}
	if !sawWarn || !sawInfo {
		t.Fatalf("expected both finding lines emitted, saw warn=%v info=%v", sawWarn, sawInfo)
	}
}

// TestFindingLineCarriesDocumentedAttrs pins the finding line's attribute
// contract: the README and steering doc document the exact keys the Loki
// dashboards and alert annotations key on (title, al_id, arr, current_group,
// recommended_group, tracker, resolution, kind, classification_reason,
// release_url, release_urls, plus the split nyaa_url/ab_url, info_hash,
// seadex_tags, and status). A silently renamed or dropped key breaks every
// dashboard without failing a test; this asserts the full rendered set for
// one warn finding, which also gives joinLinks its behavioral assertion.
func TestFindingLineCarriesDocumentedAttrs(t *testing.T) {
	reporter, recorder := newCapturedReporter()
	reporter.Report([]compare.Finding{testFinding("k1", "Frieren")}, nil, nil, time.Now())

	want := map[string]string{
		"title":                 "Frieren",
		"arr":                   "sonarr",
		"current_group":         "erai-raws",
		"recommended_group":     "SubsPlease",
		"tracker":               "Nyaa",
		"resolution":            "1080p",
		"codec":                 "x265",
		"kind":                  "encode",
		"classification_reason": "encoder marker: x265",
		"release_url":           "https://nyaa.si/view/1",
		"release_urls":          "Nyaa=https://nyaa.si/view/1 AB=https://animebytes.tv/torrents.php?id=1",
		"nyaa_url":              "https://nyaa.si/view/1",
		"ab_url":                "https://animebytes.tv/torrents.php?id=1",
		"info_hash":             "hash-k1",
		"seadex_tags":           "best · encode · 1080p · dual-audio",
		"status":                "better_release",
	}
	got := map[string]string{}
	var alID int64
	for _, rec := range recorder.Records() {
		if rec.Message != "better release available" {
			continue
		}
		rec.Attrs(func(a slog.Attr) bool {
			if a.Key == "al_id" {
				alID = a.Value.Int64()
				return true
			}
			if s, ok := a.Value.Any().(string); ok {
				got[a.Key] = s
			}
			return true
		})
	}
	if alID != 154587 {
		t.Errorf("al_id = %d, want 154587", alID)
	}
	for key, w := range want {
		if got[key] != w {
			t.Errorf("attr %q = %q, want %q", key, got[key], w)
		}
	}
}

// TestNewReporterNilLoggerFallsBackToDefault pins the documented "logger may
// be nil" contract: a nil logger falls back to slog.Default() rather than
// panicking, and the reporter's lines land on the default logger. The default
// logger is process-global, so this test must not run in parallel.
func TestNewReporterNilLoggerFallsBackToDefault(t *testing.T) {
	logger, recorder := capture.New()
	prev := slog.Default()
	slog.SetDefault(logger)
	defer slog.SetDefault(prev)

	reporter := NewReporter(nil)
	reporter.Baseline([]compare.Finding{testFinding("k", "Frieren")}, time.Now())

	if got := recorder.CountExact("cold start: findings baselined without notifying"); got != 1 {
		t.Errorf("baseline summary on default logger = %d, want 1", got)
	}
}

// TestReporterEmitSanitizesControlAndBidiRunes mirrors the audit report's
// slog-path pin (TestReportLogSanitizesControlAndBidiRunes) against the
// daemon finding emitter: slog's JSONHandler escapes C0 controls but emits C1
// controls and bidi controls raw, so every untrusted attribute emitted by
// emit and emitResolved must ride through runesafe.Sanitize first. A
// finding whose upstream-derived fields embed a C1 CSI (U+009B), an RLO bidi
// override (U+202E), and a C0 escape introducer must log spaces in their
// place on both the finding line and the resolution line.
func TestReporterEmitSanitizesControlAndBidiRunes(t *testing.T) {
	const dirty = "a\u009bb\u202ec\x1bd" // C1 CSI, RLO override, C0 ESC
	const clean = "a b c d"
	reporter, recorder := newCapturedReporter()
	finding := testFinding("dirty", dirty)
	finding.CurrentGroup = dirty
	finding.RecommendedGroup = dirty
	finding.ReleaseURL = dirty
	finding.InfoHash = dirty

	prior := reporter.Report([]compare.Finding{finding}, nil, nil, time.Now())
	reporter.Report(nil, prior, nil, time.Now()) // resolve it via emitResolved

	want := map[string]map[string]string{
		"better release available": {
			"title":             clean,
			"current_group":     clean,
			"recommended_group": clean,
			"release_url":       clean,
			"info_hash":         clean,
		},
		"finding resolved": {
			"title":             clean,
			"current_group":     clean,
			"recommended_group": clean,
		},
	}
	seen := map[string]bool{}
	for _, rec := range recorder.Records() {
		expected, ok := want[rec.Message]
		if !ok {
			continue
		}
		seen[rec.Message] = true
		rec.Attrs(func(a slog.Attr) bool {
			s, isStr := a.Value.Any().(string)
			if !isStr {
				return true
			}
			for _, bad := range []rune{'\u009b', '\u202e', '\x1b'} {
				if strings.ContainsRune(s, bad) {
					t.Errorf("%s attr %q carries raw unsafe rune %U: %q", rec.Message, a.Key, bad, s)
				}
			}
			if w, pinned := expected[a.Key]; pinned && s != w {
				t.Errorf("%s attr %q = %q, want %q", rec.Message, a.Key, s, w)
			}
			return true
		})
	}
	for msg := range want {
		if !seen[msg] {
			t.Errorf("expected a %q line, none emitted", msg)
		}
	}
}

// TestReporterReportSuppressesDuplicateCurrentKeys pins in-batch dedupe: the
// SeaDex fetcher appends every upstream record and the matcher preserves
// per-entry cardinality, so one current batch can carry the same DedupeKey
// twice. The returned state collapses them to one record, and the emitted
// notifications must collapse the same way — one line, not one per copy.
func TestReporterReportSuppressesDuplicateCurrentKeys(t *testing.T) {
	reporter, recorder := newCapturedReporter()
	finding := testFinding("duplicate", "Frieren")

	current := reporter.Report([]compare.Finding{finding, finding}, nil, nil, time.Now())

	if got := recorder.CountExact("better release available"); got != 1 {
		t.Errorf("duplicate current finding notifications = %d, want 1", got)
	}
	if len(current) != 1 {
		t.Errorf("current dedupe state entries = %d, want 1", len(current))
	}
}

// TestResolvedLineCarriesDocumentedAttrs pins the resolution line's full
// attribute contract the way TestFindingLineCarriesDocumentedAttrs pins the
// finding line: emitResolved reads the trimmed StoredFinding back, and a
// silently dropped or zeroed key (season, al_id, status) breaks the Loki
// dashboards without failing any existing test.
func TestResolvedLineCarriesDocumentedAttrs(t *testing.T) {
	reporter, recorder := newCapturedReporter()
	f := testFinding("gone", "Frieren")
	f.Season = 2
	prior := map[string]Alerted{
		"gone": {AlertedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Finding: storedFinding(&f)},
	}

	reporter.Report(nil, prior, nil, time.Now())

	wantStr := map[string]string{
		"title":             "Frieren",
		"arr":               "sonarr",
		"current_group":     "erai-raws",
		"recommended_group": "SubsPlease",
		"status":            "better_release",
	}
	gotStr := map[string]string{}
	var alID, season int64
	sawLine := false
	for _, rec := range recorder.Records() {
		if rec.Message != "finding resolved" {
			continue
		}
		sawLine = true
		rec.Attrs(func(a slog.Attr) bool {
			switch a.Key {
			case "al_id":
				alID = a.Value.Int64()
			case "season":
				season = a.Value.Int64()
			default:
				if s, ok := a.Value.Any().(string); ok {
					gotStr[a.Key] = s
				}
			}
			return true
		})
	}
	if !sawLine {
		t.Fatal("no finding-resolved line emitted")
	}
	if alID != 154587 {
		t.Errorf("al_id = %d, want 154587", alID)
	}
	if season != 2 {
		t.Errorf("season = %d, want 2", season)
	}
	for key, w := range wantStr {
		if gotStr[key] != w {
			t.Errorf("attr %q = %q, want %q", key, gotStr[key], w)
		}
	}
}

// TestReporterDuplicateOfPriorFindingKeepsOriginalAlertTime pins the
// interaction of in-batch dedupe with the prior state: when a batch carries
// the same DedupeKey twice AND that key was already alerted in a prior cycle,
// no notification is emitted, the original alert time survives the in-batch
// duplicate branch, and the stored record follows the documented
// last-payload-wins rule.
func TestReporterDuplicateOfPriorFindingKeepsOriginalAlertTime(t *testing.T) {
	reporter, recorder := newCapturedReporter()
	oldTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	f := testFinding("dup-prior", "Frieren")
	updated := testFinding("dup-prior", "Frieren (retitled)")
	prior := map[string]Alerted{
		"dup-prior": {AlertedAt: oldTime, Finding: storedFinding(&f)},
	}

	current := reporter.Report([]compare.Finding{f, updated}, prior, nil, time.Now())

	if got := recorder.CountExact("better release available"); got != 0 {
		t.Errorf("notifications for an already-alerted duplicated finding = %d, want 0", got)
	}
	rec, ok := current["dup-prior"]
	if !ok {
		t.Fatal("finding missing from returned state")
	}
	if !rec.AlertedAt.Equal(oldTime) {
		t.Errorf("AlertedAt = %s, want original %s preserved through the in-batch duplicate", rec.AlertedAt, oldTime)
	}
	if rec.Finding.Title != "Frieren (retitled)" {
		t.Errorf("stored title = %q, want last payload %q", rec.Finding.Title, "Frieren (retitled)")
	}
}

// TestFindingLineCarriesSeason pins the season attribute on the finding
// line: TestFindingLineCarriesDocumentedAttrs collects only string-valued
// attrs, so the int-valued season was the one documented finding-line key a
// mutation could zero without failing any test.
func TestFindingLineCarriesSeason(t *testing.T) {
	reporter, recorder := newCapturedReporter()
	f := testFinding("s2", "Frieren")
	f.Season = 2

	reporter.Report([]compare.Finding{f}, nil, nil, time.Now())

	season := int64(-1)
	for _, rec := range recorder.Records() {
		if rec.Message != "better release available" {
			continue
		}
		rec.Attrs(func(a slog.Attr) bool {
			if a.Key == "season" {
				season = a.Value.Int64()
			}
			return true
		})
	}
	if season != 2 {
		t.Errorf("finding line season = %d, want 2", season)
	}
}

// TestReportSummaryLineCarriesAccountingCounts pins the cycle-summary
// counters on the "findings reported" line: total, new, resolved, preserved,
// and suppressed. The existing tests only count the line's occurrences, so a
// mutation swapping or zeroing any counter (e.g. suppressed's len-newCount
// arithmetic) would pass every test while silently corrupting the Loki cycle
// accounting the operator reads.
func TestReportSummaryLineCarriesAccountingCounts(t *testing.T) {
	reporter, recorder := newCapturedReporter()
	oldTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	kept := testFinding("kept", "Kept")
	gone := testFinding("gone", "Gone")
	gone.AniListID = 111
	preserved := testFinding("preserved", "Preserved")
	preserved.AniListID = 222
	prior := map[string]Alerted{
		"kept":      {AlertedAt: oldTime, Finding: storedFinding(&kept)},
		"gone":      {AlertedAt: oldTime, Finding: storedFinding(&gone)},
		"preserved": {AlertedAt: oldTime, Finding: storedFinding(&preserved)},
	}

	reporter.Report([]compare.Finding{
		testFinding("new", "Brand New"),
		kept,
	}, prior, map[int]struct{}{222: {}}, time.Now())

	want := map[string]int64{
		"total": 2, "new": 1, "resolved": 1, "preserved": 1, "suppressed": 1,
	}
	got := map[string]int64{}
	sawLine := false
	for _, rec := range recorder.Records() {
		if rec.Message != "findings reported" {
			continue
		}
		sawLine = true
		rec.Attrs(func(a slog.Attr) bool {
			got[a.Key] = a.Value.Int64()
			return true
		})
	}
	if !sawLine {
		t.Fatal("no findings-reported summary line emitted")
	}
	for key, w := range want {
		if got[key] != w {
			t.Errorf("summary attr %q = %d, want %d", key, got[key], w)
		}
	}
}

// TestFindingLineCarriesJoinedRecommendedGroups pins the recommended_groups
// attribute with a multi-group value: the fixtures in every other finding-line
// test leave RecommendedGroups nil, so the joined attr renders "" and a
// mutation dropping the attribute or breaking the comma join passes the whole
// suite unnoticed.
func TestFindingLineCarriesJoinedRecommendedGroups(t *testing.T) {
	reporter, recorder := newCapturedReporter()
	f := testFinding("multi", "Frieren")
	f.RecommendedGroups = []string{"SubsPlease", "PMR"}

	reporter.Report([]compare.Finding{f}, nil, nil, time.Now())

	got := ""
	seen := false
	for _, rec := range recorder.Records() {
		if rec.Message != "better release available" {
			continue
		}
		rec.Attrs(func(a slog.Attr) bool {
			if a.Key == "recommended_groups" {
				seen = true
				got, _ = a.Value.Any().(string)
			}
			return true
		})
	}
	if !seen {
		t.Fatal("finding line carries no recommended_groups attribute")
	}
	if got != "SubsPlease,PMR" {
		t.Errorf("recommended_groups = %q, want %q", got, "SubsPlease,PMR")
	}
}
