package notify

import (
	"log/slog"
	"testing"

	"github.com/cplieger/seadex-scout/internal/compare"
	"github.com/cplieger/seadex-scout/internal/release"
)

func newerRevisionFinding() compare.Finding {
	f := testFinding("r1", "91 Days")
	f.Status = compare.StatusNewerRevision
	f.CurrentGroup, f.CurrentGroups = "udf", []string{"udf"}
	f.RecommendedGroup, f.RecommendedGroups = "UDF", []string{"udf"}
	f.CurrentRevision = release.Revision{Version: 1, Marker: release.RevisionNone}
	f.RecommendedRevision = release.Revision{Version: 2, Marker: release.RevisionVersion}
	return f
}

func TestNewerRevisionFindingSharesTheBetterReleaseLine(t *testing.T) {
	notifier, recorder := newCapturedNotifier()
	notifier.Report([]compare.Finding{newerRevisionFinding()}, nil)

	want := map[string]string{
		"status":               "newer_revision",
		"seadex_tags":          "newer-revision · v2 · encode · 1080p · dual-audio",
		"current_revision":     "v1",
		"recommended_revision": "v2",
		"recommended_group":    "UDF",
	}
	found := false
	for _, rec := range recorder.Records() {
		if rec.Message != "better release available" {
			continue
		}
		found = true
		if rec.Level != slog.LevelWarn {
			t.Errorf("newer_revision finding emitted at %s, want WARN", rec.Level)
		}
		got := map[string]string{}
		rec.Attrs(func(a slog.Attr) bool {
			if s, ok := a.Value.Any().(string); ok {
				got[a.Key] = s
			}
			return true
		})
		for key, w := range want {
			if got[key] != w {
				t.Errorf("attr %q = %q, want %q", key, got[key], w)
			}
		}
	}
	if !found {
		t.Fatalf("no %q line emitted for a newer_revision finding; records: %v", "better release available", recorder.Messages())
	}
}

func TestOtherFindingsCarryEmptyRevisionAttrs(t *testing.T) {
	notifier, recorder := newCapturedNotifier()
	notifier.Report([]compare.Finding{testFinding("b1", "Frieren")}, nil)
	for _, rec := range recorder.Records() {
		if rec.Message != "better release available" {
			continue
		}
		rec.Attrs(func(a slog.Attr) bool {
			if (a.Key == "current_revision" || a.Key == "recommended_revision") && a.Value.String() != "" {
				t.Errorf("better_release attr %q = %q, want empty", a.Key, a.Value.String())
			}
			return true
		})
	}
}

func TestNewerRevisionDedupeKeyDiffersFromBetterRelease(t *testing.T) {
	revision := newerRevisionFinding()
	better := revision
	better.Status = compare.StatusBetter
	if dedupeKey(&revision) == dedupeKey(&better) {
		t.Errorf("dedupeKey(newer_revision) == dedupeKey(better_release) for otherwise equal findings: %q", dedupeKey(&revision))
	}
	if level(compare.StatusNewerRevision) != slog.LevelWarn || message(compare.StatusNewerRevision) != "better release available" {
		t.Errorf("level/message(newer_revision) = %s/%q, want WARN/%q", level(compare.StatusNewerRevision), message(compare.StatusNewerRevision), "better release available")
	}
}
