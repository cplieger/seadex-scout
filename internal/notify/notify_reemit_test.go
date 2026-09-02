package notify

import (
	"log/slog"
	"maps"
	"slices"
	"testing"

	"github.com/cplieger/seadex-scout/internal/compare"
	"github.com/cplieger/slogx/capture"
)

// emittedAttrsByTitle returns the string-valued attributes of every emitted
// finding line carrying title, in emission order, so two passes can be compared
// attribute-by-attribute rather than only by line count.
func emittedAttrsByTitle(recorder *capture.Recorder, title string) []map[string]string {
	var out []map[string]string
	for _, rec := range recorder.Records() {
		attrs := map[string]string{}
		rec.Attrs(func(a slog.Attr) bool {
			if s, ok := a.Value.Any().(string); ok {
				attrs[a.Key] = s
			}
			return true
		})
		if attrs["title"] == title {
			out = append(out, attrs)
		}
	}
	return out
}

// TestReemitRestatesEveryRowAndLeavesTheSetUnchanged pins the two properties
// Reemit's contract rests on, neither of which any other test in this repo
// drives. It is the third member of the state trio - Report replaces the set
// with full deletion authority, ReportScoped replaces it within a bounded
// authority, and Reemit re-states it having compared NOTHING - and it was the
// only one with no test of its own.
//
// Both properties fail SILENTLY and both are visible in the alerting stack.
// Findings are STATE, so the shipped alerts/logql.yaml reads a lookback window over
// the emitted lines: a Reemit that emits no ROW (only its summary line) lets
// that window expire, which resolves every standing better-release alert and
// then re-fires the whole set as new on the next full pass. And a Reemit that
// mutates the set - it compares nothing, so it has no authority to delete
// anything - drops standing rows the next tick would otherwise carry forward,
// so a condition that is still true stops being reported until a reconcile
// re-derives it up to 24h later.
//
// The summary counters are deliberately NOT the subject here: internal/scout's
// tick tests already pin carried on a re-statement.
func TestReemitRestatesEveryRowAndLeavesTheSetUnchanged(t *testing.T) {
	notifier, recorder := newCapturedNotifier()
	a := findingWithID("a", "Aria", 1)
	b := findingWithID("b", "Bocchi", 2)

	notifier.Report([]compare.Finding{a, b}, nil)
	reportedKeys := slices.Sorted(maps.Keys(notifier.current))
	notifier.Reemit()

	for _, title := range []string{"Aria", "Bocchi"} {
		if got := titleCount(recorder, title); got != 2 {
			t.Errorf("row %q emitted %d times, want 2 (the report, then the re-statement): a re-statement that emits no row lets the alert rule's lookback expire", title, got)
		}
	}
	if got := slices.Sorted(maps.Keys(notifier.current)); !slices.Equal(got, reportedKeys) {
		t.Errorf("current set after Reemit = %d rows, want the reported %d unchanged (Reemit compares nothing, so it has no authority to change the set)", len(got), len(reportedKeys))
	}
}

// TestReemitEmitsTheSamePayloadAndOrder pins "unchanged" literally: a re-stated
// line carries the same attributes, in the same emission order, as the pass that
// produced it. A re-statement that dropped, altered, or reordered an attribute
// would satisfy a line-count assertion while changing what the alert's
// annotations and its `sum by` grouping read.
func TestReemitEmitsTheSamePayloadAndOrder(t *testing.T) {
	notifier, recorder := newCapturedNotifier()
	a := findingWithID("a", "Aria", 1)
	b := findingWithID("b", "Bocchi", 2)

	notifier.Report([]compare.Finding{a, b}, nil)
	reported := slices.Clone(emittedTitles(recorder))
	notifier.Reemit()

	titles := emittedTitles(recorder)
	if len(titles) != 2*len(reported) {
		t.Fatalf("the two passes emitted %d rows (%v), want %d", len(titles), titles, 2*len(reported))
	}
	if restated := titles[len(reported):]; !slices.Equal(restated, reported) {
		t.Errorf("re-stated emission order = %v, want the reported pass's %v", restated, reported)
	}
	for _, title := range reported {
		lines := emittedAttrsByTitle(recorder, title)
		if len(lines) != 2 {
			t.Fatalf("row %q has %d emitted lines, want 2", title, len(lines))
		}
		if !maps.Equal(lines[0], lines[1]) {
			t.Errorf("row %q was re-stated with different attributes:\n reported %v\n restated %v", title, lines[0], lines[1])
		}
	}
}

// TestReemitKeepsSuppressingIgnoredShows pins that the re-statement path honours
// filters.ignore. Reemit shares emitAll with Report, but nothing drove the
// ignore set through it, so a re-statement that walked the raw set would
// re-notify a show the operator consciously declined - on every quiet tick, at
// the tick cadence, which is exactly the indefinite re-notification
// filters.ignore exists to stop.
func TestReemitKeepsSuppressingIgnoredShows(t *testing.T) {
	const ignoredID = 999
	notifier, recorder := newIgnoringNotifier(ignoredID)
	notifier.Report([]compare.Finding{
		findingWithID("ignored", "Ignored Show", ignoredID),
		findingWithID("kept", "Reported Show", 1),
	}, nil)

	notifier.Reemit()

	if got := titleCount(recorder, "Ignored Show"); got != 0 {
		t.Errorf("ignored row emitted %d times across both passes, want 0", got)
	}
	if got := titleCount(recorder, "Reported Show"); got != 2 {
		t.Errorf("reported row emitted %d times, want 2 (the report, then the re-statement)", got)
	}
	for key, want := range map[string]int64{"total": 2, "emitted": 1, "suppressed": 1} {
		if got, seen := summaryCounter(t, recorder, key); !seen || got != want {
			t.Errorf("summary %s = %d (present %v), want %d", key, got, seen, want)
		}
	}
}
