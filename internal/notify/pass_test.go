package notify

import (
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/cplieger/seadex-scout/internal/compare"
	"github.com/cplieger/seadex-scout/internal/logcontract"
	"github.com/cplieger/slogx/capture"
	"pgregory.net/rapid"
)

func download(id string, bytes int64) compare.Download { return compare.Download{ID: id, Bytes: bytes} }

const conflictMsg = "one download or file carries two sizes across findings; the finding that brought the second is left out of the size totals"

func sizedFinding(key, title string, releaseBytes, currentBytes int64, downloads ...compare.Download) compare.Finding {
	f := testFinding(key, title)
	f.Downloads, f.ReleaseBytes = downloads, releaseBytes
	f.Replaced, f.CurrentBytes = []compare.Replacement{{Key: "file-" + key, Bytes: currentBytes}}, currentBytes
	return f
}

func lines(recorder *capture.Recorder, msg string) []map[string]slog.Value {
	var out []map[string]slog.Value
	for _, rec := range recorder.Records() {
		if rec.Message != msg {
			continue
		}
		attrs := map[string]slog.Value{}
		rec.Attrs(func(a slog.Attr) bool {
			attrs[a.Key] = a.Value
			return true
		})
		out = append(out, attrs)
	}
	return out
}

func sizes(t *testing.T, recorder *capture.Recorder, tier string) map[string]slog.Value {
	t.Helper()
	var last map[string]slog.Value
	for _, l := range lines(recorder, "upgrade sizes") {
		if l["hidden_tier"].String() == tier {
			last = l
		}
	}
	if last == nil {
		t.Fatalf("no upgrade sizes line for hidden_tier %q", tier)
	}
	return last
}

func int64Of(v slog.Value) int64 { return v.Int64() }

func clockedNotifier(ignored ...int) (*Notifier, *capture.Recorder, *time.Time) {
	n, recorder := newIgnoringNotifier(ignored...)
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	n.now = func() time.Time { return now }
	return n, recorder, &now
}

// TestEveryPassEmitsBothViewsSizes pins that Report, ReportScoped and Reemit
// each close with one upgrade sizes line per view, before the findings
// reported line, and that every line of the pass, the size-conflict warning
// included, carries its pass_id.
func TestEveryPassEmitsBothViewsSizes(t *testing.T) {
	n, recorder, now := clockedNotifier()
	f := sizedFinding("a", "Aria", 10, 4, download("x", 10))
	g := sizedFinding("b", "Bria", 11, 4, download("x", 11))
	g.AniListID = f.AniListID + 1
	both := []compare.Finding{f, g}
	passes := map[string]func(){
		"Report":       func() { n.Report(both, nil) },
		"ReportScoped": func() { n.ReportScoped(both, map[int]struct{}{f.AniListID: {}, g.AniListID: {}}, nil) },
		"Reemit":       n.Reemit,
	}
	for _, name := range []string{"Report", "ReportScoped", "Reemit"} {
		t.Run(name, func(t *testing.T) {
			*now = now.Add(15 * time.Minute)
			start := len(recorder.Records())
			passes[name]()
			pass := now.UnixMilli()
			var msgs, tiers []string
			for _, rec := range recorder.Records()[start:] {
				msgs = append(msgs, rec.Message)
				carries := false
				rec.Attrs(func(a slog.Attr) bool {
					if a.Key == "pass_id" {
						carries = true
						if a.Value.Int64() != pass {
							t.Errorf("%q carries pass_id %d, want %d", rec.Message, a.Value.Int64(), pass)
						}
					}
					if a.Key == "hidden_tier" && rec.Message == "upgrade sizes" {
						tiers = append(tiers, a.Value.String())
					}
					return true
				})
				if !carries {
					t.Errorf("%q carries no pass_id, want %d", rec.Message, pass)
				}
			}
			if !slices.Contains(msgs, conflictMsg) {
				t.Errorf("pass lines = %q, want the size-conflict warning among them", msgs)
			}
			if !slices.Equal(tiers, []string{"alt", "none"}) {
				t.Errorf("upgrade sizes views = %v, want [alt none]", tiers)
			}
			if msgs[len(msgs)-1] != "findings reported" {
				t.Errorf("last line = %q, want findings reported", msgs[len(msgs)-1])
			}
		})
	}
}

// TestSizeTotalsCountEachDownloadAndFileOnce pins the unions: a torrent two
// findings share, overlapping download sets and a file two findings replace
// each count once, while each row keeps its own full size.
func TestSizeTotalsCountEachDownloadAndFileOnce(t *testing.T) {
	n, recorder, _ := clockedNotifier()
	a := sizedFinding("a", "Aria", 30, 5, download("x", 10), download("y", 20))
	b := sizedFinding("b", "Bocchi", 50, 5, download("y", 20), download("z", 30))
	b.Replaced = a.Replaced
	n.Report([]compare.Finding{a, b}, nil)
	got := sizes(t, recorder, "none")
	if int64Of(got["recommended_bytes_total"]) != 60 || int64Of(got["current_bytes_replaced"]) != 5 || int64Of(got["size_change_bytes"]) != 55 {
		t.Errorf("totals = %v, want recommended 60, replaced 5, change 55", got)
	}
	if int64Of(got["upgrades_sized"]) != 2 || int64Of(got["upgrades_unsized"]) != 0 {
		t.Errorf("sized, unsized = %v, %v, want 2, 0", got["upgrades_sized"], got["upgrades_unsized"])
	}
	for _, l := range lines(recorder, "better release available") {
		if l["title"].String() == "Bocchi" && int64Of(l["recommended_bytes"]) != 50 {
			t.Errorf("Bocchi's row recommended_bytes = %v, want its full 50", l["recommended_bytes"])
		}
	}
}

// TestSizeTotalsSkipWhatTheyCannotCount pins the unsized cases: an unknown
// side, an identity carrying two sizes (one WARN names it), and a sum past
// math.MaxInt64, each leaving the totals at their last valid value in rank
// order.
func TestSizeTotalsSkipWhatTheyCannotCount(t *testing.T) {
	n, recorder, _ := clockedNotifier()
	big := sizedFinding("a", "Aria", math.MaxInt64-10, 1, download("big", math.MaxInt64-10))
	overflow := sizedFinding("b", "Bocchi", 20, 1, download("more", 20))
	conflict := sizedFinding("c", "Chainsaw", 7, 1, download("big", 7))
	unknown := testFinding("d", "Dandadan")
	unknown.ReleaseBytes = 5
	n.Report([]compare.Finding{big, overflow, conflict, unknown}, nil)
	got := sizes(t, recorder, "none")
	if int64Of(got["recommended_bytes_total"]) != math.MaxInt64-10 || int64Of(got["upgrades_sized"]) != 1 || int64Of(got["upgrades_unsized"]) != 3 {
		t.Errorf("totals = %v, want only the first-ranked finding counted", got)
	}
	if n := recorder.CountExact(conflictMsg); n != 1 {
		t.Errorf("conflict WARN logged %d times, want 1 across both views", n)
	}
}

// TestDownloadTotalCountsEveryKnownDownload pins download_bytes_total: an
// upgrade whose replaced files have no known size still adds its download,
// a torrent it shares with a sized upgrade counts once, and only an upgrade
// with no known download is left out of it.
func TestDownloadTotalCountsEveryKnownDownload(t *testing.T) {
	n, recorder, _ := clockedNotifier()
	sized := sizedFinding("a", "Aria", 10, 4, download("x", 10))
	noCurrent := testFinding("b", "Bocchi")
	noCurrent.Downloads, noCurrent.ReleaseBytes = []compare.Download{download("y", 20)}, 20
	shared := testFinding("c", "Chainsaw")
	shared.Downloads, shared.ReleaseBytes = []compare.Download{download("x", 10)}, 10
	unknown := testFinding("d", "Dandadan")
	n.Report([]compare.Finding{sized, noCurrent, shared, unknown}, nil)
	got := sizes(t, recorder, "none")
	if int64Of(got["download_bytes_total"]) != 30 || int64Of(got["upgrades_download_unsized"]) != 1 {
		t.Errorf("download total, download unsized = %v, %v, want 30, 1", got["download_bytes_total"], got["upgrades_download_unsized"])
	}
	if int64Of(got["recommended_bytes_total"]) != 10 || int64Of(got["upgrades_unsized"]) != 3 {
		t.Errorf("sized total, unsized = %v, %v, want 10, 3", got["recommended_bytes_total"], got["upgrades_unsized"])
	}
}

// TestDownloadTotalBoundsTheSizedTotalProperty pins download_bytes_total at or
// above recommended_bytes_total whichever replaced sizes are unknown.
func TestDownloadTotalBoundsTheSizedTotalProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		n, recorder := newCapturedNotifier()
		count := rapid.IntRange(0, 6).Draw(t, "findings")
		var findings []compare.Finding
		for i := range count {
			release := rapid.Int64Range(1, math.MaxInt64/8).Draw(t, "release")
			current := rapid.Int64Range(0, math.MaxInt64/8).Draw(t, "current")
			findings = append(findings, sizedFinding(fmt.Sprint(i), fmt.Sprint("T", i), release, current, download(fmt.Sprint("d", i%3), release)))
		}
		n.Report(findings, nil)
		for _, l := range lines(recorder, "upgrade sizes") {
			if floor, sized := int64Of(l["download_bytes_total"]), int64Of(l["recommended_bytes_total"]); floor < sized {
				t.Fatalf("download_bytes_total = %d, below recommended_bytes_total %d", floor, sized)
			}
		}
	})
}

// TestSizeChangeIsTheTotalsDifferenceProperty pins size_change_bytes to the
// difference of the two totals for any non-negative sizes.
func TestSizeChangeIsTheTotalsDifferenceProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		n, recorder := newCapturedNotifier()
		count := rapid.IntRange(0, 6).Draw(t, "findings")
		var findings []compare.Finding
		for i := range count {
			release := rapid.Int64Range(1, math.MaxInt64/4).Draw(t, "release")
			current := rapid.Int64Range(1, math.MaxInt64/4).Draw(t, "current")
			findings = append(findings, sizedFinding(fmt.Sprint(i), fmt.Sprint("T", i), release, current, download(fmt.Sprint("d", i), release)))
		}
		n.Report(findings, nil)
		for _, l := range lines(recorder, "upgrade sizes") {
			if got, want := int64Of(l["size_change_bytes"]), int64Of(l["recommended_bytes_total"])-int64Of(l["current_bytes_replaced"]); got != want {
				t.Fatalf("size_change_bytes = %d, want %d", got, want)
			}
		}
	})
}

// TestRanksPerView pins the per-view ranks: newer revisions first, then size
// change descending with an unknown size last, then key order; an alt-tier row
// has rank_alt 0 and no other row does, and a manual-review row takes no rank.
func TestRanksPerView(t *testing.T) {
	n, recorder, _ := clockedNotifier()
	small := sizedFinding("a", "Small", 10, 5, download("a", 10))
	large := sizedFinding("b", "Large", 100, 5, download("b", 100))
	large.Tier = compare.TierAlt
	unsized := testFinding("c", "Unsized")
	newer := sizedFinding("d", "Newer", 1, 5, download("d", 1))
	newer.Status = compare.StatusNewerRevision
	mixed := testFinding("e", "Mixed")
	mixed.Status = compare.StatusMixedGroup
	n.Report([]compare.Finding{small, large, unsized, newer, mixed}, nil)
	want := map[string][2]int64{"Newer": {1, 1}, "Large": {0, 2}, "Small": {2, 3}, "Unsized": {3, 4}}
	got := map[string][2]int64{}
	for _, l := range lines(recorder, "better release available") {
		got[l["title"].String()] = [2]int64{int64Of(l["rank_alt"]), int64Of(l["rank_none"])}
	}
	for title, ranks := range want {
		if got[title] != ranks {
			t.Errorf("%s rank_alt, rank_none = %v, want %v", title, got[title], ranks)
		}
	}
	for _, l := range lines(recorder, "better release available") {
		if l["title"].String() == "Unsized" {
			for _, key := range []string{"recommended_bytes", "current_bytes", "size_change_bytes"} {
				if _, present := l[key]; present {
					t.Errorf("an unsized row carries %s", key)
				}
			}
		}
	}
	if alt := sizes(t, recorder, "alt"); int64Of(alt["upgrades"]) != 3 || int64Of(alt["upgrades_newer_revision"]) != 1 {
		t.Errorf("alt view upgrades, newer = %v, %v, want 3, 1", alt["upgrades"], alt["upgrades_newer_revision"])
	}
}

// TestManualReviewRowsAreNumbered pins rank_check on the two manual-review
// messages, 1 upward in emission order, and their count on findings reported.
func TestManualReviewRowsAreNumbered(t *testing.T) {
	n, recorder, _ := clockedNotifier()
	mixed, unverifiable := testFinding("a", "A"), testFinding("b", "B")
	mixed.Status, unverifiable.Status = compare.StatusMixedGroup, compare.StatusUnverifiable
	n.Report([]compare.Finding{mixed, unverifiable, testFinding("c", "C")}, nil)
	var ranks []int64
	for _, rec := range recorder.Records() {
		rec.Attrs(func(a slog.Attr) bool {
			if a.Key == "rank_check" {
				ranks = append(ranks, a.Value.Int64())
			}
			return true
		})
	}
	if !slices.Equal(ranks, []int64{1, 2}) {
		t.Errorf("rank_check = %v, want [1 2] on the two manual-review rows only", ranks)
	}
	if got, _ := summaryCounter(t, recorder, "manual_review"); got != 2 {
		t.Errorf("manual_review = %d, want 2", got)
	}
}

// TestBiggestUpgradesAreBoundedAndOrdered pins at most 25 biggest upgrade
// lines per view, by absolute size change, a shrink as big as a growth.
func TestBiggestUpgradesAreBoundedAndOrdered(t *testing.T) {
	n, recorder, _ := clockedNotifier()
	var findings []compare.Finding
	for i := range 30 {
		findings = append(findings, sizedFinding(fmt.Sprint(i), fmt.Sprint("T", i), int64(100+i), 100, download(fmt.Sprint(i), int64(100+i))))
	}
	shrink := sizedFinding("s", "Shrink", 10, 1000, download("s", 10))
	findings = append(findings, shrink)
	n.Report(findings, nil)
	var titles []string
	for _, l := range lines(recorder, "biggest upgrade") {
		if l["hidden_tier"].String() == "none" {
			titles = append(titles, l["title"].String())
			if int64Of(l["rank"]) != int64(len(titles)) {
				t.Errorf("rank %v at position %d", l["rank"], len(titles))
			}
		}
	}
	if len(titles) != maxBiggestUpgrades || titles[0] != "Shrink" || titles[1] != "T29" {
		t.Errorf("biggest = %v, want 25 lines led by Shrink then T29", titles)
	}
}

// TestIgnoredFindingsReachNoPassLine pins that an ignored show is in no row,
// rank, total, ranking or event.
func TestIgnoredFindingsReachNoPassLine(t *testing.T) {
	n, recorder, _ := clockedNotifier(999)
	ignored := sizedFinding("i", "Ignored", 1000, 1, download("i", 1000))
	ignored.AniListID = 999
	n.Report([]compare.Finding{ignored, sizedFinding("k", "Kept", 10, 1, download("k", 10))}, nil)
	n.Report(nil, nil)
	for _, rec := range recorder.Records() {
		rec.Attrs(func(a slog.Attr) bool {
			if a.Key == "title" && a.Value.String() == "Ignored" || a.Key == "group" && a.Value.String() == "never" {
				t.Errorf("%q names the ignored show", rec.Message)
			}
			return true
		})
	}
	if got := lines(recorder, "upgrade sizes")[1]; int64Of(got["recommended_bytes_total"]) != 10 || int64Of(got["upgrades"]) != 1 {
		t.Errorf("totals = %v, want only the kept show", got)
	}
}

// TestUpgradeEventsFollowTheSet pins the found and resolved events, the
// after_start mark on the first pass only, and first_seen surviving passes
// and resetting on a new notifier.
func TestUpgradeEventsFollowTheSet(t *testing.T) {
	n, recorder, now := clockedNotifier()
	first := now.UnixMilli()
	a, b := testFinding("a", "Aria"), testFinding("b", "Bocchi")
	mixed := testFinding("m", "Mixed")
	mixed.Status = compare.StatusMixedGroup
	n.Report([]compare.Finding{a, mixed}, nil)
	*now = now.Add(15 * time.Minute)
	n.Report([]compare.Finding{a, b}, nil)

	type event struct {
		msg, title string
		afterStart bool
	}
	var got []event
	for _, msg := range []string{"upgrade found", "upgrade resolved"} {
		for _, l := range lines(recorder, msg) {
			got = append(got, event{msg, l["title"].String(), l["after_start"].Bool()})
		}
	}
	want := []event{{"upgrade found", "Aria", true}, {"upgrade found", "Bocchi", false}}
	if !slices.Equal(got, want) {
		t.Errorf("events = %v, want %v", got, want)
	}
	*now = now.Add(15 * time.Minute)
	n.Report([]compare.Finding{b}, nil)
	if r := lines(recorder, "upgrade resolved"); len(r) != 1 || r[0]["title"].String() != "Aria" {
		t.Errorf("resolved = %v, want Aria", r)
	}
	rows := lines(recorder, "better release available")
	if last := rows[len(rows)-1]; int64Of(last["first_seen"]) != first+15*60*1000 {
		t.Errorf("Bocchi first_seen = %v, want the pass it entered", last["first_seen"])
	}
	for _, l := range rows {
		if l["title"].String() == "Aria" && int64Of(l["first_seen"]) != first {
			t.Errorf("Aria first_seen = %v on a later pass, want %d", l["first_seen"], first)
		}
	}

	fresh, freshRecorder, freshNow := clockedNotifier()
	*freshNow = now.Add(time.Hour)
	fresh.Report([]compare.Finding{b}, nil)
	if l := lines(freshRecorder, "better release available"); int64Of(l[0]["first_seen"]) != freshNow.UnixMilli() {
		t.Errorf("a new notifier's first_seen = %v, want its own first pass", l[0]["first_seen"])
	}
	if l := lines(freshRecorder, "upgrade found"); len(l) != 1 || !l[0]["after_start"].Bool() {
		t.Errorf("a new notifier's found events = %v, want one after_start", l)
	}
}

// TestPassLinesCarryTheLogContract pins every pass message alerts/logql.yaml
// declares to the attributes it lists.
func TestPassLinesCarryTheLogContract(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "alerts", "logql.yaml"))
	if err != nil {
		t.Fatalf("read alerts/logql.yaml: %v", err)
	}
	contract, err := logcontract.Parse(raw)
	if err != nil {
		t.Fatalf("parse the log contract: %v", err)
	}
	n, recorder, now := clockedNotifier()
	a := sizedFinding("a", "Aria", 10, 4, download("x", 10))
	n.Report([]compare.Finding{a}, nil)
	*now = now.Add(time.Minute)
	n.Report(nil, nil)
	for _, msg := range []string{"findings reported", "upgrade sizes", "biggest upgrade", "upgrade found", "upgrade resolved"} {
		want := contract.Messages[msg]
		got := lines(recorder, msg)
		if len(want) == 0 || len(got) == 0 {
			t.Errorf("%q: contract attrs %v, %d lines, want both", msg, want, len(got))
			continue
		}
		for _, key := range want {
			if _, ok := got[0][key]; !ok {
				t.Errorf("%q line lacks %q, which alerts/logql.yaml declares stable", msg, key)
			}
		}
	}
}
