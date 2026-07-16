package report

import (
	"log/slog"
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
		"same": {AlertedAt: oldTime, Finding: testFinding("same", "Frieren")},
		"old":  {AlertedAt: oldTime, Finding: testFinding("old", "Old Title")},
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
		"failed-item": {AlertedAt: oldTime, Finding: failedFinding},
		"clean-gone":  {AlertedAt: oldTime, Finding: resolvable},
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

// TestStoredFindingSanitizesArrURL pins the persistence trust boundary on the
// arr deep-link (mirroring TestFindingLogSanitizesArrURL's log boundary): the
// dedupe records Report and Baseline return - which the caller persists to
// state.json - must never carry a credentialed public_url. All three storage
// sites are covered: a new finding, a suppressed (already-alerted) finding,
// and a cold-start baseline.
func TestStoredFindingSanitizesArrURL(t *testing.T) {
	reporter, _ := newCapturedReporter()
	finding := testFinding("cred", "Frieren")
	finding.ArrURL = "https://user:password@sonarr.example/series/frieren?token=secret#frag"
	const want = "https://sonarr.example/series/frieren"
	now := time.Now()

	current := reporter.Report([]compare.Finding{finding}, nil, nil, now)
	if got := current["cred"].Finding.ArrURL; got != want {
		t.Errorf("new-finding stored ArrURL = %q, want %q", got, want)
	}

	suppressed := reporter.Report([]compare.Finding{finding}, current, nil, now)
	if got := suppressed["cred"].Finding.ArrURL; got != want {
		t.Errorf("suppressed-finding stored ArrURL = %q, want %q", got, want)
	}

	baseline := reporter.Baseline([]compare.Finding{finding}, now)
	if got := baseline["cred"].Finding.ArrURL; got != want {
		t.Errorf("baselined stored ArrURL = %q, want %q", got, want)
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
