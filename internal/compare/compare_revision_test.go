package compare

import (
	"fmt"
	"testing"

	"github.com/cplieger/seadex-scout/internal/audit"
	"github.com/cplieger/seadex-scout/internal/filter"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/match"
	"github.com/cplieger/seadex-scout/internal/release"
	"github.com/cplieger/seadex-scout/internal/seadex"
)

var (
	revOriginal = release.Revision{Version: 1, Marker: release.RevisionNone}
	revV2       = release.Revision{Version: 2, Marker: release.RevisionVersion}
)

func udfFiles(token string) []seadex.File {
	return []seadex.File{
		{Name: "[UDF] 91 Days - 04" + token + " (BDRip 1080p x264 FLACx2).mkv", Length: 1 << 30},
		{Name: "[UDF] 91 Days - 05" + token + " (BDRip 1080p x264 FLACx2).mkv", Length: 1 << 30},
	}
}

// udfEntry lists UDF as best at the given token on Nyaa, beside a higher-resolution
// best from another group that a divergence would headline instead.
func udfEntry(token string) seadex.Entry {
	return seadex.Entry{AniListID: 21711, Torrents: []seadex.Torrent{
		{IsBest: true, ReleaseGroup: "UDF", Tracker: "Nyaa", URL: "https://nyaa.si/view/1", Files: udfFiles(token)},
		{IsBest: true, ReleaseGroup: "K", Tracker: "Nyaa", URL: "https://nyaa.si/view/2", Files: []seadex.File{
			{Name: "[K] 91 Days - 04 (BD 2160p HEVC).mkv", Length: 1 << 30},
		}},
	}}
}

func udfSeasonMatch(held release.Revision, entry seadex.Entry) match.Match {
	item := &library.Item{
		Title: "91 Days", Arr: library.ArrSonarr, HasFile: true,
		Groups:          []string{"udf"},
		SeasonGroups:    map[int][]string{1: {"udf"}},
		SeasonRevisions: map[int]map[string]release.Revision{1: {"udf": held}},
	}
	return match.Match{Item: item, Arr: library.ArrSonarr, Entry: entry, Record: mapping.Record{SeasonTvdb: 1}}
}

func TestCompareOlderRevisionOfBestGroupIsNewerRevision(t *testing.T) {
	got := comparer(filter.Options{}, false).Compare([]match.Match{udfSeasonMatch(revOriginal, udfEntry("v2"))})
	if len(got) != 1 {
		t.Fatalf("Compare(held UDF v1, listed UDF v2) = %d findings, want 1: %+v", len(got), got)
	}
	f := got[0]
	if f.Status != StatusNewerRevision {
		t.Errorf("status = %q, want %q", f.Status, StatusNewerRevision)
	}
	if f.RecommendedGroup != "UDF" || len(f.RecommendedGroups) != 1 || f.RecommendedGroups[0] != "udf" {
		t.Errorf("recommended = %q / %v, want UDF alone (the group already held), not the higher-resolution K", f.RecommendedGroup, f.RecommendedGroups)
	}
	if f.CurrentRevision != revOriginal || f.RecommendedRevision != revV2 {
		t.Errorf("revisions = %+v -> %+v, want %+v -> %+v", f.CurrentRevision, f.RecommendedRevision, revOriginal, revV2)
	}
	if len(f.Links) != 1 || f.Links[0].URL != "https://nyaa.si/view/1" {
		t.Errorf("links = %+v, want only the UDF release", f.Links)
	}
}

// TestCompareOlderRevisionOfDecoratedSeaDexLabel pins that the revision rule
// keys the listed and the held revisions on one group identity when SeaDex
// decorates the label, while the finding still names the group as SeaDex
// writes it.
func TestCompareOlderRevisionOfDecoratedSeaDexLabel(t *testing.T) {
	held := release.NormalizeGroup("ZR")
	item := &library.Item{
		Title: "BLUE LOCK", Arr: library.ArrSonarr, HasFile: true,
		Groups:          []string{held},
		SeasonGroups:    map[int][]string{1: {held}},
		SeasonRevisions: map[int]map[string]release.Revision{1: {held: revOriginal}},
	}
	entry := seadex.Entry{AniListID: 137822, Torrents: []seadex.Torrent{{
		IsBest: true, ReleaseGroup: "-ZR-", Tracker: "Nyaa", URL: "https://nyaa.si/view/1",
		Files: []seadex.File{{Name: "[-ZR-] BLUE LOCK - S01E03v2 [1080p].mkv", Length: 1 << 30}},
	}}}
	m := match.Match{Item: item, Arr: library.ArrSonarr, Entry: entry, Record: mapping.Record{SeasonTvdb: 1}}
	got := comparer(filter.Options{}, false).Compare([]match.Match{m})
	if len(got) != 1 || got[0].Status != StatusNewerRevision {
		t.Fatalf("Compare(held ZR v1, SeaDex best -ZR- v2) = %+v, want one %q finding", got, StatusNewerRevision)
	}
	if f := got[0]; f.RecommendedGroup != "-ZR-" || len(f.RecommendedGroups) != 1 || f.RecommendedGroups[0] != "zr" {
		t.Errorf("recommended = %q / %v, want the SeaDex label -ZR- over the identity [zr]", f.RecommendedGroup, f.RecommendedGroups)
	}
}

func TestCompareOlderRevisionOnIncompleteEntryIsIncomplete(t *testing.T) {
	entry := udfEntry("v2")
	entry.Incomplete = true
	got := comparer(filter.Options{}, false).Compare([]match.Match{udfSeasonMatch(revOriginal, entry)})
	if len(got) != 1 || got[0].Status != StatusIncomplete {
		t.Fatalf("Compare(incomplete entry, held UDF v1, listed v2) = %+v, want one incomplete finding", got)
	}
	if got[0].RecommendedGroup != "UDF" || got[0].RecommendedRevision != revV2 {
		t.Errorf("finding = %+v, want UDF at v2", got[0])
	}
}

// mixedUDFEntry lists UDF's 12-episode pack after the group reissued one
// episode: eleven untokened files and episode 07 as v2.
func mixedUDFEntry() seadex.Entry {
	entry := udfEntry("")
	files := make([]seadex.File, 0, 12)
	for i := 1; i <= 12; i++ {
		token := ""
		if i == 7 {
			token = "v2"
		}
		files = append(files, seadex.File{Name: fmt.Sprintf("[UDF] 91 Days - %02d%s (BDRip 1080p x264 FLACx2).mkv", i, token), Length: 1 << 30})
	}
	entry.Torrents[0].Files = files
	return entry
}

// TestCompareMixedPackReadsItsNewestFile pins the newest-held against
// newest-listed rule on a pack whose only reissued file is one episode. The
// held reading is the newest revision the arr recorded across the season's UDF
// files, so a library holding every original (or holding the pack without the
// fixed episode at all) reads behind, and one holding the fixed episode does not.
func TestCompareMixedPackReadsItsNewestFile(t *testing.T) {
	tests := []struct {
		desc       string
		held       release.Revision
		wantStatus Status
	}{
		{desc: "every held file original", held: revOriginal, wantStatus: StatusNewerRevision},
		{desc: "held with the fixed episode at v2", held: revV2},
		{desc: "every held file unknown", held: release.Revision{}},
	}
	for _, tc := range tests {
		got := comparer(filter.Options{}, false).Compare([]match.Match{udfSeasonMatch(tc.held, mixedUDFEntry())})
		var status Status
		if len(got) == 1 {
			status = got[0].Status
		}
		if len(got) > 1 || status != tc.wantStatus {
			t.Errorf("Compare(mixed pack, %s) = %+v, want status %q", tc.desc, got, tc.wantStatus)
			continue
		}
		if status == StatusNewerRevision && (got[0].CurrentRevision != revOriginal || got[0].RecommendedRevision != revV2) {
			t.Errorf("Compare(mixed pack, %s) revisions = %+v -> %+v, want v1 -> v2", tc.desc, got[0].CurrentRevision, got[0].RecommendedRevision)
		}
	}
}

func TestCompareGroupListsNewestAcrossItsBestTorrents(t *testing.T) {
	entry := udfEntry("")
	entry.Torrents = append(entry.Torrents, seadex.Torrent{
		IsBest: true, ReleaseGroup: "UDF", Tracker: "Nyaa", URL: "https://nyaa.si/view/4",
		Files: []seadex.File{{Name: "[UDF] 91 Days - 05v2 (BDRip 1080p x264 FLACx2).mkv", Length: 1 << 30}},
	})
	got := comparer(filter.Options{}, false).Compare([]match.Match{udfSeasonMatch(revOriginal, entry)})
	if len(got) != 1 || got[0].Status != StatusNewerRevision || got[0].RecommendedRevision != revV2 {
		t.Fatalf("Compare(UDF v1 pack beside a UDF v2 single episode, held v1) = %+v, want one newer_revision at v2", got)
	}
}

func TestCompareRevisionNeverClaimsWithoutProof(t *testing.T) {
	tests := []struct {
		desc  string
		held  release.Revision
		entry seadex.Entry
	}{
		{"held v2, listed v2", revV2, udfEntry("v2")},
		{"held v3, listed v2", release.Revision{Version: 3, Marker: release.RevisionVersion}, udfEntry("v2")},
		{"held unknown, listed v2", release.Revision{}, udfEntry("v2")},
		{"held v1, listed unversioned", revOriginal, udfEntry("")},
	}
	for _, tc := range tests {
		if got := comparer(filter.Options{}, false).Compare([]match.Match{udfSeasonMatch(tc.held, tc.entry)}); len(got) != 0 {
			t.Errorf("Compare(%s) = %+v, want no finding", tc.desc, got)
		}
	}
}

func TestCompareUnobtainableNewerRevisionKeepsDivergence(t *testing.T) {
	entry := udfEntry("v2")
	entry.Torrents[0].Tracker = "AB"
	entry.Torrents[0].URL = "/torrents.php?id=1&torrentid=2"
	got := comparer(filter.Options{}, false).Compare([]match.Match{udfSeasonMatch(revOriginal, entry)})
	if len(got) != 1 || got[0].Status != StatusBetter || got[0].RecommendedGroup != "K" {
		t.Fatalf("Compare(UDF v2 only on AnimeBytes, toggle off) = %+v, want one better_release toward K", got)
	}
	if got[0].CurrentRevision.Known() || got[0].RecommendedRevision.Known() {
		t.Errorf("better_release finding carries revisions %+v / %+v, want none", got[0].CurrentRevision, got[0].RecommendedRevision)
	}
}

func TestCompareWholeSeriesOlderRevisionIsNewerRevision(t *testing.T) {
	item := &library.Item{
		Title: "91 Days", Arr: library.ArrSonarr, HasFile: true,
		Groups:       []string{"udf"},
		SeasonGroups: map[int][]string{1: {"udf"}, 2: {"udf"}},
		SeasonRevisions: map[int]map[string]release.Revision{
			1: {"udf": revOriginal},
			2: {"udf": revOriginal},
		},
	}
	m := match.Match{Item: item, Arr: library.ArrSonarr, Entry: udfEntry("v2"), Record: mapping.Record{Type: "TV"}}
	got := comparer(filter.Options{}, false).Compare([]match.Match{m})
	if len(got) != 1 {
		t.Fatalf("Compare(seasonless entry, UDF v1 in seasons 1-2, listed v2) = %d findings, want 1: %+v", len(got), got)
	}
	f := got[0]
	if f.Status != StatusNewerRevision {
		t.Errorf("status = %q, want %q", f.Status, StatusNewerRevision)
	}
	if f.RecommendedGroup != "UDF" || len(f.RecommendedGroups) != 1 || f.RecommendedGroups[0] != "udf" {
		t.Errorf("recommended = %q / %v, want UDF alone (the group already held), not the higher-resolution K", f.RecommendedGroup, f.RecommendedGroups)
	}
	if f.CurrentRevision != revOriginal || f.RecommendedRevision != revV2 {
		t.Errorf("revisions = %+v -> %+v, want %+v -> %+v", f.CurrentRevision, f.RecommendedRevision, revOriginal, revV2)
	}
	if len(f.Links) != 1 || f.Links[0].URL != "https://nyaa.si/view/1" {
		t.Errorf("links = %+v, want only the UDF release", f.Links)
	}
}

func TestCompareMovieOlderRevisionIsNewerRevision(t *testing.T) {
	item := &library.Item{
		Title: "Given", Arr: library.ArrRadarr, HasFile: true,
		Groups:    []string{"ptp"},
		Revisions: map[string]release.Revision{"ptp": revOriginal},
	}
	entry := seadex.Entry{AniListID: 111734, Torrents: []seadex.Torrent{{
		IsBest: true, ReleaseGroup: "PTP", Tracker: "Nyaa", URL: "https://nyaa.si/view/3",
		Files: []seadex.File{{Name: "Given.2020.REPACK.1080p.BluRay.REMUX.DTS-HD.MA.5.1.AVC-PTP.mkv", Length: 1 << 30}},
	}}}
	m := match.Match{Item: item, Arr: library.ArrRadarr, Entry: entry, Record: mapping.Record{Type: "MOVIE"}}
	got := comparer(filter.Options{}, false).Compare([]match.Match{m})
	want := release.Revision{Version: 2, Marker: release.RevisionRepack}
	if len(got) != 1 || got[0].Status != StatusNewerRevision || got[0].RecommendedRevision != want {
		t.Fatalf("Compare(movie held v1, listed REPACK) = %+v, want one newer_revision at %+v", got, want)
	}
}

// TestCompareAuditAgreeOnRevision runs the same superseded and aligned fixtures through
// the daemon and the report, which must tell one story.
func TestCompareAuditAgreeOnRevision(t *testing.T) {
	tests := []struct {
		desc        string
		held        release.Revision
		entry       seadex.Entry
		wantStatus  Status
		wantVerdict audit.Verdict
	}{
		{"superseded", revOriginal, udfEntry("v2"), StatusNewerRevision, audit.VerdictOlderRevision},
		{"aligned at the listed revision", revV2, udfEntry("v2"), "", audit.VerdictBest},
		{"mixed pack superseded", revOriginal, mixedUDFEntry(), StatusNewerRevision, audit.VerdictOlderRevision},
		{"mixed pack aligned", revV2, mixedUDFEntry(), "", audit.VerdictBest},
	}
	for _, tc := range tests {
		m := udfSeasonMatch(tc.held, tc.entry)
		findings := comparer(filter.Options{}, false).Compare([]match.Match{m})
		var status Status
		if len(findings) == 1 {
			status = findings[0].Status
		}
		if len(findings) > 1 || status != tc.wantStatus {
			t.Errorf("%s: daemon findings = %+v, want status %q", tc.desc, findings, tc.wantStatus)
		}
		rep := audit.New(audit.Config{}).Audit([]match.Match{m}, nil, nil, nil)
		if len(rep.Rows) != 1 || rep.Rows[0].Verdict != tc.wantVerdict {
			t.Errorf("%s: audit rows = %+v, want one %q", tc.desc, rep.Rows, tc.wantVerdict)
			continue
		}
		if tc.wantVerdict == audit.VerdictOlderRevision && (rep.Rows[0].CurrentRevision != findings[0].CurrentRevision || rep.Rows[0].BestRevision != findings[0].RecommendedRevision) {
			t.Errorf("%s: audit revisions %+v/%+v differ from the finding's %+v/%+v", tc.desc,
				rep.Rows[0].CurrentRevision, rep.Rows[0].BestRevision, findings[0].CurrentRevision, findings[0].RecommendedRevision)
		}
	}
}
