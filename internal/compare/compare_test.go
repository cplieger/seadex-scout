package compare

import (
	"reflect"
	"strings"
	"testing"

	"github.com/cplieger/seadex-scout/internal/filter"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/match"
	"github.com/cplieger/seadex-scout/internal/release"
	"github.com/cplieger/seadex-scout/internal/seadex"
)

func TestDedupeKey(t *testing.T) {
	f := &Finding{
		AniListID:         42,
		Status:            StatusBetter,
		RecommendedGroups: []string{"b", "a"},
		CurrentGroup:      "x",
		InfoHash:          "hash1",
	}
	got := dedupeKey(f)
	want := "42|better_release|a,b|x|hash1"
	if got != want {
		t.Errorf("dedupeKey() = %q, want %q", got, want)
	}

	swap := *f
	swap.InfoHash = "hash2"
	if dedupeKey(&swap) == got {
		t.Error("a new infoHash (same-group quality swap) must produce a different dedupe key")
	}
}

func TestIntersects(t *testing.T) {
	if !release.GroupsIntersect([]string{"SubsPlease"}, []string{"subsplease"}) {
		t.Error("GroupsIntersect must be case-insensitive via NormalizeGroup")
	}
	if release.GroupsIntersect([]string{"a"}, []string{"b"}) {
		t.Error("disjoint group sets must not intersect")
	}
}

func TestRepresentativePrefersResolutionThenPublic(t *testing.T) {
	higherRes := []candidate{
		{rel: release.Release{Resolution: "720p", TrackerType: release.TrackerPublic}},
		{rel: release.Release{Resolution: "1080p", TrackerType: release.TrackerPrivate}},
	}
	if rep := representative(higherRes); rep.rel.Resolution != "1080p" {
		t.Errorf("headline resolution = %q, want highest 1080p", rep.rel.Resolution)
	}

	tie := []candidate{
		{rel: release.Release{Resolution: "1080p", TrackerType: release.TrackerPrivate}},
		{rel: release.Release{Resolution: "1080p", TrackerType: release.TrackerPublic}},
	}
	if rep := representative(tie); rep.rel.TrackerType != release.TrackerPublic {
		t.Errorf("on a resolution tie the public tracker must win, got %q", rep.rel.TrackerType)
	}
}

func TestGroupSetNormalizesDedupesSorts(t *testing.T) {
	cands := []candidate{
		{rel: release.Release{Group: "SubsPlease"}},
		{rel: release.Release{Group: "subsplease"}},
		{rel: release.Release{Group: "Erai-raws"}},
	}
	got := groupSet(cands)
	want := []string{"erai-raws", "subsplease"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("groupSet() = %v, want %v", got, want)
	}
}

func TestObtainableLinksDedupesAndPrefixesPrivateURL(t *testing.T) {
	cands := []candidate{
		{rel: release.Release{Tracker: "Nyaa"}, torrent: seadex.Torrent{Tracker: "Nyaa", URL: "https://nyaa.si/view/1"}},
		{rel: release.Release{Tracker: "AB"}, torrent: seadex.Torrent{Tracker: "AB", URL: "/torrents.php?id=1"}},
		{rel: release.Release{Tracker: "Nyaa"}, torrent: seadex.Torrent{Tracker: "Nyaa", URL: "https://nyaa.si/view/1"}},
	}
	links := obtainableLinks(cands)
	if len(links) != 2 {
		t.Fatalf("expected 2 distinct links, got %d: %+v", len(links), links)
	}
	if links[1].URL != "https://animebytes.tv/torrents.php?id=1" {
		t.Errorf("AB relative URL not prefixed, got %q", links[1].URL)
	}
}

func comparer(opts filter.Options, excludeSpecials bool) *Comparer {
	return NewComparer(Config{Filter: opts, ExcludeSpecials: excludeSpecials})
}

func TestCompareAlignedProducesNoFinding(t *testing.T) {
	item := &library.Item{Title: "Frieren", Groups: []string{"subsplease"}, SeasonGroups: map[int][]string{1: {"subsplease"}}}
	entry := seadex.Entry{AniListID: 154587, Torrents: []seadex.Torrent{
		{IsBest: true, ReleaseGroup: "SubsPlease", Tracker: "Nyaa", URL: "https://nyaa.si/view/1"},
	}}
	m := match.Match{Item: item, Arr: "sonarr", Entry: entry, Record: mapping.Record{SeasonTvdb: 1}}
	if got := comparer(filter.Options{}, false).Compare([]match.Match{m}); len(got) != 0 {
		t.Errorf("aligned item must produce no finding, got %+v", got)
	}
}

func TestCompareBetterRelease(t *testing.T) {
	item := &library.Item{Title: "Frieren", Groups: []string{"erai-raws"}, SeasonGroups: map[int][]string{1: {"erai-raws"}}}
	entry := seadex.Entry{AniListID: 154587, Torrents: []seadex.Torrent{
		{IsBest: true, ReleaseGroup: "SubsPlease", Tracker: "Nyaa", URL: "https://nyaa.si/view/1"},
	}}
	m := match.Match{Item: item, Arr: "sonarr", Entry: entry, Record: mapping.Record{SeasonTvdb: 1}}
	got := comparer(filter.Options{}, false).Compare([]match.Match{m})
	if len(got) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(got))
	}
	if got[0].Status != StatusBetter || got[0].Severity != SevWarn {
		t.Errorf("status/severity = %q/%q, want better_release/warn", got[0].Status, got[0].Severity)
	}
	if got[0].RecommendedGroup != "SubsPlease" {
		t.Errorf("RecommendedGroup = %q, want SubsPlease", got[0].RecommendedGroup)
	}
}

func TestCompareSeasonScopedFindingSeed(t *testing.T) {
	// A series whose seasons carry different groups: season 1 SubsPlease, season 2
	// Erai-raws, and item.Groups holds both. A SeaDex season-2 best for SubsPlease
	// must seed the finding from ONLY season 2's group (erai-raws), not the
	// whole-series set, in both CurrentGroup and the dedupe key.
	item := &library.Item{
		Title:        "Two Cour Show",
		Groups:       []string{"subsplease", "erai-raws"},
		SeasonGroups: map[int][]string{1: {"subsplease"}, 2: {"erai-raws"}},
	}
	entry := seadex.Entry{AniListID: 200, Torrents: []seadex.Torrent{
		{IsBest: true, ReleaseGroup: "SubsPlease", Tracker: "Nyaa", URL: "https://nyaa.si/view/200"},
	}}
	m := match.Match{Item: item, Arr: library.ArrSonarr, Entry: entry, Record: mapping.Record{SeasonTvdb: 2}}
	got := comparer(filter.Options{}, false).Compare([]match.Match{m})
	if len(got) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(got))
	}
	if got[0].CurrentGroup != "erai-raws" {
		t.Errorf("CurrentGroup = %q, want season-scoped %q (not whole-series subsplease,erai-raws)", got[0].CurrentGroup, "erai-raws")
	}
	if !strings.Contains(got[0].DedupeKey, "|erai-raws|") {
		t.Errorf("DedupeKey = %q, want it to carry the season-scoped current group |erai-raws|", got[0].DedupeKey)
	}
}

func TestCompareSeasonScopedSingleGroupNotMixed(t *testing.T) {
	// A series that spans two groups across its seasons (item.Groups = lostyears,
	// pmr) but whose season 1 carries a single group (pmr). A season-1 SeaDex best
	// PMR release is aligned; the whole-series mixed-group flag must NOT trigger a
	// spurious mixed_group_manual finding for a season that is already aligned.
	item := &library.Item{
		Title:        "Split Group Show",
		Groups:       []string{"lostyears", "pmr"},
		SeasonGroups: map[int][]string{1: {"pmr"}},
		MixedGroups:  true,
	}
	entry := seadex.Entry{AniListID: 201, Torrents: []seadex.Torrent{
		{IsBest: true, ReleaseGroup: "PMR", Tracker: "Nyaa", URL: "https://nyaa.si/view/201"},
	}}
	m := match.Match{Item: item, Arr: library.ArrSonarr, Entry: entry, Record: mapping.Record{SeasonTvdb: 1}}
	if got := comparer(filter.Options{}, false).Compare([]match.Match{m}); len(got) != 0 {
		t.Errorf("season-1 aligned item must produce no finding (not mixed_group_manual), got %+v", got)
	}
}

func TestCompareSeasonScopedRecommendationNotMaskedByOtherSeason(t *testing.T) {
	// Season 2 lacks a recommended group even though season 1 has one: a finding
	// for season 2 must still be produced (the whole-series group set must not
	// mask a later season that still needs the release).
	item := &library.Item{
		Title:        "Two Cour Show",
		Groups:       []string{"subsplease", "erai-raws"},
		SeasonGroups: map[int][]string{1: {"subsplease"}, 2: {"erai-raws"}},
	}
	entry := seadex.Entry{AniListID: 202, Torrents: []seadex.Torrent{
		{IsBest: true, ReleaseGroup: "SubsPlease", Tracker: "Nyaa", URL: "https://nyaa.si/view/202"},
	}}
	m := match.Match{Item: item, Arr: library.ArrSonarr, Entry: entry, Record: mapping.Record{SeasonTvdb: 2}}
	got := comparer(filter.Options{}, false).Compare([]match.Match{m})
	if len(got) != 1 {
		t.Fatalf("season 2 missing a recommended group must produce a finding even when another season has it, got %d", len(got))
	}
	if got[0].Status != StatusBetter {
		t.Errorf("status = %q, want %q", got[0].Status, StatusBetter)
	}
}

func TestCompareTheoreticalBestIsInfo(t *testing.T) {
	item := &library.Item{Title: "X", Groups: []string{"whatever"}}
	entry := seadex.Entry{AniListID: 1, TheoreticalBest: "a stated remux"}
	m := match.Match{Item: item, Arr: "sonarr", Entry: entry, Record: mapping.Record{}}
	got := comparer(filter.Options{}, false).Compare([]match.Match{m})
	if len(got) != 1 || got[0].Status != StatusTheoretical || got[0].Severity != SevInfo {
		t.Fatalf("expected one theoretical_best/info finding, got %+v", got)
	}
}

func TestCompareSkipsNotInLibraryAndSpecials(t *testing.T) {
	notInLib := match.Match{Arr: "sonarr", Entry: seadex.Entry{AniListID: 1}}
	special := match.Match{
		Item:   &library.Item{Title: "OVA", Groups: []string{"x"}},
		Arr:    "sonarr",
		Entry:  seadex.Entry{AniListID: 2, Torrents: []seadex.Torrent{{IsBest: true, ReleaseGroup: "Y", Tracker: "Nyaa", URL: "https://nyaa.si/view/2"}}},
		Record: mapping.Record{Type: "OVA"},
	}
	got := comparer(filter.Options{}, true).Compare([]match.Match{notInLib, special})
	if len(got) != 0 {
		t.Errorf("not-in-library and excluded specials must be skipped, got %+v", got)
	}
}

func TestCompareAnimeBytesRecommendationRequiresOptIn(t *testing.T) {
	item := &library.Item{Title: "Private Tracker Show", Groups: []string{"erai-raws"}, SeasonGroups: map[int][]string{1: {"erai-raws"}}}
	entry := seadex.Entry{AniListID: 303, Torrents: []seadex.Torrent{
		{IsBest: true, ReleaseGroup: "SubsPlease", Tracker: "AB", URL: "/torrents.php?id=9&torrentid=10"},
	}}
	m := match.Match{Item: item, Arr: library.ArrSonarr, Entry: entry, Record: mapping.Record{SeasonTvdb: 1}}

	if got := comparer(filter.Options{}, false).Compare([]match.Match{m}); len(got) != 0 {
		t.Fatalf("AnimeBytes off must make AB-only recommendations silent, got %+v", got)
	}

	got := comparer(filter.Options{AnimeBytes: true}, false).Compare([]match.Match{m})
	if len(got) != 1 {
		t.Fatalf("AnimeBytes on should surface the AB recommendation, got %d", len(got))
	}
	if got[0].Status != StatusBetter || got[0].Severity != SevWarn {
		t.Errorf("status/severity = %q/%q, want better_release/warn", got[0].Status, got[0].Severity)
	}
	if got[0].Tracker != "AB" {
		t.Errorf("Tracker = %q, want AB", got[0].Tracker)
	}
	if got[0].ReleaseURL != "https://animebytes.tv/torrents.php?id=9&torrentid=10" {
		t.Errorf("ReleaseURL = %q, want AnimeBytes absolute URL", got[0].ReleaseURL)
	}
	if len(got[0].Links) != 1 || got[0].Links[0].URL != got[0].ReleaseURL {
		t.Errorf("Links = %+v, want the same obtainable AB release URL", got[0].Links)
	}
}
