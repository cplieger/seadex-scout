package report

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/cplieger/seadex-scout/internal/compare"
)

type capturedRecord struct {
	attrs map[string]any
	msg   string
	level slog.Level
}

type captureHandler struct{ records *[]capturedRecord }

func (h captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h captureHandler) Handle(_ context.Context, r slog.Record) error {
	rec := capturedRecord{msg: r.Message, level: r.Level, attrs: make(map[string]any)}
	r.Attrs(func(a slog.Attr) bool {
		rec.attrs[a.Key] = a.Value.Any()
		return true
	})
	*h.records = append(*h.records, rec)
	return nil
}

func (h captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h captureHandler) WithGroup(string) slog.Handler      { return h }

func newCapturedReporter() (*Reporter, *[]capturedRecord) {
	records := []capturedRecord{}
	logger := slog.New(captureHandler{records: &records})
	return NewReporter(logger), &records
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
	reporter, records := newCapturedReporter()
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
	if got := countMessages(*records, "better release available"); got != 0 {
		t.Errorf("Baseline emitted %d finding notifications, want 0", got)
	}
	if got := countMessages(*records, "cold start: findings baselined without notifying"); got != 1 {
		t.Errorf("Baseline cold-start summary count = %d, want 1", got)
	}
}

func TestReporterReportSuppressesExistingAndEmitsNewAndResolved(t *testing.T) {
	reporter, records := newCapturedReporter()
	oldTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	prior := map[string]Alerted{
		"same": {AlertedAt: oldTime, Finding: testFinding("same", "Frieren")},
		"old":  {AlertedAt: oldTime, Finding: testFinding("old", "Old Title")},
	}

	current := reporter.Report([]compare.Finding{
		testFinding("same", "Frieren"),
		testFinding("new", "Bocchi"),
	}, prior, now)

	if !current["same"].AlertedAt.Equal(oldTime) {
		t.Errorf("suppressed finding AlertedAt = %s, want original %s", current["same"].AlertedAt, oldTime)
	}
	if !current["new"].AlertedAt.Equal(now) {
		t.Errorf("new finding AlertedAt = %s, want %s", current["new"].AlertedAt, now)
	}
	if _, ok := current["old"]; ok {
		t.Errorf("resolved finding still present in current state: %+v", current["old"])
	}
	if got := countMessages(*records, "better release available"); got != 1 {
		t.Errorf("new finding notification count = %d, want 1", got)
	}
	if got := countMessages(*records, "finding resolved"); got != 1 {
		t.Errorf("resolved notification count = %d, want 1", got)
	}
	if got := countMessages(*records, "findings reported"); got != 1 {
		t.Errorf("summary count = %d, want 1", got)
	}
}

func countMessages(records []capturedRecord, msg string) int {
	count := 0
	for _, rec := range records {
		if rec.msg == msg {
			count++
		}
	}
	return count
}
