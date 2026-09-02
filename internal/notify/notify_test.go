package notify

import (
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/cplieger/runesafe/v2"
	"github.com/cplieger/seadex-scout/internal/compare"
	"github.com/cplieger/slogx/capture"
)

func newCapturedNotifier() (*Notifier, *capture.Recorder) {
	logger, recorder := capture.New()
	return NewNotifier(logger, nil), recorder
}

// newIgnoringNotifier builds a notifier whose emission-suppression set holds
// the given AniList IDs, the shape config's filters.ignore produces.
func newIgnoringNotifier(ignored ...int) (*Notifier, *capture.Recorder) {
	logger, recorder := capture.New()
	ignore := make(map[int]struct{}, len(ignored))
	for _, id := range ignored {
		ignore[id] = struct{}{}
	}
	return NewNotifier(logger, ignore), recorder
}

// testFinding builds a fixture finding whose derived dedupe key is unique per
// key argument: the key rides the release URL (and its Nyaa link), the
// identity component dedupeKey falls back to for a non-40-hex InfoHash, so
// two fixtures with different keys never collapse in-batch while two with the
// same key (and any title) do - mirroring how production findings key on
// their release identity, not their title.
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
		Codec:            "x265",
		ReleaseURL:       "https://nyaa.si/view/" + key,
		InfoHash:         "hash-" + key,
		Status:           compare.StatusBetter,
		AniListID:        154587,
		Links: gradedLinks(
			compare.ReleaseLink{Tracker: "Nyaa", URL: "https://nyaa.si/view/" + key},
			compare.ReleaseLink{Tracker: "AB", URL: "https://animebytes.tv/torrents.php?id=1"},
		),
		DualAudio: true,
	}
}

// findingWithID is testFinding with a distinct AniList ID, so a fixture can be
// addressed by the incompleteIDs / ignore sets (both key on AniListID).
func findingWithID(key, title string, alID int) compare.Finding {
	f := testFinding(key, title)
	f.AniListID = alID
	return f
}

// emittedTitles returns the title of every emitted better-release line, in
// emission order, so a test can assert both the SET and the ORDER of a pass.
func emittedTitles(recorder *capture.Recorder) []string {
	var titles []string
	for _, rec := range recorder.Records() {
		if rec.Message != "better release available" {
			continue
		}
		rec.Attrs(func(a slog.Attr) bool {
			if a.Key == "title" {
				if s, ok := a.Value.Any().(string); ok {
					titles = append(titles, s)
				}
			}
			return true
		})
	}
	return titles
}

// titleCount counts the emitted better-release lines carrying title, so a
// multi-pass test can assert how many passes emitted a given row without
// swapping the notifier's logger between passes (the accumulated stream is
// exactly what Loki sees).
func titleCount(recorder *capture.Recorder, title string) int {
	n := 0
	for _, got := range emittedTitles(recorder) {
		if got == title {
			n++
		}
	}
	return n
}

// summaryCounter reads one counter off the "findings reported" summary line.
// It returns the LAST line's value, so a multi-pass test reads the pass it just
// ran rather than the one before it.
func summaryCounter(t *testing.T, recorder *capture.Recorder, key string) (int64, bool) {
	t.Helper()
	var value int64
	var seen bool
	for _, rec := range recorder.Records() {
		if rec.Message != "findings reported" {
			continue
		}
		rec.Attrs(func(a slog.Attr) bool {
			if a.Key == key {
				value, seen = a.Value.Int64(), true
			}
			return true
		})
	}
	return value, seen
}

// TestReportEmitsEveryRowOnceInDeterministicOrder pins the state contract's
// first half: Report emits the WHOLE current set, each row exactly once, and
// the order is stable across two identical passes so an operator (and a diff
// of two Loki windows) can compare one pass against the next. The second pass
// deliberately supplies the batch in a different input order: emission order
// is derived from the dedupe key, not from the caller's slice order.
func TestReportEmitsEveryRowOnceInDeterministicOrder(t *testing.T) {
	notifier, recorder := newCapturedNotifier()
	a := findingWithID("a", "Aria", 1)
	b := findingWithID("b", "Bocchi", 2)
	c := findingWithID("c", "Chihayafuru", 3)

	notifier.Report([]compare.Finding{a, b, c}, nil)
	// A different input order on the second pass: emission order is derived
	// from the dedupe key, not from the caller's slice order.
	notifier.Report([]compare.Finding{c, a, b}, nil)

	titles := emittedTitles(recorder)
	if len(titles) != 6 {
		t.Fatalf("two passes emitted %d rows (%v), want 6 - three per pass, once each", len(titles), titles)
	}
	first, second := titles[:3], titles[3:]
	if !slices.Equal(first, second) {
		t.Errorf("second pass emission order = %v, want the first pass's %v (deterministic order)", second, first)
	}
	for _, want := range []string{"Aria", "Bocchi", "Chihayafuru"} {
		if got := titleCount(recorder, want); got != 2 {
			t.Errorf("row %q emitted %d times, want once per pass", want, got)
		}
	}
	if total, seen := summaryCounter(t, recorder, "total"); !seen || total != 3 {
		t.Errorf("summary total = %d (present %v), want 3", total, seen)
	}
}

// TestReportReemitsUnchangedFindingsEveryPass is the whole point of the STATE
// shape: an unchanged condition is re-emitted on every pass so the Loki rule
// keeps firing until it stops being reported. The previous EVENT shape emitted
// each finding exactly once ever, which is what made a notification lost
// downstream permanent.
func TestReportReemitsUnchangedFindingsEveryPass(t *testing.T) {
	notifier, recorder := newCapturedNotifier()
	findings := []compare.Finding{findingWithID("same", "Frieren", 154587)}

	notifier.Report(findings, nil)
	notifier.Report(findings, nil)

	if got := recorder.CountExact("better release available"); got != 2 {
		t.Errorf("emissions across two identical passes = %d, want 2 (state, not events)", got)
	}
	if got := recorder.CountExact("findings reported"); got != 2 {
		t.Errorf("summary lines = %d, want one per pass", got)
	}
}

// TestReportDropsFindingAbsentFromTheNextPass pins resolution-by-absence: a
// condition that stops being reported simply stops being emitted, and nothing
// emits a resolution line (the alert rule's lookback window owns resolution).
func TestReportDropsFindingAbsentFromTheNextPass(t *testing.T) {
	notifier, recorder := newCapturedNotifier()
	kept := findingWithID("kept", "Kept", 1)
	gone := findingWithID("gone", "Gone", 2)

	notifier.Report([]compare.Finding{kept, gone}, nil)
	notifier.Report([]compare.Finding{kept}, nil)

	if got := titleCount(recorder, "Gone"); got != 1 {
		t.Errorf("the absent row was emitted %d times, want 1 (the first pass only)", got)
	}
	if got := titleCount(recorder, "Kept"); got != 2 {
		t.Errorf("the still-reported row was emitted %d times, want one per pass", got)
	}
	if got := recorder.Count("finding resolved"); got != 0 {
		t.Errorf("resolution lines = %d, want 0 (resolution is by absence)", got)
	}
	if total, _ := summaryCounter(t, recorder, "total"); total != 1 {
		t.Errorf("summary total = %d, want 1 (the absent row left the set)", total)
	}
	if _, ok := notifier.current[dedupeKey(&gone)]; ok {
		t.Error("the absent finding survived in the current set")
	}
}

// TestReportCarriesForwardIncompleteItems pins the ONE scope on replacement's
// delete half: an item whose evidence was incomplete this pass (a failed
// episode walk or a degraded AniList lookup) has an ABSENCE that means missing
// data, not alignment, so its prior rows are carried forward and keep being
// emitted - and the summary's preserved counter reports how many.
func TestReportCarriesForwardIncompleteItems(t *testing.T) {
	notifier, recorder := newCapturedNotifier()
	const incompleteID = 222
	carried := findingWithID("carried", "Broken Series", incompleteID)
	resolvable := findingWithID("clean", "Aligned Now", 333)

	notifier.Report([]compare.Finding{carried, resolvable}, nil)
	notifier.Report(nil, map[int]struct{}{incompleteID: {}})

	if got := titleCount(recorder, "Broken Series"); got != 2 {
		t.Errorf("the incomplete item's row was emitted %d times, want one per pass (carried forward)", got)
	}
	if got := titleCount(recorder, "Aligned Now"); got != 1 {
		t.Errorf("the cleanly-compared item's row was emitted %d times, want 1 (it resolved by absence)", got)
	}
	if preserved, seen := summaryCounter(t, recorder, "preserved"); !seen || preserved != 1 {
		t.Errorf("summary preserved = %d (present %v), want 1", preserved, seen)
	}
	if total, _ := summaryCounter(t, recorder, "total"); total != 1 {
		t.Errorf("summary total = %d, want 1 (only the carried row)", total)
	}
	if _, ok := notifier.current[dedupeKey(&resolvable)]; ok {
		t.Error("the cleanly-compared item's row survived, want it dropped")
	}
}

// TestReportCarriesForwardOnlyWhileEvidenceIsIncomplete pins the release of
// the carry-forward: once the item's evidence is complete again and it still
// produces no finding, the row is dropped like any other resolved condition.
// Without this a single transient failure would pin a row forever.
func TestReportCarriesForwardOnlyWhileEvidenceIsIncomplete(t *testing.T) {
	notifier, recorder := newCapturedNotifier()
	const incompleteID = 222
	carried := findingWithID("carried", "Broken Series", incompleteID)

	notifier.Report([]compare.Finding{carried}, nil)
	notifier.Report(nil, map[int]struct{}{incompleteID: {}})
	notifier.Report(nil, nil)

	if got := titleCount(recorder, "Broken Series"); got != 2 {
		t.Errorf("the row was emitted %d times, want 2 (the third pass has complete evidence and no finding)", got)
	}
	if total, _ := summaryCounter(t, recorder, "total"); total != 0 {
		t.Errorf("summary total = %d, want 0", total)
	}
	if len(notifier.current) != 0 {
		t.Errorf("current set = %+v, want empty", notifier.current)
	}
}

// TestReportSuppressesIgnoredAniListIDs pins filters.ignore's contract: an
// ignored show is held in the set and counted (so the operator can still see
// it is being tracked, and report mode still shows it) but never emitted, so
// continuous reporting cannot re-notify a show the operator has consciously
// decided not to upgrade.
func TestReportSuppressesIgnoredAniListIDs(t *testing.T) {
	const ignoredID = 999
	notifier, recorder := newIgnoringNotifier(ignoredID)
	notifier.Report([]compare.Finding{
		findingWithID("ignored", "Ignored Show", ignoredID),
		findingWithID("kept", "Reported Show", 1),
	}, nil)

	if got := emittedTitles(recorder); !slices.Equal(got, []string{"Reported Show"}) {
		t.Errorf("emitted %v, want only the non-ignored [Reported Show]", got)
	}
	for key, want := range map[string]int64{"total": 2, "emitted": 1, "suppressed": 1} {
		if got, seen := summaryCounter(t, recorder, key); !seen || got != want {
			t.Errorf("summary %s = %d (present %v), want %d", key, got, seen, want)
		}
	}
}

// TestReportSummaryLineCarriesEveryCounter pins all four counters on the
// summary line at once, with a fixture that makes each of them distinct: a
// zeroed or swapped counter otherwise corrupts the Loki cycle accounting an
// operator reads while every other test stays green.
func TestReportSummaryLineCarriesEveryCounter(t *testing.T) {
	const ignoredID, incompleteID = 999, 222
	notifier, recorder := newIgnoringNotifier(ignoredID)
	notifier.Report([]compare.Finding{
		findingWithID("ignored", "Ignored", ignoredID),
		findingWithID("carried", "Carried", incompleteID),
	}, nil)

	notifier.Report([]compare.Finding{
		findingWithID("ignored", "Ignored", ignoredID),
		findingWithID("fresh", "Fresh", 1),
	}, map[int]struct{}{incompleteID: {}})

	// total: ignored + carried + fresh; emitted: carried + fresh;
	// suppressed: ignored; preserved: carried.
	want := map[string]int64{"total": 3, "emitted": 2, "suppressed": 1, "preserved": 1}
	for key, w := range want {
		if got, seen := summaryCounter(t, recorder, key); !seen || got != w {
			t.Errorf("summary %s = %d (present %v), want %d", key, got, seen, w)
		}
	}
}

// TestReportCollapsesInBatchDuplicateKeys pins in-batch dedupe: the SeaDex
// fetcher appends every upstream record and the matcher preserves per-entry
// cardinality, so one batch can carry the same dedupe key twice. The set
// collapses them to one row, one line is emitted, and that line carries the
// batch's LAST payload - the same payload the set retains, so the emitted line
// and the next pass's re-emission cannot disagree.
func TestReportCollapsesInBatchDuplicateKeys(t *testing.T) {
	notifier, recorder := newCapturedNotifier()
	first := testFinding("duplicate", "Frieren (first copy)")
	last := testFinding("duplicate", "Frieren (last copy)")

	notifier.Report([]compare.Finding{first, last}, nil)

	if got := recorder.CountExact("better release available"); got != 1 {
		t.Errorf("duplicate-key emissions = %d, want 1", got)
	}
	if got := len(notifier.current); got != 1 {
		t.Errorf("current set entries = %d, want 1", got)
	}
	if got := notifier.current[dedupeKey(&first)].Title; got != last.Title {
		t.Errorf("retained title = %q, want the last payload's %q", got, last.Title)
	}
	if got, _ := recorder.AttrValue("better release available", "title"); got != last.Title {
		t.Errorf("emitted title = %q, want the last payload's %q", got, last.Title)
	}
	if total, _ := summaryCounter(t, recorder, "total"); total != 1 {
		t.Errorf("summary total = %d, want 1", total)
	}
}

// TestReportWithNoFindingsEmitsNothing pins the empty pass: a library fully
// aligned with SeaDex reports nothing but must still close with the summary
// line, so the operator can tell a clean pass from a cycle that never ran (the
// cycle deadman rule keys on that).
func TestReportWithNoFindingsEmitsNothing(t *testing.T) {
	notifier, recorder := newCapturedNotifier()

	notifier.Report(nil, nil)

	if got := recorder.CountExact("better release available"); got != 0 {
		t.Errorf("emissions on an empty pass = %d, want 0", got)
	}
	if got := recorder.CountExact("findings reported"); got != 1 {
		t.Errorf("summary lines = %d, want 1", got)
	}
	for key, want := range map[string]int64{"total": 0, "emitted": 0, "suppressed": 0, "preserved": 0} {
		if got, seen := summaryCounter(t, recorder, key); !seen || got != want {
			t.Errorf("summary %s = %d (present %v), want %d", key, got, seen, want)
		}
	}
	if len(notifier.current) != 0 {
		t.Errorf("current set = %+v, want empty", notifier.current)
	}
}

// TestReportEmptySliceReplacesTheWholeSet pins that an empty (non-nil) batch
// is a genuine "nothing is true any more" report rather than a no-op: the
// previous pass's rows are dropped, not retained.
func TestReportEmptySliceReplacesTheWholeSet(t *testing.T) {
	notifier, recorder := newCapturedNotifier()
	notifier.Report([]compare.Finding{testFinding("k", "Frieren")}, nil)

	notifier.Report([]compare.Finding{}, nil)

	if got := titleCount(recorder, "Frieren"); got != 1 {
		t.Errorf("the row was emitted %d times, want 1 (the empty batch replaced the set)", got)
	}
	if total, _ := summaryCounter(t, recorder, "total"); total != 0 {
		t.Errorf("summary total = %d, want 0", total)
	}
}

// TestNotifierUnverifiableFindingIsInfoNotBetterRelease pins the alert-rule
// safety of the unverifiable status: the SeadexScoutBetterReleaseFound Loki
// rule counts only msg="better release available" warn lines, so an
// unverifiable finding must emit at INFO with its own message and contribute
// zero warn-level better-release lines.
func TestNotifierUnverifiableFindingIsInfoNotBetterRelease(t *testing.T) {
	notifier, recorder := newCapturedNotifier()
	finding := testFinding("unv", "Unknown Evidence")
	finding.Status = compare.StatusUnverifiable

	notifier.Report([]compare.Finding{finding}, nil)

	if got := recorder.CountExact("release group unverifiable, manual review"); got != 1 {
		t.Errorf("unverifiable notification count = %d, want 1", got)
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

// TestNotifierEmitLevelFollowsStatus pins the status-to-level mapping the Loki
// alert rules and dashboards key on: a better-release finding must emit at WARN
// (the actionable level the README documents for better-release lines; the
// SeadexScoutBetterReleaseFound rule itself selects on msg only) and every
// informational nudge at INFO. The existing tests count messages only, so a
// flipped level would silently break every shipped alert without failing a test.
func TestNotifierEmitLevelFollowsStatus(t *testing.T) {
	notifier, recorder := newCapturedNotifier()
	warn := findingWithID("w", "Warn Title", 1) // testFinding status is StatusBetter
	info := findingWithID("i", "Info Title", 2)
	info.Status = compare.StatusIncomplete

	notifier.Report([]compare.Finding{warn, info}, nil)

	sawWarn, sawInfo := false, false
	for _, rec := range recorder.Records() {
		switch rec.Message {
		case "better release available":
			sawWarn = true
			if rec.Level != slog.LevelWarn {
				t.Errorf("better-release finding emitted at %s, want WARN (the documented actionable-finding level)", rec.Level)
			}
		case "SeaDex entry is incomplete":
			sawInfo = true
			if rec.Level != slog.LevelInfo {
				t.Errorf("incomplete finding emitted at %s, want INFO", rec.Level)
			}
		}
	}
	if !sawWarn || !sawInfo {
		t.Fatalf("expected both finding lines emitted, saw warn=%v info=%v", sawWarn, sawInfo)
	}
}

// TestFindingLineCarriesDocumentedAttrs pins the finding line's attribute
// contract: the README documents the exact keys the Loki
// dashboards and alert annotations key on (title, al_id, arr, current_group,
// recommended_group, tracker, resolution, kind, classification_reason,
// release_url, release_urls, plus the split nyaa_url/ab_url, info_hash,
// seadex_tags, and status). A silently renamed or dropped key breaks every
// dashboard without failing a test; this asserts the full rendered set for
// one warn finding, which also gives joinLinks its behavioral assertion.
func TestFindingLineCarriesDocumentedAttrs(t *testing.T) {
	notifier, recorder := newCapturedNotifier()
	notifier.Report([]compare.Finding{testFinding("k1", "Frieren")}, nil)

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
		"release_url":           "https://nyaa.si/view/k1",
		"release_urls":          "Nyaa=https://nyaa.si/view/k1 AB=https://animebytes.tv/torrents.php?id=1",
		"nyaa_url":              "https://nyaa.si/view/k1",
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

// TestNewNotifierNilLoggerFallsBackToDefault pins the documented "logger may
// be nil" contract: a nil logger falls back to slog.Default() rather than
// panicking, and the notifier's lines land on the default logger. The default
// logger is process-global, so this test must not run in parallel.
func TestNewNotifierNilLoggerFallsBackToDefault(t *testing.T) {
	logger, recorder := capture.New()
	prev := slog.Default()
	slog.SetDefault(logger)
	defer slog.SetDefault(prev)

	notifier := NewNotifier(nil, nil)
	notifier.Report([]compare.Finding{testFinding("k", "Frieren")}, nil)

	if got := recorder.CountExact("findings reported"); got != 1 {
		t.Errorf("summary line on default logger = %d, want 1", got)
	}
	if got := recorder.CountExact("better release available"); got != 1 {
		t.Errorf("finding line on default logger = %d, want 1", got)
	}
}

// TestNotifierEmitSanitizesControlAndBidiRunes mirrors the audit report's
// slog-path pin (TestReportLogSanitizesControlAndBidiRunes) against the
// daemon finding emitter: slog's JSONHandler escapes C0 controls but emits C1
// controls and bidi controls raw, so every untrusted attribute emitted by
// emit must ride through runesafe.Sanitize first. A finding whose
// upstream-derived fields embed a C1 CSI (U+009B), an RLO bidi override
// (U+202E), and a C0 escape introducer must log spaces in their place.
func TestNotifierEmitSanitizesControlAndBidiRunes(t *testing.T) {
	const dirty = "a\u009bb\u202ec\x1bd" // C1 CSI, RLO override, C0 ESC
	const clean = "a b c d"
	// release_url takes the plain capAttr render: alerts/logql.yaml never renders it as
	// a Markdown link destination, so the spaces the sanitizer substituted stay
	// spaces rather than arriving percent-encoded the way an interpolated
	// link-destination attribute's do (see TestFindingLineSanitizesEveryUntrustedAttr).
	notifier, recorder := newCapturedNotifier()
	finding := testFinding("dirty", dirty)
	finding.CurrentGroup = dirty
	finding.RecommendedGroup = dirty
	finding.ReleaseURL = dirty
	finding.InfoHash = dirty

	notifier.Report([]compare.Finding{finding}, nil)

	want := map[string]string{
		"title":             clean,
		"current_group":     clean,
		"recommended_group": clean,
		"release_url":       clean,
		"info_hash":         clean,
	}
	sawLine := false
	for _, rec := range recorder.Records() {
		if rec.Message != "better release available" {
			continue
		}
		sawLine = true
		rec.Attrs(func(a slog.Attr) bool {
			s, isStr := a.Value.Any().(string)
			if !isStr {
				return true
			}
			for _, bad := range []rune{'\u009b', '\u202e', '\x1b'} {
				if strings.ContainsRune(s, bad) {
					t.Errorf("attr %q carries raw unsafe rune %U: %q", a.Key, bad, s)
				}
			}
			if w, pinned := want[a.Key]; pinned && s != w {
				t.Errorf("attr %q = %q, want %q", a.Key, s, w)
			}
			return true
		})
	}
	if !sawLine {
		t.Error("expected a better-release line, none emitted")
	}
}

// TestFindingLineSanitizesEveryUntrustedAttr widens the sanitization pin to
// the whole untrusted attribute set findingKVs renders: dropping capAttr /
// the joiners from any single site (recommended_groups, tracker,
// classification_reason, release_urls, nyaa_url, ab_url, ...) would leave the
// narrower pin above green while restoring raw C1/bidi controls to a Loki
// attribute.
func TestFindingLineSanitizesEveryUntrustedAttr(t *testing.T) {
	const dirty = "a\u009bb\u202ec\x1bd"
	const clean = "a b c d"
	// The link-destination attributes the annotation renders (nyaa_url, ab_url,
	// public_url, arr_url) carry capURLAttr's Markdown escaping on top of the
	// sanitizer, so their substituted spaces arrive percent-encoded. release_url
	// and release_urls are not rendered as destinations anywhere and take the
	// plain capAttr / joiner render.
	const cleanURL = "a%20b%20c%20d"
	notifier, recorder := newCapturedNotifier()
	finding := testFinding("dirty-all", dirty)
	finding.CurrentGroup = dirty
	finding.RecommendedGroup = dirty
	finding.RecommendedGroups = []string{dirty}
	finding.Tracker = dirty
	finding.Reason = dirty
	finding.ReleaseURL = dirty
	finding.InfoHash = dirty
	finding.Links = gradedLinks(
		compare.ReleaseLink{Tracker: dirty, URL: dirty},
		compare.ReleaseLink{Tracker: "Nyaa", URL: "https://nyaa.si/view/a\u009bb\u202ec"},
	)

	notifier.Report([]compare.Finding{finding}, nil)

	want := map[string]string{
		"title":                 clean,
		"current_group":         clean,
		"recommended_group":     clean,
		"recommended_groups":    clean,
		"tracker":               clean,
		"classification_reason": clean,
		"release_url":           clean,
		"release_urls":          clean + "=" + clean + " Nyaa=https://nyaa.si/view/a b c",
		"nyaa_url":              "https://nyaa.si/view/a%20b%20c",
		"ab_url":                cleanURL,
		"info_hash":             clean,
	}
	for key, expected := range want {
		got, ok := recorder.AttrValue("better release available", key)
		if !ok {
			t.Errorf("finding line carries no %s attribute", key)
			continue
		}
		if got != expected {
			t.Errorf("finding line %s = %q, want sanitized %q", key, got, expected)
		}
	}
}

// TestFindingLineCarriesSeason pins the season attribute on the finding
// line: TestFindingLineCarriesDocumentedAttrs collects only string-valued
// attrs, so the int-valued season was the one documented finding-line key a
// mutation could zero without failing any test.
func TestFindingLineCarriesSeason(t *testing.T) {
	notifier, recorder := newCapturedNotifier()
	f := testFinding("s2", "Frieren")
	f.Season = 2

	notifier.Report([]compare.Finding{f}, nil)

	if season, ok := recorder.AttrValue("better release available", "season"); !ok || season != "2" {
		t.Errorf("finding line season = %q (found %v), want 2", season, ok)
	}
}

// TestFindingLineCarriesJoinedRecommendedGroups pins the recommended_groups
// attribute with a multi-group value: the fixtures in every other finding-line
// test leave RecommendedGroups nil, so the joined attr renders "" and a
// mutation dropping the attribute or breaking the comma join passes the whole
// suite unnoticed.
func TestFindingLineCarriesJoinedRecommendedGroups(t *testing.T) {
	notifier, recorder := newCapturedNotifier()
	f := testFinding("multi", "Frieren")
	f.RecommendedGroups = []string{"SubsPlease", "PMR"}

	notifier.Report([]compare.Finding{f}, nil)

	got, seen := recorder.AttrValue("better release available", "recommended_groups")
	if !seen {
		t.Fatal("finding line carries no recommended_groups attribute")
	}
	if got != "SubsPlease,PMR" {
		t.Errorf("recommended_groups = %q, want %q", got, "SubsPlease,PMR")
	}
}

// TestFindingAttrVolumeIsBounded pins the emit path's volume bound (capAttr):
// SeaDex admits multi-MB URLs (up to 512 per entry), and an unbounded slog
// record would exceed downstream log-pipeline line limits — silently dropping
// the very warn line the better-release alert keys on. A hostile oversized
// attribute must emit truncated with the "..." marker; a normal-length value
// must pass byte-identical to its runesafe.Sanitize form.
func TestFindingAttrVolumeIsBounded(t *testing.T) {
	notifier, recorder := newCapturedNotifier()
	huge := strings.Repeat("u", 4<<20) // 4 MB, well past the cap
	normal := "a\u009bnormal title"    // C1 CSI: sanitized, not truncated
	f := testFinding("cap", normal)
	f.ReleaseURL = huge

	notifier.Report([]compare.Finding{f}, nil)

	gotURL, ok := recorder.AttrValue("better release available", "release_url")
	if !ok {
		t.Fatal("finding line carries no release_url attribute")
	}
	if !strings.HasSuffix(gotURL, "...") {
		t.Errorf("oversized release_url not truncated with the ... marker (len %d)", len(gotURL))
	}
	if len(gotURL) > maxAttrBytes+len("...") {
		t.Errorf("release_url length = %d, want <= %d", len(gotURL), maxAttrBytes+len("..."))
	}
	gotTitle, _ := recorder.AttrValue("better release available", "title")
	if want := runesafe.Sanitize(normal); gotTitle != want {
		t.Errorf("normal-length title = %q, want byte-identical sanitized form %q", gotTitle, want)
	}
}

// TestAggregateAttrsAreBoundedBeforeJoining pins the aggregate attributes'
// bound (logattr.Joiner): recommended_groups and release_urls aggregate untrusted
// SeaDex data (up to 512 torrents, each admitting a multi-MB URL), so joining
// first would materialize a ~48 MiB aggregate before the 8 KiB cap applied - a
// plausible OOM kill of the documented 256 MiB container that would suppress
// the very warn line the better-release alert keys on. Both must emit bounded
// AND sanitized (C1 and bidi controls replaced), from bounded work.
func TestAggregateAttrsAreBoundedBeforeJoining(t *testing.T) {
	notifier, recorder := newCapturedNotifier()
	huge := strings.Repeat("u", 64<<10) // 64 KiB per source, 512 of them
	f := testFinding("agg", "bounded aggregate")
	f.Links = nil
	f.RecommendedGroups = nil
	for range 512 {
		f.Links = append(f.Links, compare.ReleaseLink{Tracker: "Nyaa\u009b", URL: "https://nyaa.si/" + huge})
		f.RecommendedGroups = append(f.RecommendedGroups, "grp\u202e"+huge)
	}

	notifier.Report([]compare.Finding{f}, nil)

	for _, key := range []string{"release_urls", "recommended_groups"} {
		got, ok := recorder.AttrValue("better release available", key)
		if !ok {
			t.Fatalf("finding line carries no %s attribute", key)
		}
		if len(got) > maxAttrBytes+len("...") {
			t.Errorf("%s length = %d, want <= %d", key, len(got), maxAttrBytes+len("..."))
		}
		if !strings.HasSuffix(got, "...") {
			t.Errorf("%s not marked truncated (len %d)", key, len(got))
		}
		if strings.ContainsAny(got, "\u009b\u202e") {
			t.Errorf("%s carries an unsanitized C1/bidi control", key)
		}
	}
}

// TestReportBoundsRetainedUntrustedStrings pins the RESIDENCY bound on the
// in-memory set. The old dedupe record persisted a 7-field projection whose
// untrusted strings were capped because it was written to state.json; deleting
// the persistence did not delete the reason. These rows stay resident for as
// long as the condition holds, so an oversized SeaDex title, group name or link
// URL would otherwise sit in a 256 MiB container bounded only by the fetch's own
// budget - invisible, because the emit path caps per attribute on the way out
// and never shrinks what it read from.
//
// The six closed-vocabulary fields (Arr, Kind, Resolution, Codec, Scope, Status) are
// deliberately absent: internal/compare writes each from package constants, so no
// oversized value can reach them and boundRetained does not cap them.
func TestReportBoundsRetainedUntrustedStrings(t *testing.T) {
	t.Parallel()
	huge := strings.Repeat("x", 4*maxAttrBytes)
	n, _ := newCapturedNotifier()
	n.Report([]compare.Finding{{
		AniListID:         7,
		Status:            compare.StatusBetter,
		Title:             huge,
		CurrentGroup:      huge,
		RecommendedGroup:  huge,
		Tracker:           huge,
		Reason:            huge,
		InfoHash:          huge,
		ReleaseURL:        huge,
		ArrURL:            huge,
		RecommendedGroups: []string{huge},
		CurrentGroups:     []string{huge},
		Links:             []compare.ReleaseLink{{Tracker: huge, URL: huge}},
	}}, nil)

	if len(n.current) != 1 {
		t.Fatalf("retained %d rows, want 1", len(n.current))
	}
	ceiling := maxAttrBytes + len(attrTruncMarker)
	for _, got := range n.current {
		check := func(field, v string) {
			t.Helper()
			if len(v) > ceiling {
				t.Errorf("retained %s is %d bytes, want <= %d", field, len(v), ceiling)
			}
		}
		check("Title", got.Title)
		check("CurrentGroup", got.CurrentGroup)
		check("RecommendedGroup", got.RecommendedGroup)
		check("Tracker", got.Tracker)
		check("Reason", got.Reason)
		check("InfoHash", got.InfoHash)
		check("ReleaseURL", got.ReleaseURL)
		check("ArrURL", got.ArrURL)
		for _, g := range got.RecommendedGroups {
			check("RecommendedGroups element", g)
		}
		for _, g := range got.CurrentGroups {
			check("CurrentGroups element", g)
		}
		for _, l := range got.Links {
			check("Links element Tracker", l.Tracker)
			check("Links element URL", l.URL)
		}
	}
}

// TestReportBoundsRetainedRowToItsDocumentedCeiling pins the RESIDENCY bound
// maxRetainedElemBytes' comment states as a number: a retained row's three
// untrusted slices are bounded at maxRetainedListItems elements, each element at
// maxRetainedElemBytes, so the worst-case row is 64 x 256 x 4 = 64 KiB rather
// than the 2 MiB the count cap alone left it at. Both halves are load-bearing
// and neither is observable from the emit path, which caps per attribute on the
// way out and never shrinks what it retained - so dropping capRetainedList's
// truncation (one SeaDex entry admits 512 torrents, internal/seadex's
// maxTorrentsPerEntry) or widening capRetainedElem's element budget restores a
// multi-MB resident row in a 256 MiB container (CWE-400) while the rest of the
// suite stays green. The sibling TestReportBoundsRetainedUntrustedStrings bounds
// each element by the Loki LOG-LINE budget, 32x looser, so it cannot see either
// regression.
func TestReportBoundsRetainedRowToItsDocumentedCeiling(t *testing.T) {
	t.Parallel()
	const upstreamMax = 512 // internal/seadex's maxTorrentsPerEntry
	huge := strings.Repeat("z", 4*maxAttrBytes)
	groups := make([]string, upstreamMax)
	current := make([]string, upstreamMax)
	links := make([]compare.ReleaseLink, upstreamMax)
	for i := range groups {
		groups[i] = huge
		current[i] = huge
		links[i] = compare.ReleaseLink{Tracker: huge, URL: huge}
	}

	n, _ := newCapturedNotifier()
	n.Report([]compare.Finding{{
		AniListID:         5,
		Status:            compare.StatusBetter,
		Title:             "Frieren",
		RecommendedGroups: groups,
		CurrentGroups:     current,
		Links:             links,
	}}, nil)

	if len(n.current) != 1 {
		t.Fatalf("retained %d rows, want 1", len(n.current))
	}
	for _, got := range n.current {
		for field, count := range map[string]int{
			"RecommendedGroups": len(got.RecommendedGroups),
			"CurrentGroups":     len(got.CurrentGroups),
			"Links":             len(got.Links),
		} {
			if count != maxRetainedListItems {
				t.Errorf("retained %s holds %d elements, want %d (the count cap)", field, count, maxRetainedListItems)
			}
		}
		elems := map[string][]string{
			"RecommendedGroups element": got.RecommendedGroups,
			"CurrentGroups element":     got.CurrentGroups,
		}
		for _, l := range got.Links {
			elems["Links element Tracker"] = append(elems["Links element Tracker"], l.Tracker)
			elems["Links element URL"] = append(elems["Links element URL"], l.URL)
		}
		for field, values := range elems {
			for _, v := range values {
				if len(v) > maxRetainedElemBytes {
					t.Errorf("retained %s is %d bytes, want <= %d (the element cap)", field, len(v), maxRetainedElemBytes)
					break
				}
			}
		}
	}
}

// TestReportDoesNotMutateCallerFindings pins the aliasing guard in
// boundRetained. The retained row is a SHALLOW copy of the caller's finding, so
// its three slice headers still point at the caller's backing arrays - bounding
// them in place would silently edit the compare result the audit report and the
// cycle's own log line also read.
func TestReportDoesNotMutateCallerFindings(t *testing.T) {
	t.Parallel()
	huge := strings.Repeat("y", 4*maxAttrBytes)
	findings := []compare.Finding{{
		AniListID:         11,
		Status:            compare.StatusBetter,
		Title:             "Frieren",
		RecommendedGroups: []string{huge},
		CurrentGroups:     []string{huge},
		Links:             []compare.ReleaseLink{{Tracker: "Nyaa", URL: huge}},
	}}
	n, _ := newCapturedNotifier()
	n.Report(findings, nil)

	if got := len(findings[0].RecommendedGroups[0]); got != len(huge) {
		t.Errorf("caller's RecommendedGroups[0] shrank to %d bytes; Report must not mutate it", got)
	}
	if got := len(findings[0].CurrentGroups[0]); got != len(huge) {
		t.Errorf("caller's CurrentGroups[0] shrank to %d bytes; Report must not mutate it", got)
	}
	if got := len(findings[0].Links[0].URL); got != len(huge) {
		t.Errorf("caller's Links[0].URL shrank to %d bytes; Report must not mutate it", got)
	}
}
