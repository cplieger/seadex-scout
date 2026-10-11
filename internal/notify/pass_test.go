package notify

import (
	"log/slog"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/cplieger/seadex-scout/internal/compare"
	"github.com/cplieger/seadex-scout/internal/logcontract"
	"github.com/cplieger/slogx/capture"
)

func sizedFinding(key, title string, releaseBytes, currentBytes int64) compare.Finding {
	f := testFinding(key, title)
	f.ReleaseBytes, f.CurrentBytes = releaseBytes, currentBytes
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

func viewLine(t *testing.T, recorder *capture.Recorder, tier string) map[string]slog.Value {
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

// TestEveryPassEmitsBothViews pins that Report, ReportScoped and Reemit each
// close with one upgrade sizes line per view, before the findings reported
// line, and that every line of the pass carries its pass_id.
func TestEveryPassEmitsBothViews(t *testing.T) {
	n, recorder, now := clockedNotifier()
	f := sizedFinding("a", "Aria", 10, 4)
	g := sizedFinding("b", "Bria", 11, 4)
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
			if !slices.Equal(tiers, []string{"alt", "none"}) {
				t.Errorf("upgrade sizes views = %v, want [alt none]", tiers)
			}
			if msgs[len(msgs)-1] != "findings reported" {
				t.Errorf("last line = %q, want findings reported", msgs[len(msgs)-1])
			}
		})
	}
}

// TestRanksPerView pins the per-view ranks: newer revisions first, then size
// change descending with an unknown size last, then key order; an alt-tier row
// has rank_alt 0 and no other row does, and a manual-review row takes no rank.
func TestRanksPerView(t *testing.T) {
	n, recorder, _ := clockedNotifier()
	small := sizedFinding("a", "Small", 10, 5)
	large := sizedFinding("b", "Large", 100, 5)
	large.Tier = compare.TierAlt
	unsized := testFinding("c", "Unsized")
	newer := sizedFinding("d", "Newer", 1, 5)
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
			for _, key := range []string{"recommended_bytes", "size_change_bytes"} {
				if _, present := l[key]; present {
					t.Errorf("an unsized row carries %s", key)
				}
			}
		}
	}
	if alt := viewLine(t, recorder, "alt"); int64Of(alt["upgrades"]) != 3 {
		t.Errorf("alt view upgrades = %v, want 3", alt["upgrades"])
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

// TestIgnoredFindingsReachNoPassLine pins that an ignored show is in no row,
// rank, count or event.
func TestIgnoredFindingsReachNoPassLine(t *testing.T) {
	n, recorder, _ := clockedNotifier(999)
	ignored := sizedFinding("i", "Ignored", 1000, 1)
	ignored.AniListID = 999
	n.Report([]compare.Finding{ignored, sizedFinding("k", "Kept", 10, 1)}, nil)
	n.Report(nil, nil)
	for _, rec := range recorder.Records() {
		rec.Attrs(func(a slog.Attr) bool {
			if a.Key == "title" && a.Value.String() == "Ignored" || a.Key == "group" && a.Value.String() == "never" {
				t.Errorf("%q names the ignored show", rec.Message)
			}
			return true
		})
	}
	if got := lines(recorder, "upgrade sizes")[1]; int64Of(got["upgrades"]) != 1 {
		t.Errorf("view line = %v, want only the kept show counted", got)
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

// TestPassLinesCarryTheLogContract pins every pass message the log contract
// declares to the attributes it lists.
func TestPassLinesCarryTheLogContract(t *testing.T) {
	contract, err := logcontract.Load(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("parse the log contract: %v", err)
	}
	n, recorder, now := clockedNotifier()
	a := sizedFinding("a", "Aria", 10, 4)
	n.Report([]compare.Finding{a}, nil)
	*now = now.Add(time.Minute)
	n.Report(nil, nil)
	for _, msg := range []string{"findings reported", "upgrade sizes", "upgrade found", "upgrade resolved"} {
		want := contract.Messages[msg]
		got := lines(recorder, msg)
		if len(want) == 0 || len(got) == 0 {
			t.Errorf("%q: contract attrs %v, %d lines, want both", msg, want, len(got))
			continue
		}
		for _, key := range want {
			if _, ok := got[0][key]; !ok {
				t.Errorf("%q line lacks %q, which the log contract declares stable", msg, key)
			}
		}
	}
}
