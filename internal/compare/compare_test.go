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
	want := `42|better_release|a,b|x|hash1`
	if got != want {
		t.Errorf("dedupeKey() = %q, want %q", got, want)
	}

	swap := *f
	swap.InfoHash = "hash2"
	if dedupeKey(&swap) == got {
		t.Error("a new infoHash (same-group quality swap) must produce a different dedupe key")
	}

	// SeaDex redacts AB info hashes: two AB-only replacement torrents differing
	// only in their torrent page URL must not share a key, or the later
	// replacement would be suppressed as already alerted.
	abA := *f
	abA.InfoHash = "<redacted>"
	abA.ReleaseURL = "https://animebytes.tv/torrents.php?id=9&torrentid=10"
	abA.Links = []ReleaseLink{{Tracker: "AB", URL: abA.ReleaseURL}}
	abB := abA
	abB.ReleaseURL = "https://animebytes.tv/torrents.php?id=9&torrentid=11"
	abB.Links = []ReleaseLink{{Tracker: "AB", URL: abB.ReleaseURL}}
	if dedupeKey(&abA) == dedupeKey(&abB) {
		t.Error("redacted AB-only findings with different ReleaseURLs must produce different dedupe keys")
	}

	// Enabling AnimeBytes adds an AB link beside an unchanged public
	// representative: the key must change so the new source re-surfaces.
	publicOnly := *f
	publicOnly.Links = []ReleaseLink{{Tracker: "Nyaa", URL: "https://nyaa.si/view/1"}}
	withAB := publicOnly
	withAB.Links = []ReleaseLink{
		{Tracker: "Nyaa", URL: "https://nyaa.si/view/1"},
		{Tracker: "AB", URL: "https://animebytes.tv/torrents.php?id=9&torrentid=10"},
	}
	if dedupeKey(&publicOnly) == dedupeKey(&withAB) {
		t.Error("adding an AnimeBytes link must change the dedupe key")
	}

	// A public-only finding (non-redacted hash, no AB links) keeps the exact
	// pre-AB-aware, unescaped key shape for delimiter-free values, so existing
	// persisted dedupe state stays valid across upgrades.
	if k := dedupeKey(&publicOnly); k != want {
		t.Errorf("public-only dedupeKey() = %q, want unchanged %q", k, want)
	}
}

// TestDedupeKeyEscapesDelimiters pins the collision-proofing: an untrusted
// component containing the key grammar's ',' or '|' delimiters (or the '\'
// escape itself) cannot make two distinct findings share a key, which would
// suppress the second as already alerted.
func TestDedupeKeyEscapesDelimiters(t *testing.T) {
	base := Finding{AniListID: 42, Status: StatusBetter, InfoHash: "hash1"}

	// One group named "a,b" vs two groups "a" and "b": identical naive join.
	oneGroup := base
	oneGroup.RecommendedGroups = []string{"a,b"}
	twoGroups := base
	twoGroups.RecommendedGroups = []string{"a", "b"}
	if dedupeKey(&oneGroup) == dedupeKey(&twoGroups) {
		t.Error(`group "a,b" and groups "a","b" must not share a dedupe key`)
	}

	// A '|' inside a component must not shift the field boundary: group "x"
	// with identity "h|y" naively joins identically to group "x|h" with
	// identity "y".
	pipeInHash := base
	pipeInHash.CurrentGroup = "x"
	pipeInHash.InfoHash = "h|y"
	pipeInGroup := base
	pipeInGroup.CurrentGroup = "x|h"
	pipeInGroup.InfoHash = "y"
	if dedupeKey(&pipeInHash) == dedupeKey(&pipeInGroup) {
		t.Error(`("x", "h|y") and ("x|h", "y") must not share a dedupe key`)
	}

	// The escape character itself must be escaped or the mapping is not
	// injective: with delimiter-only escaping, ("x\", "y") and ("x", "|y")
	// both join to x\|y.
	trailingBackslash := base
	trailingBackslash.CurrentGroup = `x\`
	trailingBackslash.InfoHash = "y"
	leadingPipe := base
	leadingPipe.CurrentGroup = "x"
	leadingPipe.InfoHash = "|y"
	if dedupeKey(&trailingBackslash) == dedupeKey(&leadingPipe) {
		t.Error(`("x\", "y") and ("x", "|y") must not share a dedupe key (backslash must be escaped)`)
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
	if !strings.Contains(got[0].DedupeKey, `|erai-raws|`) {
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
	// A real season must be on disk: file presence is checked before the
	// recommendation-emptiness nudge, so a fileless item stays silent.
	item := &library.Item{Title: "X", Arr: library.ArrSonarr, Groups: []string{"whatever"}, SeasonGroups: map[int][]string{1: {"whatever"}}}
	entry := seadex.Entry{AniListID: 1, TheoreticalBest: "a stated remux"}
	m := match.Match{Item: item, Arr: "sonarr", Entry: entry, Record: mapping.Record{}}
	got := comparer(filter.Options{}, false).Compare([]match.Match{m})
	if len(got) != 1 || got[0].Status != StatusTheoretical || got[0].Severity != SevInfo {
		t.Fatalf("expected one theoretical_best/info finding, got %+v", got)
	}
}

func TestCompareSeasonNotOnDiskTheoreticalIsSilent(t *testing.T) {
	// File presence comes before the recommendation-emptiness check: a
	// theoretical-best-only entry whose mapped season is not on disk has
	// nothing the nudge could apply to, so the daemon stays quiet (the audit
	// records the same scope as no_file).
	item := &library.Item{Title: "Short", Groups: []string{"a"}, SeasonGroups: map[int][]string{1: {"a"}}}
	entry := seadex.Entry{AniListID: 404, TheoreticalBest: "a stated remux"}
	m := match.Match{Item: item, Arr: library.ArrSonarr, Entry: entry, Record: mapping.Record{SeasonTvdb: 3}}
	if got := comparer(filter.Options{}, false).Compare([]match.Match{m}); len(got) != 0 {
		t.Errorf("a theoretical-only entry for a season with no file must be silent, got %+v", got)
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

func TestCompareMixedGroupSeasonIsInfoNudge(t *testing.T) {
	// A mapped season whose episodes span two groups: the daemon cannot attribute
	// one current group, so it emits a mixed_group_manual info nudge with the
	// recommended fields filled for the manual review.
	item := &library.Item{Title: "Mixed", Groups: []string{"a", "b"}, SeasonGroups: map[int][]string{1: {"a", "b"}}}
	entry := seadex.Entry{AniListID: 400, Torrents: []seadex.Torrent{
		{IsBest: true, ReleaseGroup: "SubsPlease", Tracker: "Nyaa", URL: "https://nyaa.si/view/400"},
	}}
	m := match.Match{Item: item, Arr: library.ArrSonarr, Entry: entry, Record: mapping.Record{SeasonTvdb: 1}}
	got := comparer(filter.Options{}, false).Compare([]match.Match{m})
	if len(got) != 1 {
		t.Fatalf("expected 1 finding, got %d: %+v", len(got), got)
	}
	if got[0].Status != StatusMixedGroup || got[0].Severity != SevInfo {
		t.Errorf("status/severity = %q/%q, want mixed_group_manual/info", got[0].Status, got[0].Severity)
	}
	if got[0].RecommendedGroup != "SubsPlease" {
		t.Errorf("RecommendedGroup = %q, want SubsPlease (fillBest must run for the nudge)", got[0].RecommendedGroup)
	}
}

func TestCompareAlignedMixedGroupSeasonIsSilent(t *testing.T) {
	// Alignment wins over the mixed-group nudge: a season that spans two groups
	// but already carries a recommended one is aligned, so it must not nag as
	// mixed_group_manual (the audit reports the same row as have_best).
	item := &library.Item{Title: "Aligned Mixed", Groups: []string{"subsplease", "erai-raws"}, SeasonGroups: map[int][]string{1: {"subsplease", "erai-raws"}}}
	entry := seadex.Entry{AniListID: 405, Torrents: []seadex.Torrent{
		{IsBest: true, ReleaseGroup: "SubsPlease", Tracker: "Nyaa", URL: "https://nyaa.si/view/405"},
	}}
	m := match.Match{Item: item, Arr: library.ArrSonarr, Entry: entry, Record: mapping.Record{SeasonTvdb: 1}}
	if got := comparer(filter.Options{}, false).Compare([]match.Match{m}); len(got) != 0 {
		t.Errorf("an aligned multi-group season must produce no finding, got %+v", got)
	}
}

func TestCompareSeasonNotOnDiskIsSilent(t *testing.T) {
	// SeaDex maps the entry to season 3 but only season 1 is on disk: there is
	// nothing on disk a better release would replace, so the daemon stays quiet
	// (the audit report records this as no_file).
	item := &library.Item{Title: "Short", Groups: []string{"a"}, SeasonGroups: map[int][]string{1: {"a"}}}
	entry := seadex.Entry{AniListID: 401, Torrents: []seadex.Torrent{
		{IsBest: true, ReleaseGroup: "SubsPlease", Tracker: "Nyaa", URL: "https://nyaa.si/view/401"},
	}}
	m := match.Match{Item: item, Arr: library.ArrSonarr, Entry: entry, Record: mapping.Record{SeasonTvdb: 3}}
	if got := comparer(filter.Options{}, false).Compare([]match.Match{m}); len(got) != 0 {
		t.Errorf("a mapped season with no file must produce no finding, got %+v", got)
	}
}

func TestCompareIncompleteSeasonEntryIsInfo(t *testing.T) {
	// Season-scoped path (not whole-series): an incomplete SeaDex entry whose
	// recommendation the item lacks downgrades better_release to incomplete/info.
	item := &library.Item{Title: "Incomplete", Groups: []string{"erai-raws"}, SeasonGroups: map[int][]string{1: {"erai-raws"}}}
	entry := seadex.Entry{AniListID: 402, Incomplete: true, Torrents: []seadex.Torrent{
		{IsBest: true, ReleaseGroup: "SubsPlease", Tracker: "Nyaa", URL: "https://nyaa.si/view/402"},
	}}
	m := match.Match{Item: item, Arr: library.ArrSonarr, Entry: entry, Record: mapping.Record{SeasonTvdb: 1}}
	got := comparer(filter.Options{}, false).Compare([]match.Match{m})
	if len(got) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(got))
	}
	if got[0].Status != StatusIncomplete || got[0].Severity != SevInfo {
		t.Errorf("status/severity = %q/%q, want incomplete/info", got[0].Status, got[0].Severity)
	}
}

func TestRecommendedSkipsNonBestAndContentFiltered(t *testing.T) {
	// A non-best torrent is never a recommendation, and a remux best is dropped
	// by the content filter when exclude_remux is on. With nothing surviving and
	// the entry neither incomplete nor theoretical-best, the daemon is silent (a
	// release the operator filtered out is absent, never a finding).
	item := &library.Item{Title: "Filtered", Groups: []string{"erai-raws"}, SeasonGroups: map[int][]string{1: {"erai-raws"}}}
	entry := seadex.Entry{AniListID: 403, Notes: "BD remux", Torrents: []seadex.Torrent{
		{IsBest: false, ReleaseGroup: "AltGrp", Tracker: "Nyaa", URL: "https://nyaa.si/view/403"},
		{IsBest: true, ReleaseGroup: "RemuxGrp", Tracker: "Nyaa", URL: "https://nyaa.si/view/404"},
	}}
	m := match.Match{Item: item, Arr: library.ArrSonarr, Entry: entry, Record: mapping.Record{SeasonTvdb: 1}}
	got := comparer(filter.Options{ExcludeRemux: true}, false).Compare([]match.Match{m})
	if len(got) != 0 {
		t.Errorf("non-best and remux-filtered releases must yield no finding, got %+v", got)
	}
}

func TestCompareMislabeledAnimeBytesURLRequiresOptIn(t *testing.T) {
	// The tracker label is untrusted upstream data: a torrent claiming "Nyaa"
	// but carrying an animebytes.tv URL - absolute, schemeless, or host:port -
	// must be invisible while the AnimeBytes toggle is off (URL-aware guard on
	// the RAW upstream URL), and surface only when it is on.
	const absURL = "https://animebytes.tv/torrents.php?id=9&torrentid=10"
	for _, sneakyURL := range []string{
		absURL,
		"animebytes.tv/torrents.php?id=9&torrentid=10",
		"animebytes.tv:443/torrents.php?id=9&torrentid=10",
	} {
		t.Run(sneakyURL, func(t *testing.T) {
			item := &library.Item{Title: "Mislabeled", Groups: []string{"erai-raws"}, SeasonGroups: map[int][]string{1: {"erai-raws"}}}
			entry := seadex.Entry{AniListID: 500, Torrents: []seadex.Torrent{
				{IsBest: true, ReleaseGroup: "Sneaky", Tracker: "Nyaa", URL: sneakyURL},
			}}
			m := match.Match{Item: item, Arr: library.ArrSonarr, Entry: entry, Record: mapping.Record{SeasonTvdb: 1}}

			if got := comparer(filter.Options{}, false).Compare([]match.Match{m}); len(got) != 0 {
				t.Fatalf("AnimeBytes off must hide a mislabeled AB-URL recommendation, got %+v", got)
			}
			got := comparer(filter.Options{AnimeBytes: true}, false).Compare([]match.Match{m})
			if len(got) != 1 {
				t.Fatalf("AnimeBytes on should surface the recommendation, got %d", len(got))
			}
			if sneakyURL == absURL && got[0].ReleaseURL != absURL {
				t.Errorf("ReleaseURL = %q, want the AB URL", got[0].ReleaseURL)
			}
		})
	}
}

func TestCompareUnknownTrackerRecommendationIsSilent(t *testing.T) {
	item := &library.Item{Title: "Unknown Tracker Show", Groups: []string{"erai-raws"}, SeasonGroups: map[int][]string{1: {"erai-raws"}}}
	entry := seadex.Entry{AniListID: 600, Torrents: []seadex.Torrent{
		{IsBest: true, ReleaseGroup: "SubsPlease", Tracker: "SomePrivateTracker", URL: "https://tracker.example/view/1"},
	}}
	m := match.Match{Item: item, Arr: library.ArrSonarr, Entry: entry, Record: mapping.Record{SeasonTvdb: 1}}

	if got := comparer(filter.Options{AnimeBytes: true}, false).Compare([]match.Match{m}); len(got) != 0 {
		t.Errorf("an unknown-tracker recommendation is unobtainable and must be silent, got %+v", got)
	}
}

func TestObtainableLinksSkipsEmptyURL(t *testing.T) {
	cands := []candidate{
		{rel: release.Release{Tracker: "Nyaa"}, torrent: seadex.Torrent{Tracker: "Nyaa"}},
		{rel: release.Release{Tracker: "Nyaa"}, torrent: seadex.Torrent{Tracker: "Nyaa", URL: "https://nyaa.si/view/2"}},
	}

	links := obtainableLinks(cands)

	if len(links) != 1 || links[0].URL != "https://nyaa.si/view/2" {
		t.Errorf("obtainableLinks() = %+v, want only the URL-carrying link", links)
	}
}

func TestCompareFindingCarriesClassifiedReleaseFields(t *testing.T) {
	item := &library.Item{Title: "Payload", Groups: []string{"erai-raws"}, SeasonGroups: map[int][]string{1: {"erai-raws"}}}
	entry := seadex.Entry{AniListID: 700, Torrents: []seadex.Torrent{{
		IsBest:       true,
		ReleaseGroup: "SubsPlease",
		Tracker:      "Nyaa",
		URL:          "https://nyaa.si/view/700",
		InfoHash:     "deadbeef",
		DualAudio:    true,
		Files:        []seadex.File{{Name: "[SubsPlease] Payload - 01 [1080p][HEVC].mkv"}},
	}}}
	m := match.Match{Item: item, Arr: library.ArrSonarr, Entry: entry, Record: mapping.Record{SeasonTvdb: 1}}

	got := comparer(filter.Options{}, false).Compare([]match.Match{m})

	if len(got) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(got))
	}
	f := got[0]
	if f.Resolution != "1080p" {
		t.Errorf("Resolution = %q, want 1080p", f.Resolution)
	}
	if f.Codec != "x265" {
		t.Errorf("Codec = %q, want x265 (HEVC normalizes to x265)", f.Codec)
	}
	if f.Kind != string(release.KindEncode) {
		t.Errorf("Kind = %q, want %q", f.Kind, release.KindEncode)
	}
	if !f.DualAudio {
		t.Error("DualAudio must carry the SeaDex dual-audio flag onto the finding")
	}
	if f.InfoHash != "deadbeef" {
		t.Errorf("InfoHash = %q, want deadbeef", f.InfoHash)
	}
	if f.Reason == "" {
		t.Error("classification reason must be filled")
	}
}
