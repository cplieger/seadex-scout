package audit

import (
	"os"
	"slices"
	"testing"
	"time"

	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/logcontract"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/match"
	"github.com/cplieger/seadex-scout/internal/seadex"
	"github.com/cplieger/slogx/capture"
)

// seasonMatch links one season-scoped SeaDex entry to item: best is the group
// SeaDex marks best for that season, alt the group it lists beside it.
func seasonMatch(item *library.Item, alID, season int, best, alt string) match.Match {
	torrents := []seadex.Torrent{{Tracker: "Nyaa", ReleaseGroup: best, IsBest: true, URL: "https://nyaa.si/view/1"}}
	if alt != "" {
		torrents = append(torrents, seadex.Torrent{Tracker: "Nyaa", ReleaseGroup: alt, URL: "https://nyaa.si/view/2"})
	}
	return match.Match{
		Item: item, Arr: item.Arr, Source: match.SourceID,
		Entry:  seadex.Entry{AniListID: alID, Torrents: torrents},
		Record: mapping.Record{Type: "TV", TvdbID: item.TvdbID, SeasonTvdb: season},
	}
}

// TestAuditItemTotals pins the item-level counts: a series with several
// entries is one item, a row nothing was compared on neither earns nor blocks
// all-at-best or all-best-or-alt, an alt row moves an item from the first to
// the second, a row below alt blocks both, and an item that is both matched
// and not_on_seadex counts once.
func TestAuditItemTotals(t *testing.T) {
	snap := &library.Snapshot{Items: []library.Item{
		{
			Arr: library.ArrSonarr, ArrID: 1, Title: "AllBest", TvdbID: 100, HasFile: true,
			Groups: []string{"good"}, SeasonGroups: map[int][]string{1: {"good"}, 2: {"good"}},
		},
		{
			Arr: library.ArrSonarr, ArrID: 2, Title: "HalfAlt", TvdbID: 200, HasFile: true,
			Groups: []string{"good", "meh"}, SeasonGroups: map[int][]string{1: {"good"}, 2: {"meh"}},
		},
		{
			Arr: library.ArrSonarr, ArrID: 3, Title: "OnlyNoFile", TvdbID: 300, HasFile: true,
			Groups: []string{"x"}, SeasonGroups: map[int][]string{1: {"x"}},
		},
		{
			Arr: library.ArrSonarr, ArrID: 4, Title: "Uncovered", TvdbID: 400, HasFile: true,
			Groups: []string{"y"}, SeasonGroups: map[int][]string{1: {"y"}},
		},
		{
			Arr: library.ArrSonarr, ArrID: 5, Title: "OfferedOnly", TvdbID: 500, HasFile: true,
			Groups: []string{"erai"}, SeasonGroups: map[int][]string{0: {"erai"}},
		},
		{
			Arr: library.ArrSonarr, ArrID: 6, Title: "AltUnlisted", TvdbID: 600, HasFile: true,
			Groups: []string{"meh", "zzz"}, SeasonGroups: map[int][]string{1: {"meh"}, 2: {"zzz"}},
		},
	}}
	idx := mapping.NewIndex([]mapping.Record{
		{AniListID: 1, Type: "TV", TvdbID: 100},
		{AniListID: 4, Type: "TV", TvdbID: 200},
		{AniListID: 6, Type: "TV", TvdbID: 300},
		{AniListID: 7, Type: "TV", TvdbID: 400},
		{AniListID: 9, Type: "TV", TvdbID: 500},
		{AniListID: 10, Type: "TV", TvdbID: 600},
	})
	offered := seasonMatch(&snap.Items[4], 8, 0, "best", "")
	offered.Record = mapping.Record{Type: "MOVIE", TvdbID: 500, SeasonKind: mapping.SeasonPresent}
	matches := []match.Match{
		seasonMatch(&snap.Items[0], 1, 1, "good", ""),
		seasonMatch(&snap.Items[0], 2, 2, "good", ""),
		seasonMatch(&snap.Items[0], 3, 3, "good", ""),
		seasonMatch(&snap.Items[1], 4, 1, "good", ""),
		seasonMatch(&snap.Items[1], 5, 2, "great", "meh"),
		seasonMatch(&snap.Items[2], 6, 2, "good", ""),
		offered,
		seasonMatch(&snap.Items[5], 10, 1, "great", "meh"),
		seasonMatch(&snap.Items[5], 11, 2, "good", ""),
	}

	rep := New(Config{}).Audit(matches, snap, idx, nil)

	verdicts := map[string][]Verdict{}
	for i := range rep.Rows {
		verdicts[rep.Rows[i].Title] = append(verdicts[rep.Rows[i].Title], rep.Rows[i].Verdict)
	}
	for title, want := range map[string][]Verdict{
		"AllBest":     {VerdictNoFile, VerdictBest, VerdictBest},
		"HalfAlt":     {VerdictAlt, VerdictBest},
		"OnlyNoFile":  {VerdictNoFile},
		"Uncovered":   {VerdictNotOnSeaDex},
		"OfferedOnly": {VerdictUnattributed, VerdictNotOnSeaDex},
		"AltUnlisted": {VerdictUnlisted, VerdictAlt},
	} {
		if got := verdicts[title]; !slices.Equal(got, want) {
			t.Fatalf("%s verdicts = %v, want %v: the fixture no longer builds the case it names", title, got, want)
		}
	}
	want := ItemTotals{Anime: 6, WithEntry: 5, AllBest: 1, AllBestOrAlt: 2}
	if rep.Items != want {
		t.Errorf("Audit(...).Items = %+v, want %+v", rep.Items, want)
	}
}

func libraryContract(t *testing.T, msg string) []string {
	t.Helper()
	raw, err := os.ReadFile("../../alerts/logql.yaml")
	if err != nil {
		t.Fatalf("read alerts/logql.yaml: %v", err)
	}
	c, err := logcontract.Parse(raw)
	if err != nil {
		t.Fatalf("parse the log contract: %v", err)
	}
	attrs, ok := c.Messages[msg]
	if !ok || len(attrs) == 0 {
		t.Fatalf("alerts/logql.yaml declares no attributes for %q", msg)
	}
	return attrs
}

func TestLogLibraryEmitsTheContract(t *testing.T) {
	log, rec := capture.New()
	r := &Report{
		GeneratedAt: time.Unix(0, 0).UTC(),
		Totals:      map[string]int{string(VerdictBest): 4, string(VerdictNoFile): 1, string(VerdictUnverified): 1, string(VerdictAlt): 1},
		Items:       ItemTotals{Anime: 9, WithEntry: 7, AllBest: 3, AllBestOrAlt: 5},
		Rows: []Row{
			{Title: "Gone", AniListID: 1, Arr: library.ArrSonarr, Verdict: VerdictNoFile, SeaDexURL: "https://releases.moe/1"},
			{Title: "Unknown", AniListID: 2, Arr: library.ArrRadarr, Verdict: VerdictUnverified, SeaDexURL: "https://releases.moe/2"},
			{Title: "Alt", AniListID: 3, Arr: library.ArrSonarr, Verdict: VerdictAlt},
			{Title: "Best", AniListID: 4, Arr: library.ArrSonarr, Verdict: VerdictBest},
		},
	}

	r.LogLibrary(log)

	recs := rec.Records()
	if len(recs) != 1 || recs[0].Message != "library summary" {
		t.Fatalf("LogLibrary emitted %v, want one library summary and no per-row line", rec.Messages())
	}
	summaryAttrs := recordAttrs(recs[0])
	for _, key := range libraryContract(t, "library summary") {
		if _, ok := summaryAttrs[key]; !ok {
			t.Errorf("library summary lacks %q, which alerts/logql.yaml declares stable", key)
		}
	}
	for key, want := range map[string]int64{
		"rows": 4, "have_best": 4, "no_file": 1, "have_alt": 1, "anime_items": 9, "items_with_entry": 7,
		"items_all_best": 3, "items_all_best_or_alt": 5,
	} {
		if summaryAttrs[key] != want {
			t.Errorf("library summary %s = %v, want %d", key, summaryAttrs[key], want)
		}
	}
}

func TestReportSummaryCarriesItemTotals(t *testing.T) {
	log, rec := capture.New()
	r := &Report{GeneratedAt: time.Unix(0, 0).UTC(), Totals: map[string]int{}, Items: ItemTotals{Anime: 3, WithEntry: 2, AllBest: 1, AllBestOrAlt: 2}}
	if err := r.Log(t.Context(), log); err != nil {
		t.Fatalf("Log: %v", err)
	}
	attrs := recordAttrs(rec.Records()[0])
	for key, want := range map[string]int64{"anime_items": 3, "items_with_entry": 2, "items_all_best": 1, "items_all_best_or_alt": 2} {
		if attrs[key] != want {
			t.Errorf("report summary %s = %v, want %d", key, attrs[key], want)
		}
	}
}
